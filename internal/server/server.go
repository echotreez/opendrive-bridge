// Package server is the Bridge REST API of whitepaper §4: a local HTTP surface
// in front of the SDK.
//
// Its reason for existing is not convenience. P0–P3 recorded 44 ways OpenDrive's
// API describes something other than what happened (docs/discrepancies.md), and
// this package is where those stop. Everything a user sees is written here, from
// the classification layer's verdict rather than from upstream's text, and the
// test for every message is the same: would somebody who has never heard of the
// OpenDrive API know what to do after reading it?
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/StormRealm/opendrive-bridge/internal/jobs"
	"github.com/StormRealm/opendrive-bridge/internal/keystore"
	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// Config describes the daemon's HTTP surface.
type Config struct {
	// Addr is the listen address. The default binds loopback only, which is
	// what makes an absent API key safe (§9.1).
	Addr string
	// APIKey authenticates callers. It is optional on loopback and required
	// everywhere else.
	APIKey string
	// Logger receives structured logs.
	Logger *slog.Logger
	// ReadHeaderTimeout bounds slow-header attacks; the body timeout is
	// deliberately absent, because an upload may legitimately take hours.
	ReadHeaderTimeout time.Duration
}

// DefaultAddr binds the loopback interface only.
const DefaultAddr = "127.0.0.1:7777"

// Auth is the slice of the SDK authenticator the daemon needs. It is an
// interface so that /v1/auth/status can be tested without a network.
type Auth interface {
	Identity() opendrive.Identity
	AuthState() opendrive.AuthState
	Login(ctx context.Context, username, password string) error
	Logout(ctx context.Context) error
}

// TokenReporter is implemented by an authenticator that can say when its
// current credential expires.
type TokenReporter interface {
	Token() (opendrive.Token, bool)
}

// QuotaReporter fetches account usage. It is separate from Auth because
// /v1/auth/status must answer without it when it cannot be had (§4.1).
type QuotaReporter interface {
	Info(ctx context.Context, opts ...opendrive.AccountInfoOptions) (*opendrive.AccountInfo, error)
}

// Server is the Bridge HTTP daemon.
type Server struct {
	cfg    Config
	log    *slog.Logger
	auth   Auth
	store  keystore.Store
	client *opendrive.Client
	engine *jobs.Engine
	router chi.Router
	http   *http.Server
}

// Option configures a Server.
type Option func(*Server)

// WithKeystore supplies the credential store, so that status can report which
// backend is in use and whether it is readable right now.
func WithKeystore(s keystore.Store) Option {
	return func(srv *Server) { srv.store = s }
}

// WithClient supplies the SDK client used for account and file operations.
func WithClient(c *opendrive.Client) Option {
	return func(srv *Server) { srv.client = c }
}

// WithJobEngine supplies the transfer engine that backs /v1/jobs.
func WithJobEngine(e *jobs.Engine) Option {
	return func(srv *Server) { srv.engine = e }
}

// New builds the daemon.
//
// It refuses to start a non-loopback listener without an API key. That refusal
// is the point: a bridge holds a password that unlocks somebody's entire cloud
// storage, and a daemon that binds 0.0.0.0 with no key hands it to the network.
// Failing to start is recoverable in a way that a silent exposure is not.
func New(cfg Config, auth Auth, opts ...Option) (*Server, error) {
	if auth == nil {
		return nil, errors.New("the bridge needs an authenticator")
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(discardHandler{})
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = 15 * time.Second
	}

	loopback := isLoopback(cfg.Addr)
	if !loopback && cfg.APIKey == "" {
		return nil, fmt.Errorf("refusing to listen on %s without an API key: "+
			"anything that can reach that address could use your OpenDrive account. "+
			"Set an API key, or bind %s instead", cfg.Addr, DefaultAddr)
	}

	srv := &Server{cfg: cfg, log: cfg.Logger, auth: auth}
	for _, o := range opts {
		o(srv)
	}
	srv.router = srv.routes(!loopback)
	srv.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	return srv, nil
}

// Handler exposes the router, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes(keyRequired bool) chi.Router {
	r := chi.NewRouter()
	r.Use(requestID(s.log))
	r.Use(recoverPanic)
	r.Use(accessLog)

	r.Route("/v1", func(r chi.Router) {
		r.Use(apiKeyAuth(s.cfg.APIKey, keyRequired))

		r.Get("/health", s.handleHealth)
		r.Route("/auth", func(r chi.Router) {
			r.Post("/login", s.handleLogin)
			r.Post("/logout", s.handleLogout)
			r.Get("/status", s.handleStatus)
		})
	})

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, &RequestError{
			Code: string(opendrive.KindNotFound), HTTP: http.StatusNotFound,
			Message: "This bridge has no endpoint at that address. " +
				"See docs/bridge-openapi.yaml for the ones it does have.",
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, &RequestError{
			Code: string(opendrive.KindInvalidRequest), HTTP: http.StatusMethodNotAllowed,
			Message: "That endpoint does not accept this HTTP method.",
		})
	})
	return r
}

// ListenAndServe runs until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", s.cfg.Addr, err)
	}
	s.log.Info("bridge listening",
		slog.String("addr", ln.Addr().String()),
		slog.Bool("api_key", s.cfg.APIKey != ""),
		slog.Bool("loopback_only", isLoopback(s.cfg.Addr)))

	errc := make(chan error, 1)
	go func() {
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdown)
	}
}

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		if log := loggerFrom(r.Context()); log != nil {
			log.Warn("cannot write the response body", slog.String("error", err.Error()))
		}
	}
}

// decodeJSON reads a request body, answering a malformed one in words the
// sender can act on.
func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return BadRequest("The request body is not the JSON this endpoint expects: " + err.Error())
	}
	return nil
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
