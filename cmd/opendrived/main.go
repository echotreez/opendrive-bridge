// opendrived is the OpenDrive Bridge daemon: a local REST API in front of
// OpenDrive.com (whitepaper §4).
//
// It binds loopback by default and refuses to listen anywhere else without an
// API key, because it holds a password that unlocks somebody's entire cloud
// storage.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
		"listen address, or set ODB_LISTEN; anything other than loopback requires an API key")
	fs.StringVar(&o.apiKey, "api-key", os.Getenv("ODB_API_KEY"),
		"API key callers must present; generated into .env on first run if unset")
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

	// The Bridge's own API key comes from the credential store, which generates
	// one on first run and keeps it (§9.2.2). The user never types or manages
	// it. A key given on the command line or in the environment still wins, for
	// the case where somebody is driving the bridge from a configuration
	// management system that owns its own secrets.
	// Whether the key was the user's choice is remembered, because two rules
	// hang off it: a key the user gave us is enforced everywhere, while one we
	// generated must not shut the local CLI out (§9.1) and is not enough on its
	// own to justify listening on a public address (server.New says why).
	apiKeyConfigured := o.apiKey != ""
	if o.apiKey == "" {
		type apiKeyer interface {
			APIKey(context.Context) (string, error)
		}
		if k, ok := store.(apiKeyer); ok {
			key, keyErr := k.APIKey(context.Background())
			if keyErr != nil {
				return keyErr
			}
			o.apiKey = key
		}
	}

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

	// Loading the stored credentials is best effort: a locked keyring is
	// reported by /v1/auth/status rather than being a reason not to start. The
	// daemon has to be reachable precisely when something is wrong with its
	// credentials — that is when somebody runs `odctl status`.
	if err := auth.EnsureFresh(context.Background()); err != nil {
		log.Warn("could not resume the stored session; /v1/auth/status has the detail",
			slog.String("error", opendrive.RedactString(err.Error())))
	}

	engine, err := newEngine(client, o, log)
	if err != nil {
		return err
	}

	// The caching gateway (§3.5), when a directory was given. It is opened before
	// the server so that a directory it cannot use is a refusal to start rather
	// than a feature that quietly is not there.
	dc, err := newDataCache(o, client, log)
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
		Upstream:      datacache.NewSDKUploader(c),
		Logger:        log,
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

func newEngine(c *opendrive.Client, o options, log *slog.Logger) (*jobs.Engine, error) {
	dir := o.stateDir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "opendrive-bridge", "jobs")
	}
	store, err := jobs.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	return jobs.New(c, jobs.WithStore(store), jobs.WithLogger(log))
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
