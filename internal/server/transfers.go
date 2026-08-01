package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/echotreez/opendrive-bridge/internal/jobs"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// Transfers come in two shapes, and the difference is not a detail:
//
//   - the job endpoints hand back an id and get on with it, which is what a
//     large file needs and what /v1/jobs reports on;
//   - the stream endpoints are the request itself, which is what a script
//     piping a file wants.
//
// /v1/jobs serialises the engine's own struct rather than a copy of it. A
// second progress shape here would be a second thing to keep in step, and the
// engine's is already the §4.3 schema.

// uploadRequest is the /v1/upload body.
type uploadRequest struct {
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
	Overwrite  bool   `json:"overwrite,omitempty"`
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		WriteError(w, r, transfersUnavailable())
		return
	}
	var req uploadRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	remote, err := normalisePath(req.RemotePath)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if req.LocalPath == "" {
		WriteError(w, r, BadRequest("'local_path' must name the file on this machine to upload."))
		return
	}
	info, statErr := os.Stat(req.LocalPath)
	if statErr != nil {
		WriteError(w, r, BadRequest(fmt.Sprintf("There is no readable file at %s on the machine "+
			"running the bridge.", filepath.Base(req.LocalPath))))
		return
	}
	if info.IsDir() {
		WriteError(w, r, BadRequest(filepath.Base(req.LocalPath)+" is a folder. Upload files one at a time."))
		return
	}

	folderID, name, err := s.uploadTarget(r, remote, req.Overwrite)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	job, err := s.engine.Submit(jobs.Spec{
		Kind: jobs.KindUpload, LocalPath: req.LocalPath, FolderID: folderID,
		Name: name, RemotePath: remote, Size: info.Size(), Overwrite: req.Overwrite,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	parentPath, _ := splitPath(remote)
	s.invalidateSubtree(remote, parentPath)
	writeJSON(w, r, http.StatusAccepted, job)
}

// uploadTarget resolves the destination folder and decides the name, refusing
// to overwrite unless told to.
//
// The existence check goes through the parent listing, never info.json — which
// still describes folders that no longer exist (D27) and would let an upload
// believe it was overwriting something already gone.
func (s *Server) uploadTarget(r *http.Request, remote string, overwrite bool) (string, string, error) {
	parentPath, name := splitPath(remote)
	if name == "" {
		return "", "", BadRequest("'remote_path' must include the file name, " +
			"for example /Documents/report.pdf.")
	}
	if err := opendrive.ValidateName(name); err != nil {
		return "", "", err
	}
	parent, err := s.requireFolder(r.Context(), parentPath)
	if err != nil {
		return "", "", notFoundIfMissing(err, parentPath)
	}
	if !overwrite {
		exists, err := s.exists(r.Context(), remote)
		if err != nil {
			return "", "", err
		}
		if exists {
			return "", "", &RequestError{
				Code: string(opendrive.KindConflict), HTTP: http.StatusConflict,
				Message: "There is already something at " + remote +
					". Send \"overwrite\": true to replace it, or choose another name.",
			}
		}
	}
	return parent.ID, name, nil
}

// handleUploadStream takes the request body as the file content.
//
// It is synchronous on purpose: a script doing `curl --upload-file` wants the
// response to mean the bytes arrived, not that a job was queued.
func (s *Server) handleUploadStream(w http.ResponseWriter, r *http.Request) {
	remote, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "true"
	folderID, name, err := s.uploadTarget(r, remote, overwrite)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if r.ContentLength < 0 {
		WriteError(w, r, BadRequest("A Content-Length header is required here: OpenDrive is told the file size "+
			"before the first byte is sent, so it cannot be worked out as the upload goes."))
		return
	}

	result, err := s.client.Uploads().Upload(r.Context(), r.Body, opendrive.UploadParams{
		FolderID: folderID, Name: name, Size: r.ContentLength, OpenIfExists: overwrite,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	parentPath, _ := splitPath(remote)
	s.invalidateSubtree(remote, parentPath)

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"path": remote, "size": result.BytesSent, "md5": result.Hash,
		"deduplicated": result.Deduplicated,
	})
}

// downloadRequest is the /v1/download body.
type downloadRequest struct {
	RemotePath string `json:"remote_path"`
	LocalPath  string `json:"local_path"`
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		WriteError(w, r, transfersUnavailable())
		return
	}
	var req downloadRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	remote, err := normalisePath(req.RemotePath)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if req.LocalPath == "" {
		WriteError(w, r, BadRequest("'local_path' must say where to save the file on the machine "+
			"running the bridge."))
		return
	}

	t, err := s.resolve(r.Context(), remote)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if t.Kind != kindFile {
		WriteError(w, r, BadRequest(remote+" is a folder. Use the archive download for a whole folder."))
		return
	}

	job, err := s.engine.Submit(jobs.Spec{
		Kind: jobs.KindDownload, FileID: t.ID, LocalPath: req.LocalPath,
		RemotePath: remote, Size: t.Entry.Size,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, job)
}

// handleDownloadStream sends the file content as the response body.
func (s *Server) handleDownloadStream(w http.ResponseWriter, r *http.Request) {
	remote, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.resolve(r.Context(), remote)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if t.Kind != kindFile {
		WriteError(w, r, BadRequest(remote+" is a folder. Use the archive download for a whole folder."))
		return
	}

	// Nothing is written until the transfer is known to be starting, so a
	// failure still answers in the error envelope rather than as a truncated
	// file the caller would save and only later find broken.
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, dlErr := s.client.Downloads().Download(r.Context(), pw, opendrive.DownloadParams{
			FileID: t.ID, Size: t.Entry.Size,
		})
		_ = pw.CloseWithError(dlErr)
		errc <- dlErr
	}()

	buf := make([]byte, 32<<10)
	n, readErr := io.ReadFull(pr, buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		_ = pr.CloseWithError(readErr)
		WriteError(w, r, <-errc)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s",
		strings.ReplaceAll(t.Name, `"`, "")))
	// No Content-Length. It could only come from the folder listing, and when
	// that disagrees with what upstream actually sends, Go truncates the body to
	// the smaller figure and the caller saves a file that is quietly incomplete.
	// A listing said 1024 for a 10400-byte file in testing, which is exactly the
	// kind of metadata this project has learned not to trust (D27, D43).
	// Chunked encoding costs a progress percentage in curl and cannot lie.
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf[:n]); err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	_, _ = io.Copy(w, pr)
	<-errc
}

// handleArchive downloads a folder as a ZIP.
//
// Only folders. upstream's endpoint accepts a list of file ids, answers 200 and
// produces a structurally valid archive containing nothing at all (D42) — so the
// parameter is not offered here. An empty archive reported as a successful
// download is worse than a missing feature.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	remote, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.requireFolder(r.Context(), remote)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		_, arcErr := s.client.Downloads().Archive(r.Context(), pw, []string{t.ID})
		_ = pw.CloseWithError(arcErr)
		errc <- arcErr
	}()

	buf := make([]byte, 32<<10)
	n, readErr := io.ReadFull(pr, buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		_ = pr.CloseWithError(readErr)
		WriteError(w, r, <-errc)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s.zip", t.Name))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf[:n]); err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	_, _ = io.Copy(w, pr)
	<-errc
}

// ---------------------------------------------------------------- jobs

// readable rewrites a failed job's message for the person who will read it.
//
// The engine copies the classifier's diagnosis verbatim and is right to: that
// text is evidence, written for whoever is reading a log, and the engine is not
// the place to decide how it should sound. But turning that into something a
// user can act on is this boundary's job, and it was only being done for
// synchronous failures. Every upload and download is a job, so the most common
// failure a new user meets was arriving as
//
//	the credential is working and upstream refused this twice, so it is a real
//	restriction on this operation rather than a credential problem
//
// — accurate, and addressed to the wrong reader. Found by following
// docs/first-run.md as written, on an account whose root is read-only.
//
// The diagnosis is still in the daemon's log for whoever needs it.
func (s *Server) readable(job *jobs.Job) *jobs.Job {
	if job == nil || job.Error == nil || job.Error.Message == "" {
		return job
	}
	wording := userWordingFor(opendrive.Kind(job.Error.Code), job.Error.Message)
	if wording == "" {
		return job
	}
	// Copied, not mutated: the engine owns that struct and is still using it.
	clone := *job
	e := *job.Error
	e.Message = wording
	clone.Error = &e
	return &clone
}

func (s *Server) readableAll(list []*jobs.Job) []*jobs.Job {
	out := make([]*jobs.Job, 0, len(list))
	for _, j := range list {
		out = append(out, s.readable(j))
	}
	return out
}

func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		WriteError(w, r, transfersUnavailable())
		return
	}
	// The engine's own struct is the §4.3 schema; a second shape here would be
	// a second thing to keep in step.
	writeJSON(w, r, http.StatusOK, map[string]any{"jobs": s.readableAll(s.engine.List())})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		WriteError(w, r, transfersUnavailable())
		return
	}
	job, err := s.engine.Get(chi.URLParam(r, "id"))
	if err != nil {
		WriteError(w, r, jobNotFound(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.readable(job))
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		WriteError(w, r, transfersUnavailable())
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.engine.Cancel(id); err != nil {
		WriteError(w, r, jobNotFound(err))
		return
	}
	job, err := s.engine.Get(id)
	if err != nil {
		WriteError(w, r, jobNotFound(err))
		return
	}
	writeJSON(w, r, http.StatusOK, s.readable(job))
}

func jobNotFound(err error) error {
	if errors.Is(err, jobs.ErrNotFound) {
		return &RequestError{Code: string(opendrive.KindNotFound), HTTP: http.StatusNotFound,
			Message: "There is no transfer with that id. It may have finished and been cleared."}
	}
	return err
}

func transfersUnavailable() error {
	return &RequestError{
		Code: string(opendrive.KindInvalidRequest), HTTP: http.StatusServiceUnavailable,
		Message: "This bridge was started without transfer support, so it cannot move files.",
	}
}

// ---------------------------------------------------------------- sharing

// shareRequest is the /v1/share/link body.
type shareRequest struct {
	Path string `json:"path"`
	// ExpiresAt is a calendar date, YYYY-MM-DD. Upstream takes a date rather
	// than a timestamp here (D31), so the Bridge does too instead of pretending
	// to an accuracy it cannot deliver.
	ExpiresAt string `json:"expires_at,omitempty"`
	MaxUses   int    `json:"max_uses,omitempty"`
}

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := normalisePath(req.Path)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expires := req.ExpiresAt
	if expires == "" {
		expires = time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	} else if _, parseErr := time.Parse("2006-01-02", expires); parseErr != nil {
		WriteError(w, r, BadRequest("'expires_at' must be a date such as 2026-12-31."))
		return
	}

	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var link *opendrive.ExpiringLink
	if t.Kind == kindFolder {
		link, err = s.client.Folders().CreateExpiringLink(r.Context(), t.ID, expires, req.MaxUses, req.MaxUses > 0)
	} else {
		link, err = s.client.Files().CreateExpiringLink(r.Context(), t.ID, expires, req.MaxUses, req.MaxUses > 0)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, map[string]any{
		"path": p, "url": link.URL(), "expires_at": expires, "max_uses": req.MaxUses,
	})
}

func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	p, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	var link *opendrive.ExpiringLink
	if t.Kind == kindFolder {
		link, err = s.client.Folders().ExpiringLinks(r.Context(), t.ID)
	} else {
		link, err = s.client.Files().ExpiringLinks(r.Context(), t.ID)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	shares := []map[string]any{}
	if link != nil && link.URL() != "" {
		shares = append(shares, map[string]any{
			"path": p, "url": link.URL(),
			"expires_at": link.ExpiringDate, "max_uses": link.CounterMax.Int(),
			"uses": link.Counter.Int(),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"path": p, "shares": shares})
}

func (s *Server) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	p, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// Revoking is the same call with the link switched off; upstream has no
	// separate delete for an expiring link.
	past := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if t.Kind == kindFolder {
		_, err = s.client.Folders().CreateExpiringLink(r.Context(), t.ID, past, 0, false)
	} else {
		_, err = s.client.Files().CreateExpiringLink(r.Context(), t.ID, past, 0, false)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"path": p, "detail": "The share link no longer works.",
	})
}
