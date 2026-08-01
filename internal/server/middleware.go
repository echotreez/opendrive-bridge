package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxLogger
)

// RequestIDHeader is the header the daemon echoes so an operator can join a
// client's report to a log line.
const RequestIDHeader = "X-Request-Id"

// requestID assigns each request an id and puts a logger carrying it into the
// context, so every line about one request can be found together.
func requestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" || len(id) > 64 {
				id = newID()
			}
			log := base.With(slog.String("request_id", id))
			ctx := context.WithValue(r.Context(), ctxRequestID, id)
			ctx = context.WithValue(ctx, ctxLogger, log)
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func loggerFrom(ctx context.Context) *slog.Logger {
	l, _ := ctx.Value(ctxLogger).(*slog.Logger)
	return l
}

// RequestIDFrom returns the id assigned to this request, if any.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// statusRecorder remembers what was written, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush lets a streaming download reach the client as it arrives.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLog records one line per request.
//
// The URL is redacted before it is written. Bridge requests are path-addressed
// and carry no tokens today, but §9.4 is absolute and the one time a credential
// reaches a log is the time it matters — an access token leaked into a test log
// through exactly this kind of oversight earlier in the project.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		log := loggerFrom(r.Context())
		if log == nil {
			return
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Info("request",
			slog.String("method", r.Method),
			slog.String("path", opendrive.RedactString(r.URL.Path)),
			slog.String("query", opendrive.RedactURL("?"+r.URL.RawQuery)),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("took", time.Since(start)))
	})
}

// recoverPanic turns a handler panic into the standard envelope rather than a
// dropped connection, and never shows the panic to the client.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if log := loggerFrom(r.Context()); log != nil {
					log.Error("handler panicked", slog.Any("panic", v))
				}
				WriteError(w, r, &RequestError{
					Code: string(opendrive.KindUpstreamError), HTTP: http.StatusInternalServerError,
					Message: "The bridge hit an internal error handling that request. " +
						"Nothing was changed. If it keeps happening, the daemon log has the detail.",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// apiKeyAuth enforces the bridge's own API key.
//
// The rule of §9.1: on loopback the key is optional, because the operating
// system is already deciding who may connect. On any other address it is
// mandatory, and a daemon configured to listen publicly without one refuses to
// start rather than silently exposing an account (see New).
func apiKeyAuth(key string, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if key == "" && !required {
				next.ServeHTTP(w, r)
				return
			}
			presented := presentedKey(r)
			if presented == "" {
				WriteError(w, r, Unauthorized("This bridge requires an API key. "+
					"Send it as 'Authorization: Bearer <key>'."))
				return
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(key)) != 1 {
				WriteError(w, r, Unauthorized("That API key is not valid for this bridge."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return ""
	}
	return r.Header.Get("X-Api-Key")
}

// isLoopback reports whether an address binds only to the local machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + time.Now().Format("150405.000")
	}
	return hex.EncodeToString(b[:])
}
