package datacache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// This file is the half of the package that holds data nobody else has. Read
// §3.5.2 before changing it.

// PutRequest describes a write-back write.
type PutRequest struct {
	// RemotePath is where the object belongs in the account.
	RemotePath string
	// FolderID and Name are where the flusher will send it. Name defaults to the
	// last element of RemotePath.
	FolderID string
	Name     string
	// Size, when the caller knows it, is checked against the dirty allowance
	// before any bytes are accepted. Zero means unknown, and the allowance is
	// then enforced as the bytes arrive.
	Size int64
}

// PutWriter accepts a write-back object's bytes.
type PutWriter struct {
	w   *writer
	req PutRequest
}

func (p *PutWriter) Write(b []byte) (int, error) { return p.w.Write(b) }

// Abort discards the partial write. Nothing else in the cache is affected, which
// is what makes it safe to abort on ErrCacheFull.
func (p *PutWriter) Abort() error { return p.w.Abort() }

// Put starts a write-back write.
//
// It returns ErrCacheFull when the dirty allowance has no room, before accepting a
// byte — §4.5's requirement that a 507 leaves the data already in the cache
// unharmed, met by not starting rather than by cleaning up.
func (c *DataCache) Put(req PutRequest) (*PutWriter, error) {
	if !c.cfg.WriteBack {
		return nil, errors.New("datacache: write-back is switched off; writes go straight upstream")
	}
	req.RemotePath = normalisePath(req.RemotePath)
	if req.Name == "" {
		req.Name = filepath.Base(req.RemotePath)
	}
	w, err := c.newWriter(req.RemotePath, req.Size, true)
	if err != nil {
		return nil, err
	}
	return &PutWriter{w: w, req: req}, nil
}

// Commit makes the object durable and queues it for upload.
//
// When this returns without an error, and only then, the caller may answer 202:
// the bytes are on this disk, the journal records them, and both have been
// flushed. It has not reached OpenDrive and the caller must not imply that it
// has — that is the whole of the durability contract, and §4.4.1 chose 202
// because "accepted, not finished" is exactly what it means.
func (p *PutWriter) Commit() (*Object, error) {
	c := p.w.c
	now := c.now()
	o := &Object{
		RemotePath: p.req.RemotePath,
		Size:       p.w.n,
		Hash:       p.w.hashHex(),
		State:      StateDirty,
		FolderID:   p.req.FolderID,
		Name:       p.req.Name,
		StoredAt:   now,
		LastAccess: now,
	}
	stored, err := p.w.commit(o)
	if err != nil {
		return nil, err
	}
	c.enqueue(o.RemotePath)
	// Eviction runs after a dirty commit too, because a dirty object still takes
	// space and clean neighbours may now be over the target. It will not touch
	// this one.
	c.evictIfNeeded()
	c.log.Info("accepted an object into the cache; it has not reached OpenDrive yet",
		slog.String("datacache_path", o.RemotePath),
		slog.Int64("datacache_size", o.Size),
		slog.Int64("datacache_dirty_bytes", c.Status().DirtyBytes))
	return stored, nil
}

// ---------------------------------------------------------------- the flusher

func (c *DataCache) startFlushWorkers() {
	for i := 0; i < c.cfg.FlushWorkers; i++ {
		c.workers.Add(1)
		go c.flushWorker()
	}
}

// requeueUnsent puts everything the journal says is unsent back on the queue.
//
// This is the recovery half of rule 2, and it includes objects recorded as
// `uploading`: that state means a worker was sending it when the process stopped,
// which tells us nothing about whether upstream received it. Sending it again is
// safe — the flush verifies the hash upstream reports, and an upload that did
// land will simply be confirmed — while not sending it would lose the file.
func (c *DataCache) requeueUnsent() {
	c.mu.Lock()
	var again []*Object
	for _, o := range c.objs {
		if o.State.Unsent() {
			again = append(again, o)
		}
	}
	c.mu.Unlock()

	// Oldest first: whatever has been waiting longest goes up first.
	sort.Slice(again, func(i, j int) bool { return again[i].StoredAt.Before(again[j].StoredAt) })
	for _, o := range again {
		c.enqueue(o.RemotePath)
	}
	if len(again) > 0 {
		c.log.Info("re-queued objects that had not reached OpenDrive before the last shutdown",
			slog.Int("datacache_requeued", len(again)))
	}
}

func (c *DataCache) enqueue(remotePath string) {
	select {
	case c.flushQueue <- remotePath:
	default:
		// The queue is full. Nothing is lost: the object is dirty in the index and
		// the sweep below finds it. Dropping the queue slot rather than blocking
		// keeps a write from waiting on the flusher.
		c.log.Debug("the flush queue is full; the object stays dirty and will be swept up",
			slog.String("datacache_path", remotePath))
	}
}

func (c *DataCache) flushWorker() {
	defer c.workers.Done()
	// The sweep is the safety net for anything the queue dropped, and the retry
	// timer for objects whose flush failed for a reason the classifier called
	// temporary.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case p := <-c.flushQueue:
			c.flushOne(c.flushCtx, p)
		case <-ticker.C:
			for _, p := range c.oldestUnsent(8) {
				if ctxDone(c.flushCtx) {
					return
				}
				c.flushOne(c.flushCtx, p)
			}
		}
	}
}

// oldestUnsent returns up to n unsent paths that no worker currently holds.
func (c *DataCache) oldestUnsent(n int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Object
	for _, o := range c.objs {
		if o.State.Unsent() && !c.inFlight[o.RemotePath] {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StoredAt.Before(out[j].StoredAt) })
	paths := make([]string, 0, n)
	for i := 0; i < len(out) && i < n; i++ {
		paths = append(paths, out[i].RemotePath)
	}
	return paths
}

// flushOne sends a single object upstream.
//
// The error handling here is the project's standing rule applied to a new caller:
// whether a failure may be retried comes from the classification layer and
// nowhere else (CLAUDE.md rule 3, §10.2). This function does not look at a status
// code and does not match a message. A permanent failure leaves the object dirty
// and records the reason, because the alternative — dropping it — would destroy
// the only copy on the strength of one refusal.
func (c *DataCache) flushOne(ctx context.Context, remotePath string) {
	c.mu.Lock()
	o := c.objs[remotePath]
	if o == nil || !o.State.Unsent() || c.inFlight[remotePath] || c.closed {
		c.mu.Unlock()
		return
	}
	c.inFlight[remotePath] = true
	obj := o.Clone()
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, remotePath)
		c.mu.Unlock()
		c.notifyWaiters()
	}()

	c.setState(remotePath, StateUploading, "", "", obj.FlushAttempts)

	f, err := os.Open(filepath.Join(c.cfg.Dir, contentName(remotePath))) // #nosec G304 -- a file this cache wrote
	if err != nil {
		// The content has gone. This is data loss and it is reported as such;
		// keeping the index entry would only promise something that is not there.
		c.log.Error("an object waiting to be uploaded has lost its content file",
			slog.String("datacache_path", remotePath),
			slog.String("datacache_error", err.Error()))
		c.forget(remotePath, "content missing before upload")
		return
	}
	defer func() { _ = f.Close() }()

	hash, err := c.cfg.Upstream.Upload(ctx, obj, f)
	if err != nil {
		attempts := obj.FlushAttempts + 1
		// The wording the user sees comes from the classifier, which is the only
		// thing entitled to say what upstream's refusal meant.
		reason := err.Error()
		var ae *opendrive.APIError
		if errors.As(err, &ae) {
			if d := ae.Diagnosis(); d != "" {
				reason = d
			} else if ae.UpstreamMsg != "" {
				reason = ae.UpstreamMsg
			}
		}
		c.setState(remotePath, StateDirty, "", reason, attempts)

		if opendrive.IsTemporary(err) {
			c.log.Warn("an upload from the cache failed and will be retried; the data is still here",
				slog.String("datacache_path", remotePath),
				slog.Int("datacache_flush_attempts", attempts),
				slog.String("datacache_error", opendrive.RedactString(reason)))
			return
		}
		c.log.Error("an upload from the cache was refused for good; the data is still here and "+
			"will not be sent again without help",
			slog.String("datacache_path", remotePath),
			slog.Int("datacache_flush_attempts", attempts),
			slog.String("datacache_error", opendrive.RedactString(reason)))
		return
	}

	// A success is not evidence. Upstream's own hash decides whether this object
	// is clean, because a 200 here would otherwise be exactly the kind of claim
	// this project has ten discrepancies about.
	if hash != "" && !equalFold(hash, obj.Hash) {
		reason := fmt.Sprintf("OpenDrive stored something that hashes to %s where this file "+
			"hashes to %s, so it has not been accepted as uploaded", hash, obj.Hash)
		c.setState(remotePath, StateDirty, "", reason, obj.FlushAttempts+1)
		c.log.Error("an upload reported success with the wrong hash; the object stays dirty",
			slog.String("datacache_path", remotePath),
			slog.String("datacache_local_hash", obj.Hash),
			slog.String("datacache_upstream_hash", hash))
		return
	}

	c.setState(remotePath, StateClean, "", "", obj.FlushAttempts)
	c.log.Info("an object reached OpenDrive",
		slog.String("datacache_path", remotePath),
		slog.Int64("datacache_size", obj.Size))
	c.evictIfNeeded()
}

// setState records a lifecycle transition, journalled and flushed before the
// in-memory index moves. The same order as a write, for the same reason: the
// durable record is what recovery will believe.
func (c *DataCache) setState(remotePath string, state State, fileID, lastErr string, attempts int) {
	rec := journalRecord{
		Op: opState, Path: remotePath, State: state, FileID: fileID,
		Err: lastErr, Attempts: attempts, At: c.now().UnixNano(),
	}
	if err := c.jnl.append(rec, true); err != nil {
		// The index is deliberately not updated when the journal write failed.
		// Believing a transition the journal does not record is how an object
		// becomes clean in memory and dirty on disk, and the next restart would
		// disagree with the answer the user was given.
		c.log.Error("could not record a cache state change; leaving the object as it was",
			slog.String("datacache_path", remotePath),
			slog.String("datacache_state", string(state)),
			slog.String("datacache_error", err.Error()))
		return
	}

	c.mu.Lock()
	o := c.objs[remotePath]
	if o == nil {
		c.mu.Unlock()
		return
	}
	wasUnsent := o.State.Unsent()
	o.State = state
	o.LastError = lastErr
	o.FlushAttempts = attempts
	if fileID != "" {
		o.UpstreamFileID = fileID
	}
	if wasUnsent && !state.Unsent() {
		c.dirty -= o.Size
	} else if !wasUnsent && state.Unsent() {
		c.dirty += o.Size
	}
	c.mu.Unlock()
}

// ---------------------------------------------------------------- flush, wait

// Flush asks for unsent objects to be sent now.
//
// path empty means everything. wait blocks until there is nothing unsent left or
// the context ends — this is what `odctl cache flush --wait` and rule 3 need: a
// way for a person to know, rather than guess, that it is safe to shut down.
func (c *DataCache) Flush(ctx context.Context, path string, wait bool) error {
	if !c.cfg.WriteBack {
		return nil
	}
	if path != "" {
		p := normalisePath(path)
		c.mu.Lock()
		o := c.objs[p]
		c.mu.Unlock()
		if o == nil {
			return ErrNotCached
		}
		c.enqueue(p)
	} else {
		for _, p := range c.oldestUnsent(1 << 20) {
			c.enqueue(p)
		}
	}
	if !wait {
		return nil
	}
	return c.waitUntilSent(ctx, path)
}

// waitUntilSent blocks until nothing (or the named object) is unsent.
func (c *DataCache) waitUntilSent(ctx context.Context, path string) error {
	for {
		if c.settled(path) {
			return nil
		}
		ch := c.waiter()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		case <-time.After(time.Second):
			// A poll alongside the notification, so that a missed wake-up delays
			// the answer by a second instead of hanging a user's terminal.
		}
	}
}

func (c *DataCache) settled(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != "" {
		o := c.objs[normalisePath(path)]
		return o == nil || !o.State.Unsent()
	}
	for _, o := range c.objs {
		if o.State.Unsent() {
			return false
		}
	}
	return true
}

func (c *DataCache) waiter() chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	c.waiters = append(c.waiters, ch)
	c.mu.Unlock()
	return ch
}

func (c *DataCache) notifyWaiters() {
	c.mu.Lock()
	waiters := c.waiters
	c.waiters = nil
	c.mu.Unlock()
	for _, ch := range waiters {
		close(ch)
	}
}

// ---------------------------------------------------------------- refresh, clear

// Refresh discards a clean object so the next read goes back to OpenDrive.
//
// A dirty object is refused with ErrDirty, and §4.4.1 is explicit about why:
// discarding one would be losing the user's data, so this is one of the few places
// the gateway says no to a direct request.
func (c *DataCache) Refresh(path string) error {
	p := normalisePath(path)
	c.mu.Lock()
	o := c.objs[p]
	if o == nil {
		c.mu.Unlock()
		return ErrNotCached
	}
	if o.State.Unsent() {
		c.mu.Unlock()
		return ErrDirty
	}
	c.mu.Unlock()
	c.forget(p, "refreshed at the caller's request")
	return nil
}

// Clear drops every clean object. It refuses outright if anything is unsent,
// rather than clearing what it can: a caller asking to empty the cache should not
// have to discover afterwards that it is not empty, and the alternative reading —
// clear everything — would destroy data.
func (c *DataCache) Clear() error {
	c.mu.Lock()
	unsent := 0
	var clean []string
	for _, o := range c.objs {
		if o.State.Unsent() {
			unsent++
			continue
		}
		clean = append(clean, o.RemotePath)
	}
	c.mu.Unlock()

	if unsent > 0 {
		return fmt.Errorf("%w: %d object(s) are still waiting to be uploaded", ErrDirty, unsent)
	}
	for _, p := range clean {
		c.forget(p, "cache cleared at the caller's request")
	}
	return nil
}

// ---------------------------------------------------------------- shutdown

// Drain is rule 4. It stops accepting writes, flushes what it can, and if it runs
// out of time it names every object it could not finish.
//
// It never exits quietly with data unsent. A log line per object is the point: a
// count tells an operator that something was lost and nothing about what, and the
// paths are the only thing that makes the loss recoverable by hand.
func (c *DataCache) Drain(ctx context.Context) error {
	if !c.cfg.WriteBack {
		return nil
	}
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()

	deadline := c.cfg.DrainTimeout
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	pending := len(c.oldestUnsent(1 << 20))
	if pending == 0 {
		c.log.Info("nothing in the cache is waiting to be uploaded; shutting down")
		return nil
	}
	c.log.Info("flushing the cache before shutdown",
		slog.Int("datacache_unsent_objects", pending),
		slog.Duration("datacache_drain_timeout", deadline))

	for _, p := range c.oldestUnsent(1 << 20) {
		c.enqueue(p)
	}
	err := c.waitUntilSent(ctx, "")
	if err == nil {
		c.log.Info("everything in the cache reached OpenDrive; it is safe to shut down")
		return nil
	}

	left := c.unsentObjects()
	// One line each. Whoever reads this log is looking for which files to deal
	// with, and a total does not answer that.
	for _, o := range left {
		c.log.Error("this object was still waiting to be uploaded when the daemon ran out of "+
			"time to shut down; its data is in the cache directory and has not reached OpenDrive",
			slog.String("datacache_path", o.RemotePath),
			slog.Int64("datacache_size", o.Size),
			slog.String("datacache_file", filepath.Join(c.cfg.Dir, contentName(o.RemotePath))),
			slog.String("datacache_last_error", o.LastError))
	}
	return fmt.Errorf("the cache still holds %d object(s) that have not reached OpenDrive; "+
		"they are listed above and their data is in %s", len(left), c.cfg.Dir)
}

// unsentObjects lists what has not gone up, oldest first.
func (c *DataCache) unsentObjects() []*Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Object
	for _, o := range c.objs {
		if o.State.Unsent() {
			out = append(out, o.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StoredAt.Before(out[j].StoredAt) })
	return out
}
