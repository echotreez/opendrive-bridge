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
	"syscall"
	"time"

	"github.com/StormRealm/opendrive-bridge/internal/cache"
	"github.com/StormRealm/opendrive-bridge/internal/jobs"
	"github.com/StormRealm/opendrive-bridge/internal/keystore"
	"github.com/StormRealm/opendrive-bridge/internal/server"
	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

var version = "0.0.0-dev"

type options struct {
	addr      string
	apiKey    string
	backend   string
	statePath string
	stateDir  string
	ephemeral bool
	logLevel  string
	showVer   bool
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
	fs.StringVar(&o.addr, "addr", server.DefaultAddr,
		"listen address; anything other than loopback requires an API key")
	fs.StringVar(&o.apiKey, "api-key", os.Getenv("ODB_API_KEY"),
		"API key callers must present (or set ODB_API_KEY)")
	fs.StringVar(&o.backend, "keystore", string(keystore.BackendAuto),
		"credential store: auto, keyring, encrypted_file or ephemeral")
	fs.StringVar(&o.statePath, "keystore-file", "",
		"path to the encrypted credential file, when that backend is used")
	fs.StringVar(&o.stateDir, "state-dir", "", "where transfer job state is kept")
	fs.BoolVar(&o.ephemeral, "ephemeral", false,
		"accept losing credentials on restart; required for the ephemeral store")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&o.showVer, "version", false, "print the version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if o.showVer {
		fmt.Println("opendrived", version)
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

	// One cache, shared: the client resolves paths through it and the server
	// drops what a write invalidated (§10.3).
	pathCache := cache.NewPathCache()
	client, err := opendrive.New(
		opendrive.WithLogger(log),
		opendrive.WithPathCache(pathCache),
	)
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

	srv, err := server.New(server.Config{
		Addr:   o.addr,
		APIKey: o.apiKey,
		Logger: log,
	}, auth,
		server.WithKeystore(store),
		server.WithClient(client),
		server.WithPathCache(pathCache),
		server.WithJobEngine(engine),
	)
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
	log.Info("stopped")
	return nil
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
