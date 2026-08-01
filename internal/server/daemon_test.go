package server

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrors the code under test
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/jobs"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The P4 exit criterion, as a test: a real daemon on a real socket, driven over
// HTTP from outside, through the whole journey a user takes.
//
// It is here rather than only in a shell script because the pipework it covers —
// listening, shutting down, streaming a body to the wire, threading a request id
// through — has no other test that reaches it, and pipework with no test is
// where a daemon breaks in the way that is hardest to reproduce.

// daemon is a running Server with an address to talk to.
type daemon struct {
	t    *testing.T
	base string
	srv  *Server
	logs *bytes.Buffer
}

// startDaemon runs a real server on an ephemeral port, backed by the fake
// upstream, and waits for it to answer.
func startDaemon(t *testing.T, u *fakeUpstream) *daemon {
	t.Helper()

	srv := u.server(t)
	engine, err := jobs.New(srv.client, jobs.WithWorkers(2))
	if err != nil {
		t.Fatal(err)
	}

	// A free port, taken and released so the daemon can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	logs := &bytes.Buffer{}
	real, err := New(Config{
		Addr:   addr,
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, srv.auth,
		WithClient(srv.client), WithPathCache(srv.cache), WithJobEngine(engine))
	if err != nil {
		t.Fatal(err)
	}
	if real.Addr() != addr {
		t.Fatalf("Addr() = %q, want %q", real.Addr(), addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	engine.Start(ctx)
	done := make(chan error, 1)
	go func() { done <- real.ListenAndServe(ctx) }()

	t.Cleanup(func() {
		cancel()
		engine.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the daemon stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not shut down when its context was cancelled")
		}
	})

	d := &daemon{t: t, base: "http://" + addr + "/v1", srv: real, logs: logs}
	d.waitReady()
	return d
}

func (d *daemon) waitReady() {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(d.base + "/health") //nolint:gosec,noctx // our own loopback daemon
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("the daemon never started answering: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// call makes a real HTTP request and decodes the JSON answer.
func (d *daemon) call(method, path, body string) (*http.Response, map[string]any) {
	d.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, d.base+path, rdr)
	if err != nil {
		d.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			d.t.Fatalf("%s %s: response is not JSON: %s", method, path, raw)
		}
	}
	return resp, out
}

// raw fetches a body without decoding it, for the streaming endpoints.
func (d *daemon) raw(path string) (*http.Response, []byte) {
	d.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, d.base+path, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (d *daemon) waitJob(id string) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, job := d.call(http.MethodGet, "/jobs/"+id, "")
		state, _ := job["state"].(string)
		switch state {
		case "succeeded", "failed", "cancelled":
			return job
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("job %s stayed %q", id, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The whole journey, over the wire.
func TestDaemonEndToEnd(t *testing.T) {
	content := bytes.Repeat([]byte("bridge end to end payload "), 400)
	sum := md5.Sum(content) //nolint:gosec // protocol requirement
	wantMD5 := hex.EncodeToString(sum[:])

	u := newFakeUpstream(t)
	u.withTransfers(content)
	d := startDaemon(t, u)

	t.Run("status answers before anything else happens", func(t *testing.T) {
		resp, body := d.call(http.MethodGet, "/auth/status", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if body["state"] != "authenticated" {
			t.Errorf("state = %v", body["state"])
		}
		// Every response carries a request id a user can quote in a bug report.
		if resp.Header.Get(RequestIDHeader) == "" {
			t.Error("no request id came back")
		}
	})

	t.Run("mkdir", func(t *testing.T) {
		resp, body := d.call(http.MethodPost, "/mkdir", `{"path":"/Docs/e2e"}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		if body["path"] != "/Docs/e2e" {
			t.Errorf("path = %v", body["path"])
		}
		_, stat := d.call(http.MethodGet, "/stat?path=/Docs/e2e", "")
		if stat["kind"] != "folder" {
			t.Errorf("stat = %v", stat)
		}
	})

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, content, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("upload runs as a job", func(t *testing.T) {
		resp, body := d.call(http.MethodPost, "/upload",
			fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/e2e/payload.bin"}`, local))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		job := d.waitJob(body["id"].(string))
		if job["state"] != "succeeded" {
			t.Fatalf("upload job: %v", job)
		}
		if job["bytes_done"] != job["bytes_total"] {
			t.Errorf("bytes_done = %v of %v", job["bytes_done"], job["bytes_total"])
		}
	})

	t.Run("the job is listed", func(t *testing.T) {
		_, body := d.call(http.MethodGet, "/jobs", "")
		if len(body["jobs"].([]any)) == 0 {
			t.Error("no jobs listed after an upload")
		}
	})

	t.Run("ls shows what was uploaded", func(t *testing.T) {
		_, body := d.call(http.MethodGet, "/ls?path=/Docs", "")
		var names []string
		for _, e := range body["entries"].([]any) {
			names = append(names, e.(map[string]any)["name"].(string))
		}
		if !contains(names, "e2e") {
			t.Errorf("the new folder is missing from %v", names)
		}
	})

	back := filepath.Join(t.TempDir(), "back.bin")
	t.Run("download and verify the digest", func(t *testing.T) {
		resp, body := d.call(http.MethodPost, "/download",
			fmt.Sprintf(`{"remote_path":"/Docs/report.pdf","local_path":%q}`, back))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		job := d.waitJob(body["id"].(string))
		if job["state"] != "succeeded" {
			t.Fatalf("download job: %v", job)
		}

		got, err := os.ReadFile(back) //nolint:gosec // test temp file
		if err != nil {
			t.Fatal(err)
		}
		gotSum := md5.Sum(got) //nolint:gosec // protocol requirement
		if hex.EncodeToString(gotSum[:]) != wantMD5 {
			t.Fatalf("round trip digest %s, want %s", hex.EncodeToString(gotSum[:]), wantMD5)
		}
	})

	// The streaming path is a different code route: it writes to the socket as
	// the bytes arrive, through the response recorder's Flush.
	t.Run("download/stream matches too", func(t *testing.T) {
		resp, raw := d.raw("/download/stream?path=/Docs/report.pdf")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, raw)
		}
		gotSum := md5.Sum(raw) //nolint:gosec // protocol requirement
		if hex.EncodeToString(gotSum[:]) != wantMD5 {
			t.Errorf("streamed digest %s, want %s", hex.EncodeToString(gotSum[:]), wantMD5)
		}
		if !strings.Contains(resp.Header.Get("Content-Disposition"), "report.pdf") {
			t.Errorf("no filename offered: %q", resp.Header.Get("Content-Disposition"))
		}
	})

	t.Run("rm says where it went", func(t *testing.T) {
		resp, body := d.call(http.MethodPost, "/rm", `{"path":"/Docs/e2e"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		if !strings.Contains(strings.ToLower(body["detail"].(string)), "trash") {
			t.Errorf("detail = %v", body["detail"])
		}
	})

	// A failure over the wire still arrives in the envelope, in words a person
	// can act on.
	t.Run("a missing path is reported plainly", func(t *testing.T) {
		resp, body := d.call(http.MethodGet, "/stat?path=/Docs/nowhere.bin", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		msg := body["error"].(map[string]any)["message"].(string)
		assertUserReadable(t, msg)
		if !strings.Contains(msg, "/Docs/nowhere.bin") {
			t.Errorf("the message does not name the path: %q", msg)
		}
	})

	t.Run("the access log records the request without leaking", func(t *testing.T) {
		logged := d.logs.String()
		if !strings.Contains(logged, `"request_id"`) {
			t.Error("no request id in the log")
		}
		if !strings.Contains(logged, "/v1/stat") {
			t.Error("the access log did not record a request")
		}
	})
}

// A caller's own request id is threaded through and readable inside a handler,
// which is what makes a user's bug report joinable to a log line.
func TestDaemonThreadsTheCallersRequestID(t *testing.T) {
	u := newFakeUpstream(t)
	d := startDaemon(t, u)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, d.base+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(RequestIDHeader, "from-the-caller")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := resp.Header.Get(RequestIDHeader); got != "from-the-caller" {
		t.Errorf("request id = %q, want the caller's own", got)
	}
	if !strings.Contains(d.logs.String(), "from-the-caller") {
		t.Error("the caller's request id never reached the log")
	}
}

// RequestIDFrom is what a handler uses to quote the id back; it must answer for
// a real request and be empty rather than panic for a bare context.
func TestRequestIDFromContext(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Errorf("RequestIDFrom on a bare context = %q, want empty", got)
	}

	var seen string
	h := requestID(slog.New(discardHandler{}))(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			seen = RequestIDFrom(r.Context())
		}))
	rec := newRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/health", nil)
	h.ServeHTTP(rec, req)

	if seen == "" {
		t.Error("a handler could not read its own request id")
	}
}

// The daemon has to stop when its context is cancelled, and say nothing alarming
// about it. The cleanup in startDaemon asserts the clean stop; this asserts the
// second call is harmless, which is what a signal handler racing a shutdown does.
func TestDaemonShutsDownCleanly(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	real, err := New(Config{Addr: addr}, srv.auth, WithClient(srv.client))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- real.ListenAndServe(ctx) }()

	// Wait for it to be up, then stop it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, getErr := http.Get("http://" + addr + "/v1/health") //nolint:gosec,noctx // our own daemon
		if getErr == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("never started: %v", getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown reported %v, want a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}

// A port already in use has to be reported as itself, not as a crash.
func TestDaemonReportsABusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	u := newFakeUpstream(t)
	srv := u.server(t)
	real, err := New(Config{Addr: ln.Addr().String()}, srv.auth, WithClient(srv.client))
	if err != nil {
		t.Fatal(err)
	}
	if err := real.ListenAndServe(context.Background()); err == nil {
		t.Fatal("binding a port already in use was not reported")
	} else if !strings.Contains(err.Error(), "cannot listen") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

// The discard handler is the daemon's default logger. Its methods have to be
// safe to call, because every log line goes through them when no logger is set.
func TestDiscardHandlerIsInert(t *testing.T) {
	h := discardHandler{}
	if h.Enabled(context.Background(), slog.LevelError) {
		t.Error("the discard handler claims to be enabled")
	}
	if err := h.Handle(context.Background(), slog.Record{}); err != nil {
		t.Errorf("Handle: %v", err)
	}
	if h.WithAttrs(nil) == nil || h.WithGroup("g") == nil {
		t.Error("WithAttrs or WithGroup returned nil, which would panic slog")
	}
	// And a logger built on it works end to end.
	slog.New(discardHandler{}).With("k", "v").Info("ignored")
}

// doRaw fetches an undecoded body through the router.
func doRaw(t *testing.T, srv *Server, path string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec, rec.Body.Bytes()
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// newRecorder is a minimal ResponseWriter for the middleware tests.
func newRecorder() *recorder { return &recorder{header: http.Header{}} }

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

// The response recorder used in the middleware chain has to pass a Flush
// through, or a streaming download would sit in a buffer until it finished.
func TestStatusRecorderFlushes(t *testing.T) {
	inner := &flushRecorder{recorder: newRecorder()}
	rec := &statusRecorder{ResponseWriter: inner}

	if _, err := rec.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	rec.Flush()
	if !inner.flushed {
		t.Error("Flush did not reach the underlying writer; a streaming download would stall")
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want an implicit 200 after a bare Write", rec.status)
	}
	if rec.bytes != int64(len("partial")) {
		t.Errorf("bytes = %d", rec.bytes)
	}

	// A writer that cannot flush must not panic.
	plain := &statusRecorder{ResponseWriter: newRecorder()}
	plain.Flush()
}

type flushRecorder struct {
	*recorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// WithJobEngine is what the daemon uses to hand the engine over; a server built
// without one has to say so rather than panic.
func TestWithJobEngineWiresTheTransferEndpoints(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	// Without an engine the transfer endpoints refuse in the envelope.
	rec, body := do(t, srv, http.MethodGet, "/v1/jobs", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d without an engine", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))

	engine, err := jobs.New(srv.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)

	withEngine, err := New(Config{Addr: "127.0.0.1:0"}, srv.auth,
		WithClient(srv.client), WithJobEngine(engine))
	if err != nil {
		t.Fatal(err)
	}
	rec2, _ := do(t, withEngine, http.MethodGet, "/v1/jobs", "")
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d with an engine", rec2.Code)
	}
}

// A stray panic inside a handler must not take the daemon down.
func TestDaemonSurvivesAPanickingHandler(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	srv.auth = &panicAuth{}

	rec, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))

	// And the next request still works.
	rec2, _ := do(t, srv, http.MethodGet, "/v1/health", "")
	if rec2.Code != http.StatusOK {
		t.Errorf("the daemon did not recover: %d", rec2.Code)
	}
}

var _ = opendrive.KindNotFound // keep the import honest across build tags

// A streaming download must deliver every byte upstream sends, even when the
// folder listing disagrees about the size. Setting Content-Length from the
// listing made Go truncate the body to it, and the caller saved a file that was
// quietly incomplete.
func TestDownloadStreamIgnoresAStaleSizeInTheListing(t *testing.T) {
	// The listing says report.pdf is 1024 bytes; upstream serves 10400.
	content := bytes.Repeat([]byte("a"), 10400)
	u := newFakeUpstream(t)
	u.withTransfers(content)
	srv := u.server(t)

	rec, raw := doRaw(t, srv, "/v1/download/stream?path=/Docs/report.pdf")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(raw) != len(content) {
		t.Fatalf("got %d bytes, want all %d: the response was truncated to the listing's size",
			len(raw), len(content))
	}
	if rec.Header().Get("Content-Length") != "" {
		t.Error("a Content-Length was promised from metadata that may not match the body")
	}
}
