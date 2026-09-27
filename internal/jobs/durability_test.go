package jobs

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// An interrupted upload is one the engine believes did not finish. Upstream may
// know better: close_file_upload can succeed and the process die before the job
// record says so. Reclaim deletes without a trash step, so these tests pin down
// that it only ever deletes what upstream shows to be an empty record (D52).

func interruptedUpload(t *testing.T) (*MemoryStore, *Job) {
	t.Helper()
	store := NewMemoryStore()
	j := &Job{
		ID: "interrupted", Kind: KindUpload, State: StateRunning, Phase: "uploading",
		BytesTotal: 4096, BytesDone: 4096,
		Spec:           Spec{Kind: KindUpload, LocalPath: "/nowhere/payload.bin", FolderID: "dest", Name: "payload.bin", Size: 4096},
		UpstreamFileID: "F9", UpstreamTempLocation: "tmp/9",
	}
	if err := store.Save(j); err != nil {
		t.Fatal(err)
	}
	return store, j
}

func TestRecoverKeepsAnUploadThatHadFinished(t *testing.T) {
	u := newUpstream(t)
	u.infoBody = `{"FileId":"F9","Name":"payload.bin","Size":"4096","FileHash":"0123456789abcdef0123456789abcdef"}`
	store, j := interruptedUpload(t)
	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Fatalf("a finished file was deleted on recovery: %v", got)
	}
	got, err := e.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSucceeded {
		t.Errorf("state = %s, want succeeded: upstream has the whole file", got.State)
	}
	stored, _ := store.Load()
	if len(stored) != 1 || stored[0].State != StateSucceeded || stored[0].UpstreamFileID != "" {
		t.Errorf("the recovered verdict was not saved: %+v", stored)
	}
}

func TestRecoverReclaimsAnEmptyRecord(t *testing.T) {
	u := newUpstream(t)
	u.infoBody = `{"FileId":"F9","Name":"payload.bin","Size":"0","FileHash":""}`
	store, _ := interruptedUpload(t)
	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.reclaims(); len(got) != 1 || got[0] != "F9" {
		t.Fatalf("reclaims = %v, want the empty record F9", got)
	}
}

func TestRecoverKeepsARecordItCannotCheck(t *testing.T) {
	u := newUpstream(t)
	u.infoStatus = http.StatusInternalServerError
	u.infoBody = `{"error":{"code":500,"message":"busy"}}`
	store, _ := interruptedUpload(t)
	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Fatalf("deleted a record it could not check: %v", got)
	}
}

// A record that is neither empty nor the expected file is left alone too: it is
// not ours to judge.
func TestReclaimKeepsARecordThatIsSomethingElse(t *testing.T) {
	u := newUpstream(t)
	u.infoBody = `{"FileId":"F9","Size":"99","FileHash":"ffffffffffffffffffffffffffffffff"}`
	store, _ := interruptedUpload(t)
	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Fatalf("deleted a record that was not an empty one: %v", got)
	}
}

// ---------------------------------------------------------------- finish

// gatedStore holds back the save of a terminal state until released.
type gatedStore struct {
	*MemoryStore
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedStore) Save(j *Job) error {
	if j.State.Terminal() {
		g.once.Do(func() { close(g.reached) })
		<-g.release
	}
	return g.MemoryStore.Save(j)
}

// A terminal state is on disk before anyone can see it. The other order is what
// a CI run caught: Get said "succeeded" while the store still said "running".
func TestAFinishedJobIsSavedBeforeItIsVisible(t *testing.T) {
	u := newUpstream(t)
	store := &gatedStore{MemoryStore: NewMemoryStore(), reached: make(chan struct{}), release: make(chan struct{})}
	e := newEngine(t, u, WithStore(store))
	e.Start(context.Background())

	path := writeLocal(t, 4096)
	job, err := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 4096})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never reached a terminal save")
	}
	// The save is held. Nothing may report the job as finished yet.
	for i := 0; i < 20; i++ {
		got, _ := e.Get(job.ID)
		if got != nil && got.State.Terminal() {
			close(store.release)
			t.Fatalf("Get reported %s before it was saved", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(store.release)
	done := waitForState(t, e, job.ID, StateSucceeded)
	if done == nil || done.State != StateSucceeded {
		t.Fatalf("job = %+v", done)
	}
	stored, _ := store.Load()
	if len(stored) != 1 || stored[0].State != StateSucceeded {
		t.Errorf("stored = %+v", stored)
	}
}
