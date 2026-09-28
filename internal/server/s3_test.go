package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/signer"

	"github.com/echotreez/opendrive-bridge/internal/cache"
	"github.com/echotreez/opendrive-bridge/internal/datacache"
	"github.com/echotreez/opendrive-bridge/internal/odfake"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// These tests drive the S3 gateway with minio-go — the S3 library restic uses —
// against the whole stack below it: gofakes3, the backend, the write-back cache
// with its real flusher, the SDK, and odfake, an OpenDrive that keeps what it is
// given. Nothing in between is a double.

var testS3Creds = S3Credentials{AccessKeyID: "ODBTESTKEY0000000000", SecretAccessKey: "test-secret-test-secret-test-secret-0000"}

type s3Rig struct {
	t     *testing.T
	fake  *odfake.Server
	srv   *Server
	dc    *datacache.DataCache
	gw    *S3Gateway
	http  *httptest.Server
	mc    *minio.Client
	cfg   S3Config
	cache opendrive.PathCache
}

func newS3Rig(t *testing.T, cfg S3Config) *s3Rig {
	t.Helper()
	fake := odfake.New()
	up := httptest.NewServer(fake.Handler())
	t.Cleanup(up.Close)

	pc := cache.NewPathCache()
	c, err := opendrive.New(
		opendrive.WithBaseURL(up.URL+"/api/v1"),
		opendrive.WithHTTPClient(up.Client()),
		opendrive.WithAuthenticator(stubAuth{}),
		opendrive.WithAccessProbe(nil),
		opendrive.WithPathCache(pc),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 0}),
	)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := datacache.Open(datacache.Config{
		Dir:           filepath.Join(t.TempDir(), "cache"),
		WriteBack:     true,
		MaxBytes:      256 << 20,
		MaxDirtyBytes: 64 << 20,
		FlushWorkers:  2,
		Upstream: datacache.NewSDKUploader(c).WithFolderResolver(
			func(ctx context.Context, remotePath string) (string, error) {
				parent, _ := opendrive.ParentPath(remotePath)
				return c.Folders().EnsurePath(ctx, parent)
			}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })

	srv, err := New(Config{Addr: "127.0.0.1:0"},
		&fakeAuth{state: opendrive.StateAuthenticated,
			identity: opendrive.Identity{Username: "d@example.com", AuthMode: opendrive.AuthModeOAuth2}},
		WithClient(c), WithPathCache(pc), WithDataCache(dc))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StagingDir == "" {
		cfg.StagingDir = filepath.Join(t.TempDir(), "multipart")
	}
	gw, err := srv.NewS3Gateway(cfg, testS3Creds)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(gw.Handler())
	t.Cleanup(hs.Close)

	return &s3Rig{t: t, fake: fake, srv: srv, dc: dc, gw: gw, http: hs, mc: minioFor(t, hs.URL, testS3Creds), cfg: cfg, cache: pc}
}

func minioFor(t *testing.T, endpoint string, c S3Credentials) *minio.Client {
	t.Helper()
	u, _ := url.Parse(endpoint)
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(c.AccessKeyID, c.SecretAccessKey, ""),
		Secure:       false,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mc
}

func (r *s3Rig) flush() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.dc.Flush(ctx, "", true); err != nil {
		r.t.Fatalf("flush: %v", err)
	}
}

func (r *s3Rig) put(bucket, key string, data []byte) {
	r.t.Helper()
	if _, err := r.mc.PutObject(context.Background(), bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{}); err != nil {
		r.t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

func (r *s3Rig) get(bucket, key string, opts minio.GetObjectOptions) ([]byte, error) {
	obj, err := r.mc.GetObject(context.Background(), bucket, key, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = obj.Close() }()
	return io.ReadAll(obj)
}

func (r *s3Rig) keys(bucket, prefix string, recursive bool) []string {
	r.t.Helper()
	var out []string
	for o := range r.mc.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: recursive}) {
		if o.Err != nil {
			r.t.Fatalf("list %s/%s: %v", bucket, prefix, o.Err)
		}
		out = append(out, o.Key)
	}
	return out
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// ---------------------------------------------------------------- the basics

func TestS3WriteListReadAndDeliver(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	if err := r.mc.MakeBucket(ctx, "backups", minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	data := randomBytes(t, 300_000)
	r.put("backups", "data/00/pack1", data)
	r.put("backups", "config", []byte("repo config"))

	// Before delivery: listed, readable, described — from the gateway alone.
	if got := r.keys("backups", "", true); strings.Join(got, ",") != "config,data/00/pack1" {
		t.Fatalf("recursive listing = %v", got)
	}
	if got := r.keys("backups", "", false); strings.Join(got, ",") != "config,data/" {
		t.Fatalf("top-level listing = %v", got)
	}
	st, err := r.mc.StatObject(ctx, "backups", "data/00/pack1", minio.StatObjectOptions{})
	if err != nil || st.Size != int64(len(data)) {
		t.Fatalf("stat = %+v, %v", st, err)
	}
	if got, err := r.get("backups", "data/00/pack1", minio.GetObjectOptions{}); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back before delivery: %v", err)
	}

	// Delivered, folders created on the way, byte for byte.
	r.flush()
	if got, ok := r.fake.File("backups/data/00/pack1"); !ok || !bytes.Equal(got, data) {
		t.Fatal("OpenDrive does not hold the object after a flush")
	}

	// And read from OpenDrive once the gateway no longer has it.
	if err := r.dc.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, err := r.get("backups", "data/00/pack1", minio.GetObjectOptions{}); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read from OpenDrive: %v", err)
	}
	if got := r.keys("backups", "data/", true); strings.Join(got, ",") != "data/00/pack1" {
		t.Fatalf("listing from OpenDrive = %v", got)
	}
}

func TestS3RangedReads(t *testing.T) {
	// A tiny whole-fetch limit sends the larger object down the streaming path.
	r := newS3Rig(t, S3Config{WholeFetchLimit: 1000})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	small := randomBytes(t, 900)
	big := randomBytes(t, 50_000)
	r.put("bkt", "small", small)
	r.put("bkt", "big", big)

	check := func(stage string) {
		for name, data := range map[string][]byte{"small": small, "big": big} {
			for _, rng := range [][2]int64{{0, 9}, {100, 199}, {int64(len(data)) - 10, int64(len(data)) - 1}} {
				opts := minio.GetObjectOptions{}
				if err := opts.SetRange(rng[0], rng[1]); err != nil {
					t.Fatal(err)
				}
				got, err := r.get("bkt", name, opts)
				if err != nil {
					t.Fatalf("%s: %s %v: %v", stage, name, rng, err)
				}
				if !bytes.Equal(got, data[rng[0]:rng[1]+1]) {
					t.Fatalf("%s: %s %v returned the wrong bytes", stage, name, rng)
				}
			}
		}
	}
	check("from the cache")
	r.flush()
	if err := r.dc.Clear(); err != nil {
		t.Fatal(err)
	}
	check("from OpenDrive")
}

func TestS3MultipartUploadIsAssembledAndDelivered(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	data := randomBytes(t, 12<<20)
	if _, err := r.mc.PutObject(ctx, "bkt", "large/file.bin", bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{PartSize: 5 << 20}); err != nil {
		t.Fatalf("multipart put: %v", err)
	}
	got, err := r.get("bkt", "large/file.bin", minio.GetObjectOptions{})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %v", err)
	}
	r.flush()
	if up, ok := r.fake.File("bkt/large/file.bin"); !ok || !bytes.Equal(up, data) {
		t.Fatal("the assembled object did not reach OpenDrive intact")
	}
	// The staging area is emptied once the object is stored.
	if entries, _ := filepath.Glob(filepath.Join(r.cfg.StagingDir, "*")); len(entries) != 0 {
		t.Errorf("staging still holds %v", entries)
	}
}

func TestS3AbortedMultipartLeavesNothing(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	core := minio.Core{Client: r.mc}
	id, err := core.NewMultipartUpload(ctx, "bkt", "k", minio.PutObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part := randomBytes(t, 5<<20)
	if _, err := core.PutObjectPart(ctx, "bkt", "k", id, 1, bytes.NewReader(part), int64(len(part)), minio.PutObjectPartOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := core.AbortMultipartUpload(ctx, "bkt", "k", id); err != nil {
		t.Fatal(err)
	}
	if entries, _ := filepath.Glob(filepath.Join(r.cfg.StagingDir, "*")); len(entries) != 0 {
		t.Errorf("staging still holds %v", entries)
	}
	if got := r.keys("bkt", "", true); len(got) != 0 {
		t.Errorf("an aborted upload is listed: %v", got)
	}
}

// Listings page at 100 upstream (§2.6 #14) and at whatever the client asks
// for here; neither may lose or repeat a key.
func TestS3ListingsPageWithoutLosingKeys(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	var want []string
	for i := 0; i < 230; i++ {
		k := fmt.Sprintf("dir/obj-%03d", i)
		r.fake.PutFile("bkt/"+k, []byte{byte(i)})
		want = append(want, k)
	}
	r.put("bkt", "dir/pending", []byte("not delivered yet"))
	want = append(want, "dir/pending")
	sort.Strings(want)

	var got []string
	for o := range r.mc.ListObjects(ctx, "bkt", minio.ListObjectsOptions{Prefix: "dir/", Recursive: true, MaxKeys: 37}) {
		if o.Err != nil {
			t.Fatal(o.Err)
		}
		got = append(got, o.Key)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged listing lost or repeated keys: got %d, want %d", len(got), len(want))
	}
}

// ---------------------------------------------------------------- deleting

func TestS3DeleteGoesToTheTrashAndStopsAPendingUpload(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	r.put("bkt", "delivered", []byte("one"))
	r.flush()

	r.fake.SetDown(true) // keep the next write unsent
	r.put("bkt", "pending", []byte("two"))
	r.fake.SetDown(false)

	for _, k := range []string{"delivered", "pending", "never-existed"} {
		if err := r.mc.RemoveObject(ctx, "bkt", k, minio.RemoveObjectOptions{}); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if got := r.keys("bkt", "", true); len(got) != 0 {
		t.Fatalf("still listed after delete: %v", got)
	}
	if !r.fake.Trashed("bkt/delivered") {
		t.Error("the delivered object was not moved to OpenDrive's trash")
	}
	r.flush()
	if _, ok := r.fake.File("bkt/pending"); ok {
		t.Error("a deleted object was uploaded afterwards")
	}
}

// ---------------------------------------------------------------- outage

func TestS3WritesDuringAnOutageAreHeldAndDelivered(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})

	r.fake.SetDown(true)
	data := randomBytes(t, 40_000)
	r.put("bkt", "snapshots/new", data) // a folder OpenDrive has never seen
	// Reading it back works from the gateway alone.
	if got, err := r.get("bkt", "snapshots/new", minio.GetObjectOptions{}); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back during the outage: %v", err)
	}
	// A listing is refused rather than answered from the gateway alone: it
	// would leave out everything OpenDrive already has, and a backup program
	// told its packs are gone draws the wrong conclusion. A 503 it retries.
	for o := range r.mc.ListObjects(ctx, "bkt", minio.ListObjectsOptions{Prefix: "snapshots/", Recursive: true}) {
		var er minio.ErrorResponse
		if o.Err == nil || !errors.As(o.Err, &er) || er.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("listing during the outage = %v, %v; want a 503", o.Key, o.Err)
		}
	}

	// A bucket nobody has seen since start cannot be vouched for.
	_, err := r.mc.PutObject(ctx, "unknown", "k", bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{})
	var er minio.ErrorResponse
	if !errors.As(err, &er) || er.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a write to an unseen bucket during an outage = %v; want a 503", err)
	}

	r.fake.SetDown(false)
	r.flush()
	if got, ok := r.fake.File("bkt/snapshots/new"); !ok || !bytes.Equal(got, data) {
		t.Fatal("the write held during the outage was not delivered")
	}
}

// ---------------------------------------------------------------- keys

func TestS3KeysOpenDriveCannotHoldRoundTrip(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	keys := []string{
		`a:b*c?d"e<f>g|h\i`,
		" leading and trailing ",
		"dir with ‛ quote/．/file",
		"tab\there",
		"日本語/ファイル",
	}
	for _, k := range keys {
		r.put("bkt", k, []byte(k))
	}
	r.flush()
	if err := r.dc.Clear(); err != nil {
		t.Fatal(err)
	}
	got := r.keys("bkt", "", true)
	sort.Strings(got)
	want := append([]string(nil), keys...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("keys did not survive the trip:\n got %q\nwant %q", got, want)
	}
	for _, k := range keys {
		if b, err := r.get("bkt", k, minio.GetObjectOptions{}); err != nil || string(b) != k {
			t.Errorf("%q read back as %q, %v", k, b, err)
		}
	}
	if _, ok := r.fake.File("bkt/a：b＊c？d＂e＜f＞g｜h＼i"); !ok {
		t.Error("the stand-in characters are not what OpenDrive was given")
	}
}

// ---------------------------------------------------------------- security

func TestS3RefusesAnythingNotSignedWithTheKey(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	_ = r.mc.MakeBucket(context.Background(), "bkt", minio.MakeBucketOptions{})
	r.put("bkt", "secret", []byte("contents"))

	// No signature at all.
	resp, err := http.Get(r.http.URL + "/bkt/secret")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte("contents")) {
		t.Fatalf("unsigned GET = %d %q", resp.StatusCode, body)
	}

	// The right key id with the wrong secret, and an unknown key id.
	for _, c := range []S3Credentials{
		{AccessKeyID: testS3Creds.AccessKeyID, SecretAccessKey: "wrong"},
		{AccessKeyID: "SOMEONEELSE", SecretAccessKey: testS3Creds.SecretAccessKey},
	} {
		if _, err := minioFor(t, r.http.URL, c).StatObject(context.Background(), "bkt", "secret", minio.StatObjectOptions{}); err == nil {
			t.Errorf("credentials %s/%s were accepted", c.AccessKeyID, c.SecretAccessKey)
		}
	}

	// A presigned URL, correctly signed, is still refused.
	u, err := r.mc.PresignedGetObject(context.Background(), "bkt", "secret", time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("presigned GET = %d; want 403", resp.StatusCode)
	}

	// After a reset the old key stops working and the new one works.
	r.gw.SetCredentials(S3Credentials{AccessKeyID: "ODBNEWKEY", SecretAccessKey: "new-secret"})
	if _, err := r.mc.StatObject(context.Background(), "bkt", "secret", minio.StatObjectOptions{}); err == nil {
		t.Error("the old key still works after a reset")
	}
	if _, err := minioFor(t, r.http.URL, S3Credentials{AccessKeyID: "ODBNEWKEY", SecretAccessKey: "new-secret"}).
		StatObject(context.Background(), "bkt", "secret", minio.StatObjectOptions{}); err != nil {
		t.Errorf("the new key does not work: %v", err)
	}
}

// A request signed over a SHA-256 the body does not have is refused, and
// nothing is stored. The signature covers the declared hash; before this check
// nothing compared the body with it.
func TestS3RefusesABodyThatDoesNotMatchItsSignedHash(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	_ = r.mc.MakeBucket(context.Background(), "bkt", minio.MakeBucketOptions{})
	body := []byte("the real body")
	req, _ := http.NewRequest(http.MethodPut, r.http.URL+"/bkt/tampered", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("X-Amz-Content-Sha256", strings.Repeat("ab", 32))
	req = signer.SignV4(*req, testS3Creds.AccessKeyID, testS3Creds.SecretAccessKey, "", "us-east-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(msg, []byte("SHA256")) {
		t.Fatalf("tampered body = %d %s", resp.StatusCode, msg)
	}
	if r.dc.Has("/bkt/tampered") {
		t.Fatal("a body that failed its hash was stored")
	}
}

// A body that ends early is never an object: gofakes3 does not check a
// streamed body's length, so the backend does.
func TestS3AShortBodyIsNotStored(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	_, err := r.gw.backend.put(context.Background(), "/bkt/short", bytes.NewReader([]byte("abc")), 10, "")
	if err == nil {
		t.Fatal("a 3-byte body was accepted as a 10-byte object")
	}
	if r.dc.Has("/bkt/short") {
		t.Fatal("the short body was stored")
	}
}

// An object too large ever to fit the unsent allowance is refused as the
// client's problem, in words, not as a retryable condition it would retry for
// ever.
func TestS3AnObjectLargerThanTheAllowanceIsRefusedPermanently(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	_ = r.mc.MakeBucket(context.Background(), "bkt", minio.MakeBucketOptions{})
	big := make([]byte, 65<<20)
	_, err := r.mc.PutObject(context.Background(), "bkt", "huge", bytes.NewReader(big), int64(len(big)),
		minio.PutObjectOptions{DisableMultipart: true})
	var er minio.ErrorResponse
	if !errors.As(err, &er) || er.StatusCode != http.StatusBadRequest || er.Code != "EntityTooLarge" {
		t.Fatalf("an object over the allowance = %v; want 400 EntityTooLarge", err)
	}
}

func TestS3GatewayRefusesToRunWithoutAKey(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	if _, err := r.srv.NewS3Gateway(S3Config{StagingDir: t.TempDir()}, S3Credentials{}); err == nil {
		t.Fatal("a gateway with no key was created; gofakes3 would accept every request")
	}
}

func TestS3StatusAndResetEndpoints(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	rec, body := do(t, r.srv, http.MethodGet, "/v1/s3", "")
	if rec.Code != http.StatusOK || body["enabled"] != true || body["access_key_id"] != testS3Creds.AccessKeyID {
		t.Fatalf("status = %d %v", rec.Code, body)
	}
}
