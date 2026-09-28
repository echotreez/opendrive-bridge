package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rclone/gofakes3"

	"github.com/echotreez/opendrive-bridge/internal/datacache"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The S3 gateway (whitepaper §3.6): gofakes3 speaks the protocol, and this file
// answers it from the caching gateway and OpenDrive.
//
// Everything here follows the two constraints of §3.6.4. An object the gateway
// has accepted and not yet delivered is part of every answer — listed, readable,
// deletable — because backup software checks what it wrote immediately after
// writing it. And a write is accepted while OpenDrive is unreachable, because
// that is when a backup most needs somewhere to go.

// S3Config is the gateway's own configuration.
type S3Config struct {
	// PermanentDelete makes DeleteObject remove files for good instead of moving
	// them to OpenDrive's trash (§3.6.3; trash is Derek's default).
	PermanentDelete bool
	// WholeFetchLimit is the largest object fetched whole into the cache before
	// the first byte is sent. Below it a read is verified against OpenDrive's
	// MD5 and served with an exact length; above it, it is streamed.
	WholeFetchLimit int64
	// StagingDir holds multipart uploads in progress.
	StagingDir string
	// MultipartExpiry is how long an upload nobody completes or aborts is kept.
	MultipartExpiry time.Duration
}

const (
	defaultWholeFetchLimit = 64 << 20
	defaultMultipartExpiry = 7 * 24 * time.Hour
)

type s3Backend struct {
	s   *Server
	cfg S3Config
	log *slog.Logger
	// known remembers buckets seen to exist since the process started, so that a
	// write into one of them can be accepted while OpenDrive is unreachable. A
	// bucket nobody has seen since the last restart cannot be vouched for, and
	// a write into it during an outage is refused rather than guessed at.
	known sync.Map
	mp    *multipartStore
}

var (
	_ gofakes3.Backend          = (*s3Backend)(nil)
	_ gofakes3.MultipartBackend = (*s3Backend)(nil)
)

// ---------------------------------------------------------------- errors

// s3Status carries a status gofakes3 cannot express. It maps every error code it
// does not know to 500, and "OpenDrive is away for a minute" must be a 503 with
// a Retry-After, or a client that would have waited gives up instead. The
// backend records the status here and s3StatusWriter applies it.
type s3Status struct {
	mu     sync.Mutex
	code   int
	retry  int
	sha256 string
}

type s3StatusKey struct{}

func statusOf(ctx context.Context) *s3Status {
	st, _ := ctx.Value(s3StatusKey{}).(*s3Status)
	return st
}

func setStatus(ctx context.Context, code, retryAfter int) {
	if st := statusOf(ctx); st != nil {
		st.mu.Lock()
		st.code, st.retry = code, retryAfter
		st.mu.Unlock()
	}
}

// declaredSHA256 is the payload hash the client signed, when it sent a real one.
func declaredSHA256(ctx context.Context) string {
	st := statusOf(ctx)
	if st == nil {
		return ""
	}
	if len(st.sha256) == 64 {
		if _, err := hex.DecodeString(st.sha256); err == nil {
			return strings.ToLower(st.sha256)
		}
	}
	return ""
}

// unavailable is the answer when OpenDrive cannot be asked right now.
func unavailable(ctx context.Context, what string) error {
	setStatus(ctx, http.StatusServiceUnavailable, 30)
	return gofakes3.ErrorMessage("ServiceUnavailable", what)
}

// s3Error turns anything else into an S3 error. Retry decisions come from the
// classification layer and nowhere else (rule 3).
func (b *s3Backend) s3Error(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var coded interface{ ErrorCode() gofakes3.ErrorCode }
	if errors.As(err, &coded) {
		return err
	}
	var ke s3KeyError
	if errors.As(err, &ke) {
		return gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument, ke.Error())
	}
	switch opendrive.ErrorKind(err) {
	case opendrive.KindKeystoreUnavailable, opendrive.KindReauthRequired, opendrive.KindTokenExpired,
		opendrive.KindCaptchaRequired, opendrive.KindUnauthorized, opendrive.KindRefreshTokenFailed:
		// Signing in again fixes this without a restart, so a client that keeps
		// retrying is doing the right thing.
		return unavailable(ctx, "The bridge is not signed in to OpenDrive. Sign in on its web page "+
			"or with `odctl login`; nothing needs restarting.")
	}
	if errors.Is(err, opendrive.ErrNoCredentials) {
		return unavailable(ctx, "The bridge is not signed in to OpenDrive yet. Sign in on its web page "+
			"or with `odctl login`.")
	}
	if opendrive.IsTemporary(err) {
		return unavailable(ctx, "OpenDrive cannot be reached right now. Try again shortly.")
	}
	msg := err.Error()
	var ae *opendrive.APIError
	if errors.As(err, &ae) {
		if d := ae.Diagnosis(); d != "" {
			msg = d
		}
	}
	b.log.Warn("an S3 request failed", slog.String("error", opendrive.RedactString(msg)))
	return gofakes3.ErrorMessage(gofakes3.ErrInternal, opendrive.RedactString(msg))
}

func isMissing(err error) bool {
	return err != nil && (errors.Is(err, opendrive.ErrNotFound) || isNotFound(err))
}

// ---------------------------------------------------------------- buckets

func bucketPath(bucket string) string { return "/" + bucketName(bucket) }

// hasUnsentUnder reports whether the gateway holds anything unsent at or below p.
func (b *s3Backend) hasUnsentUnder(p string) bool {
	if b.s.datacache == nil {
		return false
	}
	for _, o := range b.s.datacache.Unsent() {
		if o.RemotePath == p || strings.HasPrefix(o.RemotePath, p+"/") {
			return true
		}
	}
	return false
}

func (b *s3Backend) ListBuckets(ctx context.Context) ([]gofakes3.BucketInfo, error) {
	seen := map[string]bool{}
	var out []gofakes3.BucketInfo
	pager := b.s.client.Folders().Pages(opendrive.RootFolderID, opendrive.ListOptions{OnlySubfolders: true})
	for !pager.Done() {
		page, err := pager.Next(ctx)
		if err != nil {
			return nil, b.s3Error(ctx, err)
		}
		if page == nil {
			break
		}
		for _, f := range page.Folders {
			name := decodeSegment(f.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			b.known.Store(name, true)
			out = append(out, gofakes3.BucketInfo{Name: name, CreationDate: gofakes3.NewContentTime(f.DateCreated.Time)})
		}
	}
	// A bucket the gateway created a folder for only in its own cache — written
	// into during an outage — is a bucket too.
	if b.s.datacache != nil {
		_, folders := datacache.ChildrenOf(b.s.datacache.Unsent(), "/")
		for _, f := range folders {
			if name := decodeSegment(f); !seen[name] {
				seen[name] = true
				out = append(out, gofakes3.BucketInfo{Name: name, CreationDate: gofakes3.NewContentTime(time.Now())})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *s3Backend) BucketExists(ctx context.Context, name string) (bool, error) {
	bp := bucketPath(name)
	if b.hasUnsentUnder(bp) {
		return true, nil
	}
	_, err := b.s.folderIDFor(ctx, bp)
	switch {
	case err == nil:
		b.known.Store(name, true)
		return true, nil
	case isMissing(err):
		b.known.Delete(name)
		return false, nil
	case opendrive.IsTemporary(err):
		if _, ok := b.known.Load(name); ok {
			return true, nil
		}
		return false, unavailable(ctx, "OpenDrive cannot be reached, and the bridge has not seen bucket "+
			name+" since it started, so it cannot tell whether it exists. Try again shortly.")
	}
	return false, b.s3Error(ctx, err)
}

func (b *s3Backend) CreateBucket(ctx context.Context, name string) error {
	exists, err := b.BucketExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return gofakes3.ResourceError(gofakes3.ErrBucketAlreadyExists, name)
	}
	if _, err := b.s.client.Folders().Create(ctx, opendrive.CreateFolderParams{
		Name: bucketName(name), ParentID: opendrive.RootFolderID, Access: opendrive.FolderPrivate,
	}); err != nil {
		return b.s3Error(ctx, err)
	}
	b.s.invalidateSubtree(bucketPath(name))
	b.known.Store(name, true)
	return nil
}

func (b *s3Backend) DeleteBucket(ctx context.Context, name string) error {
	bp := bucketPath(name)
	if b.hasUnsentUnder(bp) {
		return gofakes3.ResourceError(gofakes3.ErrBucketNotEmpty, name)
	}
	id, err := b.s.folderIDFor(ctx, bp)
	if isMissing(err) {
		return gofakes3.BucketNotFound(name)
	}
	if err != nil {
		return b.s3Error(ctx, err)
	}
	page, err := b.s.client.Folders().List(ctx, id, opendrive.ListOptions{})
	if err != nil {
		return b.s3Error(ctx, err)
	}
	if len(page.Files) > 0 || len(page.Folders) > 0 {
		return gofakes3.ResourceError(gofakes3.ErrBucketNotEmpty, name)
	}
	if err := b.s.client.Folders().Trash(ctx, []string{id}); err != nil {
		return b.s3Error(ctx, err)
	}
	if b.cfg.PermanentDelete {
		if err := b.s.client.Folders().Remove(ctx, []string{id}); err != nil {
			return b.s3Error(ctx, err)
		}
	}
	b.s.invalidateSubtree(bp)
	b.known.Delete(name)
	return nil
}

// ---------------------------------------------------------------- listing

// listItem is one line of a listing before pagination: a key, or a common prefix.
type listItem struct {
	key     string
	prefix  bool
	size    int64
	md5     string
	modTime time.Time
}

func (b *s3Backend) ListBucket(ctx context.Context, name string, prefix *gofakes3.Prefix, page gofakes3.ListBucketPage) (*gofakes3.ObjectList, error) {
	bp := bucketPath(name)
	var pfx, delim string
	if prefix != nil {
		if prefix.HasPrefix {
			pfx = prefix.Prefix
		}
		if prefix.HasDelimiter {
			delim = prefix.Delimiter
		}
	}

	// Only the folder the prefix names is walked: everything a key with this
	// prefix could be is at or below it.
	dirKey := pfx[:strings.LastIndex(pfx, "/")+1]
	dirPath := bp
	if dirKey != "" {
		p, err := objectPath(name, dirKey)
		if err != nil {
			return gofakes3.NewObjectList(), nil // no key can have this prefix
		}
		dirPath = strings.TrimSuffix(p, "/")
	}
	oneLevel := delim == "/"

	items := map[string]listItem{}
	id, err := b.s.folderIDFor(ctx, dirPath)
	switch {
	case err == nil:
		if err := b.walk(ctx, id, dirKey, oneLevel, items); err != nil {
			return nil, b.s3Error(ctx, err)
		}
	case isMissing(err):
		// Nothing upstream; what the gateway holds may still be there.
	default:
		return nil, b.s3Error(ctx, err)
	}

	// §3.6.4 constraint 1: what the gateway accepted and has not delivered is
	// listed, replacing an older upstream copy of the same key.
	if b.s.datacache != nil {
		for _, o := range b.s.datacache.Unsent() {
			if !strings.HasPrefix(o.RemotePath, dirPath+"/") {
				continue
			}
			key := keyOf(bp, o.RemotePath)
			if oneLevel {
				rest := strings.TrimPrefix(key, dirKey)
				if i := strings.IndexByte(rest, '/'); i >= 0 {
					p := dirKey + rest[:i+1]
					items[p] = listItem{key: p, prefix: true}
					continue
				}
			}
			items[key] = listItem{key: key, size: o.Size, md5: o.Hash, modTime: o.StoredAt}
		}
	}

	// S3's own rules over what was gathered: the prefix filters, the delimiter
	// rolls keys up into common prefixes, and the result is in byte order.
	var sorted []listItem
	seenPrefix := map[string]bool{}
	for _, it := range items {
		if !strings.HasPrefix(it.key, pfx) {
			continue
		}
		if !it.prefix && delim != "" {
			if i := strings.Index(it.key[len(pfx):], delim); i >= 0 {
				p := it.key[:len(pfx)+i+len(delim)]
				if !seenPrefix[p] {
					seenPrefix[p] = true
					sorted = append(sorted, listItem{key: p, prefix: true})
				}
				continue
			}
		}
		if it.prefix {
			if delim == "" {
				continue // an empty folder is not a key
			}
			if seenPrefix[it.key] {
				continue
			}
			seenPrefix[it.key] = true
		}
		sorted = append(sorted, it)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })

	out := gofakes3.NewObjectList()
	count := int64(0)
	for _, it := range sorted {
		if page.HasMarker && it.key <= page.Marker {
			continue
		}
		if page.MaxKeys > 0 && count >= page.MaxKeys {
			out.IsTruncated = true
			break
		}
		count++
		out.NextMarker = it.key
		if it.prefix {
			out.AddPrefix(it.key)
			continue
		}
		out.Add(&gofakes3.Content{
			Key:          it.key,
			LastModified: gofakes3.NewContentTime(it.modTime),
			ETag:         `"` + strings.ToLower(it.md5) + `"`,
			Size:         it.size,
			StorageClass: gofakes3.StorageStandard,
		})
	}
	if !out.IsTruncated {
		out.NextMarker = ""
	}
	return out, nil
}

// walk adds a folder's contents to items, recursing unless oneLevel.
func (b *s3Backend) walk(ctx context.Context, folderID, keyPrefix string, oneLevel bool, items map[string]listItem) error {
	pager := b.s.client.Folders().Pages(folderID, opendrive.ListOptions{})
	for !pager.Done() {
		page, err := pager.Next(ctx)
		if err != nil {
			return err
		}
		if page == nil {
			break
		}
		for _, f := range page.Files {
			key := keyPrefix + decodeSegment(f.Name)
			items[key] = listItem{key: key, size: f.Size.Int64(), md5: f.FileHash, modTime: f.DateModified.Time}
		}
		for _, f := range page.Folders {
			sub := keyPrefix + decodeSegment(f.Name) + "/"
			if oneLevel {
				items[sub] = listItem{key: sub, prefix: true}
				continue
			}
			if err := b.walk(ctx, f.FolderID.String(), sub, false, items); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- reading

// objectMeta is what HEAD and GET need to know about an object.
type objectMeta struct {
	path    string
	fileID  string
	size    int64
	md5     string
	modTime time.Time
	cached  bool
}

// stat finds an object: in the gateway first, because an unsent object does not
// exist upstream yet (and a read-after-write that asked upstream first would be
// told so — D44), then upstream by path.
func (b *s3Backend) stat(ctx context.Context, bucket, key string) (*objectMeta, error) {
	p, err := objectPath(bucket, key)
	if err != nil {
		return nil, gofakes3.KeyNotFound(key)
	}
	if strings.HasSuffix(p, "/") {
		// A "folder marker": present when the folder is.
		if _, err := b.s.folderIDFor(ctx, strings.TrimSuffix(p, "/")); err != nil {
			if isMissing(err) {
				return nil, gofakes3.KeyNotFound(key)
			}
			return nil, b.s3Error(ctx, err)
		}
		return &objectMeta{path: p, md5: "d41d8cd98f00b204e9800998ecf8427e"}, nil
	}
	if b.s.datacache != nil {
		if o, ok := b.s.datacache.Lookup(p); ok {
			return &objectMeta{path: p, size: o.Size, md5: o.Hash, modTime: o.StoredAt, cached: true}, nil
		}
	}
	id, err := b.s.client.Files().IDByPath(ctx, p)
	if err != nil {
		if isMissing(err) {
			return nil, gofakes3.KeyNotFound(key)
		}
		return nil, b.s3Error(ctx, err)
	}
	info, err := b.s.client.Files().Info(ctx, id)
	if err != nil {
		if isMissing(err) {
			return nil, gofakes3.KeyNotFound(key)
		}
		return nil, b.s3Error(ctx, err)
	}
	if !info.DateTrashed.IsZero() {
		return nil, gofakes3.KeyNotFound(key)
	}
	return &objectMeta{path: p, fileID: id, size: info.Size.Int64(), md5: info.FileHash,
		modTime: info.DateModified.Time}, nil
}

func (m *objectMeta) object(key string) *gofakes3.Object {
	sum, _ := hex.DecodeString(m.md5)
	return &gofakes3.Object{
		Name: key,
		Size: m.size,
		Hash: sum,
		Metadata: map[string]string{
			"Last-Modified": m.modTime.UTC().Format(http.TimeFormat),
			"Content-Type":  "application/octet-stream",
		},
	}
}

func (b *s3Backend) HeadObject(ctx context.Context, bucket, key string) (*gofakes3.Object, error) {
	m, err := b.stat(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	obj := m.object(key)
	obj.Contents = io.NopCloser(strings.NewReader(""))
	return obj, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

func (b *s3Backend) GetObject(ctx context.Context, bucket, key string, rangeRequest *gofakes3.ObjectRangeRequest) (*gofakes3.Object, error) {
	m, err := b.stat(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	rng, err := rangeRequest.Range(m.size)
	if err != nil {
		return nil, err
	}
	obj := m.object(key)
	obj.Range = rng

	if strings.HasSuffix(m.path, "/") {
		obj.Contents = io.NopCloser(strings.NewReader(""))
		return obj, nil
	}

	// A small object not yet here is fetched whole, verified against the MD5
	// OpenDrive reports, and then served from this disk: the client gets an
	// exact length and bytes that have been checked, not a promise.
	if !m.cached && b.s.datacache != nil && m.size <= b.cfg.WholeFetchLimit {
		if err := b.fetchWhole(ctx, m); err == nil {
			m.cached = true
		} else {
			b.log.Debug("could not fetch an object into the cache; streaming it instead",
				slog.String("datacache_path", m.path), slog.String("error", err.Error()))
		}
	}
	if m.cached {
		rd, err := b.s.datacache.Get(m.path)
		if err == nil {
			var r io.Reader = rd
			if rng != nil {
				if _, err := rd.Seek(rng.Start, io.SeekStart); err != nil {
					_ = rd.Close()
					return nil, b.s3Error(ctx, err)
				}
				r = io.LimitReader(rd, rng.Length)
			}
			obj.Contents = readCloser{Reader: r, close: rd.Close}
			return obj, nil
		}
		if m.fileID == "" {
			// It was only here, and it has gone (delivered and evicted, or
			// deleted) between stat and read. Ask again from the top.
			m2, err := b.stat(ctx, bucket, key)
			if err != nil {
				return nil, err
			}
			m = m2
		}
	}
	if m.fileID == "" {
		return nil, gofakes3.KeyNotFound(key)
	}
	return b.stream(ctx, m, obj, rng)
}

// fetchWhole downloads an object into the cache and checks it.
func (b *s3Backend) fetchWhole(ctx context.Context, m *objectMeta) error {
	fw, err := b.s.datacache.Fill(m.path)
	if err != nil {
		return err
	}
	res, err := b.s.client.Downloads().Download(ctx, fw, opendrive.DownloadParams{
		FileID: m.fileID, Size: m.size, Hash: validMD5(m.md5),
	})
	if err != nil {
		_ = fw.Abort()
		return err
	}
	if res == nil || res.Written != m.size {
		_ = fw.Abort()
		return fmt.Errorf("download of %s stopped short", m.path)
	}
	_, err = fw.CommitClean(validMD5(m.md5))
	return err
}

func validMD5(h string) string {
	if len(h) == 32 {
		if _, err := hex.DecodeString(h); err == nil {
			return strings.ToLower(h)
		}
	}
	return ""
}

// stream sends an object straight from OpenDrive, for objects too large to
// fetch whole first. Nothing is written until the download has produced its
// first byte, so a failure is still an S3 error and not a truncated body.
func (b *s3Backend) stream(ctx context.Context, m *objectMeta, obj *gofakes3.Object, rng *gofakes3.ObjectRange) (*gofakes3.Object, error) {
	start, length := int64(0), m.size
	if rng != nil {
		start, length = rng.Start, rng.Length
	}
	dctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	params := opendrive.DownloadParams{FileID: m.fileID, Size: m.size, Offset: start}
	if start == 0 && length == m.size {
		params.Hash = validMD5(m.md5)
	}
	go func() {
		_, err := b.s.client.Downloads().Download(dctx, pw, params)
		_ = pw.CloseWithError(err)
	}()
	first := make([]byte, 1)
	if length > 0 {
		if _, err := io.ReadFull(pr, first); err != nil {
			cancel()
			_ = pr.Close()
			return nil, b.s3Error(ctx, err)
		}
	}
	r := io.LimitReader(pr, length-1)
	if length > 0 {
		r = io.MultiReader(strings.NewReader(string(first)), r)
	}
	obj.Contents = readCloser{Reader: r, close: func() error {
		cancel()
		return pr.Close()
	}}
	return obj, nil
}

// ---------------------------------------------------------------- writing

func (b *s3Backend) PutObject(ctx context.Context, bucket, key string, _ map[string]string, input io.Reader, size int64) (gofakes3.PutObjectResult, error) {
	p, err := objectPath(bucket, key)
	if err != nil {
		return gofakes3.PutObjectResult{}, b.s3Error(ctx, err)
	}
	if strings.HasSuffix(p, "/") {
		return gofakes3.PutObjectResult{}, b.putFolderMarker(ctx, key, p, input, size)
	}
	if _, err := b.put(ctx, p, input, size, declaredSHA256(ctx)); err != nil {
		return gofakes3.PutObjectResult{}, err
	}
	b.known.Store(bucket, true)
	return gofakes3.PutObjectResult{}, nil
}

// putFolderMarker handles a zero-byte "key/" object: it is a folder.
func (b *s3Backend) putFolderMarker(ctx context.Context, key, p string, input io.Reader, size int64) error {
	if size != 0 {
		return gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument,
			"a key ending in \"/\" is a folder here and cannot hold data")
	}
	_, _ = io.Copy(io.Discard, input)
	if _, err := b.s.client.Folders().EnsurePath(ctx, strings.TrimSuffix(p, "/")); err != nil {
		return b.s3Error(ctx, err)
	}
	b.s.invalidateSubtree(strings.TrimSuffix(p, "/"))
	return nil
}

// put stores size bytes from input at p through the write-back gateway, and is
// shared by PutObject, CopyObject and CompleteMultipartUpload.
//
// Exactly size bytes must arrive, and the input is read to its end so that the
// MD5 check gofakes3 runs at EOF happens (it does not otherwise). A declared
// SHA-256 is checked too: the signature covers the hash the client declared,
// and nothing else in the stack checks the body against it.
func (b *s3Backend) put(ctx context.Context, p string, input io.Reader, size int64, wantSHA256 string) (*datacache.Object, error) {
	dc := b.s.datacache
	if dc == nil || !dc.Status().WriteBack {
		return nil, gofakes3.ErrorMessage(gofakes3.ErrNotImplemented,
			"the S3 gateway needs the caching gateway with write-back switched on")
	}
	parent, name := path.Split(p)
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		parent = "/"
	}
	// The folder id is known if OpenDrive can say so; if the folder does not
	// exist yet, or OpenDrive cannot be asked, the flusher creates or resolves
	// it (§3.6.4). S3 has no folders to create first, so a missing one is
	// normal here, not an error as it is on /v1/upload.
	folderID, err := b.s.folderIDFor(ctx, parent)
	if err != nil {
		if !isMissing(err) && !opendrive.IsTemporary(err) {
			return nil, b.s3Error(ctx, err)
		}
		folderID = ""
	}

	pw, err := dc.Put(datacache.PutRequest{RemotePath: p, FolderID: folderID, Name: name, Size: size})
	if err != nil {
		return nil, b.cacheRefusal(ctx, err, size)
	}
	var w io.Writer = pw
	hasher := sha256.New()
	if wantSHA256 != "" {
		w = io.MultiWriter(pw, hasher)
	}
	n, err := io.CopyN(w, input, size)
	if err == nil {
		err = readToEOF(input)
	}
	if err != nil || n != size {
		_ = pw.Abort()
		switch {
		case errors.Is(err, datacache.ErrCacheFull):
			return nil, b.cacheRefusal(ctx, err, size)
		case gofakes3.HasErrorCode(err, gofakes3.ErrBadDigest):
			return nil, err
		}
		return nil, gofakes3.ErrIncompleteBody
	}
	if wantSHA256 != "" && hex.EncodeToString(hasher.Sum(nil)) != wantSHA256 {
		_ = pw.Abort()
		setStatus(ctx, http.StatusBadRequest, 0)
		return nil, gofakes3.ErrorMessage("XAmzContentSHA256Mismatch",
			"the body does not match the SHA-256 the request was signed with; nothing was stored")
	}
	obj, err := pw.Commit()
	if err != nil {
		// Could not be made durable, so it was not accepted. Saying otherwise is
		// the one thing this gateway must never do (§3.5.2).
		b.log.Error("an S3 write could not be stored", slog.String("datacache_path", p),
			slog.String("error", err.Error()))
		return nil, gofakes3.ErrorMessage(gofakes3.ErrInternal,
			"the bridge could not store this object durably; nothing was accepted")
	}
	b.s.invalidateSubtree(p, parent)
	return obj, nil
}

// readToEOF reads what is left of an input that should be exhausted, so that a
// hashing reader's end-of-body check runs.
func readToEOF(r io.Reader) error {
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			return gofakes3.ErrIncompleteBody
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// cacheRefusal words a write the cache could not take. Too large ever to fit is
// the client's problem and permanent; too full right now is temporary.
func (b *s3Backend) cacheRefusal(ctx context.Context, err error, size int64) error {
	if !errors.Is(err, datacache.ErrCacheFull) {
		if errors.Is(err, datacache.ErrClosed) {
			return unavailable(ctx, "The bridge is shutting down. Try again once it is back.")
		}
		return b.s3Error(ctx, err)
	}
	if max := b.s.datacache.MaxDirtyBytes(); size > max {
		setStatus(ctx, http.StatusBadRequest, 0)
		return gofakes3.ErrorMessage("EntityTooLarge", fmt.Sprintf(
			"this object is %d bytes and the bridge holds at most %d bytes that OpenDrive does not have yet "+
				"(max_dirty_bytes); raise that, or have the client use multipart uploads of smaller parts",
			size, max))
	}
	setStatus(ctx, http.StatusServiceUnavailable, 60)
	return gofakes3.ErrorMessage("SlowDown",
		"the bridge is holding as much unsent data as it is allowed to; try again once some of it has reached OpenDrive")
}

// ---------------------------------------------------------------- deleting

func (b *s3Backend) DeleteObject(ctx context.Context, bucket, key string) (gofakes3.ObjectDeleteResult, error) {
	return gofakes3.ObjectDeleteResult{}, b.deleteObject(ctx, bucket, key)
}

func (b *s3Backend) deleteObject(ctx context.Context, bucket, key string) error {
	p, err := objectPath(bucket, key)
	if err != nil {
		return nil // no such key can exist, and S3 deletes are idempotent
	}
	if strings.HasSuffix(p, "/") {
		return b.deleteFolderMarker(ctx, strings.TrimSuffix(p, "/"))
	}
	// The gateway's copy first, whatever its state, and an upload of it in
	// progress is stopped before OpenDrive's copy is deleted — otherwise the
	// upload would finish afterwards and put the object back.
	if b.s.datacache != nil {
		if _, _, err := b.s.datacache.Remove(ctx, p); err != nil {
			return b.s3Error(ctx, err)
		}
	}
	id, err := b.s.client.Files().IDByPath(ctx, p)
	if isMissing(err) {
		b.s.invalidateSubtree(p)
		return nil
	}
	if err != nil {
		return b.s3Error(ctx, err)
	}
	if err := b.s.client.Files().Trash(ctx, []string{id}); err != nil && !isMissing(err) {
		return b.s3Error(ctx, err)
	}
	if b.cfg.PermanentDelete {
		if err := b.s.client.Files().Remove(ctx, []string{id}, "", ""); err != nil && !isMissing(err) {
			return b.s3Error(ctx, err)
		}
	}
	b.s.invalidateSubtree(p)
	return nil
}

// deleteFolderMarker removes a folder only when it is empty: deleting "key/"
// in S3 removes one zero-byte object, never the keys under it.
func (b *s3Backend) deleteFolderMarker(ctx context.Context, p string) error {
	if b.hasUnsentUnder(p) {
		return nil
	}
	id, err := b.s.folderIDFor(ctx, p)
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return b.s3Error(ctx, err)
	}
	page, err := b.s.client.Folders().List(ctx, id, opendrive.ListOptions{})
	if err != nil {
		return b.s3Error(ctx, err)
	}
	if len(page.Files) > 0 || len(page.Folders) > 0 {
		return nil
	}
	if err := b.s.client.Folders().Trash(ctx, []string{id}); err != nil && !isMissing(err) {
		return b.s3Error(ctx, err)
	}
	b.s.invalidateSubtree(p)
	return nil
}

func (b *s3Backend) DeleteMulti(ctx context.Context, bucket string, keys ...string) (gofakes3.MultiDeleteResult, error) {
	var out gofakes3.MultiDeleteResult
	for _, k := range keys {
		if err := b.deleteObject(ctx, bucket, k); err != nil {
			code := gofakes3.ErrInternal
			var coded interface{ ErrorCode() gofakes3.ErrorCode }
			if errors.As(err, &coded) {
				code = coded.ErrorCode()
			}
			out.Error = append(out.Error, gofakes3.ErrorResult{Key: k, Code: code, Message: err.Error()})
			continue
		}
		out.Deleted = append(out.Deleted, gofakes3.ObjectID{Key: k})
	}
	// A partial failure is reported per key inside a 200, as S3 does; the
	// status set by a failed key must not turn the whole response into a 503.
	setStatus(ctx, 0, 0)
	return out, nil
}

// ---------------------------------------------------------------- copying

// CopyObject copies through the gateway: the bytes are read and written again,
// checked by the cache's own MD5, rather than trusting an upstream copy call
// whose success response has not been measured to mean anything (§10, "a 200
// proves nothing").
func (b *s3Backend) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, _ map[string]string) (gofakes3.CopyObjectResult, error) {
	src, err := b.GetObject(ctx, srcBucket, srcKey, nil)
	if err != nil {
		return gofakes3.CopyObjectResult{}, err
	}
	defer func() { _ = src.Contents.Close() }()
	dp, err := objectPath(dstBucket, dstKey)
	if err != nil {
		return gofakes3.CopyObjectResult{}, b.s3Error(ctx, err)
	}
	if strings.HasSuffix(dp, "/") {
		return gofakes3.CopyObjectResult{}, gofakes3.ErrorMessage(gofakes3.ErrInvalidArgument,
			"cannot copy an object onto a folder marker")
	}
	obj, err := b.put(ctx, dp, src.Contents, src.Size, "")
	if err != nil {
		return gofakes3.CopyObjectResult{}, err
	}
	b.known.Store(dstBucket, true)
	return gofakes3.CopyObjectResult{
		ETag:         `"` + obj.Hash + `"`,
		LastModified: gofakes3.NewContentTime(obj.StoredAt),
	}, nil
}
