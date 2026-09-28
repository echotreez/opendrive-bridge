package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/rclone/gofakes3"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// ---------------------------------------------------------------- buckets

func TestS3BucketsAreTopLevelFolders(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	r.fake.Mkdir("Existing Folder")
	if err := r.mc.MakeBucket(ctx, "fresh", minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	buckets, err := r.mc.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	if strings.Join(names, ",") != "Existing Folder,fresh" {
		t.Fatalf("buckets = %v", names)
	}
	if err := r.mc.MakeBucket(ctx, "fresh", minio.MakeBucketOptions{}); err == nil {
		t.Fatal("a bucket was created twice")
	}

	r.put("fresh", "k", []byte("v"))
	if err := r.mc.RemoveBucket(ctx, "fresh"); err == nil {
		t.Fatal("a bucket holding an unsent object was removed")
	}
	r.flush()
	if err := r.mc.RemoveBucket(ctx, "fresh"); err == nil {
		t.Fatal("a non-empty bucket was removed")
	}
	if err := r.mc.RemoveObject(ctx, "fresh", "k", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.mc.RemoveBucket(ctx, "fresh"); err != nil {
		t.Fatalf("an empty bucket was not removed: %v", err)
	}
	if ok, err := r.mc.BucketExists(ctx, "fresh"); err != nil || ok {
		t.Fatalf("after removal: %v %v", ok, err)
	}
	if err := r.mc.RemoveBucket(ctx, "never"); err == nil {
		t.Fatal("removing a missing bucket succeeded")
	}
}

// A bucket written into during an outage lists as a bucket before OpenDrive has
// its folder.
func TestS3ABucketHeldOnlyInTheCacheIsListed(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "held", minio.MakeBucketOptions{})
	r.fake.SetDown(true)
	r.put("held", "k", []byte("v"))
	r.fake.SetDown(false)
	// Take the folder away upstream, as if it had never been created.
	r.fake.Mkdir("other")
	names, err := r.gw.backend.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range names {
		found = found || b.Name == "held"
	}
	if !found {
		t.Fatalf("buckets = %v", names)
	}
}

// ---------------------------------------------------------------- folder markers

func TestS3FolderMarkers(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	if _, err := r.mc.PutObject(ctx, "bkt", "dir/", bytes.NewReader(nil), 0, minio.PutObjectOptions{}); err != nil {
		t.Fatalf("folder marker: %v", err)
	}
	if _, err := r.mc.StatObject(ctx, "bkt", "dir/", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("stat of a folder marker: %v", err)
	}
	if b, err := r.get("bkt", "dir/", minio.GetObjectOptions{}); err != nil || len(b) != 0 {
		t.Fatalf("get of a folder marker = %q, %v", b, err)
	}
	if _, err := r.mc.PutObject(ctx, "bkt", "bad/", bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err == nil {
		t.Fatal("a folder marker with data was accepted")
	}
	r.put("bkt", "dir/inside", []byte("x"))
	r.flush()
	// Deleting "dir/" removes the marker, never what is under it.
	if err := r.mc.RemoveObject(ctx, "bkt", "dir/", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.fake.File("bkt/dir/inside"); !ok {
		t.Fatal("deleting a folder marker deleted what was under it")
	}
	if err := r.mc.RemoveObject(ctx, "bkt", "dir/inside", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.mc.RemoveObject(ctx, "bkt", "dir/", minio.RemoveObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.mc.StatObject(ctx, "bkt", "dir/", minio.StatObjectOptions{}); err == nil {
		t.Fatal("an emptied folder marker is still there after deleting it")
	}
}

// ---------------------------------------------------------------- bulk delete, copy

func TestS3DeleteManyAtOnce(t *testing.T) {
	r := newS3Rig(t, S3Config{PermanentDelete: true})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	for _, k := range []string{"a", "b", "c"} {
		r.put("bkt", k, []byte(k))
	}
	r.flush()
	ch := make(chan minio.ObjectInfo, 4)
	for _, k := range []string{"a", "b", "c", "missing"} {
		ch <- minio.ObjectInfo{Key: k}
	}
	close(ch)
	for e := range r.mc.RemoveObjects(ctx, "bkt", ch, minio.RemoveObjectsOptions{}) {
		t.Errorf("delete %s: %v", e.ObjectName, e.Err)
	}
	if got := r.keys("bkt", "", true); len(got) != 0 {
		t.Fatalf("left after a bulk delete: %v", got)
	}
	if r.fake.Trashed("bkt/a") {
		t.Error("a permanent delete went to the trash")
	}
}

func TestS3CopyGoesThroughTheGateway(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx := context.Background()
	_ = r.mc.MakeBucket(ctx, "bkt", minio.MakeBucketOptions{})
	data := randomBytes(t, 20_000)
	r.put("bkt", "src", data)
	if _, err := r.mc.CopyObject(ctx, minio.CopyDestOptions{Bucket: "bkt", Object: "dst/copy"},
		minio.CopySrcOptions{Bucket: "bkt", Object: "src"}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	r.flush()
	if got, ok := r.fake.File("bkt/dst/copy"); !ok || !bytes.Equal(got, data) {
		t.Fatal("the copy did not reach OpenDrive intact")
	}
	if _, err := r.mc.CopyObject(ctx, minio.CopyDestOptions{Bucket: "bkt", Object: "x"},
		minio.CopySrcOptions{Bucket: "bkt", Object: "no-such"}); err == nil {
		t.Fatal("copying a missing object succeeded")
	}
}

// ---------------------------------------------------------------- keys and the API

func TestS3KeysAreMadeOnceKeptAndReset(t *testing.T) {
	ctx := context.Background()
	store, err := keystore.Open(keystore.Config{Backend: keystore.BackendEphemeral, AllowEphemeral: true})
	if err != nil {
		t.Fatal(err)
	}
	sec := store.(keystore.Secrets)
	first, created, err := LoadOrCreateS3Credentials(ctx, sec)
	if err != nil || !created || len(first.AccessKeyID) != 20 || len(first.SecretAccessKey) != 40 {
		t.Fatalf("first = %+v %v %v", first, created, err)
	}
	again, created, err := LoadOrCreateS3Credentials(ctx, sec)
	if err != nil || created || again != first {
		t.Fatalf("second load made new keys: %+v %v", again, created)
	}

	// The reset endpoint.
	r := newS3Rig(t, S3Config{})
	r.srv.store = store
	rec, body := do(t, r.srv, http.MethodPost, "/v1/s3/credentials/reset", "")
	if rec.Code != http.StatusOK || body["access_key_id"] == testS3Creds.AccessKeyID {
		t.Fatalf("reset = %d %v", rec.Code, body)
	}
	if r.gw.Credentials().AccessKeyID != body["access_key_id"] {
		t.Fatal("the gateway is not using the new key")
	}
	stored, _, _ := LoadOrCreateS3Credentials(ctx, sec)
	if stored.AccessKeyID != body["access_key_id"] {
		t.Fatal("the new key was not saved")
	}
}

func TestS3EndpointsWhenTheGatewayIsOff(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	rec, body := do(t, srv, http.MethodGet, "/v1/s3", "")
	if rec.Code != http.StatusOK || body["enabled"] != false || body["detail"] == "" {
		t.Fatalf("status = %d %v", rec.Code, body)
	}
	rec, _ = do(t, srv, http.MethodPost, "/v1/s3/credentials/reset", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset with the gateway off = %d", rec.Code)
	}
	if _, err := srv.NewS3Gateway(S3Config{StagingDir: t.TempDir()}, testS3Creds); err == nil {
		t.Fatal("a gateway was built without the caching gateway")
	}
}

// ---------------------------------------------------------------- TLS

func TestS3SelfSignedCertificateIsMadeKeptAndRenewed(t *testing.T) {
	dir := t.TempDir()
	cfg, fp, custom, err := S3TLS(dir, "", "", []string{"nas.example", "10.1.2.3"})
	if err != nil || custom || cfg == nil {
		t.Fatalf("S3TLS = %v %v", custom, err)
	}
	cert := parseCert(t, filepath.Join(dir, "s3-cert.pem"))
	for _, h := range []string{"localhost", "127.0.0.1", "nas.example", "10.1.2.3"} {
		if err := cert.VerifyHostname(h); err != nil {
			t.Errorf("the certificate does not name %s: %v", h, err)
		}
	}
	sum := sha256.Sum256(cert.Raw)
	if fp != fingerprint(sum[:]) || strings.Count(fp, ":") != 31 {
		t.Fatalf("fingerprint %s", fp)
	}
	if info, _ := os.Stat(filepath.Join(dir, "s3-key.pem")); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", info.Mode().Perm())
	}

	// Kept across starts.
	_, fp2, _, _ := S3TLS(dir, "", "", []string{"nas.example"})
	if fp2 != fp {
		t.Fatal("a still-good certificate was replaced")
	}
	// Replaced when a new name is asked for, and when it is close to expiry.
	_, fp3, _, _ := S3TLS(dir, "", "", []string{"another.example"})
	if fp3 == fp {
		t.Fatal("a certificate missing a requested name was kept")
	}
	if err := ensureSelfSigned(filepath.Join(dir, "s3-cert.pem"), filepath.Join(dir, "s3-key.pem"), nil,
		time.Now().Add(selfSignedValidity)); err != nil {
		t.Fatal(err)
	}
	if parseCert(t, filepath.Join(dir, "s3-cert.pem")).NotAfter.Before(time.Now().Add(selfSignedValidity)) {
		t.Fatal("an expiring certificate was not renewed")
	}

	// A certificate of the user's own, and the refusals around it.
	own := t.TempDir()
	if err := makeSelfSigned(filepath.Join(own, "c.pem"), filepath.Join(own, "k.pem"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, custom, err := S3TLS(dir, filepath.Join(own, "c.pem"), filepath.Join(own, "k.pem"), nil); err != nil || !custom {
		t.Fatalf("own certificate: %v %v", custom, err)
	}
	if _, _, _, err := S3TLS(dir, filepath.Join(own, "c.pem"), "", nil); err == nil {
		t.Fatal("a certificate without its key was accepted")
	}
	if _, _, _, err := S3TLS(dir, filepath.Join(own, "missing.pem"), filepath.Join(own, "k.pem"), nil); err == nil {
		t.Fatal("a missing certificate was accepted")
	}
}

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestS3ServeOverHTTPSAndRefusesPlainHTTPBeyondLoopback(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.gw.Serve(ctx, "", "0.0.0.0:0", nil, "", false); err == nil {
		t.Fatal("plain HTTP on every interface was allowed")
	}
	if err := r.gw.Serve(ctx, "127.0.0.1:0", "", nil, "", false); err == nil {
		t.Fatal("HTTPS without a certificate was allowed")
	}
	if err := r.gw.Serve(ctx, "", "", nil, "", false); err == nil {
		t.Fatal("serving on nothing was allowed")
	}

	tlsCfg, fp, _, err := S3TLS(t.TempDir(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpsAddr, httpAddr := freeAddr(t), freeAddr(t)
	done := make(chan error, 1)
	go func() { done <- r.gw.Serve(ctx, httpsAddr, httpAddr, tlsCfg, fp, false) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test checks the fingerprint itself
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = client.Get("https://" + httpsAddr + "/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned over HTTPS = %d", resp.StatusCode)
	}
	sum := sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
	if fingerprint(sum[:]) != fp {
		t.Fatal("the certificate served is not the one described")
	}
	if info := r.gw.Info(); info.HTTPSAddr != httpsAddr || info.Fingerprint != fp {
		t.Fatalf("info = %+v", info)
	}
	resp, err = http.Get("http://" + httpAddr + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

// ---------------------------------------------------------------- multipart housekeeping

func TestS3AbandonedMultipartUploadsAreRemovedAfterTheirExpiry(t *testing.T) {
	r := newS3Rig(t, S3Config{MultipartExpiry: time.Hour})
	ctx := context.Background()
	id, err := r.gw.backend.CreateMultipartUpload(ctx, "bkt", "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.gw.backend.mp.reap(time.Now())
	if _, err := os.Stat(filepath.Join(r.cfg.StagingDir, string(id))); err != nil {
		t.Fatal("a fresh upload was reaped")
	}
	r.gw.backend.mp.reap(time.Now().Add(2 * time.Hour))
	if _, err := os.Stat(filepath.Join(r.cfg.StagingDir, string(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an expired upload was kept")
	}
	// Upload ids that could not have come from here are refused, not used as paths.
	for _, bad := range []gofakes3.UploadID{"../../etc", "short", gofakes3.UploadID(strings.Repeat("z", 32))} {
		if _, err := r.gw.backend.UploadPart(ctx, "bkt", "k", bad, 1, 1, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("upload id %q was accepted", bad)
		}
	}
	// Completing with a wrong ETag, out of order, or with no parts is refused.
	id, _ = r.gw.backend.CreateMultipartUpload(ctx, "bkt", "k", nil)
	etag, err := r.gw.backend.UploadPart(ctx, "bkt", "k", id, 1, 3, bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.gw.backend.UploadPart(ctx, "bkt", "k", id, 2, 5, bytes.NewReader([]byte("ab"))); err == nil {
		t.Fatal("a short part was accepted")
	}
	for _, in := range []*gofakes3.CompleteMultipartUploadRequest{
		{},
		{Parts: []gofakes3.CompletedPart{{PartNumber: 1, ETag: `"0000"`}}},
		{Parts: []gofakes3.CompletedPart{{PartNumber: 2, ETag: etag}, {PartNumber: 1, ETag: etag}}},
		{Parts: []gofakes3.CompletedPart{{PartNumber: 1, ETag: etag}, {PartNumber: 1, ETag: etag}}},
		{Parts: []gofakes3.CompletedPart{{PartNumber: 3, ETag: etag}}},
	} {
		if _, _, err := r.gw.backend.CompleteMultipartUpload(ctx, "bkt", "k", id, in); err == nil {
			t.Errorf("completion %+v was accepted", in)
		}
	}
	if _, _, err := r.gw.backend.CompleteMultipartUpload(ctx, "other", "k", id, nil); err == nil {
		t.Error("an upload was completed under another bucket")
	}
	if err := r.gw.backend.AbortMultipartUpload(ctx, "bkt", "k", id); err != nil {
		t.Fatal(err)
	}
	if err := r.gw.backend.AbortMultipartUpload(ctx, "bkt", "k", id); err != nil {
		t.Fatalf("a second abort = %v; abort is idempotent", err)
	}
}

// ---------------------------------------------------------------- errors

func TestS3ErrorsSayWhatHappenedAndWhetherToRetry(t *testing.T) {
	r := newS3Rig(t, S3Config{})
	b := r.gw.backend
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&opendrive.APIError{Kind: opendrive.KindReauthRequired}, http.StatusServiceUnavailable, "ServiceUnavailable"},
		{opendrive.ErrNoCredentials, http.StatusServiceUnavailable, "ServiceUnavailable"},
		{&opendrive.APIError{Kind: opendrive.KindNetwork}, http.StatusServiceUnavailable, "ServiceUnavailable"},
		{&opendrive.APIError{Kind: opendrive.KindQuotaExceeded, UpstreamMsg: "full"}, 0, "InternalError"},
		{s3KeyError("bad key"), 0, "InvalidArgument"},
		{gofakes3.ErrNoSuchKey, 0, "NoSuchKey"},
		{nil, 0, ""},
	} {
		st := &s3Status{}
		ctx := context.WithValue(context.Background(), s3StatusKey{}, st)
		got := b.s3Error(ctx, tc.err)
		if tc.err == nil {
			if got != nil {
				t.Errorf("nil became %v", got)
			}
			continue
		}
		var coded interface{ ErrorCode() gofakes3.ErrorCode }
		if !errors.As(got, &coded) || string(coded.ErrorCode()) != tc.code {
			t.Errorf("%v → %v; want code %s", tc.err, got, tc.code)
		}
		if st.code != tc.status {
			t.Errorf("%v → status %d; want %d", tc.err, st.code, tc.status)
		}
	}
	// The response carries the chosen status and Retry-After.
	rec := &s3StatusWriter{ResponseWriter: httptest.NewRecorder(), st: &s3Status{code: 503, retry: 30}}
	rec.WriteHeader(http.StatusInternalServerError)
	if rec.code != 503 || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("status %d, Retry-After %q", rec.code, rec.Header().Get("Retry-After"))
	}
	rec.Flush()
	if _, err := rec.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	fakeLog{r.srv.log}.Print(gofakes3.LogErr, "an", "error")
	fakeLog{r.srv.log}.Print(gofakes3.LogWarn, "a warning")
	fakeLog{r.srv.log}.Print(gofakes3.LogInfo, "info")
}
