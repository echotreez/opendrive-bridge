package opendrive

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrors the code under test
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newUploadFixture(t *testing.T) (*mockUpstream, *UploadService) {
	t.Helper()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	return m, c.Uploads()
}

// chunkPayload pulls the file_data part back out of a recorded multipart body.
func chunkPayload(t *testing.T, call recordedRequest) []byte {
	t.Helper()
	_, params, err := mime.ParseMediaType(call.ContentType)
	if err != nil {
		t.Fatalf("content type %q: %v", call.ContentType, err)
	}
	r := multipart.NewReader(strings.NewReader(call.RawBody), params["boundary"])
	part, err := r.NextPart()
	if err != nil {
		t.Fatalf("no multipart part: %v", err)
	}
	if part.FormName() != "file_data" {
		t.Fatalf("part name = %q, want file_data", part.FormName())
	}
	data, err := io.ReadAll(part)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol requirement
	return hex.EncodeToString(sum[:])
}

// §2.4 branch 1: upstream already holds the content, so nothing is
// transferred and the close omits temp_location (docs/discrepancies.md D36).
func TestUploadDeduplicates(t *testing.T) {
	m, uploads := newUploadFixture(t)
	content := []byte("already on the server")

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP","RequireHashOnly":1,"RequireHash":1}`)
	m.push(200, `{"FileId":"FID","Name":"dedupe.txt","Size":21,"FileHash":"`+md5hex(content)+`"}`)

	var progress [][2]int64
	res, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "dedupe.txt", Size: int64(len(content)), Hash: md5hex(content),
		Progress: func(sent, total int64) { progress = append(progress, [2]int64{sent, total}) },
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !res.Deduplicated || res.Chunks != 0 || res.BytesSent != 0 {
		t.Fatalf("result = %+v, want a dedupe hit with nothing transferred", res)
	}
	if res.File == nil || res.File.FileID.String() != "FID" {
		t.Fatalf("file = %+v", res.File)
	}

	calls := m.calls()
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want create_file and close_file_upload only", len(calls))
	}
	if !strings.HasSuffix(calls[0].Path, EndpointUploadCreateFile) {
		t.Fatalf("first call = %s", calls[0].Path)
	}
	if !strings.HasSuffix(calls[1].Path, EndpointUploadCloseFile) {
		t.Fatalf("second call = %s", calls[1].Path)
	}
	// The whole point of D36.
	if _, present := calls[1].Body["temp_location"]; present {
		t.Errorf("the dedupe close sent temp_location, which upstream rejects: %v", calls[1].Body)
	}
	if calls[1].Body["file_hash"] != md5hex(content) {
		t.Errorf("close body = %v", calls[1].Body)
	}
	// Progress still reports completion, so a caller's bar reaches the end.
	if len(progress) != 1 || progress[0] != [2]int64{int64(len(content)), int64(len(content))} {
		t.Errorf("progress = %v", progress)
	}
}

// The full four-step handshake, with the chunk loop crossing a boundary.
func TestUploadFourStepHandshake(t *testing.T) {
	m, uploads := newUploadFixture(t)
	// Two full chunks at the smallest legal size, so the loop really loops.
	const chunk = MinChunkSize
	content := bytes.Repeat([]byte("abcdefgh"), chunk/4) // 2 x 64 KiB

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP-CREATE","RequireHashOnly":0}`)
	m.push(200, `{"TempLocation":"TEMP-OPEN","RequireCompression":false,"RequireHash":true,"SpeedLimit":176128}`)
	m.push(200, fmt.Sprintf(`{"TotalWritten":%d}`, chunk))
	m.push(200, fmt.Sprintf(`{"TotalWritten":%d}`, chunk))
	m.push(200, `{"FileId":"FID","Name":"two-chunks.bin","Size":131072,"FileHash":"`+md5hex(content)+`"}`)

	res, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "two-chunks.bin", Size: int64(len(content)),
		ChunkSize: chunk, ModTime: time.Unix(1785214171, 0),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Deduplicated || res.Chunks != 2 || res.BytesSent != int64(len(content)) {
		t.Fatalf("result = %+v", res)
	}
	// The hash is computed in the same pass as the upload (§10.1), so a caller
	// that supplied none still gets the right one at close.
	if res.Hash != md5hex(content) {
		t.Fatalf("hash = %q, want %q", res.Hash, md5hex(content))
	}

	calls := m.calls()
	if len(calls) != 5 {
		t.Fatalf("made %d calls, want 5", len(calls))
	}

	// Step 1: create_file carries the size, and open_if_exists is explicit.
	create := calls[0].Body
	if create["file_name"] != "two-chunks.bin" || create["file_size"] != float64(len(content)) {
		t.Errorf("create_file body = %v", create)
	}
	if create["open_if_exists"] != float64(0) {
		t.Errorf("open_if_exists = %v", create["open_if_exists"])
	}

	// Step 3: session and file id are path segments, the rest is query.
	first := calls[2]
	if first.Path != "/api/v1/upload/upload_file_chunk2.json/SID/FID" {
		t.Fatalf("chunk path = %q", first.Path)
	}
	if first.Query.Get("temp_location") != "TEMP-OPEN" {
		t.Errorf("temp_location = %q, want the one open_file_upload returned", first.Query.Get("temp_location"))
	}
	if first.Query.Get("chunk_offset") != "0" ||
		first.Query.Get("chunk_size") != fmt.Sprint(chunk) {
		t.Errorf("chunk query = %v", first.Query)
	}
	if got := chunkPayload(t, first); !bytes.Equal(got, content[:chunk]) {
		t.Errorf("first chunk carried %d bytes of the wrong content", len(got))
	}

	second := calls[3]
	if second.Query.Get("chunk_offset") != fmt.Sprint(chunk) {
		t.Errorf("second chunk offset = %q", second.Query.Get("chunk_offset"))
	}
	if got := chunkPayload(t, second); !bytes.Equal(got, content[chunk:]) {
		t.Errorf("second chunk content mismatch")
	}

	// Step 4: close carries the size, the hash and the modification time, and
	// no file_compressed because nothing was compressed.
	closeBody := calls[4].Body
	if closeBody["temp_location"] != "TEMP-OPEN" || closeBody["file_size"] != float64(len(content)) {
		t.Errorf("close body = %v", closeBody)
	}
	if closeBody["file_time"] != float64(1785214171) {
		t.Errorf("file_time = %v", closeBody["file_time"])
	}
	if _, present := closeBody["file_compressed"]; present {
		t.Errorf("file_compressed must not be sent for an uncompressed upload: %v", closeBody)
	}
}

// §2.4 branch 2: RequireCompression means Zlib level 6 on the wire and
// file_compressed at close.
func TestUploadCompresses(t *testing.T) {
	m, uploads := newUploadFixture(t)
	// Something highly compressible, so the wire payload is visibly smaller.
	content := bytes.Repeat([]byte("compress me "), 512)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP","RequireHashOnly":0,"RequireCompression":1}`)
	m.push(200, `{"TempLocation":"TEMP","RequireCompression":true}`)
	m.handleFrom(2, func(w http.ResponseWriter, r *http.Request, n int) {
		if strings.HasSuffix(r.URL.Path, EndpointUploadCloseFile) {
			_, _ = w.Write([]byte(`{"FileId":"FID","Name":"compressible.txt"}`))
			return
		}
		// Answer every chunk with the compressed length the client sent.
		_, _ = fmt.Fprintf(w, `{"TotalWritten":%s}`, r.URL.Query().Get("chunk_size"))
	})

	res, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "compressible.txt", Size: int64(len(content)), ChunkSize: MinChunkSize,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !res.Compressed {
		t.Fatal("the result does not report compression")
	}
	if res.BytesSent >= int64(len(content)) {
		t.Errorf("sent %d bytes for %d of input; compression did not happen", res.BytesSent, len(content))
	}

	calls := m.calls()
	payload := chunkPayload(t, calls[2])
	zr, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("the chunk is not zlib data: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Error("the decompressed chunk does not match the source")
	}
	if calls[len(calls)-1].Body["file_compressed"] != float64(1) {
		t.Errorf("close body = %v, want file_compressed=1", calls[len(calls)-1].Body)
	}
}

// D35: TotalWritten counts the chunk, not the running total. A mismatch is a
// corrupt transfer and must not be closed over.
func TestUploadRejectsAShortChunkWrite(t *testing.T) {
	m, uploads := newUploadFixture(t)
	content := bytes.Repeat([]byte("x"), 4096)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	m.push(200, `{"TotalWritten":4000}`) // 96 bytes short of the 4096 sent

	_, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "short.bin", Size: int64(len(content)), ChunkSize: MinChunkSize,
	})
	if ErrorKind(err) != KindInvalidResponse {
		t.Fatalf("err = %v, want invalid_response", err)
	}
	mustContain(t, err.Error(), "4000", "short write message")
}

// §2.4 branch 3: resume. Upstream refuses a chunk that does not continue where
// it left off and reports the authoritative offset (D37).
func TestUploadResumesFromTheServerOffset(t *testing.T) {
	m, uploads := newUploadFixture(t)
	const chunk = MinChunkSize
	content := bytes.Repeat([]byte("y"), 2*chunk)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	// The first chunk lands but its reply is lost. The client re-sends it and
	// upstream answers that it already holds exactly those bytes.
	m.push(400, fmt.Sprintf(
		`{"error":{"code":400,"message":"Incorrect chunk offset: uploaded=%d, chunk_offset=0"}}`, chunk))
	m.push(200, fmt.Sprintf(`{"TotalWritten":%d}`, chunk))
	m.push(200, `{"FileId":"FID"}`)

	res, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "resume.bin", Size: int64(len(content)), ChunkSize: chunk,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Chunks != 2 {
		t.Fatalf("chunk requests = %d, want 2: one refused and one accepted", res.Chunks)
	}
}

// A partial write is the other half of resume: upstream landed some of the
// chunk, so only the tail is re-sent — possible because the chunk is still
// buffered (§2.4 branch 3).
func TestUploadResendsTheTailAfterAPartialWrite(t *testing.T) {
	m, uploads := newUploadFixture(t)
	const chunk = MinChunkSize
	const landed = chunk / 4
	content := bytes.Repeat([]byte("p"), chunk)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	m.push(400, fmt.Sprintf(
		`{"error":{"code":400,"message":"Incorrect chunk offset: uploaded=%d, chunk_offset=0"}}`, landed))
	m.push(200, fmt.Sprintf(`{"TotalWritten":%d}`, chunk-landed))
	m.push(200, `{"FileId":"FID"}`)

	if _, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "partial.bin", Size: int64(len(content)), ChunkSize: chunk,
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	retry := m.calls()[3]
	if got := retry.Query.Get("chunk_offset"); got != fmt.Sprint(landed) {
		t.Errorf("retry offset = %q, want the offset upstream reported", got)
	}
	if got := chunkPayload(t, retry); len(got) != chunk-landed {
		t.Errorf("retry carried %d bytes, want the %d byte tail", len(got), chunk-landed)
	}
}

// A resume point the source cannot reach is a hard error rather than a silent
// truncation.
func TestUploadFailsWhenTheResumeOffsetIsUnreachable(t *testing.T) {
	m, uploads := newUploadFixture(t)
	const chunk = MinChunkSize
	content := bytes.Repeat([]byte("z"), 2*chunk)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	m.push(200, fmt.Sprintf(`{"TotalWritten":%d}`, chunk))
	// The second chunk starts at 65536, but upstream claims it only has 10 —
	// the source would have to rewind, which a reader cannot do.
	m.push(400, `{"error":{"code":400,"message":"Incorrect chunk offset: uploaded=10, chunk_offset=65536"}}`)

	_, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "unreachable.bin", Size: int64(len(content)), ChunkSize: chunk,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	mustContain(t, err.Error(), "cannot rewind", "resume error")
}

func TestUploadDetectsASourceShorterThanDeclared(t *testing.T) {
	m, uploads := newUploadFixture(t)
	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	m.push(200, `{"TotalWritten":10}`)

	_, err := uploads.Upload(context.Background(), strings.NewReader("0123456789"), UploadParams{
		FolderID: "DIR", Name: "short.txt", Size: 100, ChunkSize: MinChunkSize,
	})
	if ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, err.Error(), "declared file size", "size mismatch message")
}

func TestUploadValidatesLocally(t *testing.T) {
	m, uploads := newUploadFixture(t)
	ctx := context.Background()
	cases := []UploadParams{
		{Name: "bad/name", Size: 1},
		{Name: "ok.txt", Size: -1},
		{Name: "ok.txt", Size: 1, Hash: "not-a-hash"},
		{Name: "ok.txt", Size: 1, Hash: strings.Repeat("z", 32)},
	}
	for _, p := range cases {
		if _, err := uploads.Upload(ctx, strings.NewReader("x"), p); err == nil {
			t.Errorf("Upload(%+v) was accepted", p)
		}
	}
	if _, err := uploads.Upload(ctx, nil, UploadParams{Name: "ok.txt", Size: 1}); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("nil reader = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatalf("invalid input reached upstream %d times", m.callCount())
	}
}

func TestUploadChunkSizeBounds(t *testing.T) {
	if (&UploadParams{}).chunkSize() != DefaultChunkSize {
		t.Error("the zero value should mean the default chunk size")
	}
	if (&UploadParams{ChunkSize: 1}).chunkSize() != MinChunkSize {
		t.Error("a tiny chunk size should be raised to the minimum")
	}
	if got := (&UploadParams{ChunkSize: 8 << 20}).chunkSize(); got != 8<<20 {
		t.Errorf("chunk size = %d", got)
	}
}

func TestHasDedupeReference(t *testing.T) {
	m, uploads := newUploadFixture(t)
	m.push(200, `{"exists":true}`)
	got, err := uploads.HasDedupeReference(context.Background(), 320, "a12dd1cf6cb51bf52ea00d3bb2d926e0")
	if err != nil || !got {
		t.Fatalf("HasDedupeReference = %v, %v", got, err)
	}
	if m.lastCall().Body["file_size"] != float64(320) {
		t.Fatalf("body = %v", m.lastCall().Body)
	}

	if _, err := uploads.HasDedupeReference(context.Background(), 0, ""); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
}

// UploadFile fills in the name, size and modification time from the file, and
// hashes it first so the dedupe branch can trigger.
func TestUploadFileHashesFirst(t *testing.T) {
	m, uploads := newUploadFixture(t)
	content := []byte("the quick brown fox\n")
	path := filepath.Join(t.TempDir(), "fox.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	modTime := time.Unix(1785000000, 0)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP","RequireHashOnly":1}`)
	m.push(200, `{"FileId":"FID","Name":"fox.txt"}`)

	res, err := uploads.UploadFile(context.Background(), path, UploadParams{FolderID: "DIR"})
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if !res.Deduplicated {
		t.Fatal("the hash was not supplied to create_file, so dedupe could not trigger")
	}

	create := m.calls()[0].Body
	if create["file_name"] != "fox.txt" || create["file_size"] != float64(len(content)) {
		t.Fatalf("create body = %v", create)
	}
	if create["file_hash"] != md5hex(content) {
		t.Fatalf("file_hash = %v, want the MD5 of the file", create["file_hash"])
	}
	if got := m.calls()[1].Body["file_time"]; got != float64(modTime.Unix()) {
		t.Errorf("file_time = %v, want the file's mtime", got)
	}

	if _, err := uploads.UploadFile(context.Background(), filepath.Dir(path), UploadParams{}); err == nil {
		t.Error("a directory should be refused")
	}
	if _, err := uploads.UploadFile(context.Background(), path+".missing", UploadParams{}); err == nil {
		t.Error("a missing file should be refused")
	}
}

// Memory must not scale with the content: the pipeline holds one chunk.
func TestUploadHoldsOnlyOneChunk(t *testing.T) {
	m, uploads := newUploadFixture(t)
	const chunkSize = MinChunkSize
	const chunks = 8
	content := bytes.Repeat([]byte("m"), chunkSize*chunks)

	m.push(200, `{"FileId":"FID","TempLocation":"TEMP"}`)
	m.push(200, `{"TempLocation":"TEMP"}`)
	m.handleFrom(2, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/api/v1"+EndpointUploadCloseFile {
			_, _ = w.Write([]byte(`{"FileId":"FID"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"TotalWritten":%s}`, r.URL.Query().Get("chunk_size"))
	})

	res, err := uploads.Upload(context.Background(), bytes.NewReader(content), UploadParams{
		FolderID: "DIR", Name: "big.bin", Size: int64(len(content)), ChunkSize: chunkSize,
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.Chunks != chunks {
		t.Fatalf("chunks = %d, want %d", res.Chunks, chunks)
	}
	if res.BytesSent != int64(len(content)) {
		t.Fatalf("sent %d of %d bytes", res.BytesSent, len(content))
	}
}

func TestServerOffsetParsing(t *testing.T) {
	err := parseError(400, nil,
		[]byte(`{"error":{"code":400,"message":"Incorrect chunk offset: uploaded=320, chunk_offset=999"}}`),
		"POST /upload/upload_file_chunk2.json", "https://x")
	got, ok := serverOffset(err)
	if !ok || got != 320 {
		t.Fatalf("serverOffset = %d, %v", got, ok)
	}

	other := parseError(500, nil, []byte(`{"error":{"code":500,"message":"boom"}}`), "POST /x", "https://x")
	if _, ok := serverOffset(other); ok {
		t.Error("an unrelated error must not look like a resume point")
	}
	if _, ok := serverOffset(io.EOF); ok {
		t.Error("a plain error must not look like a resume point")
	}
}
