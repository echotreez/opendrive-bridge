package datacache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// State is an object's position in the write-back lifecycle (§3.5.2).
type State string

// Object states. The strings are what /v1/cache/objects reports, so they are API
// surface and may not be renamed without a version bump.
const (
	// StateDirty means the bytes are here and OpenDrive does not have them.
	// This is the state that makes the gateway the only holder of user data, and
	// the one that must never be evicted.
	StateDirty State = "dirty"
	// StateUploading means a flush worker is sending it. Still dirty for every
	// purpose that matters — it is not upstream yet, and a crash must bring it
	// back — but distinguished so a user watching /v1/cache/status can see
	// progress rather than a number that sits still.
	StateUploading State = "uploading"
	// StateClean means OpenDrive has it and the hash was verified. Only a clean
	// object may be evicted, refreshed or cleared.
	StateClean State = "clean"
)

// Unsent reports whether losing this object would lose the user's data.
//
// It is one method rather than two comparisons at each call site on purpose:
// "dirty or uploading" is the condition for every rule in §3.5.2, and the one
// way this design fails quietly is somebody writing `== StateDirty` and
// forgetting that an upload in flight has not arrived yet.
func (s State) Unsent() bool { return s == StateDirty || s == StateUploading }

// Object is one cached file.
//
// RemotePath is the identity. Not the upstream file id: a write-back gateway
// accepts a write for a path that has no file upstream at all yet, so the id is
// something the flusher learns, not something the object is keyed by.
type Object struct {
	// RemotePath is the path in the account, normalised, and the key.
	RemotePath string `json:"remote_path"`
	// Size is the object's length in bytes.
	Size int64 `json:"size"`
	// Hash is the MD5 of the content as stored here, computed while it was
	// written. For a dirty object it is what the flush will be checked against;
	// for a clean one it is what upstream confirmed.
	Hash string `json:"hash"`
	// State is the lifecycle position.
	State State `json:"state"`
	// FolderID and Name are where the flusher will put it. FolderID may be empty
	// when the write arrived before the parent was resolved; the flusher resolves
	// it then.
	FolderID string `json:"folder_id,omitempty"`
	Name     string `json:"name,omitempty"`
	// UpstreamFileID is filled in once upstream has a record for it. Persisted
	// before any content moves, so a crash mid-flush can reclaim the record
	// rather than leak it (D39).
	UpstreamFileID string `json:"upstream_file_id,omitempty"`

	// StoredAt is when the content landed here. LastAccess drives LRU eviction.
	StoredAt   time.Time `json:"stored_at"`
	LastAccess time.Time `json:"last_access"`
	// FlushAttempts counts how many times a flush has been tried and failed.
	// Reported so that a user can see an object that is stuck rather than slow.
	FlushAttempts int `json:"flush_attempts"`
	// LastError is the classifier's verdict from the most recent failed flush,
	// as the wording meant for a person. Empty when the last attempt succeeded
	// or none has been made.
	LastError string `json:"last_error,omitempty"`
}

// key is the object's filename inside the cache directory.
//
// A hash rather than the path itself. Three reasons, in order of how badly each
// one bites:
//
//   - A remote path may contain characters this filesystem forbids, and may be
//     longer than a filename may be. Escaping schemes that map one namespace
//     onto another are a known source of collisions.
//   - Two different paths must never land on one file. A hex SHA-256 cannot
//     collide by accident.
//   - The cache directory is then flat, which keeps eviction and the startup
//     scan simple, and means no path traversal is possible from a remote path —
//     the filename is 64 hex characters whatever the input was.
//
// The path is recorded in the journal, so nothing is lost by the key being
// opaque: `odctl cache objects` reports paths, not keys.
func key(remotePath string) string {
	sum := sha256.Sum256([]byte(remotePath))
	return hex.EncodeToString(sum[:])
}

// contentName is the filename for an object's bytes.
func contentName(remotePath string) string { return key(remotePath) + ".bin" }

// normalisePath puts a remote path in the one form the cache keys by, so that
// "/Docs/x.pdf" and "/Docs//x.pdf" are one object rather than two copies of the
// same file — which in write-back mode would mean two conflicting writes.
//
// It is deliberately thin: the Bridge's own path layer (internal/server/paths.go
// and pkg/opendrive/path.go) has already validated and normalised anything that
// reaches here, and a second, subtly different implementation would be a way for
// the two to disagree. This only collapses separators and strips a trailing one.
func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// Clone returns a copy safe to hand to a caller.
func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	out := *o
	return &out
}

func (o *Object) String() string {
	return fmt.Sprintf("%s (%d bytes, %s)", o.RemotePath, o.Size, o.State)
}
