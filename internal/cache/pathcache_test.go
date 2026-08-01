package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The cache must satisfy the SDK's interface, which is how the daemon wires it.
var _ opendrive.PathCache = (*PathCache)(nil)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestStoreAndLookup(t *testing.T) {
	c := NewPathCache()
	if _, ok := c.Lookup("/Finance"); ok {
		t.Fatal("an empty cache must miss")
	}
	c.Store("/Finance", "FIN")
	got, ok := c.Lookup("/Finance")
	if !ok || got != "FIN" {
		t.Fatalf("Lookup = %q, %v", got, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d", c.Len())
	}

	hits, misses, _ := c.Stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("stats = %d hits, %d misses", hits, misses)
	}

	// Empty arguments are ignored rather than poisoning the cache.
	c.Store("", "X")
	c.Store("/x", "")
	if c.Len() != 1 {
		t.Fatalf("Len = %d after empty stores", c.Len())
	}
}

// §10.3: the TTL is the backstop for changes made from another client.
func TestEntriesExpire(t *testing.T) {
	clk := newClock()
	c := NewPathCache(WithTTL(time.Minute), WithClock(clk.Now))
	c.Store("/Finance", "FIN")

	clk.Advance(59 * time.Second)
	if _, ok := c.Lookup("/Finance"); !ok {
		t.Fatal("the entry expired early")
	}
	clk.Advance(2 * time.Second)
	if _, ok := c.Lookup("/Finance"); ok {
		t.Fatal("the entry outlived its TTL")
	}
	if c.Len() != 0 {
		t.Fatal("an expired entry should be dropped, not kept")
	}
}

// The main event: a folder whose DirUpdateTime moved has stale children.
func TestObserveDropsTheSubtreeWhenDirUpdateTimeChanges(t *testing.T) {
	c := NewPathCache()
	c.Store("/Finance", "FIN")
	c.Store("/Finance/2026", "Y26")
	c.Store("/Finance/2026/Q1", "Q1")
	c.Store("/Other", "OTH")

	// First sighting establishes the baseline and changes nothing.
	c.Observe("FIN", 1000)
	if c.Len() != 4 {
		t.Fatalf("the first observation dropped entries: %d left", c.Len())
	}

	// The same value again means nothing changed upstream.
	c.Observe("FIN", 1000)
	if c.Len() != 4 {
		t.Fatalf("an unchanged DirUpdateTime dropped entries: %d left", c.Len())
	}

	// A new value means the folder's contents moved on.
	c.Observe("FIN", 2000)
	if _, ok := c.Lookup("/Finance/2026"); ok {
		t.Fatal("a stale child survived")
	}
	if _, ok := c.Lookup("/Finance/2026/Q1"); ok {
		t.Fatal("a stale grandchild survived")
	}
	if _, ok := c.Lookup("/Other"); !ok {
		t.Fatal("an unrelated branch was dropped")
	}
	// The folder itself keeps its id: it did not move, its contents changed.
	if _, ok := c.Lookup("/Finance"); !ok {
		t.Fatal("the folder's own resolution should survive")
	}

	// A zero timestamp carries no information.
	c.Observe("FIN", 0)
	c.Observe("", 5)
}

func TestInvalidatePathDropsTheSubtree(t *testing.T) {
	c := NewPathCache()
	c.Store("/Finance", "FIN")
	c.Store("/Finance/2026", "Y26")
	c.Store("/Finance2", "F2") // a prefix, not a child
	c.Store("/Other", "OTH")

	c.InvalidatePath("/Finance")
	if _, ok := c.Lookup("/Finance"); ok {
		t.Fatal("the folder itself should be gone")
	}
	if _, ok := c.Lookup("/Finance/2026"); ok {
		t.Fatal("the child should be gone")
	}
	if _, ok := c.Lookup("/Finance2"); !ok {
		t.Fatal("a sibling sharing a name prefix must survive")
	}
	if _, ok := c.Lookup("/Other"); !ok {
		t.Fatal("an unrelated entry was dropped")
	}

	c.InvalidatePath("")
	c.InvalidatePath("/nothing-cached")
}

func TestInvalidateIDDropsTheSubtreeAndForgetsTheParentTimestamp(t *testing.T) {
	c := NewPathCache()
	c.Store("/Finance", "FIN")
	c.Store("/Finance/2026", "Y26")
	c.Store("/Finance/2026/Q1", "Q1")
	c.Observe("FIN", 1000)

	c.InvalidateID("Y26")
	if _, ok := c.Lookup("/Finance/2026"); ok {
		t.Fatal("the folder should be gone")
	}
	if _, ok := c.Lookup("/Finance/2026/Q1"); ok {
		t.Fatal("the child should be gone")
	}
	if _, ok := c.Lookup("/Finance"); !ok {
		t.Fatal("the parent's own resolution is still valid")
	}

	// The parent's recorded DirUpdateTime is forgotten, so the next listing
	// re-establishes a baseline instead of comparing against a stale one.
	c.Observe("FIN", 2000)
	if _, ok := c.Lookup("/Finance"); !ok {
		t.Fatal("re-establishing the baseline must not drop the parent")
	}

	// An unknown id is a no-op.
	c.InvalidateID("nope")
	c.InvalidateID("")
}

func TestResetEmptiesEverything(t *testing.T) {
	c := NewPathCache()
	c.Store("/a", "A")
	c.Observe("A", 1)
	c.Reset()
	if c.Len() != 0 {
		t.Fatalf("Len = %d", c.Len())
	}
	if _, ok := c.Lookup("/a"); ok {
		t.Fatal("Reset did not clear the cache")
	}
}

// Re-resolving a path to a different id must not leave the old id mapped.
func TestReStoringAPathRebindsIt(t *testing.T) {
	c := NewPathCache()
	c.Store("/a", "OLD")
	c.Store("/a", "NEW")
	if got, _ := c.Lookup("/a"); got != "NEW" {
		t.Fatalf("Lookup = %q", got)
	}
	c.InvalidateID("NEW")
	if _, ok := c.Lookup("/a"); ok {
		t.Fatal("invalidating the current id should drop the path")
	}
}

func TestEvictionKeepsTheCacheBounded(t *testing.T) {
	clk := newClock()
	c := NewPathCache(WithMaxEntries(10), WithClock(clk.Now))
	for i := 0; i < 25; i++ {
		c.Store(fmt.Sprintf("/f%02d", i), fmt.Sprintf("ID%02d", i))
		clk.Advance(time.Millisecond)
	}
	if c.Len() > 10 {
		t.Fatalf("Len = %d, over the bound", c.Len())
	}
	if _, _, evictions := c.Stats(); evictions < 15 {
		t.Fatalf("evictions = %d", evictions)
	}
	// The most recent entry survives; the oldest does not.
	if _, ok := c.Lookup("/f24"); !ok {
		t.Fatal("the newest entry was evicted")
	}
	if _, ok := c.Lookup("/f00"); ok {
		t.Fatal("the oldest entry should have gone first")
	}
}

func TestOptionsAreDefensive(t *testing.T) {
	c := NewPathCache(WithTTL(-1), WithMaxEntries(0), WithClock(nil))
	if c.ttl != DefaultTTL || c.maxEntries != DefaultMaxEntries || c.now == nil {
		t.Fatalf("invalid options were applied: %+v", c)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := NewPathCache(WithMaxEntries(64))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/f%d", i)
			id := fmt.Sprintf("ID%d", i)
			for j := 0; j < 100; j++ {
				c.Store(path, id)
				c.Lookup(path)
				c.Observe(id, int64(j))
				if j%25 == 0 {
					c.InvalidateID(id)
				}
			}
		}(i)
	}
	wg.Wait()
}
