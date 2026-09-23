package datacache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the job engine asks of the gateway (§4.4.1's two legs).

func localFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The upload half of leg one: a local file becomes an unsent object with its own
// copy of the bytes. Its own copy matters — once this returns, the gateway has
// promised delivery, and depending on the caller's file would be promising something
// it does not control.
func TestPutLocalFileTakesItsOwnCopy(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	content := bytesOf(4096, 13)
	src := localFile(t, content)

	obj, err := c.PutLocalFile(context.Background(), PutRequest{
		RemotePath: "/Docs/from-disk.bin", FolderID: "FD1",
	}, src)
	if err != nil {
		t.Fatalf("PutLocalFile: %v", err)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", obj.Size, len(content))
	}
	if obj.State != StateDirty && obj.State != StateUploading {
		t.Errorf("state = %q, want an unsent one", obj.State)
	}
	// The name defaults from the path when the caller did not give one.
	if obj.Name != "from-disk.bin" {
		t.Errorf("name = %q", obj.Name)
	}

	// Deleting the source changes nothing: the gateway owns its copy.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if got := readBack(t, c, "/Docs/from-disk.bin"); !bytes.Equal(got, content) {
		t.Error("the cached copy did not survive the source being deleted")
	}
}

// A file that does not fit the unsent allowance is refused with an error the job
// engine can recognise across the package boundary, without either package importing
// the other. Getting that wrong would silently turn "upload it directly" into "fail
// the transfer".
func TestPutLocalFileRefusesWhatWillNotFitAndSaysSoRecognisably(t *testing.T) {
	c, up := newTestCache(t, Config{MaxBytes: 64 << 10, MaxDirtyBytes: 1 << 10})
	up.hold()
	defer up.release()

	src := localFile(t, bytesOf(4096, 1))
	_, err := c.PutLocalFile(context.Background(), PutRequest{
		RemotePath: "/Docs/too-big.bin", FolderID: "FD1",
	}, src)
	if !errors.Is(err, ErrCacheFull) {
		t.Fatalf("PutLocalFile over the allowance = %v, want ErrCacheFull", err)
	}

	// The marker the job engine looks for, which is what lets it fall back to a
	// direct upload instead of failing.
	var full interface{ CacheFull() bool }
	if !errors.As(err, &full) || !full.CacheFull() {
		t.Error("the error does not carry the CacheFull marker")
	}
	if c.Has("/Docs/too-big.bin") {
		t.Error("the refused file is in the index")
	}
	if left, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "writing-*")); len(left) != 0 {
		t.Errorf("the refused write left %v behind", left)
	}
}

func TestPutLocalFileRejectsWhatItCannotRead(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	ctx := context.Background()
	if _, err := c.PutLocalFile(ctx, PutRequest{RemotePath: "/x"}, "/no/such/file"); err == nil {
		t.Error("a missing source file was accepted")
	}
	if _, err := c.PutLocalFile(ctx, PutRequest{RemotePath: "/y"}, t.TempDir()); err == nil {
		t.Error("a directory was accepted as a file to upload")
	}
}

// Leg two: the job waits on the same notification `cache flush --wait` uses, so
// there is one answer to "is it really upstream" rather than two that could
// disagree.
func TestWaitUntilSentReturnsWhenTheObjectHasGone(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()

	src := localFile(t, bytesOf(1024, 3))
	if _, err := c.PutLocalFile(context.Background(), PutRequest{
		RemotePath: "/Docs/waiting.bin", FolderID: "FD1",
	}, src); err != nil {
		t.Fatal(err)
	}

	// While upstream is held it must not return.
	quick, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := c.WaitUntilSent(quick, "/Docs/waiting.bin"); err == nil {
		t.Fatal("WaitUntilSent returned while the object was still here")
	}

	up.release()
	ctx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := c.WaitUntilSent(ctx, "/Docs/waiting.bin"); err != nil {
		t.Fatalf("WaitUntilSent after the flush: %v", err)
	}
	if state, ok := c.StateOf("/Docs/waiting.bin"); !ok || state != StateClean {
		t.Errorf("state = %q (known %v), want clean", state, ok)
	}
}

// Waiting for something that was never cached is an error rather than an immediate
// success: a caller that asked about the wrong path should learn so.
func TestWaitUntilSentOnSomethingUnknownIsAnError(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()
	if err := c.WaitUntilSent(context.Background(), "/never/written"); !errors.Is(err, ErrNotCached) {
		t.Errorf("WaitUntilSent on an unknown path = %v, want ErrNotCached", err)
	}
}

// StateOf distinguishes "sent" from "still here", which is how the engine tells a
// finished upload from a wait that was cut short.
func TestStateOfReportsWhatIsThere(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	if _, ok := c.StateOf("/nothing"); ok {
		t.Error("an uncached path reported a state")
	}
	put(t, c, "/Docs/held.bin", bytesOf(256, 5))
	state, ok := c.StateOf("/Docs/held.bin")
	if !ok {
		t.Fatal("a cached object reported no state")
	}
	if !state.Unsent() {
		t.Errorf("state = %q, want an unsent one", state)
	}
}

// ---------------------------------------------------------------- CopyTo

// The download half of leg one: a hit is written to the caller's own path, and it
// costs no request at all.
func TestCopyToWritesACachedObjectLocally(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	content := bytesOf(8192, 21)
	fill(t, c, "/Docs/report.pdf", content)

	dst := filepath.Join(t.TempDir(), "nested", "out.pdf")
	served, err := c.CopyTo(context.Background(), "/Docs/report.pdf", dst)
	if err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if !served {
		t.Fatal("a cached object was not served")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("the copy does not match the cached bytes")
	}
	// No debris: the copy goes through a temporary file and is renamed, so a
	// half-written destination never exists under the name the caller asked for.
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), "*.part-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// A miss is false and no error: the file is upstream and can be fetched. Returning
// an error would turn an ordinary miss into a failed transfer.
func TestCopyToReportsAMissWithoutAnError(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	dst := filepath.Join(t.TempDir(), "out.bin")

	served, err := c.CopyTo(context.Background(), "/not/cached", dst)
	if err != nil {
		t.Errorf("a miss reported an error: %v", err)
	}
	if served {
		t.Error("a miss reported itself served")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a miss created the destination file anyway")
	}
}

// An unsent object is served like any other, which is what gives a client
// read-after-write consistency through the job path too (§3.5.3, D44).
func TestCopyToServesSomethingNotYetUploaded(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	content := bytesOf(512, 7)
	put(t, c, "/Docs/fresh.bin", content)

	dst := filepath.Join(t.TempDir(), "fresh.bin")
	served, err := c.CopyTo(context.Background(), "/Docs/fresh.bin", dst)
	if err != nil || !served {
		t.Fatalf("CopyTo on an unsent object: served=%v err=%v", served, err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) {
		t.Error("the bytes differ from what was written")
	}
}

// ---------------------------------------------------------------- FillFrom

// A read-through fill writes to the destination first and caches as a side effect. A
// caching failure must not fail the transfer it was meant to speed up — the exact
// opposite of the write path, and §3.5.2's asymmetry in one function.
func TestFillFromAlwaysWritesTheDestination(t *testing.T) {
	c, _ := newTestCache(t, Config{})
	content := bytesOf(2048, 9)

	var dst bytes.Buffer
	n, err := c.FillFrom("/Docs/streamed.bin", &dst, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("FillFrom: %v", err)
	}
	if n != int64(len(content)) || !bytes.Equal(dst.Bytes(), content) {
		t.Errorf("wrote %d bytes; destination has %d", n, dst.Len())
	}
	if !c.Has("/Docs/streamed.bin") {
		t.Error("the object was not cached")
	}
	if got := readBack(t, c, "/Docs/streamed.bin"); !bytes.Equal(got, content) {
		t.Error("the cached copy differs from what was written")
	}
}

// tolerant is what makes that promise keepable: io.MultiWriter gives up on the first
// error from any writer, which is the wrong policy when one of the writers is only
// an optimisation.
func TestAFailingCacheWriterDoesNotStopTheCopy(t *testing.T) {
	var dst bytes.Buffer
	broken := &tolerant{w: errorWriter{}}
	n, err := io.Copy(io.MultiWriter(&dst, broken), strings.NewReader("all of these bytes"))
	if err != nil {
		t.Fatalf("the copy failed because the cache side did: %v", err)
	}
	if int(n) != len("all of these bytes") || dst.String() != "all of these bytes" {
		t.Errorf("the destination got %q", dst.String())
	}
	if !broken.broken {
		t.Error("the tolerant writer did not notice it had failed")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// ---------------------------------------------------------------- adapter

// The adapter satisfies what the job engine wants, and a nil gateway adapts to
// nothing rather than to a broken something.
func TestForJobsAdaptsTheGateway(t *testing.T) {
	if ForJobs(nil) != nil {
		t.Error("ForJobs(nil) returned an adapter")
	}

	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	jc := ForJobs(c)
	if jc == nil {
		t.Fatal("ForJobs returned nil for a real gateway")
	}
	if !jc.WriteBack() {
		t.Error("WriteBack reported false on a write-back gateway")
	}

	src := localFile(t, bytesOf(1024, 2))
	size, err := jc.PutLocalFile(context.Background(), "/Docs/via-adapter.bin", "FD1", "", src)
	if err != nil {
		t.Fatalf("PutLocalFile: %v", err)
	}
	if size != 1024 {
		t.Errorf("size = %d, want 1024", size)
	}

	dst := filepath.Join(t.TempDir(), "back.bin")
	served, err := jc.CopyTo(context.Background(), "/Docs/via-adapter.bin", dst)
	if err != nil || !served {
		t.Fatalf("CopyTo through the adapter: served=%v err=%v", served, err)
	}

	up.release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := jc.WaitUntilSent(ctx, "/Docs/via-adapter.bin"); err != nil {
		t.Errorf("WaitUntilSent through the adapter: %v", err)
	}
}

// A read-only gateway says so, which is how the engine knows to upload directly.
func TestTheAdapterReportsAReadOnlyGateway(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(Config{Dir: dir, WriteBack: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if ForJobs(c).WriteBack() {
		t.Error("a read-only gateway reported that it accepts writes")
	}
}
