package datacache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The journal is what rule 2 of §3.5.2 rests on: an object's state and its place
// on disk are written here and fsynced *before* the caller is told the write
// succeeded. Everything else in this package is an optimisation; this file is the
// promise.
//
// # Why a log and not a state file
//
// A single file holding the whole index has to be rewritten on every change, and
// a rewrite has a window in which neither the old nor the new content is
// complete. The usual fix — write a temporary file and rename — is what
// jobs.FileStore does, and it is right there because each job has its own file.
// Here one write would rewrite the index for every object, so the cost grows with
// the cache rather than with the change.
//
// An append-only log has the property that matters instead: a record is either
// entirely present or entirely absent, and a torn tail is recognisable. Recovery
// replays what it can and stops at the first record it cannot trust.
//
// # Why fsync, and where
//
// A write() that returned tells you the kernel has the bytes. A power cut, a
// kernel panic or a lost storage connection at that moment loses them. For a
// cache of things OpenDrive already has, that would be a shrug; for an object
// only this directory holds, it is the user's file. So:
//
//	content file:  write, Sync, close, rename, Sync the directory
//	journal:       append the record, Sync
//	only then:     tell the caller it worked
//
// The directory sync is the step most often left out. Without it the rename can
// be lost even though the file's contents were durable, which leaves a journal
// entry pointing at nothing — the exact case the recovery scan has to throw away,
// and it would be throwing away an object the user was told had been stored.
//
// # Torn records
//
// A crash can leave a partial line at the end. Replay reads whole lines only and
// treats the first unparseable one as the end of the journal, discarding the
// remainder. The alternative — trying to repair it — would mean guessing at the
// contents of a record about the user's data, and a guess here is worse than an
// honest loss of the last write, which the client was never told had succeeded.

// journalOp is the kind of a record.
type journalOp string

const (
	// opPut records a new or replaced object, with everything needed to rebuild
	// its index entry.
	opPut journalOp = "put"
	// opState records a lifecycle transition.
	opState journalOp = "state"
	// opDrop records a removal — eviction, refresh, or an object whose content
	// went missing.
	opDrop journalOp = "drop"
	// opTouch records an access, for LRU. These are the only records that may be
	// lost without consequence, so they are not synced.
	opTouch journalOp = "touch"
)

// journalRecord is one line of the journal. Fields are omitted when empty so
// that a touch record stays short — the journal is appended to on every read hit.
type journalRecord struct {
	Op  journalOp `json:"op"`
	Obj *Object   `json:"obj,omitempty"`
	// Path identifies the object for records that do not carry the whole thing.
	Path string `json:"path,omitempty"`
	// State is the new state for opState.
	State State `json:"state,omitempty"`
	// At is the record's timestamp, as Unix nanoseconds.
	At int64 `json:"at,omitempty"`
	// FileID accompanies a state change that learned the upstream record's id,
	// so that a crash between "record created" and "content sent" can still
	// reclaim it (D39).
	FileID string `json:"file_id,omitempty"`
	// Hash accompanies the transition to clean: the hash upstream confirmed.
	Hash string `json:"hash,omitempty"`
	// Err is the classifier's wording from a failed flush, kept so that a
	// restarted daemon can still tell the user why an object is stuck.
	Err string `json:"err,omitempty"`
	// Attempts is the flush attempt count at the time of the record.
	Attempts int `json:"attempts,omitempty"`
}

// journalName is the log's filename inside the cache directory.
const journalName = "journal"

// compactAfter is the number of records past which the journal is rewritten from
// the live index. Touch records dominate a read-heavy cache, so without this the
// log would grow without bound while the index it describes stayed small.
const compactAfter = 20000

// journal is an append-only log of index changes.
type journal struct {
	dir string

	mu      sync.Mutex
	f       *os.File
	records int
	// syncs counts fsyncs, for tests that assert durability happened rather than
	// inferring it.
	syncs int
	// failSyncWhen is fault injection for rule 3, which is the one rule that
	// cannot be tested by observing success: a working journal and a journal with
	// no fsync at all behave identically until the power goes out. The only proof
	// is to make the sync fail and check that no acknowledgement came out.
	//
	// It is a predicate on the record rather than a "fail the next one" flag,
	// because the flush workers journal their own state changes continuously. A
	// counter would be consumed by whichever write happened to be next, which is
	// a race, and the test would pass or fail depending on scheduling rather than
	// on the property. Naming the record makes the test say what it means.
	failSyncWhen func(journalRecord) error
}

func openJournal(dir string) (*journal, error) {
	path := filepath.Join(dir, journalName)
	// #nosec G304 -- the configured cache directory joined with a constant
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot open the journal %s: %w", path, err)
	}
	return &journal{dir: dir, f: f}, nil
}

// append writes one record. sync says whether durability is required before
// returning, which is true for everything except a touch.
func (j *journal) append(rec journalRecord, sync bool) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("datacache: cannot encode a journal record: %w", err)
	}
	line = append(line, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()

	if _, err := j.f.Write(line); err != nil {
		return fmt.Errorf("datacache: cannot write to the journal: %w", err)
	}
	j.records++
	if !sync {
		return nil
	}
	if j.failSyncWhen != nil {
		if err := j.failSyncWhen(rec); err != nil {
			return fmt.Errorf("datacache: cannot flush the journal: %w", err)
		}
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("datacache: cannot flush the journal: %w", err)
	}
	j.syncs++
	return nil
}

// syncCount reports how many times the journal has been flushed to disk.
func (j *journal) syncCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.syncs
}

// breakSyncWhen makes durable appends fail for the records the predicate picks.
// Tests only; see failSyncWhen for why it is a predicate.
func (j *journal) breakSyncWhen(pred func(journalRecord) error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failSyncWhen = pred
}

func (j *journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// replayResult is what a startup scan found.
type replayResult struct {
	// objects is the rebuilt index.
	objects map[string]*Object
	// records is how many were read.
	records int
	// torn is true when the last record was incomplete, which means a crash
	// during a write. It is reported rather than hidden: the client of that
	// write was never told it succeeded, but an operator reading the log after
	// an incident deserves to know the daemon did not stop cleanly.
	torn bool
}

// replayJournal rebuilds the index from the log.
//
// It does not touch the content files — verification against what is actually on
// disk is a separate step, because the two failures are different and a reader
// should be able to tell them apart: a journal that stops mid-record is a crash
// during a write, while a journal entry with no content file is a rename that did
// not survive.
func replayJournal(dir string) (*replayResult, error) {
	path := filepath.Join(dir, journalName)
	// #nosec G304 -- the configured cache directory joined with a constant
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &replayResult{objects: map[string]*Object{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot read the journal %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := &replayResult{objects: map[string]*Object{}}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && (errors.Is(err, io.EOF) || err == nil) {
			if !hasCompleteLine(line, err) {
				// A tail with no newline is a record the crash cut in half.
				out.torn = true
				break
			}
			var rec journalRecord
			if jsonErr := json.Unmarshal(line, &rec); jsonErr != nil {
				// Parseable as a line but not as a record: also a torn write, and
				// also the end of what can be trusted. Anything after it is
				// discarded rather than interpreted, because the records are not
				// independent — a state change refers to a put before it.
				out.torn = true
				break
			}
			applyRecord(out.objects, rec)
			out.records++
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("datacache: cannot read the journal: %w", err)
			}
			break
		}
	}
	return out, nil
}

// hasCompleteLine reports whether a line read from the journal is whole.
func hasCompleteLine(line []byte, readErr error) bool {
	if len(line) == 0 {
		return false
	}
	if line[len(line)-1] == '\n' {
		return true
	}
	// No newline: only acceptable if this is not the end of the file, which it
	// always is when ReadBytes stopped without the delimiter.
	_ = readErr
	return false
}

// applyRecord folds one record into the index being rebuilt.
func applyRecord(objects map[string]*Object, rec journalRecord) {
	switch rec.Op {
	case opPut:
		if rec.Obj == nil || rec.Obj.RemotePath == "" {
			return
		}
		objects[rec.Obj.RemotePath] = rec.Obj.Clone()
	case opState:
		o := objects[rec.Path]
		if o == nil {
			return
		}
		if rec.State != "" {
			o.State = rec.State
		}
		if rec.FileID != "" {
			o.UpstreamFileID = rec.FileID
		}
		if rec.Hash != "" {
			o.Hash = rec.Hash
		}
		o.LastError = rec.Err
		if rec.Attempts > 0 {
			o.FlushAttempts = rec.Attempts
		}
	case opDrop:
		delete(objects, rec.Path)
	case opTouch:
		if o := objects[rec.Path]; o != nil && rec.At > 0 {
			o.LastAccess = unixNano(rec.At)
		}
	}
}

// compactJournal rewrites the log from the current index.
//
// Written to a temporary file, synced, then renamed over the old one, and the
// directory synced afterwards. The order matters for the same reason it does for
// a content file: a rename that is durable while its contents are not would leave
// a journal that parses and describes nothing.
func compactJournal(dir string, objects []*Object) (*journal, error) {
	tmpPath := filepath.Join(dir, journalName+".compacting")
	// #nosec G304 -- the configured cache directory joined with a constant
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot start journal compaction: %w", err)
	}
	w := bufio.NewWriter(tmp)
	for _, o := range objects {
		line, err := json.Marshal(journalRecord{Op: opPut, Obj: o, At: o.StoredAt.UnixNano()})
		if err != nil {
			_ = tmp.Close()
			return nil, fmt.Errorf("datacache: cannot encode %s during compaction: %w", o.RemotePath, err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			return nil, fmt.Errorf("datacache: cannot write during compaction: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("datacache: cannot flush during compaction: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("datacache: cannot sync the compacted journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("datacache: cannot close the compacted journal: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, journalName)); err != nil {
		return nil, fmt.Errorf("datacache: cannot replace the journal: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return openJournal(dir)
}

// syncDir flushes a directory entry, so that a rename inside it is durable.
func syncDir(dir string) error {
	// #nosec G304 -- the configured cache directory
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("datacache: cannot open %s to flush it: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("datacache: cannot flush the directory %s: %w", dir, err)
	}
	return nil
}
