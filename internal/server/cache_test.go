package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/datacache"
)

// The caching gateway at the Bridge boundary (§4.4.1).
//
// The boundary's standing rule applies here as much as it does to anything
// upstream says: a caller must be able to act on what it is told. Two cases carry
// the weight, and both are about not losing data rather than about returning the
// right shape:
//
//   - a 202 from a write-back upload means "on this disk, not on OpenDrive", and
//     the response has to say so in words as well as in the status code;
//   - refresh and clear answer 409 for anything unsent, because discarding it
//     would delete the user's only copy.

// heldUpstream is an Uploader that never finishes, so objects stay unsent for as
// long as a test needs them to.
type heldUpstream struct{ release chan struct{} }

func newHeldUpstream() *heldUpstream { return &heldUpstream{release: make(chan struct{})} }

func (h *heldUpstream) Upload(ctx context.Context, _ *datacache.Object, _ *os.File) (string, error) {
	select {
	case <-h.release:
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// withCache attaches a caching gateway to a test server.
func withCache(t *testing.T, srv *Server, writeBack bool) (*datacache.DataCache, *heldUpstream) {
	t.Helper()
	held := newHeldUpstream()
	cfg := datacache.Config{
		Dir:       filepath.Join(t.TempDir(), "datacache"),
		WriteBack: writeBack,
	}
	if writeBack {
		cfg.Upstream = held
	}
	dc, err := datacache.Open(cfg)
	if err != nil {
		t.Fatalf("open the cache: %v", err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	srv.datacache = dc
	return dc, held
}

func cacheStatusOf(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	rec, body := do(t, srv, http.MethodGet, "/v1/cache/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cache status = %d", rec.Code)
	}
	return body
}

// ---------------------------------------------------------------- status

// Switched off, the endpoints answer plainly rather than 404. A 404 would send
// somebody hunting for a typo in a path that is correct.
func TestTheCacheEndpointsAnswerWhenTheGatewayIsOff(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	body := cacheStatusOf(t, srv)
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	// An empty cache is safe to shut down, and saying so when the feature is off
	// keeps a client from having to special-case the answer.
	if body["safe_to_shut_down"] != true {
		t.Errorf("safe_to_shut_down = %v, want true", body["safe_to_shut_down"])
	}

	rec, _ := do(t, srv, http.MethodGet, "/v1/cache/objects", "")
	if rec.Code != http.StatusOK {
		t.Errorf("objects with no cache = %d, want 200", rec.Code)
	}
	// The ones that would do something say they cannot, with a reason and a way
	// out rather than a code name.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/cache/flush", `{}`},
		{http.MethodPost, "/v1/cache/refresh", `{"path":"/Docs/report.pdf"}`},
		{http.MethodDelete, "/v1/cache", ""},
	} {
		rec, body := do(t, srv, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, rec.Code)
			continue
		}
		msg := body["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "switched off") || !strings.Contains(msg, "--cache-dir") {
			t.Errorf("%s %s: the message does not say how to turn it on: %q", tc.method, tc.path, msg)
		}
	}
}

// safe_to_shut_down is computed by the gateway, not derived by each client, so
// that the CLI, the web UI and a script cannot disagree (§3.5.2 rule 3).
func TestStatusReportsWhatIsWaitingToBeUploaded(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	dc, held := withCache(t, srv, true)
	defer close(held.release)

	if body := cacheStatusOf(t, srv); body["safe_to_shut_down"] != true {
		t.Error("an empty cache is not safe to shut down")
	}

	w, err := dc.Put(datacache.PutRequest{RemotePath: "/Docs/new.bin", FolderID: "FD1", Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	body := cacheStatusOf(t, srv)
	if body["safe_to_shut_down"] != false {
		t.Error("safe_to_shut_down is true with an object that has not been uploaded")
	}
	if got := body["dirty_objects"].(float64); got != 1 {
		t.Errorf("dirty_objects = %v, want 1", got)
	}
	if got := body["dirty_bytes"].(float64); got != 4 {
		t.Errorf("dirty_bytes = %v, want 4", got)
	}

	// And the object listing names it, with a state that says it is not upstream.
	rec, list := do(t, srv, http.MethodGet, "/v1/cache/objects", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("objects = %d", rec.Code)
	}
	objects := list["objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("objects = %d, want 1", len(objects))
	}
	first := objects[0].(map[string]any)
	if first["remote_path"] != "/Docs/new.bin" {
		t.Errorf("remote_path = %v", first["remote_path"])
	}
	if state := first["state"].(string); state != "dirty" && state != "uploading" {
		t.Errorf("state = %q, want dirty or uploading", state)
	}
}

// ---------------------------------------------------------------- refusals

// Refreshing or clearing something unsent is 409, and the message says what to do.
// §4.4.1: "对 dirty 条目必须拒绝,否则就是丢数据".
func TestRefreshAndClearAnswer409ForUnsentData(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	dc, held := withCache(t, srv, true)
	defer close(held.release)

	w, _ := dc.Put(datacache.PutRequest{RemotePath: "/Docs/precious.bin", FolderID: "FD1", Size: 2})
	_, _ = w.Write([]byte("hi"))
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	rec, body := do(t, srv, http.MethodPost, "/v1/cache/refresh", `{"path":"/Docs/precious.bin"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("refresh on unsent data = %d, want 409", rec.Code)
	}
	env := body["error"].(map[string]any)
	if env["code"] != "conflict" {
		t.Errorf("code = %v, want conflict", env["code"])
	}
	msg := env["message"].(string)
	// It has to say why, in terms of the consequence rather than of the state
	// machine: "conflict" alone tells a user nothing about what they nearly did.
	for _, want := range []string{"only copy", "flush"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q: %q", want, msg)
		}
	}

	rec, body = do(t, srv, http.MethodDelete, "/v1/cache", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("clear with unsent data = %d, want 409", rec.Code)
	}
	if m := body["error"].(map[string]any)["message"].(string); !strings.Contains(m, "waiting to be uploaded") {
		t.Errorf("the message does not say what is in the way: %q", m)
	}
	// And nothing was removed.
	if !dc.Has("/Docs/precious.bin") {
		t.Error("the refused request removed the object anyway")
	}
}

// A clean object refreshes, and clearing empties the cache.
func TestRefreshAndClearWorkOnCleanObjects(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	dc, _ := withCache(t, srv, false)

	f, err := dc.Fill("/Docs/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("cached bytes"))
	if _, err := f.CommitClean(""); err != nil {
		t.Fatal(err)
	}

	rec, _ := do(t, srv, http.MethodPost, "/v1/cache/refresh", `{"path":"/Docs/report.pdf"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh on a clean object = %d, want 200", rec.Code)
	}
	if dc.Has("/Docs/report.pdf") {
		t.Error("the object survived a refresh")
	}
	// Refreshing what is not there is a 404 rather than a silent success: a
	// caller that asked to forget a specific path should learn it had the wrong
	// one.
	rec, _ = do(t, srv, http.MethodPost, "/v1/cache/refresh", `{"path":"/Docs/report.pdf"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("refresh on a missing object = %d, want 404", rec.Code)
	}

	rec, _ = do(t, srv, http.MethodDelete, "/v1/cache", "")
	if rec.Code != http.StatusOK {
		t.Errorf("clear = %d, want 200", rec.Code)
	}
}

// ---------------------------------------------------------------- transfers

// A write-back upload answers 202, not 201, and says what that means. The status
// code is the durability contract in one number: the bridge has it, OpenDrive does
// not.
func TestAWriteBackUploadAnswers202AndSaysWhy(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	content := []byte("the file the client just sent")
	r := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/streamed.bin",
		bytes.NewReader(content))
	r.ContentLength = int64(len(content))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("write-back upload = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cache_state"] != "dirty" {
		t.Errorf("cache_state = %v, want dirty", body["cache_state"])
	}
	if got := body["size"].(float64); int(got) != len(content) {
		t.Errorf("size = %v, want %d", got, len(content))
	}
	// The status code alone is not enough. Somebody reading a log needs the
	// sentence, and a client author meeting 202 for the first time should not
	// have to look it up.
	note, _ := body["note"].(string)
	if !strings.Contains(note, "not reached OpenDrive") {
		t.Errorf("the response does not say the file is not upstream yet: %q", note)
	}

	// And it is immediately readable, which is the read-after-write consistency
	// §3.5.3 offers against upstream's own delay (D44).
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/v1/download/stream?path=/Docs/streamed.bin", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("reading back a just-written file = %d", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if !bytes.Equal(rec2.Body.Bytes(), content) {
		t.Error("the bytes read back are not the bytes written")
	}
}

// Without write-back the endpoint is unchanged: synchronous, 201, upstream has it.
// The cache must not quietly change the meaning of a response for a deployment
// that did not ask for it.
func TestWithoutWriteBackTheUploadStaysSynchronous(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	withCache(t, srv, false)

	content := []byte("straight through")
	r := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/direct.bin",
		bytes.NewReader(content))
	r.ContentLength = int64(len(content))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload without write-back = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// A download says where the bytes came from, on a miss as well as on a hit. A
// header present only on a hit is a header nobody can rely on.
func TestADownloadReportsWhetherItCameFromTheCache(t *testing.T) {
	content := []byte("0123456789abcdef")
	u := newFakeUpstream(t)
	u.withTransfers(content)
	srv, _ := u.serverWithEngine(t)
	withCache(t, srv, false)

	// First read: a miss, from OpenDrive, and it fills the cache on the way past.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/download/stream?path=/Docs/report.pdf", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first read = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("first read X-Cache = %q, want MISS", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("first read returned %q", rec.Body.String())
	}

	// Second read: a hit, from this disk, with the same bytes.
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/v1/download/stream?path=/Docs/report.pdf", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second read = %d", rec2.Code)
	}
	if got := rec2.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second read X-Cache = %q, want HIT", got)
	}
	if !bytes.Equal(rec2.Body.Bytes(), content) {
		t.Errorf("the cached copy differs from what was downloaded: %q", rec2.Body.String())
	}

	// The hit and miss are counted, so the reported rate describes what callers
	// experienced rather than what the index contains.
	st := cacheStatusOf(t, srv)
	if st["hits"].(float64) != 1 || st["misses"].(float64) != 1 {
		t.Errorf("hits=%v misses=%v, want 1 and 1", st["hits"], st["misses"])
	}
}

// A truncated download must not become a cache hit. It would otherwise serve the
// same truncated file to everybody afterwards, with nothing to notice it by.
func TestATruncatedDownloadIsNotCached(t *testing.T) {
	u := newFakeUpstream(t)
	// An upstream that announces more than it sends: the download pipeline treats
	// the short body as a failure, and the fill must be discarded with it.
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "download/file.json") {
			if r.URL.Query().Get("test") == "1" {
				_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true}`)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", "bytes 0-1023/1024")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("short"))
			return
		}
		u.serve(w, r)
	})
	srv, _ := u.serverWithEngine(t)
	dc, _ := withCache(t, srv, false)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/download/stream?path=/Docs/report.pdf", nil))

	// Whatever the client was told, the cache must not be holding a partial file
	// as though it were the whole one.
	if dc.Has("/Docs/report.pdf") {
		r, err := dc.Get("/Docs/report.pdf")
		if err == nil {
			data, _ := io.ReadAll(r)
			_ = r.Close()
			t.Fatalf("a truncated download was cached as %d bytes: %q", len(data), data)
		}
	}
}

// 507 cache_full, and the message leads with the reassurance. "Insufficient
// storage" on its own reads as though something was dropped, and nothing was.
func TestAFullCacheAnswers507AndSaysNothingWasLost(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	held := newHeldUpstream()
	defer close(held.release)
	dc, err := datacache.Open(datacache.Config{
		Dir:           filepath.Join(t.TempDir(), "datacache"),
		WriteBack:     true,
		Upstream:      held,
		MaxBytes:      64 << 10,
		MaxDirtyBytes: 1 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	srv.datacache = dc

	// Fill the allowance.
	first := make([]byte, 900)
	r := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/first.bin",
		bytes.NewReader(first))
	r.ContentLength = int64(len(first))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the first write = %d: %s", rec.Code, rec.Body.String())
	}

	// The next one does not fit.
	second := make([]byte, 900)
	r2 := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/second.bin",
		bytes.NewReader(second))
	r2.ContentLength = int64(len(second))
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, r2)

	if rec2.Code != http.StatusInsufficientStorage {
		t.Fatalf("a full cache = %d, want 507: %s", rec2.Code, rec2.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	env := body["error"].(map[string]any)
	if env["code"] != "cache_full" {
		t.Errorf("code = %v, want cache_full", env["code"])
	}
	msg := env["message"].(string)
	for _, want := range []string{"nothing has been lost", "cache flush"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the 507 does not say %q: %q", want, msg)
		}
	}
	// And the object already accepted is untouched.
	if !dc.Has("/Docs/first.bin") {
		t.Error("the refused write disturbed the object already in the cache")
	}
}

// Flushing with wait blocks until nothing is unsent, which is the answer a person
// needs before turning the machine off.
func TestFlushWithWaitBlocksUntilEverythingIsSent(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	dc, held := withCache(t, srv, true)

	w, _ := dc.Put(datacache.PutRequest{RemotePath: "/Docs/waiting.bin", FolderID: "FD1", Size: 3})
	_, _ = w.Write([]byte("abc"))
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		rec, _ := do(t, srv, http.MethodPost, "/v1/cache/flush", `{"wait":true}`)
		done <- rec.Code
	}()

	select {
	case code := <-done:
		t.Fatalf("the flush returned %d while the upload was still held", code)
	case <-time.After(100 * time.Millisecond):
	}

	close(held.release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("flush --wait = %d, want 200", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the flush never returned after the upload was released")
	}
	if st := cacheStatusOf(t, srv); st["safe_to_shut_down"] != true {
		t.Errorf("safe_to_shut_down = %v after a completed flush", st["safe_to_shut_down"])
	}
}

// Flushing a path the cache has never heard of is a 404, not a silent success.
func TestFlushingAnUnknownPathIs404(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	rec, _ := do(t, srv, http.MethodPost, "/v1/cache/flush", `{"path":"/Docs/never-seen.bin"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("flushing an unknown path = %d, want 404", rec.Code)
	}
}

// A bad path is refused by the same path layer as everywhere else, rather than
// reaching the cache with something it would key by.
func TestTheCacheEndpointsValidatePaths(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	for _, bad := range []string{"", "Docs/relative", "/Docs/../../etc/passwd"} {
		rec, _ := do(t, srv, http.MethodPost, "/v1/cache/refresh",
			fmt.Sprintf(`{"path":%q}`, bad))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("refresh %q = %d, want 400 or 404", bad, rec.Code)
		}
	}
}
