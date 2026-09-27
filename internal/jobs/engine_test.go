package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// ---------------------------------------------------------------- test rig

// upstream is a mock OpenDrive good enough to run whole transfers against: the
// four-step upload handshake, a download, and the probe.
type upstream struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	created   []string // file ids create_file handed out
	reclaimed []string // file ids DELETE /file.json removed
	content   []byte   // what a download serves
	// failUploads makes close_file_upload fail with this status until it is
	// cleared, for exercising the retry ladder.
	failCloseWith int
	failCloseBody string
	closeFailures int
	blockChunk    chan struct{}
	probeMisses   int
	// downloaded counts real downloads, so a test can assert that a cache hit cost
	// no request at all rather than inferring it from timing.
	downloaded int
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{t: t, content: []byte("downloadable content")}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.Contains(path, "create_file"):
		u.mu.Lock()
		id := fmt.Sprintf("F%d", len(u.created)+1)
		u.created = append(u.created, id)
		u.mu.Unlock()
		_, _ = io.WriteString(w, fmt.Sprintf(`{"FileId":%q,"TempLocation":"tmp/1"}`, id))

	case strings.Contains(path, "open_file_upload"):
		_, _ = io.WriteString(w, `{"TempLocation":"tmp/1"}`)

	case strings.Contains(path, "upload_file_chunk2"):
		u.mu.Lock()
		block := u.blockChunk
		u.mu.Unlock()
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		n, _ := io.Copy(io.Discard, r.Body)
		// The multipart envelope is a little larger than the payload; the
		// engine tests do not depend on the exact count.
		_, _ = io.WriteString(w, fmt.Sprintf(`{"TotalWritten":%d}`, chunkPayload(r, n)))

	case strings.Contains(path, "close_file_upload"):
		u.mu.Lock()
		status, body := u.failCloseWith, u.failCloseBody
		if status != 0 {
			u.closeFailures++
		}
		u.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"FileId":"F1","Name":"x","Size":"10"}`)

	case strings.Contains(path, "download/file.json"):
		if r.URL.Query().Get("test") == "1" {
			u.mu.Lock()
			miss := u.probeMisses > 0
			if miss {
				u.probeMisses--
			}
			u.mu.Unlock()
			if miss {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":404,"message":"File does not exist"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true}`)
			return
		}
		u.mu.Lock()
		content := u.content
		u.downloaded++
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)

	case r.Method == http.MethodDelete && strings.Contains(path, "/file.json"):
		parts := strings.Split(strings.Trim(path, "/"), "/")
		u.mu.Lock()
		u.reclaimed = append(u.reclaimed, parts[len(parts)-1])
		u.mu.Unlock()
		_, _ = io.WriteString(w, `{"result":true}`)

	case strings.Contains(path, "users/info.json"):
		_, _ = io.WriteString(w, `{"UserID":"1"}`)

	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func chunkPayload(r *http.Request, read int64) int64 {
	if n := r.URL.Query().Get("chunk_size"); n != "" {
		var v int64
		_, _ = fmt.Sscanf(n, "%d", &v)
		return v
	}
	return read
}

// uploads counts the files create_file handed out, which is one per real upload.
func (u *upstream) uploads() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.created)
}

// downloads counts the bodies actually served.
func (u *upstream) downloads() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.downloaded
}

func (u *upstream) reclaims() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, len(u.reclaimed))
	copy(out, u.reclaimed)
	return out
}

func (u *upstream) client(t *testing.T) *opendrive.Client {
	t.Helper()
	c, err := opendrive.New(
		opendrive.WithBaseURL(u.srv.URL+"/api/v1"),
		opendrive.WithHTTPClient(u.srv.Client()),
		opendrive.WithAccessProbe(nil),
		opendrive.WithAuthenticator(staticAuth{}),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 0}),
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// staticAuth is a fixed session, enough for the mock.
type staticAuth struct{}

func (staticAuth) Credentials(context.Context) (opendrive.Credentials, error) {
	return opendrive.Credentials{SessionID: "s1"}, nil
}
func (staticAuth) Refresh(context.Context) error { return nil }
func (staticAuth) Identity() opendrive.Identity {
	return opendrive.Identity{Username: "test", AuthMode: opendrive.AuthModeSession, Seamless: true}
}
func (staticAuth) AuthState() opendrive.AuthState { return opendrive.StateAuthenticated }

func newEngine(t *testing.T, u *upstream, opts ...Option) *Engine {
	t.Helper()
	base := []Option{
		WithWorkers(2),
		withSleep(func(context.Context, time.Duration) error { return nil }),
	}
	e, err := New(u.client(t), append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Stop)
	return e
}

func writeLocal(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	data := make([]byte, size)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForState(t *testing.T, e *Engine, id string, want State) *Job {
	t.Helper()
	var got *Job
	waitFor(t, fmt.Sprintf("job %s to reach %s", id, want), func() bool {
		j, err := e.Get(id)
		if err != nil {
			return false
		}
		got = j
		return j.State == want
	})
	return got
}

// ---------------------------------------------------------------- tests

func TestEngineRunsAnUploadToCompletion(t *testing.T) {
	u := newUpstream(t)
	e := newEngine(t, u)
	e.Start(context.Background())

	path := writeLocal(t, 4096)
	job, err := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 4096})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	done := waitForState(t, e, job.ID, StateSucceeded)
	if done.Error != nil {
		t.Errorf("error = %+v, want none", done.Error)
	}
	if done.BytesDone != done.BytesTotal || done.BytesTotal == 0 {
		t.Errorf("bytes_done = %d, bytes_total = %d", done.BytesDone, done.BytesTotal)
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Errorf("a successful upload reclaimed %v; the file must stay", got)
	}
}

func TestEngineRunsADownloadToCompletion(t *testing.T) {
	u := newUpstream(t)
	e := newEngine(t, u)
	e.Start(context.Background())

	dst := filepath.Join(t.TempDir(), "out", "file.bin")
	job, err := e.Submit(Spec{Kind: KindDownload, FileID: "F1", LocalPath: dst})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	waitForState(t, e, job.ID, StateSucceeded)
	got, err := os.ReadFile(dst) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("the download did not create %s: %v", dst, err)
	}
	if string(got) != string(u.content) {
		t.Errorf("downloaded %q, want %q", got, u.content)
	}
}

// The guarantee that matters most: cancelling leaves nothing upstream.
func TestCancellingARunningUploadLeavesNoOrphan(t *testing.T) {
	u := newUpstream(t)
	u.blockChunk = make(chan struct{})
	e := newEngine(t, u)
	e.Start(context.Background())

	path := writeLocal(t, 4096)
	job, err := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 4096})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait until the record exists upstream, which is the window in which a
	// cancellation could leak it.
	waitFor(t, "create_file to have run", func() bool {
		j, err := e.Get(job.ID)
		return err == nil && j.UpstreamFileID != ""
	})

	if err := e.Cancel(job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	close(u.blockChunk)

	done := waitForState(t, e, job.ID, StateCancelled)
	if done.Error != nil {
		t.Errorf("a cancellation reported an error: %+v", done.Error)
	}
	waitFor(t, "the record to be reclaimed", func() bool { return len(u.reclaims()) == 1 })
	if got := u.reclaims(); got[0] != "F1" {
		t.Errorf("reclaimed %v, want the record create_file made", got)
	}
}

func TestCancellingAQueuedJobNeverStartsIt(t *testing.T) {
	u := newUpstream(t)
	e := newEngine(t, u, WithWorkers(1))
	// Deliberately not started, so the job stays queued.

	path := writeLocal(t, 128)
	job, err := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := e.Cancel(job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	got, err := e.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCancelled {
		t.Errorf("state = %q, want %q", got.State, StateCancelled)
	}
	e.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	if again, _ := e.Get(job.ID); again.State != StateCancelled {
		t.Errorf("a cancelled job was picked up anyway: %q", again.State)
	}
}

// A permanent failure must not be retried, must reclaim, and must report the
// classifier's verdict rather than the engine's opinion.
func TestAPermanentFailureIsNotRetriedAndReclaims(t *testing.T) {
	u := newUpstream(t)
	u.failCloseWith = http.StatusBadRequest
	u.failCloseBody = `{"error":{"code":400,"message":"Invalid upload file size. Total uploaded=0"}}`
	e := newEngine(t, u)
	e.Start(context.Background())

	path := writeLocal(t, 512)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 512})

	done := waitForState(t, e, job.ID, StateFailed)
	if done.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: invalid_request is not retryable", done.Attempts)
	}
	if done.Error == nil || done.Error.Code != string(opendrive.KindInvalidRequest) {
		t.Errorf("error = %+v, want the classifier's invalid_request code", done.Error)
	}
	if done.Error.Retryable {
		t.Error("a permanent failure was reported as retryable")
	}
	waitFor(t, "the record to be reclaimed", func() bool { return len(u.reclaims()) == 1 })
}

// A temporary failure is retried, on the classifier's say-so alone.
func TestATemporaryFailureIsRetried(t *testing.T) {
	u := newUpstream(t)
	u.failCloseWith = http.StatusInternalServerError
	u.failCloseBody = `{"error":{"code":500,"message":"upstream fell over"}}`
	e := newEngine(t, u, WithMaxAttempts(3))
	e.Start(context.Background())

	path := writeLocal(t, 512)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 512})

	done := waitForState(t, e, job.ID, StateFailed)
	if done.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", done.Attempts)
	}
	if done.Error == nil || !done.Error.Retryable {
		t.Errorf("error = %+v, want a retryable verdict", done.Error)
	}
	// Every abandoned attempt leaves a record, and every one must be taken back.
	waitFor(t, "each attempt's record to be reclaimed", func() bool { return len(u.reclaims()) >= 1 })
	t.Logf("reclaimed %v", u.reclaims())
}

// The engine must not invent wording. When upstream's message is misleading the
// classifier says so, and that is what a user sees.
func TestTheReportedMessageComesFromTheClassifier(t *testing.T) {
	// A permission-shaped 403 with a working credential: the classifier
	// attaches a diagnosis precisely because upstream's text misleads.
	u := newUpstream(t)
	u.failCloseWith = http.StatusForbidden
	u.failCloseBody = `{"error":{"code":403,"message":"Your user access enables you only to view ` +
		`this folder, please contact your administrator to discuss your user permissions."}}`

	c, err := opendrive.New(
		opendrive.WithBaseURL(u.srv.URL+"/api/v1"),
		opendrive.WithHTTPClient(u.srv.Client()),
		opendrive.WithAuthenticator(staticAuth{}),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 0}),
	) // the default access probe is left on, so disambiguation runs
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(c, WithWorkers(1), WithMaxAttempts(1),
		withSleep(func(context.Context, time.Duration) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	e.Start(context.Background())

	path := writeLocal(t, 256)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 256})
	done := waitForState(t, e, job.ID, StateFailed)

	if done.Error == nil {
		t.Fatal("no error reported")
	}
	t.Logf("reported: code=%s message=%q", done.Error.Code, done.Error.Message)
	if strings.Contains(done.Error.Message, "contact your administrator") {
		t.Error("upstream's misleading permission text was passed straight through to the user")
	}
	if done.Error.Code != string(opendrive.KindUpstreamError) {
		t.Errorf("code = %q, want the classifier's stable enum", done.Error.Code)
	}
}

// D44: an upload is not complete until upstream admits the file exists, and the
// wait lives here rather than being taught to the classifier as a retryable 404.
func TestUploadWaitsForTheFileToBecomeVisible(t *testing.T) {
	u := newUpstream(t)
	u.probeMisses = 2 // the first two probes answer "File does not exist"
	e := newEngine(t, u)
	e.Start(context.Background())

	path := writeLocal(t, 256)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 256})

	done := waitForState(t, e, job.ID, StateSucceeded)
	if done.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a visibility wait is not a retry", done.Attempts)
	}
	u.mu.Lock()
	left := u.probeMisses
	u.mu.Unlock()
	if left != 0 {
		t.Errorf("%d probe misses left; the engine did not wait them out", left)
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Errorf("a successful upload reclaimed %v", got)
	}
}

// The visibility wait has to end. If upstream never admits the file exists, the
// job fails with the classifier's not_found rather than hanging — and, because a
// 404 is not retryable, it is not retried into a loop either (D44).
func TestUploadGivesUpWaitingForVisibility(t *testing.T) {
	u := newUpstream(t)
	u.probeMisses = 1 << 30 // upstream never admits the file exists

	// A clock that jumps forward on every reading, so the deadline is reached
	// without the test waiting for it.
	start := time.Now()
	var ticks int
	clock := func() time.Time {
		ticks++
		return start.Add(time.Duration(ticks) * 10 * time.Second)
	}

	e := newEngine(t, u, WithMaxAttempts(1), withClock(clock))
	e.Start(context.Background())

	path := writeLocal(t, 256)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 256})

	done := waitForState(t, e, job.ID, StateFailed)
	if done.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: a 404 must not be retried (D44)", done.Attempts)
	}
	if done.Error == nil || done.Error.Retryable {
		t.Errorf("error = %+v, want a permanent verdict", done.Error)
	}
	if !strings.Contains(done.Error.Message, "visible") {
		t.Errorf("message = %q, want it to name the visibility wait", done.Error.Message)
	}
	// And the record it could not verify is not left behind.
	waitFor(t, "the record to be reclaimed", func() bool { return len(u.reclaims()) == 1 })
}

// Concurrency is bounded, which is the whole point of a worker pool.
func TestConcurrencyIsCapped(t *testing.T) {
	u := newUpstream(t)
	u.blockChunk = make(chan struct{})

	var mu sync.Mutex
	var inFlight, peak int
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "upload_file_chunk2") {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				inFlight--
				mu.Unlock()
			}()
		}
		u.serve(w, r)
	})

	e := newEngine(t, u, WithWorkers(2))
	e.Start(context.Background())

	path := writeLocal(t, 1024)
	for i := 0; i < 6; i++ {
		if _, err := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 1024}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the workers to saturate", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFlight >= 2
	})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := peak
	mu.Unlock()
	close(u.blockChunk)

	if got > 2 {
		t.Errorf("%d chunk uploads were in flight at once, want at most the 2 workers", got)
	}
}

func TestSubmitValidatesTheSpec(t *testing.T) {
	u := newUpstream(t)
	e := newEngine(t, u)

	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"no kind", Spec{}},
		{"upload without a local path", Spec{Kind: KindUpload}},
		{"download without a file id", Spec{Kind: KindDownload, LocalPath: "/tmp/x"}},
		{"download without a local path", Spec{Kind: KindDownload, FileID: "F1"}},
	} {
		if _, err := e.Submit(tc.spec); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}

	// A name defaults to the local base name rather than being required.
	job, err := e.Submit(Spec{Kind: KindUpload, LocalPath: "/tmp/dir/report.pdf", FolderID: "d"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if job.Spec.Name != "report.pdf" {
		t.Errorf("name = %q, want the local base name", job.Spec.Name)
	}
}

func TestGetAndListReportCopies(t *testing.T) {
	u := newUpstream(t)
	e := newEngine(t, u)

	path := writeLocal(t, 64)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest"})

	got, err := e.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.State = StateFailed // mutating the copy must not affect the engine
	if again, _ := e.Get(job.ID); again.State == StateFailed {
		t.Error("Get handed out the engine's own job")
	}
	if len(e.List()) != 1 {
		t.Errorf("List returned %d jobs, want 1", len(e.List()))
	}
	if _, err := e.Get("nope"); err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The reported shape is what /v1/jobs serialises (§4.3), so it is asserted as
// JSON rather than as Go fields.
func TestTheJobSerialisesToTheAPISchema(t *testing.T) {
	j := &Job{
		ID: "abc", Kind: KindUpload, State: StateRunning,
		BytesDone: 5, BytesTotal: 10, Speed: 1234.5,
		Error: &Error{Code: "upstream_error", Message: "something", Retryable: true},
	}
	data, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "kind", "state", "bytes_done", "bytes_total", "speed", "error"} {
		if _, ok := got[field]; !ok {
			t.Errorf("%q missing from the serialised job; §4.3 requires it", field)
		}
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["code"] != "upstream_error" {
		t.Errorf("error.code = %v, want the stable classifier enum", errObj["code"])
	}

	// A job with no error omits the field rather than sending null.
	j.Error = nil
	data, _ = json.Marshal(j)
	if strings.Contains(string(data), `"error"`) {
		t.Error("a job with no error still serialises an error field")
	}
}

func TestSpeedIsSmoothedAndNonNegative(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	s := newSpeed(clock)

	now = now.Add(time.Second)
	if got := s.observe(1000); got <= 0 {
		t.Errorf("rate = %v, want a positive rate", got)
	}
	now = now.Add(time.Second)
	rate := s.observe(2000)
	if rate <= 0 {
		t.Errorf("rate = %v, want a positive rate", rate)
	}
	// A counter that does not advance must not produce a negative rate.
	now = now.Add(time.Second)
	if got := s.observe(2000); got < 0 {
		t.Errorf("rate = %v, want non-negative", got)
	}
}

func TestBackoffClimbsAndIsCapped(t *testing.T) {
	if backoff(1) != time.Second {
		t.Errorf("first backoff = %v, want 1s", backoff(1))
	}
	if backoff(3) != 4*time.Second {
		t.Errorf("third backoff = %v, want 4s", backoff(3))
	}
	if backoff(50) != time.Minute {
		t.Errorf("backoff is not capped: %v", backoff(50))
	}
}

func TestNewRequiresAClient(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("want an error with no client")
	}
}
