//go:build integration

package opendrive_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The download pipeline against the live sandbox (whitepaper §2.5, §6.2). The
// mock suite can only assert what the SDK sends; every finding that shaped this
// pipeline — the offset off-by-one of D41, the empty archive of D42 — came from
// here.

// uploadForDownload puts known content upstream and returns its id and digest.
func uploadForDownload(t *testing.T, ctx context.Context, c *opendrive.Client,
	folder, name string, content []byte) (string, string) {
	t.Helper()
	want := md5hex(content)
	res, err := c.Uploads().Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID: folder, Name: name, Size: int64(len(content)), Hash: want,
	})
	skipIfUpstreamIsRefusing(t, err, "upload the fixture to download")
	if res.File == nil {
		t.Fatal("upload returned no file metadata")
	}
	fileID := res.File.FileID.String()

	// A file is not always downloadable the instant close_file_upload returns:
	// the download endpoint has been seen answering 404 "File does not exist"
	// for a file whose upload had just been confirmed (D44). The fixtures wait
	// for it rather than the pipeline pretending a 404 means something else.
	waitUntilDownloadable(t, ctx, c, fileID)
	return fileID, want
}

// waitUntilDownloadable polls the cheap test=1 probe until upstream admits the
// file exists (D44).
func waitUntilDownloadable(t *testing.T, ctx context.Context, c *opendrive.Client, fileID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		_, err := c.Downloads().Probe(ctx, fileID)
		if err == nil {
			if attempt > 1 {
				t.Logf("the upload took %d probes to become downloadable (D44)", attempt)
			}
			return
		}
		if !errors.Is(err, opendrive.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("the uploaded file never became downloadable: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// The round trip that matters: what comes back is byte for byte what went up.
func TestSandboxDownloadRoundTrip(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(64<<10, fmt.Sprintf("rt-%d", time.Now().UnixNano()))
	fileID, want := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-dl-%d.bin", time.Now().Unix()), content)

	var buf bytes.Buffer
	res, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{
		FileID: fileID, Size: int64(len(content)), Hash: want,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("downloaded %d bytes, want %d and identical", buf.Len(), len(content))
	}
	if !res.HashVerified {
		t.Error("a whole-file download did not verify its digest")
	}
	if res.Total != int64(len(content)) {
		t.Errorf("total = %d, want %d", res.Total, len(content))
	}
	t.Logf("round trip ok: %d bytes, md5 %s, name %q", res.Written, res.Hash, res.Name)
}

// Interrupt a download and resume it: the file on disk must end up complete and
// correct, whichever way upstream answers the range.
func TestSandboxDownloadResumesAfterAnInterruption(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(96<<10, fmt.Sprintf("resume-%d", time.Now().UnixNano()))
	fileID, want := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-resume-%d.bin", time.Now().Unix()), content)

	path := filepath.Join(t.TempDir(), "partial.bin")

	// First attempt: stop part way, exactly as a dropped connection would.
	cut := int64(40 << 10)
	cutCtx, cancel := context.WithCancel(ctx)
	f, err := os.Create(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Downloads().Download(cutCtx, f, opendrive.DownloadParams{
		FileID: fileID, Size: int64(len(content)),
		Progress: func(written, _ int64) {
			if written >= cut {
				cancel()
			}
		},
	})
	_ = f.Close()
	cancel()
	if err == nil {
		t.Skip("the whole file arrived before the interruption could take effect")
	}
	partial, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("interrupted after %d of %d bytes (%v)", partial.Size(), len(content), err)
	if partial.Size() == 0 || partial.Size() >= int64(len(content)) {
		t.Skipf("the interruption left %d bytes; nothing to resume", partial.Size())
	}

	// Second attempt: resume from what is on disk.
	res, err := c.Downloads().DownloadFile(ctx, path, opendrive.DownloadParams{
		FileID: fileID, Size: int64(len(content)), Hash: want,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	t.Logf("resumed from %d, upstream honoured the range: %v", res.Offset, res.Resumed)

	got, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("resumed file is %d bytes and does not match the original (%d)", len(got), len(content))
	}
	if md5hex(got) != want {
		t.Errorf("resumed file hashes to %s, want %s", md5hex(got), want)
	}
}

// D41: the offset parameter is off by one at the final byte, and upstream
// answers 200 with the whole file rather than the one-byte tail. The Range
// header is correct there. This test is the tripwire: if upstream ever fixes the
// parameter, or breaks the header, we hear about it.
func TestSandboxDownloadOffsetBoundary(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(9000, fmt.Sprintf("edge-%d", time.Now().UnixNano()))
	fileID, _ := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-edge-%d.bin", time.Now().Unix()), content)

	for _, off := range []int64{1, int64(len(content)) / 2, int64(len(content)) - 2, int64(len(content)) - 1} {
		var buf bytes.Buffer
		res, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{
			FileID: fileID, Offset: off, Size: int64(len(content)),
		})
		if err != nil {
			t.Fatalf("offset %d: %v", off, err)
		}
		// Whatever upstream does, the caller gets the right bytes.
		if !bytes.Equal(buf.Bytes(), content[off:]) {
			t.Errorf("offset %d: got %d bytes, want the %d byte tail",
				off, buf.Len(), int64(len(content))-off)
		}
		t.Logf("offset %-5d resumed=%-5v written=%d", off, res.Resumed, res.Written)
	}
}

// test=1 is a cheap pre-flight, and the one place an exhausted allowance can be
// seen before committing to a transfer.
func TestSandboxDownloadProbe(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(2048, fmt.Sprintf("probe-%d", time.Now().UnixNano()))
	fileID, _ := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-probe-%d.bin", time.Now().Unix()), content)

	probe, err := c.Downloads().Probe(ctx, fileID)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !probe.Result.Bool() {
		t.Errorf("probe says the file cannot be downloaded: %+v", probe)
	}
	if probe.BWExceeded.Bool() {
		t.Error("the account's bandwidth allowance is exhausted; other download tests will be unreliable")
	}
}

// T7, for files: unlike folder/info.json, the download endpoint does report a
// permanently deleted file as gone, and says so in JSON rather than by handing
// back stale bytes.
func TestSandboxDownloadOfADeletedFile(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(1024, fmt.Sprintf("gone-%d", time.Now().UnixNano()))
	fileID, _ := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-gone-%d.bin", time.Now().Unix()), content)

	if err := c.Files().DeleteTrashed(ctx, fileID, "", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The deletion is usually visible to the download endpoint at once, but not
	// always: one run served the content after a successful delete. So this
	// measures how long it takes rather than assuming it is instant — the
	// assertion that matters is that it does become unavailable, and that the
	// refusal classifies as not_found rather than as something a job engine
	// would retry.
	deadline := time.Now().Add(15 * time.Second)
	var (
		lastErr  error
		buf      bytes.Buffer
		attempts int
	)
	for {
		buf.Reset()
		attempts++
		_, lastErr = c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{FileID: fileID})
		if lastErr != nil || time.Now().After(deadline) {
			break
		}
		t.Logf("attempt %d still served %d bytes for a deleted file", attempts, buf.Len())
		time.Sleep(time.Second)
	}
	if lastErr == nil {
		t.Fatalf("a permanently deleted file was still downloadable after %d attempts over 15s",
			attempts)
	}
	if attempts > 1 {
		t.Logf("the deletion took %d attempts to become visible to the download endpoint", attempts)
	}
	if !errors.Is(lastErr, opendrive.ErrNotFound) {
		t.Errorf("kind = %q, want not_found: %v", opendrive.ErrorKind(lastErr), lastErr)
	}
	if opendrive.IsTemporary(lastErr) {
		t.Error("a deleted file was reported as worth retrying")
	}

	// The probe must agree, so a job engine never opens a transfer for it.
	if _, err := c.Downloads().Probe(ctx, fileID); !errors.Is(err, opendrive.ErrNotFound) {
		t.Errorf("probe kind = %q, want not_found: %v", opendrive.ErrorKind(err), err)
	}
}

// D42: download/all.json produces a real archive for folders — and answers a
// files list with a structurally valid, entirely empty one. The binding offers
// only folders; this test holds upstream to that.
func TestSandboxDownloadArchive(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	names := []string{"one.txt", "two.txt"}
	for i, n := range names {
		uploadForDownload(t, ctx, c, scratch, n,
			uploadPayload(512+i*32, fmt.Sprintf("arch-%d-%d", i, time.Now().UnixNano())))
	}

	var buf bytes.Buffer
	n, err := c.Downloads().Archive(ctx, &buf, []string{scratch})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("reported %d bytes, buffered %d", n, buf.Len())
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("the archive does not parse as a zip (%d bytes): %v", buf.Len(), err)
	}
	var got []string
	for _, f := range zr.File {
		got = append(got, filepath.Base(f.Name))
	}
	t.Logf("archive holds %v", got)
	for _, want := range names {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from the archive; it holds %v", want, got)
		}
	}
	if len(zr.File) == 0 {
		t.Error("the archive is empty — this is the D42 failure mode, now on folders too")
	}
}

// D32, settled. The password gates the public web page only; the owner is never
// asked, and verifypassword never validates anything. If any of that changes,
// this test says so, because P4's /v1/download/stream depends on it.
func TestSandboxPasswordDoesNotGateTheOwner(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	// Not a secret: a password this test puts on a file it just created and
	// deletes, on a sandbox account, to prove the owner is never asked for it.
	const password = "not-a-secret-file-password"
	content := uploadPayload(512, fmt.Sprintf("pw-%d", time.Now().UnixNano()))
	fileID, want := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-pw-%d.bin", time.Now().Unix()), content)

	pw := password
	if err := c.Files().UpdateSettings(ctx, fileID, opendrive.FileSettings{Password: &pw}); err != nil {
		t.Fatalf("set password: %v", err)
	}
	info, err := c.Files().Info(ctx, fileID)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Password == "" {
		t.Fatal("the password did not take; the parameter is file_password, not password")
	}
	t.Logf("password is set and masked as %q", info.Password)

	// The owner downloads it without ever being asked.
	var buf bytes.Buffer
	res, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{
		FileID: fileID, Hash: want,
	})
	if err != nil {
		t.Fatalf("the owner was refused their own password-protected file: %v", err)
	}
	if !res.HashVerified || !bytes.Equal(buf.Bytes(), content) {
		t.Error("the owner's download of a password-protected file did not round trip")
	}

	// verifypassword still refuses the correct password, so the documented
	// verify → TempKey → download flow remains unavailable.
	verified, err := c.Files().VerifyPassword(ctx, fileID, password, "")
	if err != nil {
		t.Logf("verifypassword errored: %v", err)
		return
	}
	if verified.Result.Bool() {
		t.Errorf("verifypassword now accepts the correct password (%+v) — D32 has changed "+
			"upstream and the P4 password story should be revisited", verified)
	} else {
		t.Logf("verifypassword still answers false for the correct password, as D32 records")
	}
}

// The bandwidth verdict is never retried. There is no way to exhaust the
// allowance on demand, so this asserts the wiring rather than the condition:
// were a transfer to meet it, it must stop rather than hammer.
func TestSandboxDownloadBandwidthVerdictIsFinal(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(256, fmt.Sprintf("bw-%d", time.Now().UnixNano()))
	fileID, _ := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-bw-%d.bin", time.Now().Unix()), content)

	info, err := c.Files().Info(ctx, fileID)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.BWExceeded.Bool() {
		var buf bytes.Buffer
		_, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{FileID: fileID})
		if !errors.Is(err, opendrive.ErrBandwidthExceeded) {
			t.Errorf("BWExceeded is set upstream but the download reported %v", err)
		}
		if opendrive.IsTemporary(err) {
			t.Error("bandwidth exhaustion was reported as retryable")
		}
		return
	}
	t.Logf("BWExceeded is not set for this account, so only the wiring is asserted")
	if !strings.Contains(fmt.Sprint(opendrive.ErrBandwidthExceeded), "bandwidth_exceeded") {
		t.Error("the bandwidth sentinel no longer reports its kind")
	}
}

// Downloading straight to a path is the shape the job engine will use.
func TestSandboxDownloadToFile(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(32<<10, fmt.Sprintf("tofile-%d", time.Now().UnixNano()))
	fileID, want := uploadForDownload(t, ctx, c, scratch,
		fmt.Sprintf("odb-tofile-%d.bin", time.Now().Unix()), content)

	path := filepath.Join(t.TempDir(), "out.bin")
	res, err := c.Downloads().DownloadFile(ctx, path, opendrive.DownloadParams{
		FileID: fileID, Size: int64(len(content)), Hash: want,
	})
	if err != nil {
		t.Fatalf("download to file: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("file is %d bytes, want %d", len(got), len(content))
	}

	// Downloading again over a complete file must cost nothing.
	res2, err := c.Downloads().DownloadFile(ctx, path, opendrive.DownloadParams{
		FileID: fileID, Size: int64(len(content)),
	})
	if err != nil {
		t.Fatalf("second download: %v", err)
	}
	if res2.Written != 0 {
		t.Errorf("re-downloaded %d bytes over a complete file", res2.Written)
	}
	_ = res
	_ = io.Discard
}
