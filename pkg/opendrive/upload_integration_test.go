//go:build integration

package opendrive_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrors the code under test
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The upload pipeline against the live sandbox (whitepaper §2.4, §6.2). These
// are the tests the three branches actually depend on: the mock suite can only
// assert what the SDK sends, and every upload finding so far — TotalWritten
// counting per chunk (D35), the dedupe close refusing temp_location (D36) —
// came from here.

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol requirement
	return hex.EncodeToString(sum[:])
}

// uploadPayload builds deterministic, compressible-but-not-trivial content of
// exactly n bytes.
func uploadPayload(n int, seed string) []byte {
	block := []byte("opendrive-bridge upload payload " + seed + " ")
	out := make([]byte, 0, n+len(block))
	for len(out) < n {
		out = append(out, block...)
	}
	return out[:n]
}

// findUploaded looks for a file in its parent listing. D27 forbids using Info
// as an existence check, so every assertion here goes through the listing.
func findUploaded(t *testing.T, ctx context.Context, c *opendrive.Client, folder, fileID string) *opendrive.FileEntry {
	t.Helper()
	listing, err := c.Folders().List(ctx, folder, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list %s: %v", folder, err)
	}
	for i := range listing.Files {
		if listing.Files[i].FileID.String() == fileID {
			return &listing.Files[i]
		}
	}
	return nil
}

// TestSandboxUploadSmallFile is the plain path: one chunk, no compression, no
// dedupe, and the hash upstream reports must equal the local one.
func TestSandboxUploadSmallFile(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(4096, fmt.Sprintf("small-%d", time.Now().UnixNano()))
	want := md5hex(content)

	res, err := c.Uploads().Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID: scratch,
		Name:     fmt.Sprintf("odb-small-%d.bin", time.Now().Unix()),
		Size:     int64(len(content)),
		Hash:     want,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	t.Logf("uploaded %d bytes in %d chunk(s), dedupe=%v", res.BytesSent, res.Chunks, res.Deduplicated)

	if res.File == nil {
		t.Fatal("close_file_upload returned no file")
	}
	// The whole point of the exercise: what came back is what went up.
	if res.File.FileHash != want {
		t.Errorf("upstream FileHash = %q, local MD5 = %q", res.File.FileHash, want)
	}
	if res.File.Size.Int64() != int64(len(content)) {
		t.Errorf("upstream size = %d, want %d", res.File.Size.Int64(), len(content))
	}

	entry := findUploaded(t, ctx, c, scratch, res.File.FileID.String())
	if entry == nil {
		t.Fatal("the uploaded file is absent from its parent listing")
	}
	if entry.FileHash != want {
		t.Errorf("listing reports hash %q, want %q", entry.FileHash, want)
	}
}

// TestSandboxUploadCrossesChunkBoundary uses the smallest legal chunk so the
// loop runs several times, with a size that is deliberately not a multiple of
// it (§6.2 asks for at least 2×chunk+1).
func TestSandboxUploadCrossesChunkBoundary(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	const chunk = opendrive.MinChunkSize
	size := 2*chunk + 1
	content := uploadPayload(size, fmt.Sprintf("multi-%d", time.Now().UnixNano()))
	want := md5hex(content)

	var lastSent int64
	res, err := c.Uploads().Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID:  scratch,
		Name:      fmt.Sprintf("odb-multi-%d.bin", time.Now().Unix()),
		Size:      int64(size),
		Hash:      want,
		ChunkSize: chunk,
		Progress: func(sent, total int64) {
			if sent < lastSent {
				t.Errorf("progress went backwards: %d after %d", sent, lastSent)
			}
			lastSent = sent
		},
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Chunks != 3 {
		t.Errorf("chunks = %d, want 3 for %d bytes at %d per chunk", res.Chunks, size, chunk)
	}
	if lastSent != int64(size) {
		t.Errorf("progress stopped at %d of %d", lastSent, size)
	}
	if res.File.FileHash != want {
		t.Errorf("upstream FileHash = %q, local MD5 = %q", res.File.FileHash, want)
	}
	if res.File.Size.Int64() != int64(size) {
		t.Errorf("upstream size = %d, want %d", res.File.Size.Int64(), size)
	}
	if findUploaded(t, ctx, c, scratch, res.File.FileID.String()) == nil {
		t.Error("the multi-chunk upload is absent from its parent listing")
	}
}

// TestSandboxUploadDeduplicates is the §2.4 fast path: the same content under a
// new name must transfer nothing at all.
func TestSandboxUploadDeduplicates(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)
	uploads := c.Uploads()

	content := uploadPayload(8192, fmt.Sprintf("dedupe-%d", time.Now().UnixNano()))
	hash := md5hex(content)
	stamp := time.Now().Unix()

	first, err := uploads.Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID: scratch, Name: fmt.Sprintf("odb-dedupe-a-%d.bin", stamp),
		Size: int64(len(content)), Hash: hash,
	})
	skipIfUpstreamIsRefusing(t, err, "first upload")
	if first.Deduplicated {
		t.Log("the content was already on the server from an earlier run")
	}

	// Now upstream holds the content, so the probe should say so.
	exists, err := uploads.HasDedupeReference(ctx, int64(len(content)), hash)
	if err != nil {
		t.Errorf("has_ddref: %v", err)
	} else if !exists {
		t.Error("has_ddref does not know about content that was just uploaded")
	}

	second, err := uploads.Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID: scratch, Name: fmt.Sprintf("odb-dedupe-b-%d.bin", stamp),
		Size: int64(len(content)), Hash: hash,
	})
	if err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if !second.Deduplicated {
		t.Fatalf("the second upload of identical content transferred %d bytes in %d chunks; "+
			"the RequireHashOnly branch did not trigger", second.BytesSent, second.Chunks)
	}
	if second.BytesSent != 0 || second.Chunks != 0 {
		t.Errorf("a dedupe hit still sent %d bytes in %d chunks", second.BytesSent, second.Chunks)
	}
	if second.File.FileHash != hash {
		t.Errorf("deduplicated file hash = %q, want %q", second.File.FileHash, hash)
	}
	if second.File.Size.Int64() != int64(len(content)) {
		t.Errorf("deduplicated file size = %d, want %d", second.File.Size.Int64(), len(content))
	}

	// Both files must be real and distinct.
	if first.File.FileID == second.File.FileID {
		t.Error("the dedupe hit reused the first file's id")
	}
	for _, id := range []string{first.File.FileID.String(), second.File.FileID.String()} {
		if findUploaded(t, ctx, c, scratch, id) == nil {
			t.Errorf("file %s is absent from the listing", id)
		}
	}
}

// TestSandboxUploadFilePreservesModTime covers the local-file entry point,
// including the extra hashing pass that makes dedupe possible.
func TestSandboxUploadFilePreservesModTime(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(2048, fmt.Sprintf("mtime-%d", time.Now().UnixNano()))
	path := filepath.Join(t.TempDir(), fmt.Sprintf("odb-mtime-%d.bin", time.Now().Unix()))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	modTime := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}

	res, err := c.Uploads().UploadFile(ctx, path, opendrive.UploadParams{FolderID: scratch})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.Hash != md5hex(content) {
		t.Errorf("hash = %q, want %q", res.Hash, md5hex(content))
	}
	if res.File.FileHash != md5hex(content) {
		t.Errorf("upstream FileHash = %q", res.File.FileHash)
	}
	// §2.4 asks for the local mtime to survive the round trip.
	if got := res.File.DateModified.Unix(); got != modTime.Unix() {
		t.Errorf("DateModified = %d, want the local mtime %d", got, modTime.Unix())
	}
}

// TestSandboxUploadRejectsAWrongOffset pins down the resume mechanism: the
// refusal carries the authoritative offset, which is what the pipeline reads
// (docs/discrepancies.md D37).
func TestSandboxUploadRejectsAWrongOffset(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	content := uploadPayload(1024, fmt.Sprintf("offset-%d", time.Now().UnixNano()))
	name := fmt.Sprintf("odb-offset-%d.bin", time.Now().Unix())

	// Drive the handshake by hand so a deliberately wrong offset can be sent.
	var created struct {
		FileID       opendrive.FlexString `json:"FileId"`
		TempLocation string               `json:"TempLocation"`
	}
	if err := c.Do(ctx, opendrive.Request{
		Method: "POST", Path: opendrive.EndpointUploadCreateFile,
		SessionPlacement: opendrive.SessionInBody,
		Body: map[string]any{
			"folder_id": scratch, "file_name": name,
			"file_size": len(content), "open_if_exists": 0,
		},
	}, &created); err != nil {
		t.Fatalf("create_file: %v", err)
	}

	var opened struct {
		TempLocation string `json:"TempLocation"`
	}
	if err := c.Do(ctx, opendrive.Request{
		Method: "POST", Path: opendrive.EndpointUploadOpenFile,
		SessionPlacement: opendrive.SessionInBody,
		Body:             map[string]any{"file_id": created.FileID.String(), "file_size": len(content)},
	}, &opened); err != nil {
		t.Fatalf("open_file_upload: %v", err)
	}

	// The chunk endpoint only accepts multipart, so the probe builds one.
	var payload bytes.Buffer
	mw := multipart.NewWriter(&payload)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file_data"; filename="chunk"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("junk")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	err = c.Do(ctx, opendrive.Request{
		Method: "POST", Path: opendrive.EndpointUploadChunk,
		SessionPlacement: opendrive.SessionInPath,
		PathSegments:     []string{created.FileID.String()},
		Query: map[string][]string{
			"temp_location": {opened.TempLocation},
			"chunk_offset":  {"999999"},
			"chunk_size":    {"4"},
		},
		Body:           payload.Bytes(),
		ContentType:    mw.FormDataContentType(),
		NeedsSessionID: true,
	}, nil)
	if err == nil {
		t.Fatal("upstream accepted a chunk at a nonsensical offset")
	}
	// The message must still carry "uploaded=N"; the resume logic parses it.
	t.Logf("offset refusal: %v", err)
	if !bytes.Contains([]byte(err.Error()), []byte("uploaded=")) {
		t.Errorf("the refusal no longer reports the resume offset; revisit D37: %v", err)
	}
}

// An upload that fails after create_file must take its record back. This is the
// live half of the orphan-reclamation work: create_file makes a real,
// zero-length file before any content moves, and D39 showed what a folder full
// of abandoned records does to every later write into it.
//
// The failure is provoked the cheapest honest way — a declared size larger than
// the content — which fails at the end of the chunk loop, after the record and
// the temp location both exist.
func TestSandboxUploadReclaimsItsOrphan(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	name := fmt.Sprintf("odb-orphan-%d.bin", time.Now().UnixNano())
	content := uploadPayload(2048, "orphan")

	var recordID string
	_, err := c.Uploads().Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID:        scratch,
		Name:            name,
		Size:            int64(len(content)) + 4096, // a size the source cannot satisfy
		OnRecordCreated: func(fileID, _ string) { recordID = fileID },
	})
	if err == nil {
		t.Fatal("upstream accepted an upload whose content was shorter than its declared size")
	}
	t.Logf("expected failure: %v", err)
	if recordID == "" {
		t.Fatal("create_file never reported a record, so there is nothing to check")
	}

	// An unfinished upload is not listed in its folder, so the parent listing
	// cannot answer this one. file/info.json can: unlike the folder endpoint of
	// D27, it does report a removed *file* as gone.
	info, err := c.Files().Info(ctx, recordID)
	if err == nil {
		t.Fatalf("the failed upload left record %s behind (%q, %s bytes); enough of these and "+
			"the folder starts refusing writes (D39)", recordID, info.Name, info.Size.String())
	}
	t.Logf("record %s is gone, as it should be: %v", recordID, err)
}

// The opposite setting, for the job engine: a caller that intends to resume
// keeps the record, and is responsible for it afterwards.
func TestSandboxUploadKeepsTheRecordWhenAsked(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	name := fmt.Sprintf("odb-keep-%d.bin", time.Now().UnixNano())
	content := uploadPayload(2048, "keep")

	var recordID, recordTemp string
	_, err := c.Uploads().Upload(ctx, bytes.NewReader(content), opendrive.UploadParams{
		FolderID:      scratch,
		Name:          name,
		Size:          int64(len(content)) + 4096,
		KeepOnFailure: true,
		OnRecordCreated: func(fileID, tempLocation string) {
			recordID, recordTemp = fileID, tempLocation
		},
	})
	if err == nil {
		t.Fatal("want the upload to fail")
	}
	if recordID == "" {
		t.Fatal("the record was never reported, so a crash-resume could not find it")
	}
	if recordTemp == "" {
		t.Error("no temp location was reported; a resume would have nowhere to send bytes")
	}
	// Whatever happens next, this test cleans up after itself (D39).
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.Uploads().Reclaim(cleanupCtx, recordID, "", ""); err != nil {
			t.Logf("cleanup reclaim: %v", err)
		}
	})

	if _, err := c.Files().Info(ctx, recordID); err != nil {
		t.Errorf("KeepOnFailure was set, but record %s is gone (%v); a resume would have "+
			"nothing to resume into", recordID, err)
	}
}
