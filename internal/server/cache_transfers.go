package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/echotreez/opendrive-bridge/internal/datacache"
)

// Where the caching gateway sits in the transfer endpoints (§4.4.1), and one
// deliberate departure from a literal reading of that section, recorded here
// rather than left to be discovered.
//
// # What changes
//
//   - GET /v1/download/stream answers from the cache when it can, filling the
//     cache when it cannot, and says which happened in X-Cache.
//   - PUT /v1/upload/stream, with write-back on, lands the body in the cache and
//     answers 202 with cache_state "dirty". The bytes are on this disk and
//     journalled; they are not on OpenDrive, and 202 is the status that says
//     exactly that.
//
// # What does not, and why
//
// §4.4.1 lists POST /v1/upload alongside the stream endpoint. That endpoint takes
// a `local_path` — a file on the machine running the daemon — and it already
// answers 202 with a job id, because the job engine has owned delivery, retry and
// crash recovery for it since P3.
//
// Routing it through the cache as well would copy the whole file into the cache
// directory, doubling the disk it occupies, to gain nothing: the bytes are already
// on this disk, and the engine already re-queues the transfer after a crash. The
// one thing a copy would add is protection against the user deleting their own
// file between the 202 and the upload — which the pre-cache behaviour did not
// offer either, and which no wording in §4.3 ever promised.
//
// So POST /v1/upload keeps the job engine, and reports `phase` like every other
// job. The stream endpoint is the one where write-back genuinely matters, because
// there the bytes arrive in a request body and exist nowhere else once it ends.
//
// # That reasoning was wrong, and here is what it cost
//
// The efficiency argument holds. The conclusion does not, and the smoke test found
// out why: `odctl up` and `odctl down` both use the job endpoints, so with this
// design **the entire caching gateway is unreachable from the CLI.** A user
// following the documentation gets no caching at all, and §3.5.1 describes the
// gateway as the thing clients exchange data with rather than a side door for
// people who write their own HTTP.
//
// There is also a clue in the specification that was read past. §4.4.1 adds a
// `phase` field to jobs precisely to distinguish "client to gateway" from "gateway
// to OpenDrive" — two legs, which a job only has if it goes through the gateway.
// The field is evidence that POST /v1/upload was meant to.
//
// What is missing is the coupling: a job would have to stay open across both legs,
// moving from `caching` to `uploading`, which means the job engine needs to learn
// when the flusher finished a particular object. That is a real piece of design —
// the flusher has its own worker pool on purpose (§10.2d), so that flushing does
// not compete with transfers a user is watching — and it is not something to
// improvise at the end of the change that revealed the need for it.
//
// So it is written down instead of half-built. Until it exists: the gateway works,
// it is correct, it is reachable over HTTP, and the CLI does not use it. That is a
// gap, not a decision, and CLAUDE.md says so where the next person will look.

// cacheState is the response field §4.4.1 asks for.
const (
	cacheStateDirty = "dirty"
	cacheStateNone  = "not_cached"
)

// writeBackEnabled reports whether a write should land in the cache.
func (s *Server) writeBackEnabled() bool {
	return s.datacache != nil && s.datacache.Status().WriteBack
}

// serveFromCache answers a download from the cache, or reports that it could not.
//
// Returns true when it answered. The X-Cache header is set either way, because a
// caller — and the GUI — needs to know whether the bytes came from here or from
// OpenDrive, and a header present only on a hit is a header nobody can rely on.
func (s *Server) serveFromCache(w http.ResponseWriter, r *http.Request, remote, name string) bool {
	if s.datacache == nil {
		return false
	}
	rd, err := s.datacache.Get(remote)
	if err != nil {
		w.Header().Set("X-Cache", "MISS")
		return false
	}
	defer func() { _ = rd.Close() }()

	w.Header().Set("X-Cache", "HIT")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s",
		strings.ReplaceAll(name, `"`, "")))
	// A Content-Length is safe here and nowhere else in this file: the size is
	// the size of a file on this disk, measured by this process, rather than a
	// number from a folder listing that has been wrong before (D27, D43).
	w.Header().Set("Content-Length", fmt.Sprintf("%d", rd.Object.Size))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rd); err != nil {
		s.log.Debug("a client went away mid-download from the cache",
			slog.String("datacache_path", remote), slog.String("datacache_error", err.Error()))
	}
	return true
}

// fillingWriter tees a download into the cache as it goes to the client.
//
// The client's copy is the one that matters: if the cache write fails, the
// download continues and the cache simply does not keep it. That ordering is
// deliberate — a read cache exists to save a round trip, and failing a transfer
// because an optimisation went wrong would be the tail wagging the dog. It is the
// exact opposite of the write path, where a failure to store must fail the
// request, and the asymmetry is §3.5.2's whole point.
type fillingWriter struct {
	dst  io.Writer
	fill *datacache.FillWriter
	// broken records that the cache side gave up, so it is neither written to
	// again nor committed.
	broken bool
}

func (f *fillingWriter) Write(p []byte) (int, error) {
	n, err := f.dst.Write(p)
	if n > 0 && !f.broken && f.fill != nil {
		if _, ferr := f.fill.Write(p[:n]); ferr != nil {
			f.broken = true
			_ = f.fill.Abort()
		}
	}
	return n, err
}

// commit stores what was cached, or discards it.
//
// complete is the caller's answer to "did the whole file arrive". Only a complete
// transfer is stored: a truncated download that became a cache hit would serve the
// same truncated file to everyone afterwards, and it would do so with no way for
// anybody to notice, which is the worst failure a read cache has available.
//
// No hash is compared. There is nothing to compare against — the Bridge's Entry
// shape deliberately carries no upstream identifiers or digests (see paths.go) —
// so the object's hash is computed from the bytes that actually arrived. That is
// the right value to keep either way: it is what a later flush or verification
// would be checked against, and it describes what is on this disk rather than what
// something else said should be.
func (f *fillingWriter) commit(s *Server, remote string, complete bool) {
	if f.fill == nil {
		return
	}
	if f.broken || !complete {
		_ = f.fill.Abort()
		return
	}
	if _, err := f.fill.CommitClean(""); err != nil {
		s.log.Warn("not caching a download",
			slog.String("datacache_path", remote), slog.String("datacache_error", err.Error()))
	}
}

// beginFill starts caching a download, or returns a no-op when there is no cache.
func (s *Server) beginFill(remote string) *fillingWriter {
	if s.datacache == nil {
		return &fillingWriter{}
	}
	fw, err := s.datacache.Fill(remote)
	if err != nil {
		s.log.Debug("not caching this download",
			slog.String("datacache_path", remote), slog.String("datacache_error", err.Error()))
		return &fillingWriter{}
	}
	return &fillingWriter{fill: fw}
}

// handleUploadStreamCached is PUT /v1/upload/stream with write-back on.
//
// The response is 202, and the difference between 202 and 201 here is the whole
// durability contract in one status code: the bytes are on this disk and recorded
// in the journal, and they are not on OpenDrive. §4.4.1 chose 202 because
// "accepted, not finished" is precisely what has happened, and a 201 would be the
// gateway claiming credit for work it has not done.
func (s *Server) handleUploadStreamCached(w http.ResponseWriter, r *http.Request) {
	remote, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "true"

	// The parent is still resolved upstream before a byte is accepted. It costs a
	// metadata call the cache cannot avoid, and it buys the caller a refusal now
	// rather than an object that sits dirty for ever because its folder does not
	// exist. Failing fast on an impossible path is worth one round trip.
	folderID, name, err := s.uploadTarget(r, remote, overwrite)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	pw, err := s.datacache.Put(datacache.PutRequest{
		RemotePath: remote, FolderID: folderID, Name: name, Size: r.ContentLength,
	})
	if err != nil {
		WriteError(w, r, cacheError(err))
		return
	}
	if _, err := io.Copy(pw, r.Body); err != nil {
		_ = pw.Abort()
		// ErrCacheFull can arrive here rather than up front when the caller sent
		// no Content-Length. It is still a 507 and still means nothing was lost.
		if errors.Is(err, datacache.ErrCacheFull) {
			WriteError(w, r, cacheError(err))
			return
		}
		WriteError(w, r, BadRequest("The upload stopped before the whole file arrived, so "+
			"nothing was stored. Try again."))
		return
	}
	obj, err := pw.Commit()
	if err != nil {
		// A failed commit means the gateway could not make the bytes durable, and
		// it must not pretend otherwise. This is the one place where returning an
		// error is the feature.
		WriteError(w, r, cacheError(err))
		return
	}

	parentPath, _ := splitPath(remote)
	s.invalidateSubtree(remote, parentPath)

	writeJSON(w, r, http.StatusAccepted, map[string]any{
		"path":        obj.RemotePath,
		"size":        obj.Size,
		"md5":         obj.Hash,
		"cache_state": cacheStateDirty,
		"phase":       "uploading",
		// Said in words as well as in the status code, because a client author
		// reading a 202 for the first time should not have to look it up.
		"note": "Accepted and stored on the bridge. It has not reached OpenDrive yet — " +
			"watch /v1/cache/status, or wait for it with `odctl cache flush --wait`.",
	})
}
