package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// An upstream that accepts the connection and never answers is worse for a client
// than one that is plainly down: nothing fails, so nothing is retried quickly, and
// the SDK's whole retry ladder runs with an HTTP timeout on every rung. Measured
// against a real daemon on main before the budget existed: 27 s to accept a write
// while the mock was stopped, 20 s to refuse one, and no answer in 150 s at all
// for a blackhole. The budget makes a slow upstream count as an unreachable one.

// stallUpstream makes every request wait until the client gives up — or until the
// test ends, because httptest.Server.Close waits for handlers and a request made
// by a background worker has no deadline of its own to end it.
func (u *fakeUpstream) stallUpstream(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // registered after the server's, so it runs first
	u.srv.Config.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
}

// answeredWithin runs a request and fails the test if it takes longer than limit,
// without waiting out the very hang being tested for.
func answeredWithin(t *testing.T, limit time.Duration, do func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do() }()
	select {
	case rec := <-done:
		return rec
	case <-time.After(limit):
		t.Fatalf("no answer within %s: the destination check is not bounded", limit)
		return nil
	}
}

func shortBudget(t *testing.T) {
	t.Helper()
	old := outagePrecheckBudget
	outagePrecheckBudget = 150 * time.Millisecond
	t.Cleanup(func() { outagePrecheckBudget = old })
}

func TestAStalledUpstreamIsTreatedAsAnOutageForAWriteTheGatewayCanHold(t *testing.T) {
	shortBudget(t)
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)
	u.stallUpstream(t)

	rec := answeredWithin(t, 5*time.Second, func() *httptest.ResponseRecorder {
		return putStream(srv, "/Docs/stalled.bin", []byte("x"), true)
	})
	if rec.Code != http.StatusAccepted {
		t.Errorf("overwrite=true against a stalled upstream = %d, want 202: %s", rec.Code, rec.Body)
	}

	rec = answeredWithin(t, 5*time.Second, func() *httptest.ResponseRecorder {
		return putStream(srv, "/Docs/stalled2.bin", []byte("x"), false)
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("overwrite=false against a stalled upstream = %d, want 503 in words: %s", rec.Code, rec.Body)
	}
}

// The same bound applies to the job route, which shares the function.
func TestAStalledUpstreamDoesNotHangTheJobRouteEither(t *testing.T) {
	shortBudget(t)
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)
	u.stallUpstream(t)

	local := t.TempDir() + "/f.bin"
	if err := os.WriteFile(local, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := answeredWithin(t, 5*time.Second, func() *httptest.ResponseRecorder {
		r, _ := do(t, srv, http.MethodPost, "/v1/upload",
			`{"local_path":"`+local+`","remote_path":"/Docs/job.bin","overwrite":true}`)
		return r
	})
	if rec.Code != http.StatusAccepted {
		t.Errorf("job route against a stalled upstream = %d, want 202: %s", rec.Code, rec.Body)
	}
}

// Without write-back nothing can be held, so a slow upstream is just slow: the
// check is allowed to finish and its answer stands. A path whose folder does not
// exist is the case that tells the two apart — cut short it would be deferred and
// accepted; allowed to finish it is the 404 it always was.
func TestWithoutWriteBackTheDestinationCheckIsNotCutShort(t *testing.T) {
	shortBudget(t)
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	withCache(t, srv, false)
	base := u.srv.Config.Handler
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond) // answers, but slower than the budget
		base.ServeHTTP(w, r)
	})

	local := t.TempDir() + "/f.bin"
	if err := os.WriteFile(local, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := answeredWithin(t, 15*time.Second, func() *httptest.ResponseRecorder {
		r, _ := do(t, srv, http.MethodPost, "/v1/upload",
			`{"local_path":"`+local+`","remote_path":"/NoSuchFolder/x.bin","overwrite":true}`)
		return r
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing folder with write-back off and a slow upstream = %d, want 404: %s", rec.Code, rec.Body)
	}
}
