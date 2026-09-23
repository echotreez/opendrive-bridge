package server

import (
	"errors"
	"net/http"

	"github.com/echotreez/opendrive-bridge/internal/datacache"
)

// The cache endpoints of whitepaper §4.4.1.
//
// Two of them exist to stop somebody losing data and are the reason this file is
// not just plumbing:
//
//   - /v1/cache/status carries dirty_bytes, dirty_objects and safe_to_shut_down.
//     Rule 3 of §3.5.2 asks for the user to be able to see what has not gone up
//     yet, and "safe to shut down" is computed in one place — the gateway — so
//     that the CLI, the GUI and a script cannot each derive a different answer
//     from the same numbers.
//   - refresh and delete refuse to touch anything unsent, with 409. Discarding a
//     dirty object is not a cache operation, it is deleting the user's only copy,
//     and an endpoint whose name suggests housekeeping must not do it quietly.

// handleCacheStatus answers GET /v1/cache/status.
func (s *Server) handleCacheStatus(w http.ResponseWriter, r *http.Request) {
	if s.datacache == nil {
		writeJSON(w, r, http.StatusOK, datacache.Status{Enabled: false, SafeToShutDown: true})
		return
	}
	writeJSON(w, r, http.StatusOK, s.datacache.Status())
}

// handleCacheObjects answers GET /v1/cache/objects.
func (s *Server) handleCacheObjects(w http.ResponseWriter, r *http.Request) {
	if s.datacache == nil {
		writeJSON(w, r, http.StatusOK, map[string]any{"objects": []any{}})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"objects": s.datacache.Objects()})
}

// cacheFlushRequest is the /v1/cache/flush body.
type cacheFlushRequest struct {
	// Path flushes one object; empty means everything unsent.
	Path string `json:"path,omitempty"`
	// Wait blocks until there is nothing unsent left. This is what
	// `odctl cache flush --wait` uses, and the answer a person needs before
	// turning the machine off.
	Wait bool `json:"wait,omitempty"`
}

func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	if s.datacache == nil {
		WriteError(w, r, cacheUnavailable())
		return
	}
	var req cacheFlushRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if req.Path != "" {
		p, err := normalisePath(req.Path)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		req.Path = p
	}

	// A blocking flush is bounded by the request's own context, so a client that
	// gives up releases the daemon. It is deliberately not given a timeout of its
	// own: how long a user is prepared to wait for their data to reach OpenDrive
	// is the user's decision, and `--wait` means wait.
	if err := s.datacache.Flush(r.Context(), req.Path, req.Wait); err != nil {
		WriteError(w, r, cacheError(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.datacache.Status())
}

// cacheRefreshRequest is the /v1/cache/refresh body.
type cacheRefreshRequest struct {
	Path string `json:"path"`
}

// handleCacheRefresh drops one clean object so the next read goes back to
// OpenDrive. A dirty object is refused: see §4.4.1, "对 dirty 条目必须拒绝,
// 否则就是丢数据".
func (s *Server) handleCacheRefresh(w http.ResponseWriter, r *http.Request) {
	if s.datacache == nil {
		WriteError(w, r, cacheUnavailable())
		return
	}
	var req cacheRefreshRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := normalisePath(req.Path)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := s.datacache.Refresh(p); err != nil {
		WriteError(w, r, cacheError(err))
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"path": p, "refreshed": true})
}

// handleCacheClear empties the cache of clean objects, and refuses outright when
// anything is unsent.
func (s *Server) handleCacheClear(w http.ResponseWriter, r *http.Request) {
	if s.datacache == nil {
		WriteError(w, r, cacheUnavailable())
		return
	}
	if err := s.datacache.Clear(); err != nil {
		WriteError(w, r, cacheError(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.datacache.Status())
}

// cacheUnavailable is the answer when the gateway is switched off. It is a plain
// sentence rather than a 404, because the endpoint exists and the feature does
// not — and a 404 would send somebody looking for a typo.
func cacheUnavailable() error {
	return &RequestError{
		Code: "cache_disabled", HTTP: http.StatusServiceUnavailable,
		Message: "The caching gateway is switched off on this bridge, so there is nothing " +
			"to report or flush. Turn it on with --cache-dir, or the datacache block in " +
			"config.yaml.",
	}
}

// cacheError maps the gateway's own errors onto §4.5 codes.
//
// These do not go through the classification layer, and that is not an oversight:
// the classification layer's job is deciding what *upstream* meant, and these are
// statements about this gateway's own state. Nothing upstream said is involved.
// They still owe the user the same standard of wording, which is why the messages
// live on the sentinel errors in the datacache package rather than being invented
// here — one place to read them, and they are the same in the CLI.
func cacheError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, datacache.ErrCacheFull):
		// 507, `cache_full` (§4.5). The message has to lead with the reassurance:
		// "insufficient storage" on its own reads as though something was dropped,
		// and nothing was.
		return &RequestError{
			Code: "cache_full", HTTP: http.StatusInsufficientStorage,
			Message: err.Error(),
		}
	case errors.Is(err, datacache.ErrDirty):
		return &RequestError{
			Code: "conflict", HTTP: http.StatusConflict,
			Message: err.Error(),
		}
	case errors.Is(err, datacache.ErrNotCached):
		return &RequestError{
			Code: "not_found", HTTP: http.StatusNotFound,
			Message: "That path is not in the cache.",
		}
	case errors.Is(err, datacache.ErrClosed):
		return &RequestError{
			Code: "unavailable", HTTP: http.StatusServiceUnavailable,
			Message: err.Error(),
		}
	default:
		return err
	}
}
