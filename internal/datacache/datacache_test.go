package datacache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers

// fakeUpstream stands in for OpenDrive. Everything about the durability rules has
// to be tested against an upstream that can be made to fail, hang or lie on
// demand, because those are the conditions the rules exist for.
type fakeUpstream struct {
	mu sync.Mutex
	// uploaded records what arrived, by remote path, with the bytes.
	uploaded map[string][]byte
	// err, when set, is returned by every Upload.
	err error
	// failFirst makes the first n attempts per path fail with err before
	// succeeding, for testing retry without testing the retry ladder's timing.
	failFirst map[string]int
	// hash, when set for a path, is returned instead of the real one — for the
	// "a 200 proves nothing" case.
	hash map[string]string
	// block, when non-nil, holds every upload until it is closed. This is how a
	// test keeps objects dirty for as long as it needs them.
	block chan struct{}
	calls int
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{
		uploaded:  map[string][]byte{},
		failFirst: map[string]int{},
		hash:      map[string]string{},
	}
}

func (u *fakeUpstream) Upload(ctx context.Context, obj *Object, content *os.File) (string, error) {
	u.mu.Lock()
	block := u.block
	u.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if n := u.failFirst[obj.RemotePath]; n > 0 {
		u.failFirst[obj.RemotePath] = n - 1
		if u.err != nil {
			return "", u.err
		}
		return "", errors.New("upstream said no")
	}
	if u.err != nil {
		return "", u.err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	u.uploaded[obj.RemotePath] = data
	if h, ok := u.hash[obj.RemotePath]; ok {
		return h, nil
	}
	return obj.Hash, nil
}

func (u *fakeUpstream) got(path string) ([]byte, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	b, ok := u.uploaded[path]
	return b, ok
}

func (u *fakeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.uploaded)
}

func (u *fakeUpstream) hold() { u.mu.Lock(); u.block = make(chan struct{}); u.mu.Unlock() }
func (u *fakeUpstream) release() {
	u.mu.Lock()
	if u.block != nil {
		close(u.block)
		u.block = nil
	}
	u.mu.Unlock()
}

// newTestCache opens a write-back gateway over a temporary directory.
func newTestCache(t *testing.T, cfg Config) (*DataCache, *fakeUpstream) {
	t.Helper()
	up := newFakeUpstream()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "cache")
	}
	if cfg.Upstream == nil {
		cfg.Upstream = up
	}
	cfg.WriteBack = true
	c, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, up
}

// put writes an object and returns it, failing the test on error.
func put(t *testing.T, c *DataCache, path string, content []byte) *Object {
	t.Helper()
	o, err := tryPut(c, path, content)
	if err != nil {
		t.Fatalf("Put %s: %v", path, err)
	}
	return o
}

// tryPut is put without the assertion, for the tests that expect a refusal.
func tryPut(c *DataCache, path string, content []byte) (*Object, error) {
	w, err := c.Put(PutRequest{RemotePath: path, FolderID: "1", Size: int64(len(content))})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(content); err != nil {
		return nil, err
	}
	return w.Commit()
}

// fill stores a clean object, as a read-through would.
func fill(t *testing.T, c *DataCache, path string, content []byte) *Object {
	t.Helper()
	w, err := c.Fill(path)
	if err != nil {
		t.Fatalf("Fill %s: %v", path, err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	o, err := w.CommitClean("")
	if err != nil {
		t.Fatalf("CommitClean %s: %v", path, err)
	}
	return o
}

func readBack(t *testing.T, c *DataCache, path string) []byte {
	t.Helper()
	r, err := c.Get(path)
	if err != nil {
		t.Fatalf("Get %s: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// eventually polls until cond holds, so a test does not depend on how quickly a
// flush worker gets there.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func bytesOf(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// ---------------------------------------------------------------- read cache

func TestAReadCacheServesWhatItStored(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	want := bytesOf(4096, 7)
	fill(t, c, "/Docs/report.pdf", want)

	if got := readBack(t, c, "/Docs/report.pdf"); string(got) != string(want) {
		t.Fatal("the cached bytes are not the bytes that were stored")
	}
	st := c.Status()
	if st.Hits != 1 || st.Misses != 0 {
		t.Errorf("hits=%d misses=%d, want 1 and 0", st.Hits, st.Misses)
	}
	if st.Objects != 1 || st.Bytes != int64(len(want)) {
		t.Errorf("objects=%d bytes=%d", st.Objects, st.Bytes)
	}
}

func TestAMissIsAMiss(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	if _, err := c.Get("/nothing/here.txt"); !errors.Is(err, ErrNotCached) {
		t.Fatalf("Get on an empty cache = %v, want ErrNotCached", err)
	}
	if st := c.Status(); st.Misses != 1 || st.Hits != 0 {
		t.Errorf("misses=%d hits=%d", st.Misses, st.Hits)
	}
}

// A path that means the same thing must be one object, not two. In write-back
// mode two entries for one file would be two conflicting writes racing to upload.
func TestPathsThatMeanTheSameThingAreOneObject(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	fill(t, c, "/Docs/report.pdf", []byte("first"))
	fill(t, c, "/Docs//report.pdf", []byte("second"))

	if st := c.Status(); st.Objects != 1 {
		t.Fatalf("objects=%d, want 1", st.Objects)
	}
	if got := readBack(t, c, "/Docs/report.pdf"); string(got) != "second" {
		t.Errorf("content = %q, want the second write", got)
	}
}

// A fill whose content does not hash to what upstream said is refused rather than
// stored. A cache that can serve the wrong bytes is worse than no cache.
func TestAFillThatDoesNotMatchUpstreamsHashIsRefused(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	w, err := c.Fill("/Docs/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("the bytes that arrived")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CommitClean("00000000000000000000000000000000"); err == nil {
		t.Fatal("a mismatched fill was accepted into the cache")
	}
	if c.Has("/Docs/report.pdf") {
		t.Error("the mismatched object is in the index")
	}
	if left, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "*.bin")); len(left) != 0 {
		t.Errorf("content files left behind: %v", left)
	}
}

// An aborted fill — a download that failed halfway — must not become a hit.
func TestAnAbortedFillIsNotAHit(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	w, err := c.Fill("/Docs/half.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytesOf(1024, 3)); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if c.Has("/Docs/half.bin") {
		t.Fatal("an aborted download is in the cache")
	}
	if tmps, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "writing-*")); len(tmps) != 0 {
		t.Errorf("temporary files left behind: %v", tmps)
	}
}

// Read-after-write: a client that has just written gets its own bytes back, which
// is what §3.5.3 offers against upstream's own read-after-write delay (D44).
func TestAWriteIsImmediatelyReadable(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold() // nothing reaches upstream during this test
	defer up.release()

	want := bytesOf(2048, 11)
	put(t, c, "/Docs/fresh.bin", want)
	if got := readBack(t, c, "/Docs/fresh.bin"); string(got) != string(want) {
		t.Fatal("a just-written object did not read back as itself")
	}
}

// The directory holds the user's files in the clear, so the mode matters and is
// the only protection there is (§3.5.4).
func TestTheCacheDirectoryIsPrivate(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	info, err := os.Stat(c.cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("cache directory mode = %04o, want 0700", mode)
	}
}

func TestRefreshDropsACleanObject(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	fill(t, c, "/Docs/a.bin", bytesOf(512, 1))
	if err := c.Refresh("/Docs/a.bin"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if c.Has("/Docs/a.bin") {
		t.Error("the object survived a refresh")
	}
	if err := c.Refresh("/Docs/a.bin"); !errors.Is(err, ErrNotCached) {
		t.Errorf("refreshing what is gone = %v, want ErrNotCached", err)
	}
}

func TestClearDropsEveryCleanObject(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	for i := 0; i < 5; i++ {
		fill(t, c, fmt.Sprintf("/Docs/%d.bin", i), bytesOf(256, byte(i)))
	}
	if err := c.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if st := c.Status(); st.Objects != 0 || st.Bytes != 0 {
		t.Errorf("after Clear: objects=%d bytes=%d", st.Objects, st.Bytes)
	}
	left, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "*.bin"))
	if len(left) != 0 {
		t.Errorf("content files left behind: %v", left)
	}
}

// The index survives a restart, and the hit that follows it proves the content did
// too — not just the metadata.
func TestTheIndexAndTheContentSurviveARestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	want := bytesOf(8192, 21)
	fill(t, c, "/Docs/kept.bin", want)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	if got := readBack(t, again, "/Docs/kept.bin"); string(got) != string(want) {
		t.Fatal("the content did not survive the restart")
	}
	if st := again.Status(); st.Objects != 1 || st.Bytes != int64(len(want)) {
		t.Errorf("after restart: objects=%d bytes=%d", st.Objects, st.Bytes)
	}
}

// A content file that disappeared behind the cache's back is a miss, not a
// truncated read or a panic. Two shapes: gone entirely, and shorter than recorded.
func TestAnObjectWhoseContentWentMissingIsAMiss(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		c, _ := newTestCache(t, Config{})
		fill(t, c, "/Docs/vanishing.bin", bytesOf(1024, 5))
		if err := os.Remove(filepath.Join(c.cfg.Dir, contentName("/Docs/vanishing.bin"))); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get("/Docs/vanishing.bin"); !errors.Is(err, ErrNotCached) {
			t.Fatalf("Get = %v, want ErrNotCached", err)
		}
		if c.Has("/Docs/vanishing.bin") {
			t.Error("the index still claims the object")
		}
	})

	t.Run("truncated across a restart", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cache")
		up := newFakeUpstream()
		c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
		if err != nil {
			t.Fatal(err)
		}
		fill(t, c, "/Docs/short.bin", bytesOf(4096, 9))
		_ = c.Close()

		// A write that did not finish leaves a file shorter than the journal says.
		if err := os.Truncate(filepath.Join(dir, contentName("/Docs/short.bin")), 100); err != nil {
			t.Fatal(err)
		}
		again, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = again.Close() }()
		if again.Has("/Docs/short.bin") {
			t.Fatal("a truncated object came back as a cache hit; it would have served the " +
				"wrong bytes and called it a success")
		}
	})
}

// Content on disk that the journal does not mention is the debris of a write that
// did not commit. It is removed rather than adopted: nothing knows what path it
// belongs to, so it could only ever be served as the wrong file.
func TestOrphanedContentIsRemovedAtStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, contentName("/Docs/never-committed.bin"))
	if err := os.WriteFile(orphan, bytesOf(512, 2), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: newFakeUpstream()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned content survived startup: %v", err)
	}
	if st := c.Status(); st.Objects != 0 {
		t.Errorf("objects=%d, want 0", st.Objects)
	}
}

// MaxDirtyBytes above MaxBytes is refused at configuration time, because it makes
// rule 1 unsatisfiable: unsent data could fill the store with nothing clean left
// to evict, and every write would then be refused with the allowance apparently
// unused.
func TestAnImpossibleAllowanceIsRefusedUpFront(t *testing.T) {
	_, err := Open(Config{
		Dir: filepath.Join(t.TempDir(), "c"), WriteBack: true, Upstream: newFakeUpstream(),
		MaxBytes: 1 << 20, MaxDirtyBytes: 4 << 20,
	})
	if err == nil {
		t.Fatal("max_dirty_bytes above max_bytes was accepted")
	}
	for _, want := range []string{"max_dirty_bytes", "max_bytes", "nothing left to evict"} {
		if !contains(err.Error(), want) {
			t.Errorf("the message does not explain %q: %v", want, err)
		}
	}
}

// Write-back needs somewhere to flush to; a gateway that accepted writes with no
// upstream would be a very slow way to lose files.
func TestWriteBackWithoutAnUpstreamIsRefused(t *testing.T) {
	_, err := Open(Config{Dir: filepath.Join(t.TempDir(), "c"), WriteBack: true})
	if err == nil {
		t.Fatal("write-back with no upstream was accepted")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// slogText builds a text logger over a buffer, for tests that assert what the
// daemon told the operator. Several do, because in this package a log line is
// sometimes the only output a failure has.
func slogText(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
