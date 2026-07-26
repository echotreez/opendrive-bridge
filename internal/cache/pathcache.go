// Package cache holds the path to folder-id cache the Bridge puts in front of
// upstream (whitepaper §10.3).
//
// Resolving "/Finance/2026/Q1" costs one upstream round trip per lookup, and
// the Bridge resolves a path on nearly every request, so caching is what keeps
// the daemon from being chatty. The hard part is invalidation, and upstream
// hands it to us: every folder carries a DirUpdateTime that changes whenever
// its contents change, so a listing that comes back with a different value
// proves the cached children are stale. A TTL backs that up for changes made
// from another client entirely.
package cache

import (
	"strings"
	"sync"
	"time"
)

// DefaultTTL is the fallback expiry for an entry nothing has confirmed
// recently (§10.3 suggests 60 seconds).
const DefaultTTL = 60 * time.Second

// DefaultMaxEntries bounds memory use; the least recently used entries go
// first once the cache is full.
const DefaultMaxEntries = 4096

type entry struct {
	id       string
	expires  time.Time
	lastUsed time.Time
}

// PathCache maps normalised folder paths to upstream folder ids. It implements
// opendrive.PathCache and is safe for concurrent use.
type PathCache struct {
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	byPath  map[string]entry
	byID    map[string]string // folder id to path, for id-driven invalidation
	updated map[string]int64  // folder id to the DirUpdateTime last observed

	hits, misses, evictions int
}

// Option configures a PathCache.
type Option func(*PathCache)

// WithTTL overrides the fallback expiry.
func WithTTL(d time.Duration) Option {
	return func(c *PathCache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithMaxEntries bounds the cache size.
func WithMaxEntries(n int) Option {
	return func(c *PathCache) {
		if n > 0 {
			c.maxEntries = n
		}
	}
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(c *PathCache) {
		if now != nil {
			c.now = now
		}
	}
}

// NewPathCache creates an empty cache.
func NewPathCache(opts ...Option) *PathCache {
	c := &PathCache{
		ttl:        DefaultTTL,
		maxEntries: DefaultMaxEntries,
		now:        time.Now,
		byPath:     map[string]entry{},
		byID:       map[string]string{},
		updated:    map[string]int64{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Lookup returns the cached id for a path.
func (c *PathCache) Lookup(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.byPath[path]
	if !ok {
		c.misses++
		return "", false
	}
	now := c.now()
	if now.After(e.expires) {
		c.dropPathLocked(path)
		c.misses++
		return "", false
	}
	e.lastUsed = now
	c.byPath[path] = e
	c.hits++
	return e.id, true
}

// Store records a resolution.
func (c *PathCache) Store(path, id string) {
	if path == "" || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.byPath[path] = entry{id: id, expires: now.Add(c.ttl), lastUsed: now}
	c.byID[id] = path
	c.evictLocked()
}

// Observe records the DirUpdateTime last seen for a folder. A change means the
// folder's contents moved on, so everything cached below it is dropped — this
// is the invalidation signal upstream gives us for free (§10.3).
func (c *PathCache) Observe(id string, dirUpdateTime int64) {
	if id == "" || dirUpdateTime == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	previous, seen := c.updated[id]
	c.updated[id] = dirUpdateTime
	if seen && previous != dirUpdateTime {
		if path, ok := c.byID[id]; ok {
			c.dropSubtreeLocked(path, false)
		}
	}
}

// InvalidatePath drops a path and everything under it.
func (c *PathCache) InvalidatePath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropSubtreeLocked(path, true)
}

// InvalidateID drops the folder with this id, everything under it, and its
// parent listing, because a write changes the parent too.
func (c *PathCache) InvalidateID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.updated, id)
	path, ok := c.byID[id]
	if !ok {
		return
	}
	c.dropSubtreeLocked(path, true)
	// The parent's own entry stays valid — its id did not change — but its
	// observed DirUpdateTime is now meaningless.
	if i := strings.LastIndex(path, "/"); i > 0 {
		if parentID, ok := c.byPath[path[:i]]; ok {
			delete(c.updated, parentID.id)
		}
	}
}

// Reset empties the cache.
func (c *PathCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byPath = map[string]entry{}
	c.byID = map[string]string{}
	c.updated = map[string]int64{}
}

// Len returns how many resolutions are cached.
func (c *PathCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byPath)
}

// Stats reports cache effectiveness, which the daemon exposes for tuning.
func (c *PathCache) Stats() (hits, misses, evictions int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.evictions
}

// ---------------------------------------------------------------- internals

// dropSubtreeLocked removes a path and its descendants. The caller must hold
// c.mu.
func (c *PathCache) dropSubtreeLocked(path string, includeSelf bool) {
	if path == "" {
		return
	}
	prefix := path
	if prefix != "/" {
		prefix += "/"
	}
	for p := range c.byPath {
		isSelf := p == path
		if isSelf && !includeSelf {
			continue
		}
		if isSelf || strings.HasPrefix(p, prefix) {
			c.dropPathLocked(p)
		}
	}
}

// dropPathLocked removes one entry. The caller must hold c.mu.
func (c *PathCache) dropPathLocked(path string) {
	e, ok := c.byPath[path]
	if !ok {
		return
	}
	delete(c.byPath, path)
	if current, ok := c.byID[e.id]; ok && current == path {
		delete(c.byID, e.id)
	}
	delete(c.updated, e.id)
}

// evictLocked enforces the size bound, oldest use first. The caller must hold
// c.mu.
func (c *PathCache) evictLocked() {
	for len(c.byPath) > c.maxEntries {
		var (
			oldestPath string
			oldestTime time.Time
		)
		for p, e := range c.byPath {
			if oldestPath == "" || e.lastUsed.Before(oldestTime) {
				oldestPath, oldestTime = p, e.lastUsed
			}
		}
		if oldestPath == "" {
			return
		}
		c.dropPathLocked(oldestPath)
		c.evictions++
	}
}
