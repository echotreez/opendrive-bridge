package datacache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The five acceptance tests of whitepaper §3.5.2.
//
// These are not ordinary unit tests and they are not optional. Every other stage
// of this project could fail by refusing to do something; this one can fail by
// losing a file somebody believes is stored. Each rule gets a test that tries to
// break it, and the names say what is being promised rather than what is being
// called.
//
//	1. TestRule1: a dirty entry is never evicted, whatever the capacity pressure
//	2. TestRule2: max_dirty_bytes refuses the write and harms nothing already here
//	3. TestRule3: nothing is acknowledged before the journal is on the platter
//	4. TestRule4: a killed process loses nothing that was acknowledged  (crash_test.go)
//	5. TestRule5: shutdown drains, and names what it could not finish

// ---------------------------------------------------------------- rule 1

// Capacity pressure takes clean entries and leaves unsent ones alone, even when
// that means running over the size target.
//
// The trap this guards against is an eviction policy written as "least recently
// used", full stop. A dirty object that nobody has read since it was written is
// the *most* attractive LRU candidate there is, and evicting it deletes the only
// copy of the user's file to save disk space.
func TestRule1DirtyObjectsAreNeverEvicted(t *testing.T) {
	c, up := newTestCache(t, Config{
		MaxBytes:      20 << 10,
		MaxDirtyBytes: 8 << 10,
		HighWatermark: 0.9,
		LowWatermark:  0.5,
	})
	up.hold() // so the dirty objects stay dirty for the whole test
	defer up.release()

	// Four unsent objects, written first, so they are also the least recently
	// used — the worst case for a naive policy.
	dirtyPaths := []string{"/unsent/1", "/unsent/2", "/unsent/3", "/unsent/4"}
	for i, p := range dirtyPaths {
		put(t, c, p, bytesOf(2<<10, byte(i)))
	}
	// Then clean ones, enough to go over the high watermark.
	for i := 0; i < 8; i++ {
		fill(t, c, fmt.Sprintf("/clean/%d", i), bytesOf(2<<10, byte(100+i)))
	}

	st := c.Status()
	if st.DirtyObjects != len(dirtyPaths) {
		t.Fatalf("dirty_objects = %d, want %d", st.DirtyObjects, len(dirtyPaths))
	}
	// Every unsent object is still here, and still readable with its own bytes.
	for i, p := range dirtyPaths {
		if !c.Has(p) {
			t.Errorf("%s was evicted; it was the only copy of that data", p)
			continue
		}
		want := bytesOf(2<<10, byte(i))
		if got := readBack(t, c, p); !bytes.Equal(got, want) {
			t.Errorf("%s came back with the wrong bytes", p)
		}
	}
	// And eviction did happen — otherwise this test would pass on a cache that
	// never evicts anything, which proves nothing about rule 1.
	remainingClean := 0
	for i := 0; i < 8; i++ {
		if c.Has(fmt.Sprintf("/clean/%d", i)) {
			remainingClean++
		}
	}
	if remainingClean == 8 {
		t.Fatal("nothing was evicted at all, so this test did not exercise capacity pressure")
	}
	t.Logf("kept all %d unsent objects; evicted %d of 8 clean ones",
		len(dirtyPaths), 8-remainingClean)
}

// The limit case of rule 1: when everything is unsent and the store is over its
// target, nothing is evicted and the cache goes over budget. Deliberately.
func TestRule1AnAllDirtyCacheGoesOverBudgetRatherThanLoseData(t *testing.T) {
	var logged bytes.Buffer
	c, up := newTestCache(t, Config{
		MaxBytes:      6 << 10,
		MaxDirtyBytes: 6 << 10,
		HighWatermark: 0.5,
		LowWatermark:  0.25,
		Logger:        slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	up.hold()
	defer up.release()

	// Five objects of 1 KiB against a 6 KiB store whose high watermark is half of
	// that: 5120 bytes over a 3072-byte target, all of it unsent. The numbers are
	// spelled out because the first version of this test used three objects, which
	// landed exactly *on* the watermark rather than over it, and skipped itself.
	// An acceptance test that decides it cannot run is not an acceptance test, so
	// the guard below fails rather than skips if that ever happens again.
	const objects = 5
	for i := 0; i < objects; i++ {
		put(t, c, fmt.Sprintf("/unsent/%d", i), bytesOf(1<<10, byte(i)))
	}
	st := c.Status()
	if st.DirtyObjects != objects {
		t.Fatalf("dirty_objects = %d, want %d", st.DirtyObjects, objects)
	}
	high := int64(float64(st.MaxBytes) * 0.5)
	if st.Bytes <= high {
		t.Fatalf("the store holds %d bytes against a high watermark of %d, so the limit case "+
			"was never reached and this test proved nothing", st.Bytes, high)
	}
	// Over the target, and everything still here.
	for i := 0; i < objects; i++ {
		if !c.Has(fmt.Sprintf("/unsent/%d", i)) {
			t.Fatalf("/unsent/%d was evicted", i)
		}
	}
	// And it said so, rather than silently exceeding a configured limit.
	if !strings.Contains(logged.String(), "never discarded to save space") {
		t.Errorf("the log does not explain why the cache is over its target:\n%s", logged.String())
	}
}

// ---------------------------------------------------------------- rule 2

// Reaching max_dirty_bytes refuses the write, and everything already in the cache
// is untouched.
//
// "宁可明确拒绝,也不丢已接收的数据" — better a clear refusal than losing data
// already received. The refusal has to be checkable by the caller, so it is a
// sentinel error the REST layer turns into 507 `cache_full`, and its message has
// to tell a person their data is safe, because "insufficient storage" on its own
// reads like something was dropped.
func TestRule2AFullCacheRefusesTheWriteAndHarmsNothing(t *testing.T) {
	c, up := newTestCache(t, Config{MaxBytes: 64 << 10, MaxDirtyBytes: 4 << 10})
	up.hold()
	defer up.release()

	// Fill the dirty allowance exactly.
	first := bytesOf(3<<10, 1)
	put(t, c, "/unsent/first", first)
	before := c.Status()

	// The next write does not fit.
	refused, err := tryPut(c, "/unsent/second", bytesOf(3<<10, 2))
	if !errors.Is(err, ErrCacheFull) {
		t.Fatalf("Put over the allowance = %v (%v), want ErrCacheFull", err, refused)
	}
	// The message is for a person, and it has to say the data is safe.
	for _, want := range []string{"nothing has been lost", "cache flush"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}

	// Nothing already accepted was harmed: same object, same bytes, same numbers.
	after := c.Status()
	if after.DirtyObjects != before.DirtyObjects || after.DirtyBytes != before.DirtyBytes {
		t.Errorf("the refusal changed the cache: before %d/%d, after %d/%d",
			before.DirtyObjects, before.DirtyBytes, after.DirtyObjects, after.DirtyBytes)
	}
	if got := readBack(t, c, "/unsent/first"); !bytes.Equal(got, first) {
		t.Error("the object that was already here came back with different bytes")
	}
	if c.Has("/unsent/second") {
		t.Error("the refused object is in the index")
	}
	// And no debris: a refused write leaves no temporary or content file.
	for _, pattern := range []string{"writing-*", contentName("/unsent/second")} {
		if left, _ := filepath.Glob(filepath.Join(c.cfg.Dir, pattern)); len(left) != 0 {
			t.Errorf("the refused write left %v behind", left)
		}
	}

	// Once the allowance frees up, the same write succeeds — the refusal was
	// about capacity, not about the request.
	up.release()
	eventually(t, "the first object to reach upstream", func() bool {
		return c.Status().DirtyObjects == 0
	})
	if _, err := tryPut(c, "/unsent/second", bytesOf(3<<10, 2)); err != nil {
		t.Errorf("the same write failed after the allowance freed up: %v", err)
	}
}

// A caller that does not declare a size — a chunked upload has no Content-Length —
// must not be able to walk past the allowance. The limit is enforced as the bytes
// arrive, and crossing it leaves everything else alone.
func TestRule2AnUndeclaredSizeCannotWalkPastTheAllowance(t *testing.T) {
	c, up := newTestCache(t, Config{MaxBytes: 64 << 10, MaxDirtyBytes: 4 << 10})
	up.hold()
	defer up.release()

	kept := bytesOf(2<<10, 5)
	put(t, c, "/unsent/kept", kept)

	// Size 0: the cache does not know how big this is until it stops arriving.
	w, err := c.Put(PutRequest{RemotePath: "/unsent/streamed", FolderID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	var wrote int
	var writeErr error
	for i := 0; i < 64; i++ {
		n, err := w.Write(bytesOf(512, byte(i)))
		wrote += n
		if err != nil {
			writeErr = err
			break
		}
	}
	if !errors.Is(writeErr, ErrCacheFull) {
		t.Fatalf("writing past the allowance = %v after %d bytes, want ErrCacheFull", writeErr, wrote)
	}
	if c.Has("/unsent/streamed") {
		t.Error("the over-long object is in the index")
	}
	if got := readBack(t, c, "/unsent/kept"); !bytes.Equal(got, kept) {
		t.Error("the object that was already here was damaged")
	}
	if left, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "writing-*")); len(left) != 0 {
		t.Errorf("the aborted write left %v behind", left)
	}
}

// ---------------------------------------------------------------- rule 3

// Nothing is acknowledged before the journal reaches the disk.
//
// This is the only rule that cannot be tested by observing success, because a
// working system and a system with no fsync at all behave identically until the
// power goes out. So the sync is made to fail on purpose, and the assertion is
// about what the caller was told: an error, and an index that does not claim the
// object.
func TestRule3NothingIsAcknowledgedBeforeTheJournalIsDurable(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	// Durability is asserted, not assumed: a successful write syncs the journal.
	before := c.jnl.syncCount()
	put(t, c, "/unsent/durable", bytesOf(1024, 1))
	if after := c.jnl.syncCount(); after <= before {
		t.Fatalf("a committed write did not flush the journal (syncs %d -> %d)", before, after)
	}

	// Now break it, for exactly the record that records this object. Naming the
	// record rather than saying "the next sync" matters: the flush workers journal
	// their own state changes all the time, so a "next sync" flag would be
	// consumed by whichever write got there first and this test would pass or fail
	// on scheduling. Which is how it failed the first time it was run.
	c.jnl.breakSyncWhen(func(rec journalRecord) error {
		if rec.Op == opPut && rec.Obj != nil && rec.Obj.RemotePath == "/unsent/never-acknowledged" {
			return errors.New("the disk went away")
		}
		return nil
	})
	o, err := tryPut(c, "/unsent/never-acknowledged", bytesOf(1024, 2))
	if err == nil {
		t.Fatalf("a write was acknowledged though its journal record was not durable: %v", o)
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Errorf("the error does not say what failed: %v", err)
	}
	if c.Has("/unsent/never-acknowledged") {
		t.Error("the index claims an object whose record was never written")
	}
	if st := c.Status(); st.DirtyObjects != 1 {
		t.Errorf("dirty_objects = %d, want 1 — only the first write counted", st.DirtyObjects)
	}
	// The content file must be gone too. Leaving it would produce, on the next
	// startup, either an orphan to clean up or — much worse, if the record had
	// landed — an object the client was never told about.
	if _, err := os.Stat(filepath.Join(c.cfg.Dir, contentName("/unsent/never-acknowledged"))); !os.IsNotExist(err) {
		t.Errorf("content was left on disk for an unacknowledged write: %v", err)
	}
}

// The other half of rule 3, and the one that matters to a user: what was
// acknowledged is recoverable. Everything acknowledged before a simulated loss of
// the process comes back after it.
func TestRule3WhatWasAcknowledgedIsRecoverable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := map[string][]byte{}
	for i := 0; i < 6; i++ {
		p := fmt.Sprintf("/unsent/%d", i)
		content := bytesOf(1024+i, byte(i))
		if _, err := tryPut(c, p, content); err != nil {
			t.Fatalf("Put %s: %v", p, err)
		}
		acknowledged[p] = content
	}
	// No Close: the process is treated as having stopped without warning. The
	// journal is all there is to go on. (A real kill is TestRule4, in
	// crash_test.go; this asserts the same property without the process
	// machinery, so a failure here is easier to read.)

	up2 := newFakeUpstream()
	again, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up2})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()

	if st := again.Status(); st.DirtyObjects != len(acknowledged) {
		t.Fatalf("after recovery dirty_objects = %d, want %d", st.DirtyObjects, len(acknowledged))
	}
	// And they reach upstream with the right bytes, without anyone asking.
	eventually(t, "every recovered object to reach upstream", func() bool {
		return up2.count() == len(acknowledged)
	})
	for p, want := range acknowledged {
		got, ok := up2.got(p)
		if !ok {
			t.Errorf("%s never reached upstream", p)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s reached upstream with the wrong bytes", p)
		}
	}
	up.release()
}

// ---------------------------------------------------------------- rule 5

// Shutdown drains: SIGTERM stops new writes, flushes what it can, and if it runs
// out of time it names every object it could not finish.
func TestRule5ShutdownDrains(t *testing.T) {
	t.Run("it finishes when it can", func(t *testing.T) {
		c, up := newTestCache(t, Config{DrainTimeout: 10 * time.Second})
		up.hold()
		for i := 0; i < 5; i++ {
			put(t, c, fmt.Sprintf("/unsent/%d", i), bytesOf(512, byte(i)))
		}
		if c.Status().DirtyObjects != 5 {
			t.Fatal("the objects did not stay dirty")
		}
		up.release()

		if err := c.Drain(context.Background()); err != nil {
			t.Fatalf("Drain: %v", err)
		}
		st := c.Status()
		if st.DirtyObjects != 0 {
			t.Errorf("after a successful drain dirty_objects = %d", st.DirtyObjects)
		}
		if !st.SafeToShutDown {
			t.Error("safe_to_shut_down is false after a drain that finished")
		}
		if up.count() != 5 {
			t.Errorf("%d of 5 objects reached upstream", up.count())
		}
	})

	t.Run("it names what it could not finish", func(t *testing.T) {
		var logged bytes.Buffer
		c, up := newTestCache(t, Config{
			DrainTimeout: 300 * time.Millisecond,
			Logger:       slog.New(slog.NewTextHandler(&logged, nil)),
		})
		up.hold() // nothing will ever be flushed
		defer up.release()

		paths := []string{"/unsent/alpha", "/unsent/beta", "/unsent/gamma"}
		for i, p := range paths {
			put(t, c, p, bytesOf(256, byte(i)))
		}

		err := c.Drain(context.Background())
		if err == nil {
			t.Fatal("Drain reported success with data still unsent")
		}
		// The summary says how many and where.
		if !strings.Contains(err.Error(), "3 object") || !strings.Contains(err.Error(), c.cfg.Dir) {
			t.Errorf("the error does not say how many or where: %v", err)
		}
		// And the log names each one. A count tells an operator that something was
		// lost and nothing about what; the paths are what makes it recoverable by
		// hand, which is the whole point of rule 4's second half.
		out := logged.String()
		for _, p := range paths {
			if !strings.Contains(out, p) {
				t.Errorf("the log does not name %s:\n%s", p, out)
			}
		}
	})

	t.Run("draining refuses new writes", func(t *testing.T) {
		c, up := newTestCache(t, Config{DrainTimeout: 100 * time.Millisecond})
		up.hold()
		defer up.release()
		put(t, c, "/unsent/one", bytesOf(128, 1))

		_ = c.Drain(context.Background())

		if _, err := tryPut(c, "/unsent/two", bytesOf(128, 2)); !errors.Is(err, ErrClosed) {
			t.Errorf("a write during shutdown = %v, want ErrClosed", err)
		}
		// A read is still fine: refusing those would break a client that is
		// finishing up, and reads risk nothing.
		if _, err := c.Get("/unsent/one"); err != nil {
			t.Errorf("a read during shutdown was refused: %v", err)
		}
	})
}

// ---------------------------------------------------------------- flush, states

// The three-state lifecycle is visible, and `uploading` still counts as unsent.
// Somebody writing `== StateDirty` instead of Unsent() is the way this design
// fails quietly: an upload in flight has not arrived, and a crash must bring it
// back.
func TestAnUploadInFlightStillCountsAsUnsent(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	put(t, c, "/unsent/inflight", bytesOf(1024, 1))
	eventually(t, "the object to reach the uploading state", func() bool {
		for _, o := range c.Objects() {
			if o.RemotePath == "/unsent/inflight" && o.State == StateUploading {
				return true
			}
		}
		return false
	})
	st := c.Status()
	if st.DirtyObjects != 1 || st.DirtyBytes != 1024 {
		t.Errorf("an uploading object is not counted as unsent: dirty=%d/%d",
			st.DirtyObjects, st.DirtyBytes)
	}
	if st.SafeToShutDown {
		t.Error("safe_to_shut_down is true while an upload is in flight")
	}
	if !StateUploading.Unsent() {
		t.Error("StateUploading.Unsent() is false")
	}
}

// A flush that succeeds with the wrong hash does not make the object clean. "A 200
// proves nothing" applies to this caller as much as to any other.
func TestAFlushThatReportsTheWrongHashLeavesTheObjectDirty(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hash["/unsent/lied-about"] = "ffffffffffffffffffffffffffffffff"

	put(t, c, "/unsent/lied-about", bytesOf(1024, 1))
	eventually(t, "the flush to be attempted", func() bool {
		for _, o := range c.Objects() {
			if o.RemotePath == "/unsent/lied-about" && o.FlushAttempts > 0 {
				return true
			}
		}
		return false
	})
	for _, o := range c.Objects() {
		if o.RemotePath != "/unsent/lied-about" {
			continue
		}
		if !o.State.Unsent() {
			t.Error("an object upstream hashed differently was marked clean")
		}
		if !strings.Contains(o.LastError, "hashes to") {
			t.Errorf("the recorded reason does not mention the hashes: %q", o.LastError)
		}
	}
}

// A permanent refusal keeps the data. Dropping an object because upstream said no
// once would destroy the only copy on the strength of one answer — and this
// project has two discrepancies showing that a refusal's wording does not even
// reliably say what it was (D39, D40).
func TestAPermanentRefusalKeepsTheData(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.err = errors.New("no")

	content := bytesOf(1024, 3)
	put(t, c, "/unsent/refused", content)
	eventually(t, "the flush to fail", func() bool {
		for _, o := range c.Objects() {
			if o.RemotePath == "/unsent/refused" && o.FlushAttempts > 0 {
				return true
			}
		}
		return false
	})
	if !c.Has("/unsent/refused") {
		t.Fatal("a refused object was dropped from the cache")
	}
	if got := readBack(t, c, "/unsent/refused"); !bytes.Equal(got, content) {
		t.Error("the refused object's bytes changed")
	}
	if st := c.Status(); st.DirtyObjects != 1 {
		t.Errorf("dirty_objects = %d, want 1", st.DirtyObjects)
	}
}

// Flush --wait is rule 3's answer to "is it safe to turn this off": it blocks
// until nothing is unsent, rather than making the user watch a number.
func TestFlushWaitBlocksUntilEverythingIsSent(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	for i := 0; i < 4; i++ {
		put(t, c, fmt.Sprintf("/unsent/%d", i), bytesOf(256, byte(i)))
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var flushErr error
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		flushErr = c.Flush(ctx, "", true)
	}()

	// It must still be waiting while anything is unsent.
	time.Sleep(50 * time.Millisecond)
	if c.Status().DirtyObjects == 0 {
		t.Fatal("the objects were flushed while upstream was held")
	}
	up.release()
	wg.Wait()

	if flushErr != nil {
		t.Fatalf("Flush --wait: %v", flushErr)
	}
	if st := c.Status(); st.DirtyObjects != 0 || !st.SafeToShutDown {
		t.Errorf("after Flush --wait: dirty=%d safe=%v", st.DirtyObjects, st.SafeToShutDown)
	}
}

// Refreshing or clearing something unsent would throw away the only copy, so both
// refuse. §4.4.1 calls for it explicitly.
func TestRefreshAndClearRefuseToDiscardUnsentData(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	put(t, c, "/unsent/precious", bytesOf(512, 1))
	fill(t, c, "/clean/ordinary", bytesOf(512, 2))

	if err := c.Refresh("/unsent/precious"); !errors.Is(err, ErrDirty) {
		t.Errorf("Refresh on an unsent object = %v, want ErrDirty", err)
	}
	if err := c.Clear(); !errors.Is(err, ErrDirty) {
		t.Errorf("Clear with unsent data = %v, want ErrDirty", err)
	}
	// Refusing means refusing everything, not clearing what it could: a caller
	// who asked to empty the cache must not have to find out afterwards that it
	// is not empty.
	if !c.Has("/clean/ordinary") {
		t.Error("a refused Clear removed the clean objects anyway")
	}
	if !c.Has("/unsent/precious") {
		t.Fatal("the unsent object was removed")
	}
}

// Status answers the question a person actually has, in one field, so that the
// CLI, the GUI and anything else agree on the answer (rule 3).
func TestStatusAnswersWhetherItIsSafeToShutDown(t *testing.T) {
	c, up := newTestCache(t, Config{})
	if st := c.Status(); !st.SafeToShutDown {
		t.Error("an empty cache is not safe to shut down")
	}
	up.hold()
	put(t, c, "/unsent/x", bytesOf(64, 1))
	if st := c.Status(); st.SafeToShutDown {
		t.Error("safe_to_shut_down is true with unsent data")
	}
	if st := c.Status(); st.OldestDirtyAge < 0 {
		t.Errorf("oldest_dirty_age = %v", st.OldestDirtyAge)
	}
	up.release()
	eventually(t, "the flush", func() bool { return c.Status().SafeToShutDown })
}
