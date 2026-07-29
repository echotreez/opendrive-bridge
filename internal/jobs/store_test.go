package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStoreRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	j := &Job{
		ID: "job1", Kind: KindUpload, State: StateRunning,
		BytesDone: 10, BytesTotal: 100, CreatedAt: time.Now(),
		Spec:           Spec{Kind: KindUpload, LocalPath: "/tmp/x", FolderID: "d"},
		UpstreamFileID: "F1",
	}
	if err := s.Save(j); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].ID != "job1" {
		t.Fatalf("loaded %d jobs, want 1", len(got))
	}
	// The resume state is the point of persisting at all.
	if got[0].UpstreamFileID != "F1" || got[0].Spec.FolderID != "d" {
		t.Errorf("resume state was lost: %+v", got[0])
	}

	if err := s.Delete("job1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.Load(); len(got) != 0 {
		t.Errorf("delete left %d jobs", len(got))
	}
	// Deleting again is not an error.
	if err := s.Delete("job1"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// A save must be atomic, so a crash mid-write cannot leave a file that fails to
// parse — which on recovery would mean an upload record nobody knows to reclaim.
func TestFileStoreLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)

	for i := 0; i < 50; i++ {
		j := &Job{ID: "job1", Kind: KindUpload, State: StateRunning, BytesDone: int64(i)}
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
		// Whatever is on disk at any moment must be a complete, parseable job.
		data, err := os.ReadFile(filepath.Join(dir, "job1"+jobSuffix)) //nolint:gosec // test dir
		if err != nil {
			t.Fatal(err)
		}
		var parsed Job
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("iteration %d left an unparseable file: %v", i, err)
		}
	}

	// No temporary files survive.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestFileStoreReportsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	if err := s.Save(&Job{ID: "good", Kind: KindUpload}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad"+jobSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load()
	if err == nil {
		t.Error("an unreadable job file was skipped silently; that is how a record goes unreclaimed")
	}
	if len(got) != 1 {
		t.Errorf("loaded %d readable jobs, want 1: the good ones must still come back", len(got))
	}
}

func TestFileStoreRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	if err := s.Save(&Job{ID: "../escape"}); err == nil {
		t.Error("a job id containing a path separator was accepted")
	}
	if err := s.Save(&Job{ID: ""}); err == nil {
		t.Error("an empty job id was accepted")
	}
}

func TestNewFileStoreNeedsADirectory(t *testing.T) {
	if _, err := NewFileStore(""); err == nil {
		t.Fatal("want an error with no directory")
	}
}

// ---------------------------------------------------------------- recovery

// A job caught mid-transfer by a crash must come back, and the record its
// interrupted upload left upstream must be reclaimed rather than leaked (D39).
func TestRecoverRequeuesAndReclaimsAnInterruptedUpload(t *testing.T) {
	u := newUpstream(t)
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	// What a crash would have left behind.
	crashed := &Job{
		ID: "crashed", Kind: KindUpload, State: StateRunning,
		BytesDone: 512, BytesTotal: 4096, CreatedAt: time.Now(),
		Spec:                 Spec{Kind: KindUpload, LocalPath: writeLocal(t, 4096), FolderID: "dest", Size: 4096},
		UpstreamFileID:       "F99",
		UpstreamTempLocation: "tmp/99",
	}
	if err := store.Save(crashed); err != nil {
		t.Fatal(err)
	}

	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// The stale record goes first, before anything is retried.
	waitFor(t, "the stale record to be reclaimed", func() bool { return len(u.reclaims()) >= 1 })
	if got := u.reclaims(); got[0] != "F99" {
		t.Errorf("reclaimed %v, want the record the crash left", got)
	}

	got, err := e.Get("crashed")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateQueued {
		t.Errorf("state = %q, want %q", got.State, StateQueued)
	}
	if got.UpstreamFileID != "" || got.BytesDone != 0 {
		t.Errorf("stale resume state survived recovery: %+v", got)
	}

	e.Start(context.Background())
	done := waitForState(t, e, "crashed", StateSucceeded)
	if done.BytesDone != done.BytesTotal {
		t.Errorf("bytes_done = %d, bytes_total = %d", done.BytesDone, done.BytesTotal)
	}
}

// A download resumes properly, because the answer is on the local disk.
func TestRecoverResumesADownloadFromTheLocalFile(t *testing.T) {
	u := newUpstream(t)
	u.content = []byte("0123456789abcdefghij")
	dir := t.TempDir()
	store, _ := NewFileStore(dir)

	dst := filepath.Join(t.TempDir(), "partial.bin")
	if err := os.WriteFile(dst, u.content[:8], 0o600); err != nil {
		t.Fatal(err)
	}

	crashed := &Job{
		ID: "dl", Kind: KindDownload, State: StateRunning, CreatedAt: time.Now(),
		Spec: Spec{Kind: KindDownload, FileID: "F1", LocalPath: dst, Size: int64(len(u.content))},
	}
	if err := store.Save(crashed); err != nil {
		t.Fatal(err)
	}

	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	e.Start(context.Background())
	waitForState(t, e, "dl", StateSucceeded)

	got, err := os.ReadFile(dst) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(u.content) {
		t.Errorf("resumed file = %q, want %q", got, u.content)
	}
}

// A job that had already finished is not run again.
func TestRecoverLeavesTerminalJobsAlone(t *testing.T) {
	u := newUpstream(t)
	store, _ := NewFileStore(t.TempDir())
	for _, st := range []State{StateSucceeded, StateFailed, StateCancelled} {
		if err := store.Save(&Job{ID: string(st), Kind: KindUpload, State: st, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	e := newEngine(t, u, WithStore(store))
	if err := e.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	e.Start(context.Background())
	time.Sleep(100 * time.Millisecond)

	for _, st := range []State{StateSucceeded, StateFailed, StateCancelled} {
		got, err := e.Get(string(st))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != st {
			t.Errorf("a %s job was restarted into %s", st, got.State)
		}
		if got.Attempts != 0 {
			t.Errorf("a %s job was picked up again", st)
		}
	}
	if got := u.reclaims(); len(got) != 0 {
		t.Errorf("recovery reclaimed %v for jobs that had already finished", got)
	}
}

// Progress survives a restart, which is what makes the persisted state worth
// having at all.
func TestProgressIsPersistedAsItHappens(t *testing.T) {
	u := newUpstream(t)
	store, _ := NewFileStore(t.TempDir())
	e := newEngine(t, u, WithStore(store))
	e.Start(context.Background())

	path := writeLocal(t, 4096)
	job, _ := e.Submit(Spec{Kind: KindUpload, LocalPath: path, FolderID: "dest", Size: 4096})
	waitForState(t, e, job.ID, StateSucceeded)

	stored, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("persisted %d jobs, want 1", len(stored))
	}
	if stored[0].State != StateSucceeded || stored[0].BytesDone == 0 {
		t.Errorf("persisted state is stale: %+v", stored[0])
	}
}

func TestMemoryStoreRoundTrips(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Save(&Job{ID: "a", State: StateQueued}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v, %d jobs", err, len(got))
	}
	got[0].State = StateFailed
	if again, _ := s.Load(); again[0].State != StateQueued {
		t.Error("MemoryStore handed out its own copy")
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(); len(got) != 0 {
		t.Error("delete did nothing")
	}
}
