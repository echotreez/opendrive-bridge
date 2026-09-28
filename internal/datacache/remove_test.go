package datacache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A delete drops an unsent object: otherwise the flusher uploads a file the user
// has just deleted, and listings show it as pending for ever.
func TestRemoveDropsAnUnsentObjectForGood(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, up := newTestCache(t, Config{Dir: dir, MaxBytes: 1 << 20, MaxDirtyBytes: 1 << 20, FlushWorkers: 1})
	up.hold()
	// Keep it dirty: the first attempt is held, so nothing can have been sent.
	put(t, c, "/a/one", []byte("one"))
	put(t, c, "/a/b/two", []byte("two"))
	put(t, c, "/other", []byte("three"))

	found, unsent, err := c.Remove(context.Background(), "/other")
	if err != nil || !found || !unsent {
		t.Fatalf("Remove = %v %v %v", found, unsent, err)
	}
	if c.Has("/other") {
		t.Fatal("still cached after Remove")
	}
	if st := c.Status(); st.DirtyBytes != int64(len("one")+len("two")) {
		t.Errorf("dirty bytes = %d; the removed object is still counted", st.DirtyBytes)
	}
	up.release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, err := c.RemoveTree(ctx, "/a"); err != nil || n != 2 {
		t.Fatalf("RemoveTree = %d, %v; want 2", n, err)
	}
	if len(c.Objects()) != 0 {
		t.Fatalf("left behind: %v", c.Objects())
	}
	if _, ok := up.got("/other"); ok {
		t.Error("a deleted object was uploaded")
	}

	// And it stays gone across a restart: the drop is journalled.
	crashed(c)
	again, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up, MaxBytes: 1 << 20, MaxDirtyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if len(again.Objects()) != 0 {
		t.Fatalf("resurrected by replay: %v", again.Objects())
	}
}

// Removing an object mid-upload stops the upload and waits for it to return, so
// that the caller's upstream delete comes after it rather than being undone by
// it — and so that a delete does not take as long as the upload would have.
func TestRemoveStopsAnUploadInProgress(t *testing.T) {
	c, up := newTestCache(t, Config{MaxBytes: 1 << 20, MaxDirtyBytes: 1 << 20, FlushWorkers: 1})
	up.hold()
	defer up.release()
	put(t, c, "/k", []byte("data"))
	eventually(t, "the upload to start", func() bool { s, _ := c.StateOf("/k"); return s == StateUploading })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	found, unsent, err := c.Remove(ctx, "/k")
	if err != nil || !found || !unsent {
		t.Fatalf("Remove = %v %v %v", found, unsent, err)
	}
	if c.Has("/k") {
		t.Fatal("still cached")
	}
	if _, ok := up.got("/k"); ok {
		t.Fatal("the stopped upload was recorded as delivered")
	}
	// The worker is free again.
	put(t, c, "/next", []byte("n"))
	up.release()
	eventually(t, "the next object to be sent", func() bool { _, ok := up.got("/next"); return ok })
}
