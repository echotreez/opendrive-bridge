package datacache

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The §8.3.1 container check, and the journal machinery underneath it.

// Outside a container there is nothing to warn about, whatever the directory is.
// The check must not fire on an ordinary laptop, because a warning that appears
// for everyone is a warning nobody reads.
func TestDurabilityIsQuietOutsideAContainer(t *testing.T) {
	if inContainer() {
		t.Skip("this test asserts the answer for a host, and this is a container")
	}
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	st := c.Status()
	if !st.Durable {
		t.Errorf("durable = false on a host: %q", st.DurabilityNote)
	}
	if st.DurabilityNote != "" {
		t.Errorf("a host got a durability note: %q", st.DurabilityNote)
	}
}

// With write-back off the question does not arise: nothing here is anything
// OpenDrive does not already have, so a disposable directory costs a re-download.
func TestDurabilityIsNotAQuestionWithoutWriteBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(Config{Dir: dir, WriteBack: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	st := c.Status()
	if !st.Durable || st.DurabilityNote != "" {
		t.Errorf("a read-only cache reported durable=%v note=%q", st.Durable, st.DurabilityNote)
	}
	if st.WriteBack {
		t.Error("write_back is true on a read-only cache")
	}
	// And a write is refused rather than quietly going somewhere unexpected.
	if _, err := c.Put(PutRequest{RemotePath: "/x", Size: 1}); err == nil {
		t.Error("a write-back write was accepted with write-back off")
	}
}

// isMountPoint walks up: a volume at /data makes /data/cache durable too, and
// checking only the exact path would warn about a setup that is fine.
func TestIsMountPointLooksAtAncestorsNotJustTheLeaf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/mountinfo is Linux only")
	}
	// The root filesystem is a mount, so anything under it answers true. This is
	// the ancestor walk working; the check is only meaningful inside a container,
	// where the interesting ancestors are the volumes.
	ok, err := isMountPoint("/tmp/nothing/here/at/all")
	if err != nil {
		t.Fatalf("isMountPoint: %v", err)
	}
	if !ok {
		t.Error("nothing under / was recognised as being on a mount")
	}
}

// The kernel escapes awkward characters in mount points, and a path with a space
// in it is not exotic enough to get wrong.
func TestMountPathsAreUnescaped(t *testing.T) {
	cases := map[string]string{
		`/data`:                 `/data`,
		`/mnt/my\040volume`:     `/mnt/my volume`,
		`/mnt/tab\011here`:      "/mnt/tab\there",
		`/mnt/back\134slash`:    `/mnt/back\slash`,
		`/mnt/two\040\040space`: `/mnt/two  space`,
	}
	for in, want := range cases {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- journal

// The journal survives being rewritten, and the index it describes comes back
// intact. Compaction is the one operation that replaces the whole log, so getting
// it wrong would lose everything at once.
func TestCompactionKeepsTheIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()
	defer up.release()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		fill(t, c, "/clean/"+string(rune('a'+i)), bytesOf(256, byte(i)))
	}
	put(t, c, "/unsent/kept", bytesOf(512, 9))
	before := c.snapshot()
	_ = c.jnl.close()

	jnl, err := compactJournal(dir, before)
	if err != nil {
		t.Fatalf("compactJournal: %v", err)
	}
	_ = jnl.close()

	replayed, err := replayJournal(dir)
	if err != nil {
		t.Fatalf("replay after compaction: %v", err)
	}
	if replayed.torn {
		t.Error("the compacted journal reads as torn")
	}
	if len(replayed.objects) != len(before) {
		t.Fatalf("after compaction the index has %d objects, want %d",
			len(replayed.objects), len(before))
	}
	for _, o := range before {
		got := replayed.objects[o.RemotePath]
		if got == nil {
			t.Errorf("%s was lost by compaction", o.RemotePath)
			continue
		}
		if got.State != o.State || got.Size != o.Size || got.Hash != o.Hash {
			t.Errorf("%s came back as %+v, want %+v", o.RemotePath, got, o)
		}
	}
	_ = c.Close()
}

// A journal that is not there yet is an empty cache, not an error. This is the
// first run.
func TestAnAbsentJournalIsAnEmptyCache(t *testing.T) {
	dir := t.TempDir()
	res, err := replayJournal(dir)
	if err != nil {
		t.Fatalf("replayJournal on an empty directory: %v", err)
	}
	if len(res.objects) != 0 || res.torn {
		t.Errorf("empty directory gave %d objects, torn=%v", len(res.objects), res.torn)
	}
}

// A touch record is the one kind that may be lost without consequence, so it is
// not flushed. Asserting that is worth it because the opposite mistake — syncing
// on every read — would make a read-heavy cache slower than no cache at all.
func TestAReadDoesNotFlushTheJournal(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	fill(t, c, "/clean/read-me", bytesOf(128, 1))
	before := c.jnl.syncCount()
	for i := 0; i < 20; i++ {
		readBack(t, c, "/clean/read-me")
	}
	if after := c.jnl.syncCount(); after != before {
		t.Errorf("20 reads caused %d journal flushes; a read is not a durability event",
			after-before)
	}
}

// Values that would break a line-delimited log have to survive it: a path with a
// newline in it would end the record early, and a journal that could be corrupted
// by a filename is a journal that loses data for a reason nobody would guess.
func TestAwkwardPathsSurviveTheJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()
	defer up.release()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	awkward := []string{
		"/Docs/with space.pdf",
		"/Docs/with\"quote.pdf",
		"/Docs/with\\backslash.pdf",
		"/Docs/中文文件.pdf",
		"/Docs/emoji-🙂.bin",
	}
	for i, p := range awkward {
		fill(t, c, p, bytesOf(64+i, byte(i)))
	}
	_ = c.Close()

	again, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = again.Close() }()
	for i, p := range awkward {
		if !again.Has(p) {
			t.Errorf("%q did not survive the journal", p)
			continue
		}
		want := bytesOf(64+i, byte(i))
		if got := readBack(t, again, p); !bytes.Equal(got, want) {
			t.Errorf("%q came back with the wrong bytes", p)
		}
	}
}

// Turning write-back off while unsent data is still here does not lose it and does
// not hide it. Refusing to start would leave the user no way to get the data up;
// starting quietly would leave them not knowing it was stranded.
func TestUnsentDataWithWriteBackTurnedOffIsReportedLoudly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "/unsent/stranded.bin", bytesOf(256, 3))
	_ = c.Close()
	up.release()

	var logged bytes.Buffer
	again, err := Open(Config{Dir: dir, WriteBack: false, Logger: slogText(&logged)})
	if err != nil {
		t.Fatalf("reopen with write-back off: %v", err)
	}
	defer func() { _ = again.Close() }()

	// Still here, and still readable.
	if !again.Has("/unsent/stranded.bin") {
		t.Fatal("the unsent object was dropped when write-back was turned off")
	}
	out := logged.String()
	for _, want := range []string{"never reached OpenDrive", "write-back"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not mention %q:\n%s", want, out)
		}
	}
}

// Objects are listed most recently used first, which is what a person scanning the
// cache panel wants and what the eviction order is derived from.
func TestObjectsAreListedMostRecentlyUsedFirst(t *testing.T) {
	c, up := newTestCache(t, Config{})
	up.hold()
	defer up.release()

	for _, p := range []string{"/clean/first", "/clean/second", "/clean/third"} {
		fill(t, c, p, bytesOf(64, 1))
	}
	// Touch the first one, so it should lead.
	readBack(t, c, "/clean/first")

	objects := c.Objects()
	if len(objects) != 3 {
		t.Fatalf("objects = %d, want 3", len(objects))
	}
	if objects[0].RemotePath != "/clean/first" {
		t.Errorf("the most recently read object is %q, want /clean/first", objects[0].RemotePath)
	}
}

// A store with no directory is a configuration error, not a panic later.
func TestADirectoryIsRequired(t *testing.T) {
	if _, err := Open(Config{}); err == nil {
		t.Fatal("a cache with no directory was accepted")
	}
}

// The content filename is derived from the path by hashing, so a remote path can
// never reach outside the cache directory however it is spelled.
func TestContentNamesCannotEscapeTheDirectory(t *testing.T) {
	for _, p := range []string{
		"/Docs/report.pdf",
		"../../etc/passwd",
		"/../../../etc/passwd",
		"/Docs/" + strings.Repeat("long", 500),
		"",
	} {
		name := contentName(p)
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("contentName(%q) = %q, which contains a separator", p, name)
		}
		if filepath.Base(name) != name {
			t.Errorf("contentName(%q) = %q, which is not a bare filename", p, name)
		}
		// 64 hex characters and the suffix, whatever went in.
		if len(name) != 64+len(".bin") {
			t.Errorf("contentName(%q) = %q, length %d", p, name, len(name))
		}
	}
}

// Two different paths never share a file. In write-back mode a collision would be
// one user's file overwriting another's.
func TestDifferentPathsNeverShareAFile(t *testing.T) {
	seen := map[string]string{}
	for _, p := range []string{
		"/a", "/b", "/Docs/a", "/Docs/b", "/Docs/a.pdf", "/docs/a",
		"/Docs//a", "/Docs/a/", "/Docs/ a",
	} {
		name := contentName(normalisePath(p))
		if prev, ok := seen[name]; ok && prev != normalisePath(p) {
			t.Errorf("%q and %q share the file %q", prev, p, name)
		}
		seen[name] = normalisePath(p)
	}
}

// normalisePath collapses what should be one object, and is idempotent — applying
// it twice must not change the answer, or the same file could be keyed two ways
// depending on which call site got there first.
func TestNormalisationIsIdempotent(t *testing.T) {
	for _, p := range []string{
		"/Docs/a.pdf", "/Docs//a.pdf", "Docs/a.pdf", "/Docs/a.pdf/", "  /Docs/a.pdf  ",
		"/", "//", "", "///Docs///a.pdf///",
	} {
		once := normalisePath(p)
		twice := normalisePath(once)
		if once != twice {
			t.Errorf("normalisePath(%q) = %q, then %q", p, once, twice)
		}
		if !strings.HasPrefix(once, "/") {
			t.Errorf("normalisePath(%q) = %q, which is not anchored", p, once)
		}
		if once != "/" && strings.HasSuffix(once, "/") {
			t.Errorf("normalisePath(%q) = %q, which has a trailing slash", p, once)
		}
	}
}

var _ = os.Remove // keep os imported for the helpers above
