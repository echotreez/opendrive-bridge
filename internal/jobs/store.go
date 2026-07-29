package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store persists job state so that a crash costs the in-flight bytes and
// nothing else.
type Store interface {
	// Save writes a job. It must be atomic: a reader after a crash sees either
	// the previous state or the new one, never a half-written file.
	Save(j *Job) error
	// Delete removes a job's state.
	Delete(id string) error
	// Load returns every persisted job.
	Load() ([]*Job, error)
}

// MemoryStore keeps state in memory. It is for tests and for a daemon
// explicitly configured without recovery; a crash loses every job.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{jobs: map[string]*Job{}} }

func (s *MemoryStore) Save(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j.Clone()
	return nil
}

func (s *MemoryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
	return nil
}

func (s *MemoryStore) Load() ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.Clone())
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

// FileStore persists one file per job under a directory.
//
// One file per job rather than one file for all of them is what keeps a write
// cheap: a progress update rewrites a few hundred bytes instead of the whole
// queue, and two workers updating different jobs never contend.
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore prepares dir and returns a store over it.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("job store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the job store directory: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

const jobSuffix = ".job.json"

func (s *FileStore) path(id string) string {
	return filepath.Join(s.dir, id+jobSuffix)
}

// Save writes the job atomically: a temporary file in the same directory, then
// a rename. A rename within a directory is atomic on every platform the bridge
// targets, so a crash mid-write leaves the previous state intact rather than a
// truncated file that would fail to parse on recovery.
func (s *FileStore) Save(j *Job) error {
	if j == nil || j.ID == "" {
		return fmt.Errorf("cannot persist a job with no id")
	}
	if strings.ContainsAny(j.ID, `/\`) {
		return fmt.Errorf("job id %q is not a safe file name", j.ID)
	}
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("cannot encode job %s: %w", j.ID, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.dir, j.ID+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file for job %s: %w", j.ID, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write job %s: %w", j.ID, err)
	}
	// Without the sync, a rename can be durable while the contents are not.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot flush job %s: %w", j.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close job %s: %w", j.ID, err)
	}
	if err := os.Rename(tmpName, s.path(j.ID)); err != nil {
		return fmt.Errorf("cannot commit job %s: %w", j.ID, err)
	}
	return nil
}

func (s *FileStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cannot delete job %s: %w", id, err)
	}
	return nil
}

// Load reads every persisted job. A file that cannot be parsed is reported
// rather than skipped silently: recovery losing a job without saying so is how
// an orphaned upload record goes unnoticed.
func (s *FileStore) Load() ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read the job store: %w", err)
	}

	var (
		out  []*Job
		bad  []string
		errs []error
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), jobSuffix) {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(s.dir, e.Name())) //nolint:gosec // our own directory
		if readErr != nil {
			bad = append(bad, e.Name())
			errs = append(errs, readErr)
			continue
		}
		var j Job
		if jsonErr := json.Unmarshal(data, &j); jsonErr != nil || j.ID == "" {
			bad = append(bad, e.Name())
			if jsonErr != nil {
				errs = append(errs, jsonErr)
			}
			continue
		}
		out = append(out, &j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.Before(out[k].CreatedAt) })

	if len(bad) > 0 {
		return out, fmt.Errorf("%d unreadable job files (%s): %w",
			len(bad), strings.Join(bad, ", "), errors.Join(errs...))
	}
	return out, nil
}
