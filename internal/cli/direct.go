package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/StormRealm/opendrive-bridge/internal/cache"
	"github.com/StormRealm/opendrive-bridge/internal/jobs"
	"github.com/StormRealm/opendrive-bridge/internal/keystore"
	"github.com/StormRealm/opendrive-bridge/internal/server"
	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// --direct runs the bridge inside odctl, for a machine with no daemon.
//
// It does not reimplement anything: it builds the same server, and reaches it
// through a transport that calls the handler instead of opening a socket. That
// is the point — a second code path would be a second set of behaviours to keep
// in step, and the wording, the error mapping and the discrepancy handling all
// live in `internal/server`. Direct mode and daemon mode therefore cannot
// disagree about anything, because they are the same code.
//
// What differs is lifetime: the transfers run inside this process, so odctl has
// to wait for them, and a cancelled command reclaims its own upstream records
// before exiting (D39).

// directTransport dispatches to an http.Handler in-process.
type directTransport struct{ handler http.Handler }

func (d directTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := newResponseRecorder()
	d.handler.ServeHTTP(rec, r)
	return rec.result(r), nil
}

// directBridge is the in-process daemon plus what it needs shutting down.
type directBridge struct {
	client *Client
	stop   func()
	once   sync.Once
}

func (d *directBridge) Close() {
	d.once.Do(func() {
		if d.stop != nil {
			d.stop()
		}
	})
}

// newDirectBridge builds the whole daemon in memory.
// openKeystore is a variable so tests can drive both branches — a machine with a
// vault and one without — on any runner. Which files a platform compiles is not
// something a coverage gate should depend on.
var openKeystore = func() (keystore.Store, error) { return keystore.Open(keystore.Config{}) }

func newDirectBridge(ctx context.Context, o *Options) (*directBridge, error) {
	store, err := openKeystore()
	if err != nil {
		// The credential store is the reason --direct can work at all: it holds
		// the login the daemon would otherwise be keeping warm.
		return nil, &APIError{Code: "keystore_unavailable", HTTP: 503, Message: err.Error()}
	}

	pathCache := cache.NewPathCache()
	sdk, err := opendrive.New(
		opendrive.WithLogger(slog.New(slog.NewTextHandler(o.Err(), &slog.HandlerOptions{
			Level: slog.LevelError,
		}))),
		opendrive.WithPathCache(pathCache),
	)
	if err != nil {
		return nil, err
	}
	auth := opendrive.NewOAuth2(sdk, opendrive.WithCredentialStore(store))
	sdk.SetAuthenticator(auth)
	// Best effort: a locked keyring is reported by the command that needs it,
	// not by refusing to start.
	_ = auth.EnsureFresh(ctx)

	engine, err := jobs.New(sdk, jobs.WithStore(jobs.NewMemoryStore()))
	if err != nil {
		return nil, err
	}
	engineCtx, cancel := context.WithCancel(ctx)
	engine.Start(engineCtx)

	srv, err := server.New(server.Config{Addr: server.DefaultAddr}, auth,
		server.WithKeystore(store),
		server.WithClient(sdk),
		server.WithPathCache(pathCache),
		server.WithJobEngine(engine),
	)
	if err != nil {
		cancel()
		return nil, err
	}

	c := NewClient("http://odctl-direct", "", o.Timeout)
	c.hc = &http.Client{Transport: directTransport{handler: srv.Handler()}, Timeout: c.hc.Timeout}

	return &directBridge{
		client: c,
		stop: func() {
			// Stopping cancels anything still transferring, which reclaims the
			// records those transfers created (D39).
			engine.Stop()
			cancel()
		},
	}, nil
}

// responseRecorder is enough of an http.ResponseWriter to turn a handler call
// into a response the standard client can read.
//
// It buffers rather than streams. Direct mode is for one command at a time on a
// machine with no daemon, so holding a response in memory is acceptable in a way
// it would not be for the daemon — which is exactly why the daemon streams and
// this does not.
type responseRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: http.Header{}}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(b)
}

// Flush is a no-op: the body is already in memory.
func (r *responseRecorder) Flush() {}

func (r *responseRecorder) result(req *http.Request) *http.Response {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return &http.Response{
		StatusCode:    r.code,
		Status:        fmt.Sprintf("%d %s", r.code, http.StatusText(r.code)),
		Header:        r.header,
		Body:          io.NopCloser(bytes.NewReader(r.body.Bytes())),
		Request:       req,
		ContentLength: int64(r.body.Len()),
	}
}
