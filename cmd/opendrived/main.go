// opendrived is the OpenDrive Bridge daemon: a local REST API in front of
// OpenDrive.com (whitepaper §4).
//
// It binds loopback by default. An API key is optional; listening anywhere else
// without one is allowed and logged as a warning, because the bridge holds a
// password that unlocks somebody's entire cloud storage.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/cache"
	"github.com/echotreez/opendrive-bridge/internal/datacache"
	"github.com/echotreez/opendrive-bridge/internal/jobs"
	"github.com/echotreez/opendrive-bridge/internal/keystore"
	"github.com/echotreez/opendrive-bridge/internal/server"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// Set by the linker at release time (see .goreleaser.yaml) so that a binary
// can be traced back to the commit it came from.
var (
	version   = "0.0.0-dev"
	commit    = "none"
	buildDate = "unknown"
)

type options struct {
	addr      string
	apiKey    string
	backend   string
	statePath string
	stateDir  string
	ephemeral bool
	logLevel  string
	showVer   bool

	// The datacache block of §3.4. Named for that block and not for the metadata
	// cache, which has no flags of its own — §3.5.4 asks for the two not to share
	// a vocabulary, and a flag called --cache-dir that meant the metadata cache
	// would be exactly the confusion it warns about.
	cacheDir       string
	cacheMaxBytes  int64
	cacheMaxDirty  int64
	cacheWriteBack bool
	drainTimeout   time.Duration

	// The S3 gateway (§3.6). It runs whenever the caching gateway does, with
	// write-back, unless switched off: it is the reason most people run this.
	s3          bool
	s3HTTPSAddr string
	s3HTTPAddr  string
	s3TLSCert   string
	s3TLSKey    string
	s3TLSHosts  string
	s3Delete    string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "opendrived:", err)
		os.Exit(1)
	}
}

func run() error {
	var o options
	fs := flag.NewFlagSet("opendrived", flag.ContinueOnError)
	// ODB_LISTEN is what §8.3 uses to configure a container, where passing a
	// flag means rewriting the image's command line.
	fs.StringVar(&o.addr, "addr", envOr("ODB_LISTEN", server.DefaultAddr),
		"listen address, or set ODB_LISTEN; beyond loopback, set an API key too")
	fs.StringVar(&o.apiKey, "api-key", os.Getenv("ODB_API_KEY"),
		"API key callers must present; unset means none is required")
	fs.StringVar(&o.backend, "keystore", envOr("ODB_KEYSTORE", string(keystore.BackendAuto)),
		"credential store: auto, encrypted_file or ephemeral (or set ODB_KEYSTORE)")
	fs.StringVar(&o.statePath, "keystore-file", os.Getenv("ODB_KEYSTORE_FILE"),
		"path to the encrypted credential file, when that backend is used")
	fs.StringVar(&o.stateDir, "state-dir", os.Getenv("ODB_STATE_DIR"),
		"where transfer job state is kept (or set ODB_STATE_DIR)")
	fs.BoolVar(&o.ephemeral, "ephemeral", false,
		"accept losing credentials on restart; required for the ephemeral store")
	fs.StringVar(&o.logLevel, "log-level", envOr("ODB_LOG_LEVEL", "info"),
		"debug, info, warn or error, or set ODB_LOG_LEVEL")
	fs.BoolVar(&o.showVer, "version", false, "print the version and exit")
	// The caching gateway (§3.5). Off unless a directory is given, because it is
	// the one feature here that holds the user's data and turning that on should
	// be somebody's decision rather than a default they discover afterwards.
	fs.StringVar(&o.cacheDir, "cache-dir", os.Getenv("ODB_CACHE_DIR"),
		"directory for the caching gateway; empty switches it off (or set ODB_CACHE_DIR)")
	fs.Int64Var(&o.cacheMaxBytes, "cache-max-bytes", envInt64("ODB_CACHE_MAX_BYTES", 0),
		"total the cache may occupy, in bytes; 0 uses the default")
	fs.Int64Var(&o.cacheMaxDirty, "cache-max-dirty-bytes", envInt64("ODB_CACHE_MAX_DIRTY_BYTES", 0),
		"most data the cache may hold that OpenDrive does not have yet; 0 uses the default")
	fs.BoolVar(&o.cacheWriteBack, "cache-write-back", envBool("ODB_CACHE_WRITE_BACK", true),
		"accept writes into the cache and upload them in the background; false writes straight through")
	fs.DurationVar(&o.drainTimeout, "cache-drain-timeout", 0,
		"how long shutdown waits for the cache to finish uploading; 0 uses the default")
	fs.BoolVar(&o.s3, "s3", envBool("ODB_S3", true),
		"serve S3 when the caching gateway is on with write-back (or set ODB_S3)")
	fs.StringVar(&o.s3HTTPSAddr, "s3-https-addr", envOr("ODB_S3_HTTPS_LISTEN", "0.0.0.0:9751"),
		"S3 over HTTPS, reachable from the network; empty switches it off (or set ODB_S3_HTTPS_LISTEN)")
	fs.StringVar(&o.s3HTTPAddr, "s3-http-addr", envOr("ODB_S3_HTTP_LISTEN", "127.0.0.1:9752"),
		"S3 over plain HTTP, loopback only; empty switches it off (or set ODB_S3_HTTP_LISTEN)")
	fs.StringVar(&o.s3TLSCert, "s3-tls-cert", os.Getenv("ODB_S3_TLS_CERT"),
		"certificate for S3 HTTPS; unset makes and uses a self-signed one (or set ODB_S3_TLS_CERT)")
	fs.StringVar(&o.s3TLSKey, "s3-tls-key", os.Getenv("ODB_S3_TLS_KEY"),
		"private key for --s3-tls-cert (or set ODB_S3_TLS_KEY)")
	fs.StringVar(&o.s3TLSHosts, "s3-tls-hosts", os.Getenv("ODB_S3_TLS_HOSTS"),
		"extra names or addresses, comma separated, for the self-signed certificate (or set ODB_S3_TLS_HOSTS)")
	fs.StringVar(&o.s3Delete, "s3-delete", envOr("ODB_S3_DELETE", "trash"),
		"what an S3 delete does: trash (recoverable) or permanent (or set ODB_S3_DELETE)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if o.showVer {
		fmt.Printf("opendrived %s (commit %s, built %s)\n", version, commit, buildDate)
		return nil
	}

	log := newLogger(o.logLevel)

	// The credential store comes first. Its failure mode is "no persistence =
	// configuration error" (§9.2): a bridge that starts, looks configured, and
	// forgets everything on reboot is worse than one that refuses to start.
	store, err := keystore.Open(keystore.Config{
		Backend:        keystore.Backend(o.backend),
		Path:           o.statePath,
		AllowEphemeral: o.ephemeral,
	})
	if err != nil {
		return err
	}
	log.Info("credential store ready", slog.String("backend", string(store.Backend())))

	// The Bridge's own API key is optional (1.3). Set one with --api-key or
	// ODB_API_KEY and every caller must present it; leave it unset and the API is
	// open to whatever can reach the port — which, with the default loopback
	// address or a port published to 127.0.0.1, is this machine only.
	apiKeyConfigured := o.apiKey != ""

	// One cache, shared: the client resolves paths through it and the server
	// drops what a write invalidated (§10.3).
	pathCache := cache.NewPathCache()
	clientOpts := []opendrive.Option{
		opendrive.WithLogger(log),
		opendrive.WithPathCache(pathCache),
	}
	// ODB_BASE_URL points the daemon at something other than the live API. It
	// exists for the release smoke test, which has to prove a freshly built
	// binary runs on its target platform without depending on somebody's
	// account — and it is the same seam anyone would need to test against a
	// staging deployment.
	if base := os.Getenv("ODB_BASE_URL"); base != "" {
		log.Warn("using a non-default OpenDrive address", slog.String("base_url", base))
		clientOpts = append(clientOpts, opendrive.WithBaseURL(base))
	}
	client, err := opendrive.New(clientOpts...)
	if err != nil {
		return err
	}
	auth := opendrive.NewOAuth2(client, opendrive.WithCredentialStore(store))
	client.SetAuthenticator(auth)

	// Loading the stored credentials is best effort: a credential that cannot be used is
	// reported by /v1/auth/status rather than being a reason not to start. The
	// daemon has to be reachable precisely when something is wrong with its
	// credentials — that is when somebody runs `odctl status`.
	if err := auth.EnsureFresh(context.Background()); err != nil {
		switch {
		case errors.Is(err, opendrive.ErrNoCredentials) || auth.AuthState() == opendrive.StateNotConfigured:
			// Nobody has signed in yet. That is the normal first start (1.3), not a
			// fault, and the log is where a container user looks first — so it says
			// what to do rather than sounding like something broke.
			log.Info("not signed in yet: open the web page at /ui and sign in, or run `odctl login <username>`")
		case auth.AuthState() == opendrive.StateReauthRequired:
			log.Warn("OpenDrive no longer accepts the saved password: sign in again on the web page at /ui, " +
				"or run `odctl login <username>`")
		default:
			log.Warn("could not resume the stored session; /v1/auth/status has the detail",
				slog.String("error", opendrive.RedactString(err.Error())))
		}
	}

	// The caching gateway (§3.5), when a directory was given. It is opened before
	// the engine so that transfers can go through it, and before the server so that
	// a directory it cannot use is a refusal to start rather than a feature that
	// quietly is not there.
	dc, err := newDataCache(o, client, log)
	if err != nil {
		return err
	}

	engine, err := newEngine(client, o, log, datacache.ForJobs(dc))
	if err != nil {
		return err
	}

	srvOpts := []server.Option{
		server.WithKeystore(store),
		server.WithClient(client),
		server.WithPathCache(pathCache),
		server.WithJobEngine(engine),
	}
	if dc != nil {
		srvOpts = append(srvOpts, server.WithDataCache(dc))
	}
	srv, err := server.New(server.Config{
		Addr:             o.addr,
		APIKey:           o.apiKey,
		APIKeyConfigured: apiKeyConfigured,
		Logger:           log,
	}, auth, srvOpts...)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := startS3(ctx, o, store, dc, srv, log); err != nil {
		return err
	}

	engine.Start(ctx)
	// Anything caught mid-transfer by the last shutdown is requeued, and the
	// upstream records those transfers left behind are reclaimed (D39).
	if err := engine.Recover(ctx); err != nil {
		log.Error("job recovery reported a problem", slog.String("error", err.Error()))
	}

	go func() {
		<-ctx.Done()
		log.Info("shutting down; cancelling transfers and reclaiming their records")
		engine.Stop()
	}()

	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	// Rule 4 of §3.5.2: the drain happens after the listener has stopped, so that
	// nothing new arrives while it works, and before the process exits. A drain
	// that ran in a goroutine alongside the shutdown would be racing the exit it
	// is supposed to delay.
	//
	// It uses a fresh context on purpose. ctx is already cancelled — that is why
	// we are here — and handing a cancelled context to the drain would make it
	// give up instantly and report everything as unfinished, which is the opposite
	// of what SIGTERM is supposed to achieve.
	if dc != nil {
		drainCtx, cancelDrain := context.WithCancel(context.Background())
		if err := dc.Drain(drainCtx); err != nil {
			log.Error("the cache could not finish uploading before shutdown; the objects it "+
				"could not send are listed above and their data is still on disk",
				slog.String("error", err.Error()))
		}
		cancelDrain()
		if err := dc.Close(); err != nil {
			log.Error("closing the cache reported a problem", slog.String("error", err.Error()))
		}
	}
	log.Info("stopped")
	return nil
}

// newDataCache opens the caching gateway, or returns nil when it is switched off.
func newDataCache(o options, c *opendrive.Client, log *slog.Logger) (*datacache.DataCache, error) {
	if o.cacheDir == "" {
		return nil, nil
	}
	dc, err := datacache.Open(datacache.Config{
		Dir:           o.cacheDir,
		MaxBytes:      o.cacheMaxBytes,
		MaxDirtyBytes: o.cacheMaxDirty,
		WriteBack:     o.cacheWriteBack,
		DrainTimeout:  o.drainTimeout,
		// An object can arrive with no folder id — written while OpenDrive was
		// unreachable, or under folders nobody created yet — and the flusher
		// makes the path when it can reach upstream (§3.6.4). Without this the
		// object would sit dirty for ever.
		Upstream: datacache.NewSDKUploader(c).WithFolderResolver(
			func(ctx context.Context, remotePath string) (string, error) {
				parent, _ := opendrive.ParentPath(remotePath)
				return c.Folders().EnsurePath(ctx, parent)
			}),
		Logger: log,
	})
	if err != nil {
		return nil, err
	}
	st := dc.Status()
	log.Info("caching gateway ready",
		slog.String("datacache_dir", st.Dir),
		slog.Bool("datacache_write_back", st.WriteBack),
		slog.Int64("datacache_max_bytes", st.MaxBytes),
		slog.Int64("datacache_max_dirty_bytes", st.MaxDirtyBytes),
		slog.Int("datacache_objects", st.Objects),
		slog.Int("datacache_dirty_objects", st.DirtyObjects))
	return dc, nil
}

// startS3 starts the S3 gateway when it should run (§3.6). A misconfiguration
// the user can fix — a certificate that will not load, an unknown delete mode —
// stops the daemon with a sentence. A credential file that cannot be read does
// not: the daemon must still start so that somebody can sign in again, and the
// log and /v1/s3 say the gateway is off and why.
func startS3(ctx context.Context, o options, store keystore.Store, dc *datacache.DataCache,
	srv *server.Server, log *slog.Logger) error {
	if !o.s3 {
		return nil
	}
	if dc == nil || !dc.Status().WriteBack {
		log.Info("the S3 gateway is off: it needs the caching gateway with write-back (--cache-dir)")
		return nil
	}
	var permanent bool
	switch o.s3Delete {
	case "trash", "":
	case "permanent":
		permanent = true
	default:
		return fmt.Errorf("--s3-delete must be trash or permanent, not %q", o.s3Delete)
	}
	sec, ok := store.(keystore.Secrets)
	if !ok {
		log.Error("the S3 gateway is off: the credential store cannot hold its keys")
		return nil
	}
	creds, created, err := server.LoadOrCreateS3Credentials(ctx, sec)
	if err != nil {
		log.Error("the S3 gateway is off: its keys could not be read or saved",
			slog.String("error", err.Error()))
		return nil
	}
	if created {
		log.Info("made S3 access keys for this bridge; see them with `odctl s3` or on the web page")
	}

	// Everything the gateway keeps lives beside the cache: multipart uploads in
	// progress and the certificate.
	base := filepath.Dir(dc.Status().Dir)
	var hosts []string
	for _, h := range strings.Split(o.s3TLSHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	var tlsCfg *tls.Config
	var fingerprint string
	var custom bool
	if o.s3HTTPSAddr != "" {
		tlsCfg, fingerprint, custom, err = server.S3TLS(base, o.s3TLSCert, o.s3TLSKey, hosts)
		if err != nil {
			return err
		}
	}
	gw, err := srv.NewS3Gateway(server.S3Config{
		PermanentDelete: permanent,
		StagingDir:      filepath.Join(base, "s3-multipart"),
	}, creds)
	if err != nil {
		return err
	}
	go func() {
		if err := gw.Serve(ctx, o.s3HTTPSAddr, o.s3HTTPAddr, tlsCfg, fingerprint, custom); err != nil {
			log.Error("the S3 gateway stopped; the rest of the bridge is still running",
				slog.String("error", err.Error()))
		}
	}()
	return nil
}

// envInt64 reads an integer from the environment, ignoring anything unparseable:
// a malformed value falls back to the default rather than stopping the daemon over
// a number.
func envInt64(name string, fallback int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

// envBool reads a boolean from the environment.
func envBool(name string, fallback bool) bool {
	if v := os.Getenv(name); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// envOr reads an environment variable with a fallback, so a container can be
// configured without a command line.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// defaultStateDir is jobs/ beside the binary, for the same reason credentials.key is there
// (§8.2): everything a user backs up or deletes is in the folder they unpacked.
//
// Until 1.2 this was the per-user configuration directory, left behind when v1.1
// moved the credentials into the folder. Walking first-run.md on a fresh unpack
// found it: the new install's web page listed transfers from every install that
// had ever run on the machine, and deleting the folder did not uninstall it.
func defaultStateDir() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Join(filepath.Dir(exe), "jobs")
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "opendrive-bridge", "jobs")
}

func newEngine(c *opendrive.Client, o options, log *slog.Logger, cache *datacache.JobCache) (*jobs.Engine, error) {
	dir := o.stateDir
	if dir == "" {
		dir = defaultStateDir()
	}
	store, err := jobs.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	opts := []jobs.Option{jobs.WithStore(store), jobs.WithLogger(log)}
	// A nil adapter means no gateway, and the engine behaves as it did before v1.2.
	if cache != nil {
		opts = append(opts, jobs.WithCache(cache))
	}
	return jobs.New(c, opts...)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Belt and braces over §9.4: every string that reaches a log goes
			// through redaction, whatever the call site did.
			if a.Value.Kind() == slog.KindString {
				a.Value = slog.StringValue(opendrive.RedactString(a.Value.String()))
			}
			return a
		},
	})).With(slog.String("version", version), slog.Time("started", time.Now()))
}
