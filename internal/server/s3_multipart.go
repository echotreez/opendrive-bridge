package server

import (
	"context"
	"crypto/md5" //nolint:gosec // S3's ETag is an MD5 by definition, not a security control
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/gofakes3"
)

// Multipart uploads (§3.6.4). Each part is streamed to its own file under the
// staging directory and flushed before its ETag is returned, so that a part the
// client was told had arrived survives a crash. Completing an upload streams the
// parts, in order, through the same write path as a single PUT; the assembled
// object then obeys every rule of §3.5.2. Nothing is ever held in memory beyond
// a copy buffer — gofakes3's own multipart implementation buffers every part in
// RAM, which is exactly what a backup of a large file cannot afford, and why the
// backend implements the streaming interface itself.

type multipartStore struct {
	dir    string
	expiry time.Duration
	log    *slog.Logger

	mu    sync.Mutex
	locks map[gofakes3.UploadID]*sync.Mutex
}

// uploadManifest is what a staged upload knows about itself.
type uploadManifest struct {
	Bucket  string    `json:"bucket"`
	Key     string    `json:"key"`
	Created time.Time `json:"created"`
}

// partRecord accompanies a part's data: its size and MD5, so that completing an
// upload does not hash every part again.
type partRecord struct {
	Size int64  `json:"size"`
	MD5  string `json:"md5"`
}

func newMultipartStore(dir string, expiry time.Duration, log *slog.Logger) (*multipartStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("s3: cannot create the multipart staging directory %s: %w", dir, err)
	}
	m := &multipartStore{dir: dir, expiry: expiry, log: log, locks: map[gofakes3.UploadID]*sync.Mutex{}}
	m.reap(time.Now())
	return m, nil
}

func (m *multipartStore) lock(id gofakes3.UploadID) func() {
	m.mu.Lock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// uploadDir returns the directory for an upload id, refusing anything that is
// not an id this store could have issued — the id arrives in a query string.
func (m *multipartStore) uploadDir(id gofakes3.UploadID) (string, error) {
	s := string(id)
	if len(s) != 32 {
		return "", gofakes3.ErrNoSuchUpload
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", gofakes3.ErrNoSuchUpload
	}
	return filepath.Join(m.dir, s), nil
}

func (m *multipartStore) manifest(id gofakes3.UploadID, bucket, key string) (string, error) {
	dir, err := m.uploadDir(id)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "upload.json")) // #nosec G304 -- inside the staging dir, id validated
	if err != nil {
		return "", gofakes3.ErrNoSuchUpload
	}
	var man uploadManifest
	if err := json.Unmarshal(raw, &man); err != nil || man.Bucket != bucket || man.Key != key {
		return "", gofakes3.ErrNoSuchUpload
	}
	return dir, nil
}

// reap removes uploads nobody completed or aborted within the expiry.
func (m *multipartStore) reap(now time.Time) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(m.dir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "upload.json")) // #nosec G304 -- inside the staging dir
		var man uploadManifest
		if err == nil {
			err = json.Unmarshal(raw, &man)
		}
		if err != nil {
			info, ierr := e.Info()
			if ierr != nil || now.Sub(info.ModTime()) < time.Hour {
				continue // possibly being created right now
			}
		} else if now.Sub(man.Created) < m.expiry {
			continue
		}
		if err := os.RemoveAll(dir); err == nil {
			m.log.Info("removed a multipart upload that was never completed",
				slog.String("s3_bucket", man.Bucket), slog.String("s3_key", man.Key),
				slog.Time("s3_started", man.Created))
		}
	}
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- staging dir
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ---------------------------------------------------------------- backend

func (b *s3Backend) CreateMultipartUpload(ctx context.Context, bucket, key string, _ map[string]string) (gofakes3.UploadID, error) {
	p, err := objectPath(bucket, key)
	if err != nil {
		return "", b.s3Error(ctx, err)
	}
	if strings.HasSuffix(p, "/") {
		return "", gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, "a key ending in \"/\" is a folder here")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := gofakes3.UploadID(hex.EncodeToString(raw[:]))
	dir := filepath.Join(b.mp.dir, string(id))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", b.s3Error(ctx, err)
	}
	man, _ := json.Marshal(uploadManifest{Bucket: bucket, Key: key, Created: time.Now().UTC()})
	if err := writeSynced(filepath.Join(dir, "upload.json"), man); err != nil {
		_ = os.RemoveAll(dir)
		return "", b.s3Error(ctx, err)
	}
	return id, nil
}

func partName(n int) string { return fmt.Sprintf("part-%05d", n) }

func (b *s3Backend) UploadPart(ctx context.Context, bucket, key string, id gofakes3.UploadID, partNumber int, contentLength int64, body io.Reader) (string, error) {
	dir, err := b.mp.manifest(id, bucket, key)
	if err != nil {
		return "", err
	}
	if partNumber < 1 || partNumber > gofakes3.MaxUploadPartNumber {
		return "", gofakes3.ErrInvalidPart
	}
	f, err := os.CreateTemp(dir, "receiving-*")
	if err != nil {
		return "", b.s3Error(ctx, err)
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	h := md5.New() //nolint:gosec // protocol ETag
	sh := sha256Writer(ctx)
	w := io.MultiWriter(f, h, sh)
	n, err := io.CopyN(w, body, contentLength)
	if err == nil {
		err = readToEOF(body)
	}
	// gofakes3 does not check a streamed part's length for us (its own
	// documentation says so): a short part accepted here would be a truncated
	// object after Complete, with every step reporting success.
	if err != nil || n != contentLength {
		if gofakes3.HasErrorCode(err, gofakes3.ErrBadDigest) {
			return fail(err)
		}
		return fail(gofakes3.ErrIncompleteBody)
	}
	if want := declaredSHA256(ctx); want != "" && sh.sum() != want {
		setStatus(ctx, 400, 0)
		return fail(gofakes3.ErrorMessage("XAmzContentSHA256Mismatch",
			"the part does not match the SHA-256 the request was signed with"))
	}
	if err := f.Sync(); err != nil {
		return fail(b.s3Error(ctx, err))
	}
	if err := f.Close(); err != nil {
		return fail(b.s3Error(ctx, err))
	}

	unlock := b.mp.lock(id)
	defer unlock()
	if _, err := os.Stat(filepath.Join(dir, "upload.json")); err != nil {
		_ = os.Remove(tmp)
		return "", gofakes3.ErrNoSuchUpload // completed or aborted meanwhile
	}
	sum := hex.EncodeToString(h.Sum(nil))
	rec, _ := json.Marshal(partRecord{Size: n, MD5: sum})
	if err := writeSynced(filepath.Join(dir, partName(partNumber)+".json"), rec); err != nil {
		_ = os.Remove(tmp)
		return "", b.s3Error(ctx, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, partName(partNumber))); err != nil {
		_ = os.Remove(tmp)
		return "", b.s3Error(ctx, err)
	}
	if err := syncDirectory(dir); err != nil {
		return "", b.s3Error(ctx, err)
	}
	return `"` + sum + `"`, nil
}

func (b *s3Backend) CompleteMultipartUpload(ctx context.Context, bucket, key string, id gofakes3.UploadID, in *gofakes3.CompleteMultipartUploadRequest) (gofakes3.VersionID, string, error) {
	unlock := b.mp.lock(id)
	defer unlock()
	dir, err := b.mp.manifest(id, bucket, key)
	if err != nil {
		return "", "", err
	}
	if in == nil || len(in.Parts) == 0 {
		return "", "", gofakes3.ErrorMessage(gofakes3.ErrMalformedXML, "the request lists no parts")
	}
	parts := in.Parts
	if !sort.SliceIsSorted(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber }) {
		return "", "", gofakes3.ErrInvalidPartOrder
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	var readers []io.Reader
	var total int64
	etagSum := md5.New() //nolint:gosec // S3 multipart ETag
	prev := 0
	for _, part := range parts {
		if part.PartNumber == prev {
			return "", "", gofakes3.ErrInvalidPartOrder
		}
		prev = part.PartNumber
		raw, err := os.ReadFile(filepath.Join(dir, partName(part.PartNumber)+".json")) // #nosec G304 -- staging dir
		if err != nil {
			return "", "", gofakes3.ErrInvalidPart
		}
		var rec partRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return "", "", gofakes3.ErrInvalidPart
		}
		if strings.Trim(part.ETag, `" `) != rec.MD5 {
			return "", "", gofakes3.ErrInvalidPart
		}
		f, err := os.Open(filepath.Join(dir, partName(part.PartNumber))) // #nosec G304 -- staging dir
		if err != nil {
			return "", "", gofakes3.ErrInvalidPart
		}
		files = append(files, f)
		readers = append(readers, f)
		total += rec.Size
		sum, _ := hex.DecodeString(rec.MD5)
		etagSum.Write(sum)
	}
	p, err := objectPath(bucket, key)
	if err != nil {
		return "", "", b.s3Error(ctx, err)
	}
	if _, err := b.put(ctx, p, io.MultiReader(readers...), total, ""); err != nil {
		return "", "", err
	}
	// Stored and journalled: the staging copy is no longer the only one.
	if err := os.RemoveAll(dir); err != nil {
		b.log.Warn("could not remove a completed multipart upload's parts",
			slog.String("dir", dir), slog.String("error", err.Error()))
	}
	b.mp.mu.Lock()
	delete(b.mp.locks, id)
	b.mp.mu.Unlock()
	b.known.Store(bucket, true)
	etag := fmt.Sprintf(`"%s-%s"`, hex.EncodeToString(etagSum.Sum(nil)), strconv.Itoa(len(parts)))
	return "", etag, nil
}

func (b *s3Backend) AbortMultipartUpload(ctx context.Context, bucket, key string, id gofakes3.UploadID) error {
	unlock := b.mp.lock(id)
	defer unlock()
	dir, err := b.mp.manifest(id, bucket, key)
	if err != nil {
		if errors.Is(err, gofakes3.ErrNoSuchUpload) || gofakes3.HasErrorCode(err, gofakes3.ErrNoSuchUpload) {
			return nil // already gone: abort is idempotent
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return b.s3Error(ctx, err)
	}
	return nil
}

// hashSink hashes what is written to it when it has a hash to feed.
type hashSink struct{ h hash.Hash }

func (s *hashSink) Write(p []byte) (int, error) {
	if s.h != nil {
		_, _ = s.h.Write(p)
	}
	return len(p), nil
}

func (s *hashSink) sum() string {
	if s.h == nil {
		return ""
	}
	return hex.EncodeToString(s.h.Sum(nil))
}

// sha256Writer hashes a part when the client declared a SHA-256 for it.
func sha256Writer(ctx context.Context) *hashSink {
	if declaredSHA256(ctx) == "" {
		return &hashSink{}
	}
	return &hashSink{h: sha256.New()}
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- staging dir
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
