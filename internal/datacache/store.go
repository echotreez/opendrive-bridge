package datacache

import (
	"context"
	"crypto/md5" //nolint:gosec // matches OpenDrive's protocol hash, not a security use
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DataCache is the gateway. It is safe for concurrent use.
//
// The name is not Cache, because `internal/cache` already owns that word for the
// metadata cache and §3.5.4 asks for the two to be impossible to confuse.
type DataCache struct {
	cfg Config
	log *slog.Logger
	jnl *journal

	mu    sync.Mutex
	objs  map[string]*Object
	bytes int64 // total on disk
	dirty int64 // bytes in unsent objects
	// draining is set by Drain and makes every new write return ErrClosed. Rule
	// 4's first half: stop accepting before trying to finish.
	draining bool
	closed   bool

	// hits and misses are the read-through counters /v1/cache/status reports.
	hits   int64
	misses int64

	// flush plumbing, present only in write-back mode.
	flushQueue chan string
	inFlight   map[string]bool
	waiters    []chan struct{}
	workers    sync.WaitGroup
	stop       chan struct{}
	stopOnce   sync.Once
	// flushCtx bounds the uploads in flight. Close cancels it, which is what lets
	// a shutdown finish while an upload is stuck: the workers used to call Upload
	// with context.Background(), so Close waited on a worker that could never
	// notice it had been asked to stop. A test deadlocked on it, which is a
	// better place to find that than a daemon that will not exit.
	//
	// Drain deliberately does not cancel it — it needs the uploads to proceed —
	// so the order at shutdown is Drain, then Close.
	flushCtx    context.Context
	flushCancel context.CancelFunc

	now func() time.Time
}

// Open prepares the cache directory and rebuilds the index from the journal.
//
// The order is: replay the journal, then check the content files against it. Both
// halves report what they found, because the two failures mean different things
// and an operator reading the log after an incident should be able to tell them
// apart — a journal that stops mid-record is a crash during a write, while a
// journal entry whose content file is missing or short is a rename or a write
// that did not survive.
//
// An entry whose content cannot be accounted for is dropped, and if it was unsent
// that is data loss and is logged at error level with the path in it. This is the
// one place the gateway can discover it has failed a user, and it says so rather
// than starting up quietly with a smaller cache than it had.
func Open(cfg Config) (*DataCache, error) {
	if err := cfg.withDefaults(); err != nil {
		return nil, err
	}
	// 0700: the contents are the user's files in the clear (§3.5.4).
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("datacache: cannot create %s: %w", cfg.Dir, err)
	}
	// #nosec G302 -- reviewed: gosec wants 0600 or less, which is the right advice
	// for a file and wrong for a directory. Without the execute bit the owner
	// cannot traverse into it, so the cache would be unreadable by the process
	// that just created it. 0700 is the tightest mode a usable directory has, and
	// it is the same mode §9.2.2 uses for the credential file's directory.
	if err := os.Chmod(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("datacache: cannot restrict %s: %w", cfg.Dir, err)
	}

	replayed, err := replayJournal(cfg.Dir)
	if err != nil {
		return nil, err
	}

	c := &DataCache{
		cfg:      cfg,
		log:      cfg.Logger,
		objs:     replayed.objects,
		inFlight: map[string]bool{},
		stop:     make(chan struct{}),
		now:      time.Now,
	}

	if replayed.torn {
		c.log.Warn("the cache journal ends in an incomplete record, so the daemon did not "+
			"stop cleanly last time; the unfinished write was never acknowledged to its client",
			slog.String("datacache_dir", cfg.Dir),
			slog.Int("datacache_records", replayed.records))
	}

	if err := c.verifyAgainstDisk(); err != nil {
		return nil, err
	}

	// A compaction on the way up keeps the log from growing across restarts in a
	// read-heavy deployment, where touch records outnumber everything else.
	if replayed.records > compactAfter {
		jnl, err := compactJournal(cfg.Dir, c.snapshot())
		if err != nil {
			return nil, err
		}
		c.jnl = jnl
	} else {
		jnl, err := openJournal(cfg.Dir)
		if err != nil {
			return nil, err
		}
		c.jnl = jnl
	}

	// §8.3.1's second layer: say so at startup, not only when somebody asks.
	c.warnIfNotDurable()

	if cfg.WriteBack {
		c.flushQueue = make(chan string, 4096)
		c.flushCtx, c.flushCancel = context.WithCancel(context.Background())
		c.startFlushWorkers()
		// Rule 2's other half: everything unsent at startup goes back on the
		// queue. A crash must cost the in-flight bytes and nothing else.
		c.requeueUnsent()
	} else if unsent := c.unsentCount(); unsent > 0 {
		// Write-back was on and is now off, with data still here. Refusing to
		// start would leave the user no way to get their data up; the honest
		// thing is to say plainly what is sitting here and that nothing will send
		// it until write-back is switched back on.
		c.log.Error("the cache holds data that has never reached OpenDrive, and write-back "+
			"is switched off, so nothing will send it; turn write-back back on, or copy these "+
			"files out of the cache directory yourself",
			slog.Int("datacache_unsent_objects", unsent),
			slog.String("datacache_dir", cfg.Dir))
	}
	return c, nil
}

// verifyAgainstDisk reconciles the replayed index with the files present.
func (c *DataCache) verifyAgainstDisk() error {
	entries, err := os.ReadDir(c.cfg.Dir)
	if err != nil {
		return fmt.Errorf("datacache: cannot read %s: %w", c.cfg.Dir, err)
	}
	onDisk := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".bin" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		onDisk[e.Name()] = info.Size()
	}

	var lost []string
	for path, o := range c.objs {
		name := contentName(path)
		size, present := onDisk[name]
		switch {
		case !present:
			if o.State.Unsent() {
				lost = append(lost, path)
			}
			delete(c.objs, path)
		case size != o.Size:
			// A short file is a write that did not finish. Trusting the journal's
			// size over the file's would hand a caller a truncated object and call
			// it a hit.
			if o.State.Unsent() {
				lost = append(lost, path)
			}
			delete(c.objs, path)
			_ = os.Remove(filepath.Join(c.cfg.Dir, name))
		default:
			c.bytes += o.Size
			if o.State.Unsent() {
				c.dirty += o.Size
			}
			delete(onDisk, name)
		}
	}
	// Anything on disk the journal does not mention is an aborted write: the
	// content landed and the record did not. It is not an object, because nothing
	// knows what path it belongs to, so it is removed.
	for name := range onDisk {
		_ = os.Remove(filepath.Join(c.cfg.Dir, name))
	}

	if len(lost) > 0 {
		// The worst thing this package can report, so it is reported in full,
		// one path at a time, at error level.
		c.log.Error("data that had not reached OpenDrive is missing from the cache directory "+
			"and cannot be recovered",
			slog.Int("datacache_lost_objects", len(lost)),
			slog.Any("datacache_lost_paths", lost))
	}
	return nil
}

func (c *DataCache) unsentCount() int {
	n := 0
	for _, o := range c.objs {
		if o.State.Unsent() {
			n++
		}
	}
	return n
}

func (c *DataCache) snapshot() []*Object {
	out := make([]*Object, 0, len(c.objs))
	for _, o := range c.objs {
		out = append(out, o.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RemotePath < out[j].RemotePath })
	return out
}

// ---------------------------------------------------------------- reading

// Reader is an open handle on a cached object's bytes.
type Reader struct {
	*os.File
	// Object is the metadata as it stood when the handle was opened.
	Object *Object
}

// Get opens a cached object.
//
// It returns ErrNotCached on a miss, which the caller turns into a fetch from
// upstream and, usually, a Fill. The hit and miss counters move here, so
// /v1/cache/status reports what callers actually experienced rather than what the
// index contains.
//
// A dirty object is a hit like any other, and that is the read-after-write
// consistency §3.5.3 promises: a client that has just written gets back the bytes
// it wrote, rather than meeting upstream's own read-after-write delay (D44).
func (c *DataCache) Get(remotePath string) (*Reader, error) {
	remotePath = normalisePath(remotePath)

	c.mu.Lock()
	o := c.objs[remotePath]
	if o == nil {
		c.misses++
		c.mu.Unlock()
		return nil, ErrNotCached
	}
	o.LastAccess = c.now()
	c.hits++
	clone := o.Clone()
	c.mu.Unlock()

	path := filepath.Join(c.cfg.Dir, contentName(remotePath))
	f, err := os.Open(path) // #nosec G304 -- a file this cache wrote, named by hash
	if err != nil {
		// The index and the directory have diverged since startup. Treat it as a
		// miss so the caller still gets their data from upstream, and drop the
		// entry so the next call does not repeat the work.
		c.log.Warn("a cached object's content file has gone missing; falling back to OpenDrive",
			slog.String("datacache_path", remotePath),
			slog.String("datacache_error", err.Error()))
		c.forget(remotePath, "content file missing")
		c.mu.Lock()
		c.hits--
		c.misses++
		c.mu.Unlock()
		return nil, ErrNotCached
	}
	// LRU bookkeeping is not worth an fsync: losing a touch record costs a
	// slightly wrong eviction order, which is a performance question, not a
	// correctness one.
	_ = c.jnl.append(journalRecord{Op: opTouch, Path: remotePath, At: clone.LastAccess.UnixNano()}, false)
	return &Reader{File: f, Object: clone}, nil
}

// Has reports whether an object is cached, without counting a hit or a miss. It
// exists for the status and object-listing endpoints, which must not move the
// numbers they are reporting.
func (c *DataCache) Has(remotePath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.objs[normalisePath(remotePath)]
	return ok
}

// ---------------------------------------------------------------- writing

// writer accumulates an object's bytes in a temporary file, hashing as it goes,
// and commits it into the cache.
//
// Both paths into the cache use it — a read-through fill and a write-back write —
// because both need the same durability sequence and only the state they commit
// differs. Two implementations of that sequence would be two chances to get it
// wrong, and one of them would be the one that matters.
type writer struct {
	c    *DataCache
	path string
	tmp  *os.File
	md5  hash.Hash
	n    int64
	// limit is the most this writer may accept before the dirty allowance is
	// exceeded. Zero means unlimited, which is the case for a clean fill: the
	// bytes already exist upstream, so storing them risks nothing.
	limit  int64
	closed bool
}

// newWriter starts an object.
//
// expectedSize, when known, is checked against the dirty allowance before a
// single byte is accepted. That is the difference between refusing a write and
// accepting five gigabytes and then refusing it: §4.5 requires that when
// cache_full comes back, the data already in the cache is unharmed, and the
// cheapest way to honour that is not to start.
func (c *DataCache) newWriter(remotePath string, expectedSize int64, dirty bool) (*writer, error) {
	c.mu.Lock()
	if c.closed || (dirty && c.draining) {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if dirty {
		room := c.cfg.MaxDirtyBytes - c.dirty
		// An object already here is being replaced, so its bytes come back.
		if old := c.objs[remotePath]; old != nil && old.State.Unsent() {
			room += old.Size
		}
		if room <= 0 || (expectedSize > 0 && expectedSize > room) {
			c.mu.Unlock()
			return nil, ErrCacheFull
		}
		c.mu.Unlock()
		tmp, err := os.CreateTemp(c.cfg.Dir, "writing-*.tmp")
		if err != nil {
			return nil, fmt.Errorf("datacache: cannot start a write in %s: %w", c.cfg.Dir, err)
		}
		return &writer{c: c, path: remotePath, tmp: tmp, md5: md5.New(), limit: room}, nil //nolint:gosec // protocol hash
	}
	c.mu.Unlock()
	tmp, err := os.CreateTemp(c.cfg.Dir, "writing-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot start a write in %s: %w", c.cfg.Dir, err)
	}
	return &writer{c: c, path: remotePath, tmp: tmp, md5: md5.New()}, nil //nolint:gosec // protocol hash
}

// Write takes bytes.
//
// The limit is enforced here as well as up front, because a caller may not know
// the size — a chunked upload has no Content-Length — and a declared size may
// simply be wrong. Crossing it aborts this object and leaves every other one
// exactly as it was.
func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("datacache: write after commit")
	}
	if w.limit > 0 && w.n+int64(len(p)) > w.limit {
		_ = w.Abort()
		return 0, ErrCacheFull
	}
	n, err := w.tmp.Write(p)
	w.n += int64(n)
	_, _ = w.md5.Write(p[:n])
	if err != nil {
		return n, fmt.Errorf("datacache: cannot write to the cache: %w", err)
	}
	return n, nil
}

// Abort throws the partial object away. It never touches the index, so an
// aborted write is invisible to everything else.
func (w *writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	name := w.tmp.Name()
	_ = w.tmp.Close()
	return os.Remove(name)
}

// hashHex is the MD5 of what has been written so far.
func (w *writer) hashHex() string { return hex.EncodeToString(w.md5.Sum(nil)) }

// commit is the durability sequence, and the order of these steps is the whole
// point of rule 2:
//
//  1. flush the content to the platter
//  2. rename it into place
//  3. flush the directory, so the rename itself survives
//  4. append the journal record and flush that
//  5. only now update the index and return success
//
// If any step fails, the caller is told the write failed and nothing claims the
// object exists. Step 3 is the one usually left out, and leaving it out produces
// the worst available outcome: a journal entry, durable, pointing at a file the
// filesystem forgot — which recovery then has to discard, for an object the
// client was told had been stored.
func (w *writer) commit(o *Object) (*Object, error) {
	if w.closed {
		return nil, errors.New("datacache: commit after commit")
	}
	w.closed = true
	tmpName := w.tmp.Name()
	// From here every failure path removes the temporary file, so a failed commit
	// leaves nothing behind for the startup scan to clean up.
	fail := func(err error) (*Object, error) {
		_ = w.tmp.Close()
		_ = os.Remove(tmpName)
		return nil, err
	}

	if err := w.tmp.Sync(); err != nil {
		return fail(fmt.Errorf("datacache: cannot flush %s: %w", o.RemotePath, err))
	}
	if err := w.tmp.Close(); err != nil {
		return fail(fmt.Errorf("datacache: cannot close the cache file for %s: %w", o.RemotePath, err))
	}
	final := filepath.Join(w.c.cfg.Dir, contentName(o.RemotePath))
	if err := os.Rename(tmpName, final); err != nil {
		return fail(fmt.Errorf("datacache: cannot store %s: %w", o.RemotePath, err))
	}
	if err := syncDir(w.c.cfg.Dir); err != nil {
		// The content may or may not be there. Remove it and report failure:
		// claiming a stored object whose rename might evaporate is precisely the
		// promise this sequence exists to keep.
		_ = os.Remove(final)
		return nil, err
	}
	if err := w.c.jnl.append(journalRecord{Op: opPut, Obj: o, At: o.StoredAt.UnixNano()}, true); err != nil {
		_ = os.Remove(final)
		return nil, err
	}

	w.c.mu.Lock()
	if old := w.c.objs[o.RemotePath]; old != nil {
		w.c.bytes -= old.Size
		if old.State.Unsent() {
			w.c.dirty -= old.Size
		}
	}
	w.c.objs[o.RemotePath] = o
	w.c.bytes += o.Size
	if o.State.Unsent() {
		w.c.dirty += o.Size
	}
	w.c.mu.Unlock()
	return o.Clone(), nil
}

// Fill stores an object the gateway has just fetched from OpenDrive.
//
// It is the read-through half and it carries none of the write-back risk:
// upstream is the authority, so losing this costs one download. The returned
// writer is committed with CommitClean once the body has been copied through it.
func (c *DataCache) Fill(remotePath string) (*FillWriter, error) {
	w, err := c.newWriter(normalisePath(remotePath), 0, false)
	if err != nil {
		return nil, err
	}
	return &FillWriter{w: w}, nil
}

// FillWriter stores a downloaded object.
type FillWriter struct{ w *writer }

func (f *FillWriter) Write(p []byte) (int, error) { return f.w.Write(p) }

// Abort discards the partial download. A download that failed halfway must not
// become a cache hit.
func (f *FillWriter) Abort() error { return f.w.Abort() }

// CommitClean records the object as already upstream.
//
// wantHash, when given, is what upstream said the file hashes to. A mismatch is
// refused rather than stored: a cache that can serve the wrong bytes is worse
// than no cache, and this is the one moment the two can be compared for free.
func (f *FillWriter) CommitClean(wantHash string) (*Object, error) {
	got := f.w.hashHex()
	if wantHash != "" && !equalFold(got, wantHash) {
		_ = f.w.Abort()
		return nil, fmt.Errorf("datacache: refusing to cache %s: it hashes to %s and OpenDrive "+
			"says %s", f.w.path, got, wantHash)
	}
	now := f.w.c.now()
	o := &Object{
		RemotePath: f.w.path,
		Size:       f.w.n,
		Hash:       got,
		State:      StateClean,
		StoredAt:   now,
		LastAccess: now,
	}
	stored, err := f.w.commit(o)
	if err != nil {
		return nil, err
	}
	f.w.c.evictIfNeeded()
	return stored, nil
}

// equalFold compares two hex digests without pulling in strings for one call.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// forget drops an object from the index and removes its content.
func (c *DataCache) forget(remotePath, why string) {
	c.mu.Lock()
	o := c.objs[remotePath]
	if o == nil {
		c.mu.Unlock()
		return
	}
	delete(c.objs, remotePath)
	c.bytes -= o.Size
	if o.State.Unsent() {
		c.dirty -= o.Size
	}
	c.mu.Unlock()

	_ = os.Remove(filepath.Join(c.cfg.Dir, contentName(remotePath)))
	_ = c.jnl.append(journalRecord{Op: opDrop, Path: remotePath, At: c.now().UnixNano()}, true)
	c.log.Debug("dropped a cached object",
		slog.String("datacache_path", remotePath),
		slog.String("datacache_reason", why))
}

// ---------------------------------------------------------------- eviction

// evictIfNeeded enforces the capacity target, and rule 1 while doing it.
//
// Only clean objects are candidates. If the store is over its high watermark and
// everything in it is unsent, nothing is evicted and the cache simply runs over
// the target — which is the right answer: the alternative is deleting the only
// copy of somebody's file to save disk space. Writes are refused by the dirty
// allowance long before that becomes a real problem, which is why
// MaxDirtyBytes must not exceed MaxBytes.
func (c *DataCache) evictIfNeeded() {
	high := int64(float64(c.cfg.MaxBytes) * c.cfg.HighWatermark)
	low := int64(float64(c.cfg.MaxBytes) * c.cfg.LowWatermark)

	c.mu.Lock()
	if c.bytes <= high {
		c.mu.Unlock()
		return
	}
	// Least recently used first, clean only.
	var candidates []*Object
	for _, o := range c.objs {
		if o.State == StateClean {
			candidates = append(candidates, o)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastAccess.Before(candidates[j].LastAccess)
	})
	var doomed []string
	freed := c.bytes
	for _, o := range candidates {
		if freed <= low {
			break
		}
		doomed = append(doomed, o.RemotePath)
		freed -= o.Size
	}
	overBudget := c.bytes > high && len(doomed) == 0
	dirtyBytes, totalBytes := c.dirty, c.bytes
	c.mu.Unlock()

	if overBudget {
		c.log.Warn("the cache is over its size target and everything in it is waiting to be "+
			"uploaded, so nothing can be evicted; unsent data is never discarded to save space",
			slog.Int64("datacache_bytes", totalBytes),
			slog.Int64("datacache_dirty_bytes", dirtyBytes),
			slog.Int64("datacache_max_bytes", c.cfg.MaxBytes))
		return
	}
	for _, p := range doomed {
		c.forget(p, "evicted to stay within the size target")
	}
	if len(doomed) > 0 {
		c.log.Debug("evicted clean objects",
			slog.Int("datacache_evicted", len(doomed)),
			slog.Int64("datacache_bytes", totalBytes))
	}
}

// ---------------------------------------------------------------- reporting

// Status is what /v1/cache/status answers (§4.4.1).
type Status struct {
	Enabled   bool   `json:"enabled"`
	WriteBack bool   `json:"write_back"`
	Dir       string `json:"dir"`

	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
	Objects  int   `json:"objects"`

	// DirtyBytes and DirtyObjects are rule 3: the number a user needs before
	// they turn the machine off.
	DirtyBytes    int64 `json:"dirty_bytes"`
	DirtyObjects  int   `json:"dirty_objects"`
	MaxDirtyBytes int64 `json:"max_dirty_bytes"`
	// OldestDirtyAge is how long the longest-waiting unsent object has been
	// waiting, in seconds. A number that keeps growing is the signal that
	// flushing is stuck rather than slow.
	OldestDirtyAge float64 `json:"oldest_dirty_age_seconds"`

	// SafeToShutDown is the plain answer to the question a user actually has.
	// It is computed here rather than left to each client to derive from
	// dirty_objects, so that the CLI, the GUI and anything else agree.
	SafeToShutDown bool `json:"safe_to_shut_down"`

	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	HitRate float64 `json:"hit_rate"`

	// Durable reports whether the cache directory looks like it will survive the
	// process. False means unsent data is at risk from something as ordinary as
	// recreating a container (§8.3.1).
	Durable bool `json:"durable"`
	// DurabilityNote explains a false Durable in a sentence meant for a person.
	DurabilityNote string `json:"durability_note,omitempty"`
}

// Status reports the gateway's state.
func (c *DataCache) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := Status{
		Enabled:       true,
		WriteBack:     c.cfg.WriteBack,
		Dir:           c.cfg.Dir,
		Bytes:         c.bytes,
		MaxBytes:      c.cfg.MaxBytes,
		Objects:       len(c.objs),
		DirtyBytes:    c.dirty,
		MaxDirtyBytes: c.cfg.MaxDirtyBytes,
		Hits:          c.hits,
		Misses:        c.misses,
	}
	oldest := time.Time{}
	for _, o := range c.objs {
		if !o.State.Unsent() {
			continue
		}
		st.DirtyObjects++
		if oldest.IsZero() || o.StoredAt.Before(oldest) {
			oldest = o.StoredAt
		}
	}
	if !oldest.IsZero() {
		st.OldestDirtyAge = c.now().Sub(oldest).Seconds()
	}
	st.SafeToShutDown = st.DirtyObjects == 0
	if total := st.Hits + st.Misses; total > 0 {
		st.HitRate = float64(st.Hits) / float64(total)
	}
	st.Durable, st.DurabilityNote = c.durability()
	return st
}

// Objects lists what is cached, newest access first, for /v1/cache/objects.
func (c *DataCache) Objects() []*Object {
	c.mu.Lock()
	out := c.snapshot()
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastAccess.After(out[j].LastAccess) })
	return out
}

// Close stops the flush workers and closes the journal. It does not drain; call
// Drain first if unsent data should be given a chance to leave.
func (c *DataCache) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.stopOnce.Do(func() {
		close(c.stop)
		// Cancel whatever is mid-upload. Without this, Close waits for a worker
		// that is inside an upload which has no reason to return — and a daemon
		// that cannot exit is worse than one that abandons a transfer it was
		// going to have to retry anyway. The object stays dirty and is re-queued
		// at the next start, which is exactly what rule 2 promises.
		if c.flushCancel != nil {
			c.flushCancel()
		}
	})
	c.workers.Wait()
	return c.jnl.close()
}

// unixNano converts a journal timestamp back to a time, treating zero as zero
// rather than as 1970 — an object with no recorded access would otherwise always
// sort first for eviction.
func unixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// ctxDone reports whether a context is finished, without the caller needing a
// select. Used by the flush and drain loops.
func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

var _ io.Writer = (*FillWriter)(nil)
