package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echotreez/opendrive-bridge/internal/datacache"
	"github.com/echotreez/opendrive-bridge/internal/jobs"
)

// A name that is taken when the object is finally sent.
//
// write-back checks the destination when the request arrives and sends the file
// later, so a file can appear at that name in between — someone uploads it on the
// web, another client wins the race. The caller said overwrite=false; the flusher
// must not replace it, and the job that is waiting for the upload must not wait
// for ever on something that is never going to happen (D54).

// takenNames makes create_file answer the way the live API was measured to
// (D54): open_if_exists=0 is a 409 that creates nothing. Apply after
// withTransfers.
func (u *fakeUpstream) takenNames() {
	base := u.srv.Config.Handler
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "create_file") {
			var b struct {
				OpenIfExists float64 `json:"open_if_exists"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &b)
			if b.OpenIfExists == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprint(w, `{"error":{"code":409,"message":"File with such a name already exists."}}`)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
}

// refusingServer is a server whose write-back gateway sends through the real SDK
// uploader to an upstream that finds every no-overwrite name taken.
func refusingServer(t *testing.T) (*Server, *datacache.DataCache) {
	t.Helper()
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	u.takenNames()
	srv := u.server(t)
	dc, err := datacache.Open(datacache.Config{
		Dir: filepath.Join(t.TempDir(), "datacache"), WriteBack: true,
		Upstream: datacache.NewSDKUploader(srv.client),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	srv.datacache = dc
	return srv, dc
}

func TestFlushingARefusedPathAndWaitingSaysSoInsteadOfHanging(t *testing.T) {
	srv, _ := refusingServer(t)

	rec, _ := do(t, srv, http.MethodPut, "/v1/upload/stream?path=/Docs/taken.bin", "x")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("setup write = %d: %s", rec.Code, rec.Body)
	}
	rec, body := do(t, srv, http.MethodPost, "/v1/cache/flush", `{"path":"/Docs/taken.bin","wait":true}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("flush --wait on a refused path = %d, want 409: %s", rec.Code, rec.Body)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	assertUserReadable(t, msg)
}

func TestAJobWhoseNameIsTakenAtFlushFailsInsteadOfHanging(t *testing.T) {
	srv, dc := refusingServer(t)
	engine, err := jobs.New(srv.client, jobs.WithWorkers(1), jobs.WithMaxAttempts(1),
		jobs.WithCache(datacache.ForJobs(dc)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)
	engine.Start(context.Background())
	srv.engine = engine

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The destination is free when the request arrives (the fake lists nothing at
	// /Docs/new.bin), so the request is accepted; the name is "taken" only by the
	// time the flusher asks.
	rec, body := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/new.bin"}`, local))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	job := waitJob(t, srv, body["id"].(string)) // fails the test if the job never finishes
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed: a refused name is not a success and not a wait", job.State)
	}
	if job.Error == nil {
		t.Fatal("the failed job carries no error")
	}
	if job.Error.Code != "conflict" || job.Error.Retryable {
		t.Errorf("error = code %q retryable %v, want a permanent conflict", job.Error.Code, job.Error.Retryable)
	}
	assertUserReadable(t, job.Error.Message)

	// And nothing was discarded.
	st, ok := dc.StateOf("/Docs/new.bin")
	if !ok || !st.Unsent() {
		t.Errorf("the refused file is no longer held (cached=%v state=%s)", ok, st)
	}
}

// The stream route stores the caller's wish with the object.
func TestAStreamWriteKeepsTheOverwriteChoice(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	dc, held := withCache(t, srv, true)
	defer close(held.release)

	for _, tc := range []struct {
		path      string
		overwrite string
		wantNo    bool
	}{
		{"/Docs/keep.bin", "", true},
		{"/Docs/replace.bin", "&overwrite=true", false},
	} {
		rec, _ := do(t, srv, http.MethodPut, "/v1/upload/stream?path="+tc.path+tc.overwrite, "x")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s = %d: %s", tc.path, rec.Code, rec.Body)
		}
		found := false
		for _, o := range dc.Objects() {
			if o.RemotePath == tc.path {
				found = true
				if o.NoOverwrite != tc.wantNo {
					t.Errorf("%s NoOverwrite = %v, want %v", tc.path, o.NoOverwrite, tc.wantNo)
				}
			}
		}
		if !found {
			t.Errorf("%s was not stored", tc.path)
		}
	}
}
