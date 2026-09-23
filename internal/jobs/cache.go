package jobs

import (
	"context"
	"errors"
	"log/slog"
)

// The caching gateway's place in a job (whitepaper §4.4.1).
//
// A job has two legs when the gateway is in front of it, and `phase` is how a
// caller tells which one is running:
//
//	upload:    caching (this machine → the gateway) then uploading (gateway → OpenDrive)
//	download:  a cache hit costs no network at all; a miss downloads and fills
//
// # Why the engine does not know about internal/datacache
//
// It talks to the interface below instead, for two reasons that both matter. The
// engine's tests must be able to make the cache accept, refuse and fail on demand,
// which a concrete gateway over a real directory makes awkward. And §10.2d gives
// the flusher its own worker pool precisely so that flushing does not compete with
// the transfers a user is watching — a seam here keeps that separation visible
// rather than letting the two grow into each other.
//
// The interface is deliberately four methods wide. Everything the engine needs is
// "hold this", "tell me when it is really upstream", "have you got this" and "keep
// a copy of what I just fetched". Anything more would be the engine reaching into
// the gateway's business.

// Cache is the slice of the caching gateway a job needs.
type Cache interface {
	// PutLocalFile copies a local file into the cache as an unsent object. It
	// returns an error satisfying IsCacheFull when the file does not fit the
	// gateway's allowance for data OpenDrive does not have.
	PutLocalFile(ctx context.Context, remotePath, folderID, name, localPath string) (size int64, err error)
	// WaitUntilSent blocks until the object has reached OpenDrive.
	WaitUntilSent(ctx context.Context, remotePath string) error
	// CopyTo writes a cached object to a local file, reporting false on a miss.
	CopyTo(ctx context.Context, remotePath, localPath string) (bool, error)
	// WriteBack reports whether the gateway accepts writes. With it false the
	// gateway is a read cache and uploads go straight to OpenDrive.
	WriteBack() bool
}

// cacheFull is the error PutLocalFile returns when an object will not fit. It is an
// interface rather than a sentinel so that internal/datacache's own ErrCacheFull
// satisfies it without either package importing the other.
type cacheFull interface{ CacheFull() bool }

// IsCacheFull reports whether an error means the gateway has no room for unsent
// data. The distinction matters: it is not a failure, it is a reason to upload
// directly instead, and refusing a large file because the *cache* is full would be
// a worse answer than not caching it.
func IsCacheFull(err error) bool {
	var full cacheFull
	if errors.As(err, &full) {
		return full.CacheFull()
	}
	return false
}

// WithCache puts the caching gateway in front of transfers. Without it the engine
// behaves exactly as it did before v1.2.
func WithCache(c Cache) Option {
	return func(e *Engine) {
		if c != nil {
			e.cache = c
		}
	}
}

// initialPhase is the leg a job starts on, decided when it is queued so that a job
// waiting for a worker already reports something true.
//
// An upload starts on the caching leg when there is a gateway that accepts writes,
// because that is what will happen first. It is a prediction, and the one way it can
// be wrong — the gateway having no room — is corrected by uploadThroughCache before
// any bytes move. A download has one leg either way: a cache hit is not a separate
// phase, it is the same leg served from nearer by.
func (e *Engine) initialPhase(k Kind) Phase {
	if k == KindDownload {
		return PhaseDownloading
	}
	if e.cache != nil && e.cache.WriteBack() {
		return PhaseCaching
	}
	return PhaseUploading
}

// setPhase records which leg a job is on, and persists it, because a client
// watching /v1/jobs across a restart should see the same answer.
func (e *Engine) setPhase(id string, p Phase) {
	e.mu.Lock()
	j, ok := e.jobs[id]
	if ok {
		j.Phase = p
		j.UpdatedAt = e.now()
	}
	e.mu.Unlock()
	if ok {
		_ = e.persist(id)
	}
}

// uploadThroughCache runs an upload's two legs, or reports that it did not.
//
// Returns handled=false when the gateway is not in play or had no room, which tells
// the caller to upload directly. That fallback is the whole reason this returns a
// flag rather than an error: a file too large for the cache is a file to send
// straight up, not a failed transfer.
func (e *Engine) uploadThroughCache(ctx context.Context, id string, spec Spec) (handled bool, err error) {
	if e.cache == nil || !e.cache.WriteBack() {
		return false, nil
	}

	// Leg one. The job is "caching" while the bytes are being copied in, which for
	// a local file is quick — but it is the leg that can refuse, so it is worth
	// being able to see.
	e.setPhase(id, PhaseCaching)
	size, err := e.cache.PutLocalFile(ctx, spec.RemotePath, spec.FolderID, spec.Name, spec.LocalPath)
	if err != nil {
		if IsCacheFull(err) {
			e.log.Info("the cache has no room for this file, so it goes straight to OpenDrive",
				slog.String("job", id),
				slog.String("remote_path", spec.RemotePath))
			e.setPhase(id, PhaseUploading)
			return false, nil
		}
		return false, err
	}

	// The bytes are on this disk and journalled. From here the gateway owns
	// delivery — retries, crash recovery, the drain at shutdown — and this job is
	// watching rather than working.
	e.mu.Lock()
	if j, ok := e.jobs[id]; ok {
		j.BytesDone, j.BytesTotal = size, size
	}
	e.mu.Unlock()
	e.setPhase(id, PhaseUploading)

	// Leg two. The same notification `odctl cache flush --wait` uses, so there is
	// one answer to "is it really upstream" rather than two that could disagree.
	//
	// A cancelled context stops the waiting and not the sending: the object stays
	// in the cache, queued, and the gateway will finish it. The job is reported
	// cancelled, which is true of the job.
	if err := e.cache.WaitUntilSent(ctx, spec.RemotePath); err != nil {
		return true, err
	}
	return true, nil
}

// downloadThroughCache serves a download from the cache when it can.
//
// Returns handled=true when the file was written from the cache, which costs no
// request at all — the one case where the gateway saves the whole round trip rather
// than part of it.
func (e *Engine) downloadThroughCache(ctx context.Context, id string, spec Spec) (bool, error) {
	if e.cache == nil || spec.RemotePath == "" {
		return false, nil
	}
	served, err := e.cache.CopyTo(ctx, spec.RemotePath, spec.LocalPath)
	if err != nil {
		// A cache that cannot serve is a miss, not a failure: the file is upstream
		// and can be fetched. Worth a line, because a cache failing every read is
		// something an operator should see.
		e.log.Warn("could not serve this download from the cache; fetching it instead",
			slog.String("job", id),
			slog.String("remote_path", spec.RemotePath),
			slog.String("error", err.Error()))
		return false, nil
	}
	if !served {
		return false, nil
	}
	e.log.Debug("served a download from the cache",
		slog.String("job", id), slog.String("remote_path", spec.RemotePath))
	return true, nil
}
