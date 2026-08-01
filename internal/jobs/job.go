// Package jobs is the transfer job engine of whitepaper §10.2: a bounded worker
// pool over queued uploads and downloads, with progress, cancellation and
// recovery after a crash.
//
// Two rules shape everything here, and both are about *not* doing things:
//
//   - The engine never judges an error. Whether a failure may be retried, how
//     long to wait, and what to tell the user all come from
//     `pkg/opendrive`'s classification layer (`docs/error-taxonomy.md`). The
//     engine reads Temporary(), RetryAfter(), RetryBudget() and Diagnosis(); it
//     does not look at a status code, match a message, or write its own wording.
//     Upstream's messages are unreliable enough that a second opinion here would
//     only add a second way to be wrong.
//   - The engine never leaves an artefact upstream. Every terminal path —
//     cancellation, a permanent failure, an abandoned recovery — reclaims the
//     record `create_file` made, because an abandoned record is invisible to the
//     user and enough of them make a folder refuse writes
//     (docs/discrepancies.md D39).
package jobs

import (
	"errors"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// State is the lifecycle of a job. The values are the strings the Bridge REST
// API reports in /v1/jobs (whitepaper §4.3).
type State string

// Job states.
const (
	// StateQueued means accepted and waiting for a worker.
	StateQueued State = "queued"
	// StateRunning means a worker is transferring it.
	StateRunning State = "running"
	// StateSucceeded is terminal and means the bytes are upstream, verified.
	StateSucceeded State = "succeeded"
	// StateFailed is terminal. Error carries the classifier's verdict.
	StateFailed State = "failed"
	// StateCancelled is terminal and requested by the caller. Any upstream
	// record the transfer had created has been reclaimed by the time a job
	// reaches it.
	StateCancelled State = "cancelled"
)

// Terminal reports whether no further work will happen on a job in this state.
func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// Kind distinguishes the two transfer directions.
type Kind string

// Job kinds.
const (
	KindUpload   Kind = "upload"
	KindDownload Kind = "download"
)

// Error is the failure shape the Bridge REST API serialises (§4.3, §4.5).
//
// Every field comes from the classification layer. Code is the stable
// `opendrive.Kind` enum, so a client can switch on it; Message is the
// classifier's Diagnosis when it has one, because that is precisely the case
// where upstream's own message is known to be misleading — a resource-pressure
// refusal must never reach a user as "contact your administrator"
// (docs/error-taxonomy.md T2).
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	// HTTPStatus is upstream's status, for operators. 0 when there was none.
	HTTPStatus int `json:"http_status,omitempty"`
}

// newError translates an SDK error into the reported shape. It is the only
// place the engine touches an error's contents, and it copies rather than
// interprets.
func newError(err error) *Error {
	if err == nil {
		return nil
	}
	out := &Error{
		Code:      string(opendrive.ErrorKind(err)),
		Message:   err.Error(),
		Retryable: opendrive.IsTemporary(err),
	}
	if out.Code == "" {
		out.Code = string(opendrive.KindUpstreamError)
	}
	var ae *opendrive.APIError
	if errors.As(err, &ae) {
		out.HTTPStatus = ae.HTTPCode
		// The classifier speaks up exactly when upstream's wording cannot be
		// trusted. When it does, it wins.
		if d := ae.Diagnosis(); d != "" {
			out.Message = d
		} else if ae.UpstreamMsg != "" {
			out.Message = ae.UpstreamMsg
		}
	}
	return out
}

// Spec describes a transfer to run. Exactly one direction is filled in.
type Spec struct {
	Kind Kind
	// LocalPath is the file on disk: the source for an upload, the destination
	// for a download.
	LocalPath string
	// FolderID is the upload destination folder.
	FolderID string
	// Name is the remote file name for an upload. Defaults to the local base
	// name.
	Name string
	// FileID is the download source.
	FileID string
	// RemotePath is carried through for reporting only.
	RemotePath string
	// Size, when known, is reported as bytes_total before the transfer starts
	// and lets a download decide whether to pre-flight (§2.5).
	Size int64
	// Hash is the expected MD5, verified on download.
	Hash string
	// Overwrite asks upstream to reuse an existing file of the same name
	// instead of failing. It also disables record reclamation, since the
	// record may not be ours to delete.
	Overwrite bool
}

// Job is one unit of work, and the exact shape /v1/jobs serialises (§4.3). It
// is also what is persisted, so a crash loses nothing but the in-flight bytes.
type Job struct {
	ID         string `json:"id"`
	Kind       Kind   `json:"kind"`
	State      State  `json:"state"`
	LocalPath  string `json:"local_path,omitempty"`
	RemotePath string `json:"remote_path,omitempty"`

	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
	// Speed is bytes per second over the recent past, 0 when not running.
	Speed float64 `json:"speed"`

	Error *Error `json:"error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Attempts counts how many times a worker has picked this job up.
	Attempts int `json:"attempts"`

	// The fields below are engine state rather than API surface. They are
	// persisted because they are what makes recovery possible, and they are
	// omitted from a response by the REST layer.

	// Spec is everything needed to run the job again.
	Spec Spec `json:"spec"`
	// UpstreamFileID and UpstreamTempLocation are what create_file handed back
	// before any content moved. Persisting them before the first byte is sent
	// is what lets a crashed upload's record be reclaimed rather than leaked
	// (D39).
	UpstreamFileID       string `json:"upstream_file_id,omitempty"`
	UpstreamTempLocation string `json:"upstream_temp_location,omitempty"`
}

// Clone returns a deep-enough copy for handing to a caller without exposing
// engine state to mutation.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	out := *j
	if j.Error != nil {
		e := *j.Error
		out.Error = &e
	}
	return &out
}
