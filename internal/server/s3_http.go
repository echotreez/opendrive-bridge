package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/gofakes3"
	"github.com/rclone/gofakes3/signature"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
)

// S3Credentials is the one access key the gateway accepts.
type S3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// The names under which the keys live in credentials.key (keystore.Secrets).
// They name fields; the values are generated and stored encrypted.
const ( // #nosec G101 -- field names, not credentials
	secretS3AccessKeyID = "S3_ACCESS_KEY_ID"
	secretS3Secret      = "S3_SECRET_ACCESS_KEY"
)

// LoadOrCreateS3Credentials returns the stored S3 keys, generating and storing
// them the first time (§3.6.5: nobody prepares anything). They are kept only in
// the encrypted credential file and are never logged.
func LoadOrCreateS3Credentials(ctx context.Context, sec keystore.Secrets) (S3Credentials, bool, error) {
	id, err1 := sec.LoadSecret(ctx, secretS3AccessKeyID)
	secret, err2 := sec.LoadSecret(ctx, secretS3Secret)
	if err1 == nil && err2 == nil {
		return S3Credentials{AccessKeyID: id, SecretAccessKey: secret}, false, nil
	}
	for _, err := range []error{err1, err2} {
		if err != nil && !errors.Is(err, keystore.ErrNoSecret) {
			return S3Credentials{}, false, err
		}
	}
	c, err := ResetS3Credentials(ctx, sec)
	return c, err == nil, err
}

// ResetS3Credentials replaces the keys. Every client set up with the old ones
// stops working, which is the point of resetting them.
func ResetS3Credentials(ctx context.Context, sec keystore.Secrets) (S3Credentials, error) {
	id, err := randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 17)
	if err != nil {
		return S3Credentials{}, err
	}
	secret, err := randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", 40)
	if err != nil {
		return S3Credentials{}, err
	}
	c := S3Credentials{AccessKeyID: "ODB" + id, SecretAccessKey: secret}
	if err := sec.SaveSecret(ctx, secretS3AccessKeyID, c.AccessKeyID); err != nil {
		return S3Credentials{}, err
	}
	if err := sec.SaveSecret(ctx, secretS3Secret, c.SecretAccessKey); err != nil {
		return S3Credentials{}, err
	}
	return c, nil
}

func randomString(alphabet string, n int) (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for i := 0; i < n; i++ {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[k.Int64()])
	}
	return b.String(), nil
}

// ---------------------------------------------------------------- the gateway

// S3Gateway is the S3 endpoint: gofakes3 over s3Backend, behind the guard below.
type S3Gateway struct {
	backend *s3Backend
	fake    *gofakes3.GoFakeS3
	handler http.Handler
	log     *slog.Logger

	mu    sync.Mutex
	creds S3Credentials
	info  S3Info
}

// S3Info is what /v1/s3 reports about where the gateway listens.
type S3Info struct {
	HTTPSAddr   string `json:"https_addr,omitempty"`
	HTTPAddr    string `json:"http_addr,omitempty"`
	Fingerprint string `json:"certificate_sha256,omitempty"`
	CustomCert  bool   `json:"custom_certificate"`
	Delete      string `json:"delete"`
}

// NewS3Gateway builds the S3 endpoint over this server's cache and client.
func (s *Server) NewS3Gateway(cfg S3Config, creds S3Credentials) (*S3Gateway, error) {
	if s.datacache == nil || !s.datacache.Status().WriteBack {
		return nil, errors.New("the S3 gateway needs the caching gateway with write-back on " +
			"(--cache-dir, and --cache-write-back left at true)")
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		// gofakes3 accepts every request when it has no keys. That default is
		// never allowed to be reached.
		return nil, errors.New("the S3 gateway refuses to start without an access key")
	}
	if cfg.WholeFetchLimit <= 0 {
		cfg.WholeFetchLimit = defaultWholeFetchLimit
	}
	if cfg.MultipartExpiry <= 0 {
		cfg.MultipartExpiry = defaultMultipartExpiry
	}
	if cfg.StagingDir == "" {
		return nil, errors.New("the S3 gateway needs a directory for multipart uploads")
	}
	log := s.log.With(slog.String("component", "s3"))
	mp, err := newMultipartStore(cfg.StagingDir, cfg.MultipartExpiry, log)
	if err != nil {
		return nil, err
	}
	b := &s3Backend{s: s, cfg: cfg, log: log, mp: mp}
	// No keys are given to gofakes3. The guard below verifies every signature
	// itself, with gofakes3's own verifier, because it has to see the request
	// path before gofakes3 strips a trailing "/" from it — and a signature is
	// computed over the path, so it cannot be checked after the path is
	// rewritten. That makes the guard the only authentication there is, which
	// is why NewS3Gateway refuses to build without a key, Handler is the only way
	// in, and TestS3RefusesAnythingNotSignedWithTheKey tries every way around it.
	fake := gofakes3.New(b,
		gofakes3.WithoutVersioning(),
		gofakes3.WithIntegrityCheck(true),
		gofakes3.WithTimeSkewLimit(15*time.Minute),
		gofakes3.WithLogger(fakeLog{log}),
	)
	g := &S3Gateway{backend: b, fake: fake, log: log, creds: creds}
	g.info.Delete = "trash"
	if cfg.PermanentDelete {
		g.info.Delete = "permanent"
	}
	g.handler = g.guard(fake.Server())
	s.s3.Store(g)
	return g, nil
}

// Handler is the S3 endpoint with its guard.
func (g *S3Gateway) Handler() http.Handler { return g.handler }

// Credentials returns the key in use.
func (g *S3Gateway) Credentials() S3Credentials {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.creds
}

// SetCredentials swaps the accepted key. The old one stops working at once.
func (g *S3Gateway) SetCredentials(c S3Credentials) {
	g.mu.Lock()
	g.creds = c
	g.mu.Unlock()
}

// lookupKey is the signature verifier's view of the accepted key.
func (g *S3Gateway) lookupKey(accessKey string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.creds.AccessKeyID == "" || g.creds.SecretAccessKey == "" ||
		subtle.ConstantTimeCompare([]byte(accessKey), []byte(g.creds.AccessKeyID)) != 1 {
		return "", false
	}
	return g.creds.SecretAccessKey, true
}

// Info reports where the gateway listens.
func (g *S3Gateway) Info() S3Info {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.info
}

// guard is everything in front of gofakes3 that gofakes3 does not do itself.
//
//   - Presigned URLs are refused. None of the supported clients uses them, and
//     gofakes3 puts no upper bound on how long one stays valid: a leaked URL
//     would be a leaked credential.
//   - The status a backend method chose (s3Status) replaces the 500 gofakes3
//     writes for any error code it does not know.
//   - Requests are logged without their query string or headers, which is
//     where signatures and keys travel.
func (g *S3Gateway) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		q := r.URL.Query()
		if q.Get("X-Amz-Signature") != "" || q.Get("X-Amz-Credential") != "" {
			writeS3Error(w, http.StatusForbidden, "AccessDenied",
				"presigned URLs are not accepted by this gateway; sign the request instead")
			g.logRequest(r, http.StatusForbidden, start)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			// Anonymous, or signed with the old version 2 scheme: neither is
			// accepted, and S3 answers both with AccessDenied.
			writeS3Error(w, http.StatusForbidden, "AccessDenied",
				"requests must be signed with AWS Signature Version 4 and this gateway's access key")
			g.logRequest(r, http.StatusForbidden, start)
			return
		}
		if code := signature.V4SignVerifyWithLookup(r, g.lookupKey); code != signature.ErrNone {
			e := signature.GetAPIError(code)
			status := e.HTTPStatusCode
			if status == 0 || status == http.StatusBadRequest {
				status = http.StatusForbidden
			}
			writeS3Error(w, status, e.Code, e.Description)
			g.logRequest(r, status, start)
			return
		}
		markFolderKey(r)
		emptyBodyLength(r)
		st := &s3Status{sha256: r.Header.Get("X-Amz-Content-Sha256")}
		sw := &s3StatusWriter{ResponseWriter: w, st: st}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), s3StatusKey{}, st)))
		g.logRequest(r, sw.code, start)
	})
}

// folderKeySuffix stands in for a key's trailing "/" on its way through
// gofakes3, which trims slashes from both ends of the request path: without it,
// a PUT of the folder marker "dir/" arrives as an object called "dir" and is
// stored as a file, and the next write to "dir/x" needs a folder by the same
// name. It is a byte that is never valid UTF-8, so no real key — which
// objectPath requires to be valid UTF-8 — can contain it.
const folderKeySuffix = "\xff"

// markFolderKey replaces the trailing "/" of an object key in the request path,
// after the signature has been checked against the path as it was sent.
func markFolderKey(r *http.Request) {
	p := r.URL.Path
	if !strings.HasSuffix(p, "/") || strings.Count(strings.Trim(p, "/"), "/") < 1 {
		return // not an object, or the bucket itself
	}
	r.URL.Path = strings.TrimSuffix(p, "/") + folderKeySuffix
	r.URL.RawPath = ""
}

// emptyBodyLength gives an empty PUT the Content-Length: 0 it did not send.
//
// Go's HTTP client sends a zero-length body with a non-nil reader as chunked,
// with no Content-Length, and minio-go does exactly that for an empty object.
// gofakes3 answers 411 to any PUT without the header, so every empty file in a
// backup failed. Only a body that turns out to be empty is changed; a chunked
// body with data is left for gofakes3 to refuse, because its length cannot be
// known without holding it.
//
// The same check stops a streamed (aws-chunked) body sent without a
// Content-Length — minio-go sends an empty object exactly so, over plain HTTP —
// even though such a body carries its real length in
// X-Amz-Decoded-Content-Length, which gofakes3 goes on to use. For those the
// decoded length is copied into the header gofakes3 checks; the body is read
// by that length either way.
func emptyBodyLength(r *http.Request) {
	if r.Method != http.MethodPut || r.Header.Get("Content-Length") != "" || r.Body == nil {
		return
	}
	if decoded := r.Header.Get("X-Amz-Decoded-Content-Length"); decoded != "" {
		r.Header.Set("Content-Length", decoded)
		return
	}
	br := bufio.NewReaderSize(r.Body, 16)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		r.Header.Set("Content-Length", "0")
		r.ContentLength = 0
		r.Body = http.NoBody
		return
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
}

func (g *S3Gateway) logRequest(r *http.Request, code int, start time.Time) {
	if code == 0 {
		code = http.StatusOK
	}
	level := slog.LevelDebug
	switch {
	case code == http.StatusForbidden:
		level = slog.LevelWarn // a refused signature is worth seeing
	case code >= 500:
		level = slog.LevelWarn
	}
	g.log.Log(r.Context(), level, "s3 request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", code),
		slog.String("remote", r.RemoteAddr),
		slog.Duration("took", time.Since(start)))
}

func writeS3Error(w http.ResponseWriter, code int, s3code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
		`<Error><Code>%s</Code><Message>%s</Message></Error>`, s3code, xmlEscape(msg))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// s3StatusWriter applies a status the backend chose to the one gofakes3 writes.
type s3StatusWriter struct {
	http.ResponseWriter
	st   *s3Status
	code int
}

func (w *s3StatusWriter) WriteHeader(code int) {
	w.st.mu.Lock()
	want, retry := w.st.code, w.st.retry
	w.st.mu.Unlock()
	if want != 0 && code == http.StatusInternalServerError {
		code = want
		if retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
		}
	}
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *s3StatusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *s3StatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// fakeLog sends gofakes3's own messages to the daemon's log at a level that
// matches, so its per-request chatter stays at debug.
type fakeLog struct{ log *slog.Logger }

func (l fakeLog) Print(level gofakes3.LogLevel, v ...interface{}) {
	msg := strings.TrimSpace(fmt.Sprintln(v...))
	switch level {
	case gofakes3.LogErr:
		l.log.Error("s3: " + msg)
	case gofakes3.LogWarn:
		l.log.Warn("s3: " + msg)
	default:
		l.log.Debug("s3: " + msg)
	}
}

// ---------------------------------------------------------------- serving

// Serve runs the S3 listeners until ctx ends: HTTPS on httpsAddr, and plain
// HTTP on httpAddr, which must be a loopback address. Either may be empty.
func (g *S3Gateway) Serve(ctx context.Context, httpsAddr, httpAddr string, tlsCfg *tls.Config, fingerprint string, custom bool) error {
	if httpAddr != "" && !isLoopback(httpAddr) {
		return fmt.Errorf("the plain-HTTP S3 address %s is not a loopback address; "+
			"anything beyond this machine must use HTTPS", httpAddr)
	}
	if httpsAddr != "" && tlsCfg == nil {
		return errors.New("the HTTPS S3 listener needs a certificate")
	}
	g.mu.Lock()
	g.info.HTTPSAddr, g.info.HTTPAddr, g.info.Fingerprint, g.info.CustomCert = httpsAddr, httpAddr, fingerprint, custom
	g.mu.Unlock()

	newServer := func(addr string) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           g.handler,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
			TLSConfig:         tlsCfg,
			// No read or write timeout: a multi-gigabyte object on a slow line
			// legitimately takes hours.
		}
	}
	var servers []*http.Server
	errc := make(chan error, 2)
	if httpsAddr != "" {
		srv := newServer(httpsAddr)
		ln, err := net.Listen("tcp", httpsAddr)
		if err != nil {
			return fmt.Errorf("cannot listen for S3 on %s: %w", httpsAddr, err)
		}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(tls.NewListener(ln, tlsCfg)) }()
		g.log.Info("S3 gateway listening (HTTPS)", slog.String("addr", httpsAddr),
			slog.String("certificate_sha256", fingerprint))
	}
	if httpAddr != "" {
		srv := newServer(httpAddr)
		srv.TLSConfig = nil
		ln, err := net.Listen("tcp", httpAddr)
		if err != nil {
			for _, s := range servers {
				_ = s.Close()
			}
			return fmt.Errorf("cannot listen for S3 on %s: %w", httpAddr, err)
		}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(ln) }()
		g.log.Info("S3 gateway listening (HTTP, this machine only)", slog.String("addr", httpAddr))
	}
	if len(servers) == 0 {
		return errors.New("the S3 gateway has no address to listen on")
	}

	reap := time.NewTicker(time.Hour)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, s := range servers {
				_ = s.Shutdown(shutdownCtx)
			}
			return nil
		case err := <-errc:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				for _, s := range servers {
					_ = s.Close()
				}
				return err
			}
		case now := <-reap.C:
			g.backend.mp.reap(now)
		}
	}
}
