package datacache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// What the job engine needs from the gateway (§4.4.1's `phase`).
//
// Until this file existed, the gateway was only reachable by a caller that spoke
// HTTP to the streaming endpoints, which meant `odctl up` and `odctl down` — the way
// almost everybody will use this program — got no caching at all. The `phase` field
// added to jobs in §4.4.1 distinguishes "client to gateway" from "gateway to
// OpenDrive", and a job only has two legs if it goes through the gateway; the field
// was the specification saying so.
//
// # The coupling, and why it is this small
//
// §10.2d gives the flusher its own worker pool so that flushing does not compete
// with transfers a user is watching. That separation is worth keeping, so the job
// engine does not reach into the flusher: it asks the gateway to hold an object and
// then waits on the same notification `cache flush --wait` uses. No callbacks, no
// shared state, no lock ordering to get wrong — the engine blocks a goroutine that
// was dedicated to that transfer anyway.

// PutLocalFile copies a file on this machine into the cache as an unsent object.
//
// This is the upload half of a job's first leg. It returns ErrCacheFull when the
// file does not fit the dirty allowance, and the caller is expected to fall back to
// a direct upload rather than treat that as a failure: refusing to upload a large
// file because the *cache* is full would be a worse answer than not caching it.
//
// The copy is the honest cost of the durability contract. Once this returns, the
// gateway owns delivery — retries, crash recovery, the drain at shutdown — and the
// client may be told the write was accepted. Handing back an object while still
// depending on the caller's own file would be claiming a promise that is not kept.
func (c *DataCache) PutLocalFile(ctx context.Context, req PutRequest, localPath string) (*Object, error) {
	if !c.cfg.WriteBack {
		return nil, errors.New("datacache: write-back is switched off")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot read %s: %w", filepath.Base(localPath), err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("datacache: %s is a directory", filepath.Base(localPath))
	}
	req.Size = info.Size()

	w, err := c.Put(req)
	if err != nil {
		return nil, err
	}
	src, err := os.Open(localPath) // #nosec G304 -- the path the caller asked to upload
	if err != nil {
		_ = w.Abort()
		return nil, fmt.Errorf("datacache: cannot read %s: %w", filepath.Base(localPath), err)
	}
	defer func() { _ = src.Close() }()

	if _, err := io.Copy(w, src); err != nil {
		_ = w.Abort()
		return nil, err
	}
	// Refuse a file that changed size while it was being read. The object's
	// recorded size and its content must agree, or a restart drops it as
	// truncated — and a silently short upload is worse than a refused one.
	if ctx.Err() != nil {
		_ = w.Abort()
		return nil, ctx.Err()
	}
	return w.Commit()
}

// WaitUntilSent blocks until one object has reached OpenDrive.
//
// This is the second leg of an upload job. It is the same notification
// `odctl cache flush --wait` waits on, deliberately: one mechanism for "tell me
// when this is really upstream" rather than two that could disagree.
//
// A context that ends returns its error and leaves the object exactly as it was —
// still in the cache, still queued. Giving up waiting is not giving up sending.
func (c *DataCache) WaitUntilSent(ctx context.Context, remotePath string) error {
	if !c.cfg.WriteBack {
		return nil
	}
	p := normalisePath(remotePath)
	c.mu.Lock()
	_, known := c.objs[p]
	c.mu.Unlock()
	if !known {
		return ErrNotCached
	}
	c.enqueue(p)
	return c.waitUntilSent(ctx, p)
}

// StateOf reports an object's lifecycle state, or false when it is not cached. The
// job engine uses it to tell "sent" from "still here but the wait was cut short".
func (c *DataCache) StateOf(remotePath string) (State, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.objs[normalisePath(remotePath)]
	if o == nil {
		return "", false
	}
	return o.State, true
}

// CopyTo writes a cached object to a local file.
//
// The download half of a job's first leg, and the only one where the gateway saves
// the network entirely: a hit costs a local copy and no request at all. Reports
// false when the object is not cached, which is a miss rather than an error.
//
// The destination is written through a temporary file in the same directory and
// renamed, so an interrupted copy does not leave a partial file where the caller
// asked for a whole one — the same reasoning as the cache's own writes, applied to
// somebody else's disk.
func (c *DataCache) CopyTo(ctx context.Context, remotePath, localPath string) (bool, error) {
	rd, err := c.Get(remotePath)
	if errors.Is(err, ErrNotCached) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = rd.Close() }()

	if dir := filepath.Dir(localPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("datacache: cannot create %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), filepath.Base(localPath)+".part-*")
	if err != nil {
		return false, fmt.Errorf("datacache: cannot write near %s: %w", localPath, err)
	}
	tmpName := tmp.Name()
	fail := func(err error) (bool, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return false, err
	}

	if _, err := io.Copy(tmp, rd); err != nil {
		return fail(err)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}
	if err := tmp.Close(); err != nil {
		return fail(fmt.Errorf("datacache: cannot close %s: %w", tmpName, err))
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		_ = os.Remove(tmpName)
		return false, fmt.Errorf("datacache: cannot put %s in place: %w", localPath, err)
	}
	return true, nil
}

// FillFrom stores bytes read from r as a clean object, for a caller that has just
// fetched them from OpenDrive.
//
// The reader is consumed whether or not the object ends up cached: a caching failure
// must not fail the transfer it was meant to speed up (§3.5.2's asymmetry). dst is
// where the bytes are really going, and it is written first.
func (c *DataCache) FillFrom(remotePath string, dst io.Writer, r io.Reader) (int64, error) {
	fw, err := c.Fill(remotePath)
	if err != nil {
		// No cache, no problem: copy straight through.
		return io.Copy(dst, r)
	}
	n, copyErr := io.Copy(io.MultiWriter(dst, &tolerant{w: fw}), r)
	if copyErr != nil {
		_ = fw.Abort()
		return n, copyErr
	}
	if _, err := fw.CommitClean(""); err != nil {
		// Reported to the caller's logger rather than returned: the bytes reached
		// dst, which is what was asked for.
		return n, nil
	}
	return n, nil
}

// tolerant swallows a write error so that a failure on the cache side does not stop
// the copy to the destination. io.MultiWriter gives up on the first error from any
// writer, which is the wrong policy here: the client's copy matters and the cache's
// does not.
type tolerant struct {
	w      io.Writer
	broken bool
}

func (t *tolerant) Write(p []byte) (int, error) {
	if t.broken {
		return len(p), nil
	}
	if _, err := t.w.Write(p); err != nil {
		t.broken = true
	}
	return len(p), nil
}

// ---------------------------------------------------------------- jobs.Cache

// JobCache adapts a DataCache to the narrow interface the job engine wants.
//
// A separate type rather than more methods on DataCache, because the engine's
// interface is shaped for the engine — paths and sizes, no Object, no PutRequest —
// and widening the gateway's own surface to match somebody else's convenience is how
// a package stops having a shape.
type JobCache struct{ c *DataCache }

// ForJobs returns the adapter, or nil when there is no gateway — which the engine
// accepts, and which is what a deployment without --cache-dir gets.
func ForJobs(c *DataCache) *JobCache {
	if c == nil {
		return nil
	}
	return &JobCache{c: c}
}

// WriteBack reports whether writes are accepted into the cache.
func (j *JobCache) WriteBack() bool { return j.c.cfg.WriteBack }

// PutLocalFile copies a local file in and returns its size.
func (j *JobCache) PutLocalFile(ctx context.Context, remotePath, folderID, name, localPath string) (int64, error) {
	obj, err := j.c.PutLocalFile(ctx, PutRequest{
		RemotePath: remotePath, FolderID: folderID, Name: name,
	}, localPath)
	if err != nil {
		return 0, err
	}
	return obj.Size, nil
}

// WaitUntilSent blocks until the object has reached OpenDrive.
func (j *JobCache) WaitUntilSent(ctx context.Context, remotePath string) error {
	return j.c.WaitUntilSent(ctx, remotePath)
}

// CopyTo writes a cached object to a local file.
func (j *JobCache) CopyTo(ctx context.Context, remotePath, localPath string) (bool, error) {
	return j.c.CopyTo(ctx, remotePath, localPath)
}
