package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A job's two legs (§4.4.1).
//
// The point of routing jobs through the gateway is that `phase` becomes a real
// answer rather than a constant, and that `odctl up` returns as soon as the bytes are
// safe on this machine rather than when OpenDrive has them. These tests assert both,
// and — the part that matters most — that a job does **not** report success until
// the object has really left.

// fakeCache is a jobs.Cache whose two legs the test controls.
type fakeCache struct {
	mu sync.Mutex

	writeBack bool
	// full makes PutLocalFile refuse with a cache-full error, so the fallback to a
	// direct upload can be exercised.
	full bool
	// putErr makes PutLocalFile fail for some other reason.
	putErr error

	// sent is closed to let WaitUntilSent return.
	sent chan struct{}
	// waitErr is what WaitUntilSent returns once released.
	waitErr error

	// cached is the set of paths CopyTo will serve.
	cached map[string][]byte

	puts   []string
	waits  []string
	copies []string
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		writeBack: true,
		sent:      make(chan struct{}),
		cached:    map[string][]byte{},
	}
}

func (f *fakeCache) WriteBack() bool { return f.writeBack }

func (f *fakeCache) PutLocalFile(_ context.Context, remotePath, _, _, localPath string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.full {
		return 0, errCacheFullForTest{}
	}
	if f.putErr != nil {
		return 0, f.putErr
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	f.puts = append(f.puts, remotePath)
	return info.Size(), nil
}

func (f *fakeCache) WaitUntilSent(ctx context.Context, remotePath string) error {
	f.mu.Lock()
	f.waits = append(f.waits, remotePath)
	ch, err := f.sent, f.waitErr
	f.mu.Unlock()
	select {
	case <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeCache) CopyTo(_ context.Context, remotePath, localPath string) (bool, error) {
	f.mu.Lock()
	content, ok := f.cached[remotePath]
	if ok {
		f.copies = append(f.copies, remotePath)
	}
	f.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeCache) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.sent:
	default:
		close(f.sent)
	}
}

func (f *fakeCache) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeCache) copyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.copies)
}

// errCacheFullForTest satisfies the CacheFull marker without importing datacache.
type errCacheFullForTest struct{}

func (errCacheFullForTest) Error() string   { return "no room" }
func (errCacheFullForTest) CacheFull() bool { return true }

// IsCacheFull recognises the marker rather than a sentinel, so the two packages do
// not have to import each other. Worth its own assertion, because getting it wrong
// would silently turn "upload it directly" into "fail the transfer".
func TestIsCacheFullRecognisesTheMarkerAndNothingElse(t *testing.T) {
	if !IsCacheFull(errCacheFullForTest{}) {
		t.Error("the marker was not recognised")
	}
	if IsCacheFull(errors.New("something else")) {
		t.Error("an unrelated error was taken for a full cache")
	}
	if IsCacheFull(nil) {
		t.Error("nil was taken for a full cache")
	}
	// Through a wrapper, because the engine sees wrapped errors.
	if !IsCacheFull(fmt.Errorf("while uploading: %w", errCacheFullForTest{})) {
		t.Error("the marker was not found through a wrapper")
	}
}

// startedEngine is newEngine plus Start, since every test here needs workers
// running. The package's newEngine deliberately does not start them, because some
// tests inspect a queued job before a worker can touch it.
func startedEngine(t *testing.T, u *upstream, opts ...Option) *Engine {
	t.Helper()
	e := newEngine(t, u, opts...)
	e.Start(context.Background())
	return e
}

// waitForState2 is waitForState with an assertion attached, so a test that times out
// says which state it was actually in.
func waitForState2(t *testing.T, e *Engine, id string, want State) *Job {
	t.Helper()
	j := waitForState(t, e, id, want)
	if j == nil || j.State != want {
		got := "nothing"
		if j != nil {
			got = string(j.State)
		}
		t.Fatalf("job %s is %s, want %s", id, got, want)
	}
	return j
}

// ---------------------------------------------------------------- upload

// An upload through the gateway has two legs, and does not succeed until the second
// one finishes. That last clause is the whole contract: a job that reported success
// when the bytes reached the cache would be telling the user their file is on
// OpenDrive when it is not.
func TestAnUploadThroughTheCacheHasTwoLegs(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache()
	e := startedEngine(t, up, WithCache(cache))

	local := writeLocal(t, 4096)
	job, err := e.Submit(Spec{
		Kind: KindUpload, LocalPath: local, FolderID: "FD1",
		Name: "report.pdf", RemotePath: "/Docs/report.pdf", Size: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A queued upload already says which leg it is on, because initialPhase predicts
	// it rather than leaving the field empty until a worker gets there.
	if job.Phase != PhaseCaching {
		t.Errorf("a queued upload reports phase %q, want %q", job.Phase, PhaseCaching)
	}

	// Leg one has to actually happen before leg two can be asserted, and the job's
	// own phase is not the signal to wait on — it is PhaseCaching at submit time, so
	// waiting for a phase would return before a worker had run. The cache's own
	// record of the put is the thing that says leg one finished.
	waitFor(t, "the file to be copied into the cache", func() bool {
		return cache.putCount() == 1
	})
	waitFor(t, "the job to reach the uploading leg", func() bool {
		j, _ := e.Get(job.ID)
		return j != nil && j.Phase == PhaseUploading
	})
	j, _ := e.Get(job.ID)
	if j.State.Terminal() {
		t.Fatalf("the job finished while the file was still only in the cache: %s", j.State)
	}
	if cache.putCount() != 1 {
		t.Errorf("the file was put into the cache %d times, want 1", cache.putCount())
	}
	// And nothing went to OpenDrive directly.
	if n := up.uploads(); n != 0 {
		t.Errorf("%d direct uploads happened; the gateway should own delivery", n)
	}

	// Now the gateway reports it sent.
	cache.release()
	final := waitForState2(t, e, job.ID, StateSucceeded)
	if final.Phase != PhaseUploading {
		t.Errorf("final phase = %q, want %q", final.Phase, PhaseUploading)
	}
	if final.BytesDone != final.BytesTotal || final.BytesTotal == 0 {
		t.Errorf("bytes = %d/%d", final.BytesDone, final.BytesTotal)
	}
}

// A file the gateway has no room for goes straight to OpenDrive. A full cache is a
// reason not to cache, not a reason to refuse the upload.
func TestAFileTooBigForTheCacheIsUploadedDirectly(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache()
	cache.full = true
	e := startedEngine(t, up, WithCache(cache))

	local := writeLocal(t, 2048)
	job, err := e.Submit(Spec{
		Kind: KindUpload, LocalPath: local, FolderID: "FD1",
		Name: "big.bin", RemotePath: "/Docs/big.bin", Size: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}

	final := waitForState2(t, e, job.ID, StateSucceeded)
	if up.uploads() != 1 {
		t.Errorf("%d direct uploads, want 1", up.uploads())
	}
	if cache.putCount() != 0 {
		t.Error("the file was cached after the cache said it had no room")
	}
	// The phase says it went straight up, which is true and is what a caller
	// watching two legs needs to see.
	if final.Phase != PhaseUploading {
		t.Errorf("phase = %q, want %q", final.Phase, PhaseUploading)
	}
}

// Write-back off means the gateway is a read cache: uploads go straight up and the
// caching leg never happens.
func TestWithWriteBackOffUploadsGoStraightUp(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache()
	cache.writeBack = false
	e := startedEngine(t, up, WithCache(cache))

	local := writeLocal(t, 1024)
	job, err := e.Submit(Spec{
		Kind: KindUpload, LocalPath: local, FolderID: "FD1",
		Name: "x.bin", RemotePath: "/Docs/x.bin", Size: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForState2(t, e, job.ID, StateSucceeded)
	if up.uploads() != 1 {
		t.Errorf("%d direct uploads, want 1", up.uploads())
	}
	if cache.putCount() != 0 {
		t.Error("a read-only cache accepted a write")
	}
}

// A failure on the second leg fails the job, and the message reaches the user. The
// data is still in the cache — the gateway keeps it and retries — but the job the
// user was watching has to tell them it did not finish.
func TestAFailureWaitingForTheGatewayFailsTheJob(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache()
	cache.waitErr = errors.New("OpenDrive refused this and will refuse it again")
	e := startedEngine(t, up, WithCache(cache))

	local := writeLocal(t, 512)
	job, err := e.Submit(Spec{
		Kind: KindUpload, LocalPath: local, FolderID: "FD1",
		Name: "stuck.bin", RemotePath: "/Docs/stuck.bin", Size: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	cache.release()

	final := waitForState2(t, e, job.ID, StateFailed)
	if final.Error == nil {
		t.Fatal("the job failed with no error recorded")
	}
	if final.Error.Message == "" {
		t.Error("the failure has no message for the user")
	}
}

// ---------------------------------------------------------------- download

// A download served from the cache costs no request at all.
func TestADownloadServedFromTheCacheTouchesNothingUpstream(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache()
	cache.cached["/Docs/report.pdf"] = []byte("the cached bytes")
	e := startedEngine(t, up, WithCache(cache))

	dst := filepath.Join(t.TempDir(), "out.pdf")
	job, err := e.Submit(Spec{
		Kind: KindDownload, FileID: "FILE1", LocalPath: dst,
		RemotePath: "/Docs/report.pdf", Size: 16,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitForState2(t, e, job.ID, StateSucceeded)
	if cache.copyCount() != 1 {
		t.Errorf("the cache served %d copies, want 1", cache.copyCount())
	}
	if n := up.downloads(); n != 0 {
		t.Errorf("%d downloads went upstream for a file that was cached", n)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the cached bytes" {
		t.Errorf("the file contains %q", got)
	}
}

// A miss goes upstream as it always did. The cache must not turn a miss into a
// failure.
func TestADownloadMissGoesUpstream(t *testing.T) {
	up := newUpstream(t)
	cache := newFakeCache() // nothing cached
	e := startedEngine(t, up, WithCache(cache))

	dst := filepath.Join(t.TempDir(), "out.pdf")
	job, err := e.Submit(Spec{
		Kind: KindDownload, FileID: "FILE1", LocalPath: dst,
		RemotePath: "/Docs/missing-from-cache.pdf", Size: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForState2(t, e, job.ID, StateSucceeded)
	if up.downloads() != 1 {
		t.Errorf("%d downloads went upstream, want 1", up.downloads())
	}
}

// Without a cache at all, nothing changes. This is the deployment that did not ask
// for the gateway, and it must behave exactly as it did before v1.2.
func TestWithoutACacheTheEngineIsUnchanged(t *testing.T) {
	up := newUpstream(t)
	e := startedEngine(t, up)

	local := writeLocal(t, 256)
	job, err := e.Submit(Spec{
		Kind: KindUpload, LocalPath: local, FolderID: "FD1",
		Name: "plain.bin", RemotePath: "/Docs/plain.bin", Size: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForState2(t, e, job.ID, StateSucceeded)
	if up.uploads() != 1 {
		t.Errorf("%d direct uploads, want 1", up.uploads())
	}
	// Phase is still reported, because a client should not have to special-case a
	// deployment without a cache.
	if final.Phase != PhaseUploading {
		t.Errorf("phase = %q, want %q", final.Phase, PhaseUploading)
	}
}
