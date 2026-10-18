package datacache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The caller's "do not replace what is there" has to survive until the flusher
// runs, which can be a restart and many minutes later. These tests pin the three
// places it can be lost: the object, the journal, and the request the flusher
// finally makes.

type sdkAuth struct{}

func (sdkAuth) Credentials(context.Context) (opendrive.Credentials, error) {
	return opendrive.Credentials{SessionID: "s1"}, nil
}
func (sdkAuth) Refresh(context.Context) error { return nil }
func (sdkAuth) Identity() opendrive.Identity {
	return opendrive.Identity{Username: "t", AuthMode: opendrive.AuthModeOAuth2, Seamless: true}
}
func (sdkAuth) AuthState() opendrive.AuthState { return opendrive.StateAuthenticated }

// takenNameUpstream answers create_file the way the live API was measured to
// (probe of 2026-10-17): open_if_exists=0 on a taken name is a 409 that creates
// nothing. It records the flag it was sent and refuses everything after create_file
// so that a test can see what the first call said without building the whole
// upload pipeline.
type takenNameUpstream struct {
	mu    sync.Mutex
	flags []float64
	srv   *httptest.Server
}

func newTakenNameUpstream(t *testing.T) *takenNameUpstream {
	t.Helper()
	u := &takenNameUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if filepath.Base(r.URL.Path) == "create_file.json" {
			f, _ := body["open_if_exists"].(float64)
			u.mu.Lock()
			u.flags = append(u.flags, f)
			u.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if f == 0 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":409,"message":"File with such a name already exists"}}`))
				return
			}
		}
		// Anything else: stop here. The test is about what create_file was asked.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"probe ends here"}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *takenNameUpstream) uploader(t *testing.T) *SDKUploader {
	t.Helper()
	c, err := opendrive.New(
		opendrive.WithBaseURL(u.srv.URL+"/api/v1"),
		opendrive.WithHTTPClient(u.srv.Client()),
		opendrive.WithAuthenticator(sdkAuth{}),
		opendrive.WithAccessProbe(nil),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 0}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return NewSDKUploader(c)
}

func (u *takenNameUpstream) sent() []float64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]float64(nil), u.flags...)
}

func uploadObject(t *testing.T, up *SDKUploader, obj *Object) error {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("payload"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	obj.Size = 7
	_, err = up.Upload(context.Background(), obj, f)
	return err
}

func TestTheFlusherTellsOpenDriveWhetherToReplace(t *testing.T) {
	cases := []struct {
		name        string
		noOverwrite bool
		want        float64
	}{
		{"default replaces, as every journal written before the field meant", false, 1},
		{"a caller who said overwrite=false is not replaced", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newTakenNameUpstream(t)
			_ = uploadObject(t, u.uploader(t), &Object{
				RemotePath: "/Docs/x.bin", FolderID: "FD1", Name: "x.bin", NoOverwrite: tc.noOverwrite,
			})
			got := u.sent()
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("open_if_exists sent = %v, want [%v]", got, tc.want)
			}
		})
	}
}

// The whole point, end to end through the real flusher: a refused name is not
// retried into submission, not dropped — and not waited on for ever. The object
// stays, with a reason, and whoever is waiting for it is told it will not arrive.
func TestAConflictLeavesTheObjectDirtyAndTellsWhoIsWaiting(t *testing.T) {
	u := newTakenNameUpstream(t)
	c, err := Open(Config{
		Dir: filepath.Join(t.TempDir(), "cache"), WriteBack: true,
		Upstream: u.uploader(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	w, err := c.Put(PutRequest{RemotePath: "/Docs/x.bin", FolderID: "FD1", Name: "x.bin",
		Size: 7, NoOverwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("payload"))
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	werr := c.WaitUntilSent(ctx, "/Docs/x.bin")
	if werr == nil {
		t.Fatal("WaitUntilSent returned nil for an object OpenDrive refused")
	}
	if ctx.Err() != nil {
		t.Fatalf("WaitUntilSent hung until the deadline instead of reporting the refusal: %v", werr)
	}
	var refused *RefusedError
	if !errors.As(werr, &refused) {
		t.Fatalf("error = %T %v, want a *RefusedError", werr, werr)
	}
	var api *opendrive.APIError
	if !errors.As(werr, &api) || api.Kind != opendrive.KindConflict {
		t.Errorf("the classifier's error did not come through: %v", werr)
	}

	st, ok := c.StateOf("/Docs/x.bin")
	if !ok || !st.Unsent() {
		t.Errorf("state = %s (cached=%v): a refused name must leave the only copy in place", st, ok)
	}
	for _, o := range c.Objects() {
		if o.RemotePath == "/Docs/x.bin" && o.LastError == "" {
			t.Error("the object carries no reason")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(u.sent()); n != 1 {
		t.Errorf("create_file was called %d times; a conflict is permanent and must not be retried", n)
	}

	// Writing the same path again with overwrite replaces the stuck object, and the
	// new one does not inherit the old one's refusal.
	w2, err := c.Put(PutRequest{RemotePath: "/Docs/x.bin", FolderID: "FD1", Name: "x.bin", Size: 7})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w2.Write([]byte("payload"))
	if _, err := w2.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, o := range c.Objects() {
		if o.RemotePath == "/Docs/x.bin" && o.NoOverwrite {
			t.Error("the replacement still carries the old refusal wish")
		}
	}
	if err := c.refusalOf("/Docs/x.bin"); err != nil {
		t.Errorf("the replacement inherited the old object's refusal: %v", err)
	}
}

// A restart replays the journal. The wish has to come back with the object, or an
// overwrite=false file accepted before a crash would be replaced after it.
func TestNoOverwriteSurvivesARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()
	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		path string
		no   bool
	}{{"/a/keep.bin", true}, {"/a/replace.bin", false}} {
		w, err := c.Put(PutRequest{RemotePath: p.path, FolderID: "1", Size: 1, NoOverwrite: p.no})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
		if _, err := w.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	crashed(c)
	up.release()

	up2 := newFakeUpstream()
	up2.hold()
	defer up2.release()
	c2, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	got := map[string]bool{}
	for _, o := range c2.Objects() {
		got[o.RemotePath] = o.NoOverwrite
	}
	if v, ok := got["/a/keep.bin"]; !ok || !v {
		t.Errorf("keep.bin after restart: present=%v NoOverwrite=%v, want true", ok, v)
	}
	if v, ok := got["/a/replace.bin"]; !ok || v {
		t.Errorf("replace.bin after restart: present=%v NoOverwrite=%v, want false", ok, v)
	}
}

// The job adapter is a translation (overwrite -> NoOverwrite), and a translation
// that inverts the flag is the kind of bug that passes every test that only ever
// uses one value.
func TestTheJobAdapterTranslatesTheOverwriteWish(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()
	jc := ForJobs(c)

	for _, overwrite := range []bool{true, false} {
		path := "/Docs/adapter-keep.bin"
		if overwrite {
			path = "/Docs/adapter-replace.bin"
		}
		src := localFile(t, bytesOf(64, 1))
		if _, err := jc.PutLocalFile(context.Background(), path, "FD1", "", src, overwrite); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, o := range c.Objects() {
			if o.RemotePath == path {
				found = true
				if o.NoOverwrite == overwrite {
					t.Errorf("overwrite=%v stored NoOverwrite=%v", overwrite, o.NoOverwrite)
				}
			}
		}
		if !found {
			t.Errorf("%s not stored", path)
		}
	}
}

// A refusal describes one attempt. If the object is tried again and fails for a
// reason that is only temporary, the old "refused for good" must not be left
// standing — a job waiting on it would be told something that is no longer true,
// and would give up on a file that is about to go.
func TestAStaleRefusalIsClearedWhenTheObjectIsTriedAgain(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.mu.Lock()
	up.err = &opendrive.APIError{Kind: opendrive.KindConflict, HTTPCode: 409}
	up.mu.Unlock()
	put(t, c, "/Docs/again.bin", []byte("abc"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.WaitUntilSent(ctx, "/Docs/again.bin"); err == nil {
		t.Fatal("setup: the permanent refusal was not reported")
	}

	up.mu.Lock()
	up.err = &opendrive.APIError{Kind: opendrive.KindNetwork}
	before := up.calls
	up.mu.Unlock()
	if err := c.Flush(ctx, "/Docs/again.bin", false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		up.mu.Lock()
		n := up.calls
		up.mu.Unlock()
		if n > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the object was not tried again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The retry has finished once it has recorded its own reason.
	for {
		if st, _ := c.StateOf("/Docs/again.bin"); st == StateDirty && c.refusalOf("/Docs/again.bin") == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a refusal from an earlier attempt is still standing after a temporary failure: %v",
				c.refusalOf("/Docs/again.bin"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
