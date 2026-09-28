package datacache

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// An object rewritten while its previous version is being uploaded must not be
// marked clean when that previous upload finishes. The state change belongs to
// the version that was sent; applying it by path alone told the gateway that
// bytes OpenDrive had never seen were safe, and eviction could then delete the
// only copy. S3 clients overwrite keys routinely (rclone, Hyper Backup's index),
// so this is an ordinary sequence, not a race only a test can find.
func TestARewriteDuringAnUploadIsNotMarkedClean(t *testing.T) {
	c, up := newTestCache(t, Config{MaxBytes: 1 << 20, MaxDirtyBytes: 1 << 20, FlushWorkers: 1})
	up.hold()

	put(t, c, "/k", []byte("version one"))
	eventually(t, "the first upload to start", func() bool { s, _ := c.StateOf("/k"); return s == StateUploading })

	v2 := put(t, c, "/k", []byte("version two, newer"))
	up.release()

	// The first upload completes with version one's bytes. Version two is still
	// the only copy of what the client last wrote.
	eventually(t, "the first upload to land", func() bool { _, ok := up.got("/k"); return ok })
	time.Sleep(50 * time.Millisecond)
	if s, _ := c.StateOf("/k"); s == StateClean {
		if b, _ := up.got("/k"); !bytes.Equal(b, []byte("version two, newer")) {
			t.Fatalf("version two is marked clean, but upstream holds %q", b)
		}
	}

	// And it is delivered, not left dirty for ever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Flush(ctx, "/k", true); err != nil {
		t.Fatalf("flush: %v", err)
	}
	b, _ := up.got("/k")
	if !bytes.Equal(b, []byte("version two, newer")) {
		t.Fatalf("upstream holds %q, want version two", b)
	}
	if s, _ := c.StateOf("/k"); s != StateClean {
		t.Fatalf("state = %s after delivering version two", s)
	}
	_ = v2
}

// The same property across a restart: the journal's "clean" record for version
// one must not be replayed onto version two.
func TestAStaleCleanRecordIsNotReplayedOntoANewerVersion(t *testing.T) {
	objects := map[string]*Object{}
	v1 := &Object{RemotePath: "/k", Size: 1, Hash: "aa", State: StateDirty, StoredAt: time.Unix(0, 100)}
	v2 := &Object{RemotePath: "/k", Size: 2, Hash: "bb", State: StateDirty, StoredAt: time.Unix(0, 200)}
	applyRecord(objects, journalRecord{Op: opPut, Obj: v1})
	applyRecord(objects, journalRecord{Op: opState, Path: "/k", State: StateUploading, Of: 100})
	applyRecord(objects, journalRecord{Op: opPut, Obj: v2})
	applyRecord(objects, journalRecord{Op: opState, Path: "/k", State: StateClean, Of: 100})
	if got := objects["/k"]; got.State != StateDirty || got.Hash != "bb" {
		t.Fatalf("after replay: %+v; want version two, still dirty", got)
	}
	// Records written before versions were recorded still apply.
	applyRecord(objects, journalRecord{Op: opState, Path: "/k", State: StateClean})
	if objects["/k"].State != StateClean {
		t.Fatal("an unversioned state record was ignored")
	}
}
