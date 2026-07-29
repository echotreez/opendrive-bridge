package opendrive

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/md5" //nolint:gosec // upstream's protocol requires MD5; not a security use
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// UploadService implements the four-step upload handshake of whitepaper §2.4:
//
//	create_file → open_file_upload → upload_file_chunk2 … → close_file_upload
//
// with the three branches the pipeline has to get right: deduplication (the
// server already holds this content, so nothing is transferred), compression
// (the server asks for Zlib), and resume (a chunk is re-sent from the offset
// the server reports).
//
// Three details were settled against the live API rather than the spec, and
// each one silently breaks the pipeline if assumed the other way:
//
//   - TotalWritten counts the bytes of *that chunk*, not the running total
//     (docs/discrepancies.md D35);
//   - a deduplicated upload must close *without* temp_location, or upstream
//     rejects it with "Total uploaded=0" (D36);
//   - a wrong offset is refused with a message carrying the authoritative
//     resume point, which is what makes resume possible at all (D37).
type UploadService struct {
	c *Client
}

// Uploads returns the upload pipeline bound to this client.
func (c *Client) Uploads() *UploadService { return &UploadService{c: c} }

// DefaultChunkSize is the official sample's value (whitepaper §2.4, §10.1).
// It is also the peak memory a single upload holds, because a chunk is
// buffered so it can be re-sent without rewinding the source.
const DefaultChunkSize = 50 << 20

// MinChunkSize guards against a caller asking for something pathological.
const MinChunkSize = 64 << 10

// zlibLevel is the compression level upstream asks for (§2.4).
const zlibLevel = 6

// ---------------------------------------------------------------- models

// createFileResponse is the reply of upload/create_file.json. The three
// Require* flags arrive as 0/1 integers here and as real JSON booleans from
// open_file_upload.json, which is why they are FlexBool (§2.6 #5).
type createFileResponse struct {
	FileID             FlexString `json:"FileId"`
	FileIDUpper        FlexString `json:"FileID"`
	Name               string     `json:"Name"`
	TempLocation       string     `json:"TempLocation"`
	RequireCompression FlexBool   `json:"RequireCompression"`
	RequireHash        FlexBool   `json:"RequireHash"`
	RequireHashOnly    FlexBool   `json:"RequireHashOnly"`
	DirUpdateTime      UnixTime   `json:"DirUpdateTime"`
}

func (r createFileResponse) id() string {
	if r.FileID != "" {
		return r.FileID.String()
	}
	return r.FileIDUpper.String()
}

// openFileResponse is the reply of upload/open_file_upload.json.
type openFileResponse struct {
	TempLocation       string   `json:"TempLocation"`
	RequireCompression FlexBool `json:"RequireCompression"`
	RequireHash        FlexBool `json:"RequireHash"`
	RequireHashOnly    FlexBool `json:"RequireHashOnly"`
	SpeedLimit         FlexInt  `json:"SpeedLimit"`
}

// chunkResponse is the reply of upload/upload_file_chunk2.json.
//
// TotalWritten is the size of the chunk just accepted, despite the name and
// despite the whitepaper describing it as a running total (D35).
type chunkResponse struct {
	TotalWritten FlexInt `json:"TotalWritten"`
}

// DedupeStatus is the reply of upload/has_ddref.json, the undocumented probe
// that answers "do you already hold this content?" (D13).
type DedupeStatus struct {
	Exists FlexBool `json:"exists"`
}

// UploadResult reports what an upload did, so a caller can tell a transfer
// from a dedupe hit without inspecting byte counters.
type UploadResult struct {
	// File is the metadata close_file_upload returned.
	File *FileInfo
	// Deduplicated is true when the server already held the content and
	// nothing was transferred (§2.4, the RequireHashOnly branch).
	Deduplicated bool
	// Compressed is true when the chunks were Zlib compressed at upstream's
	// request.
	Compressed bool
	// BytesSent counts the bytes actually put on the wire, after compression.
	BytesSent int64
	// Chunks counts the chunk requests made.
	Chunks int
	// Hash is the MD5 of the source content, in hex.
	Hash string
}

// UploadParams describes one upload.
type UploadParams struct {
	// FolderID is the destination folder; empty means the account root.
	FolderID string
	// Name is the file name upstream will use. It is validated locally first
	// (§2.6 #10).
	Name string
	// Size is the content length in bytes. It is required: upstream needs it
	// in create_file and verifies it again at close.
	Size int64
	// Hash is the hex MD5 of the content. Supplying it is what makes
	// deduplication possible, because upstream decides at create_file time.
	// When empty the content is hashed as it is sent and the hash is only
	// available at close, so the dedupe branch cannot trigger.
	Hash string
	// ModTime is preserved as the file's modification time when set (§2.4).
	ModTime time.Time
	// OpenIfExists asks upstream to return the existing file instead of
	// answering 409 when the name is taken (§2.4).
	OpenIfExists bool
	// ChunkSize overrides DefaultChunkSize.
	ChunkSize int64
	// AccessFolderID and SharingID scope the call.
	AccessFolderID string
	SharingID      string
	// Progress, when set, is called after each accepted chunk with the number
	// of source bytes sent so far and the total.
	Progress func(sent, total int64)
	// OnRecordCreated, when set, is called as soon as create_file has made the
	// upstream record, with everything a later attempt needs to resume into it.
	// The job engine persists this so a crash mid-transfer is recoverable
	// (§10.2); it is called before a single byte is sent.
	OnRecordCreated func(fileID, tempLocation string)
	// KeepOnFailure leaves the upstream record in place when the upload fails,
	// for a caller that intends to resume into it.
	//
	// Off by default, and the default is the important case: create_file makes
	// a real, zero-length record before any content moves, and an abandoned one
	// is invisible in the UI while still counting against the folder. Once
	// enough of them accumulate in a single folder, upstream starts refusing
	// further writes there with a message about permissions that has nothing to
	// do with permissions (docs/discrepancies.md D39, docs/error-taxonomy.md
	// T2). A cancelled bulk transfer must not be able to poison its own
	// destination.
	KeepOnFailure bool
}

func (p *UploadParams) chunkSize() int64 {
	if p.ChunkSize >= MinChunkSize {
		return p.ChunkSize
	}
	if p.ChunkSize > 0 {
		return MinChunkSize
	}
	return DefaultChunkSize
}

func (p *UploadParams) validate() error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if p.Size < 0 {
		return invalidRequest("upload size must not be negative")
	}
	if p.Hash != "" {
		if len(p.Hash) != 32 {
			return invalidRequest("file_hash must be a 32 character hex MD5, got %d characters", len(p.Hash))
		}
		if _, err := hex.DecodeString(p.Hash); err != nil {
			return invalidRequest("file_hash must be hexadecimal: %v", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- entry points

// HasDedupeReference reports whether upstream already holds content with this
// size and MD5.
//
// POST /upload/has_ddref.json. Undocumented in the PDF (D13); it answers
// {"exists":true}. Upload does not need it — create_file decides on its own —
// but it lets a caller predict a dedupe hit before committing to anything.
func (s *UploadService) HasDedupeReference(ctx context.Context, size int64, hash string) (bool, error) {
	if size <= 0 || hash == "" {
		return false, invalidRequest("a dedupe probe needs a size and an MD5")
	}
	var out DedupeStatus
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointUploadHasDedupeRef,
		SessionPlacement: SessionInBody,
		Body: map[string]any{
			"file_size": size,
			"file_hash": hash,
		},
	}, &out); err != nil {
		return false, err
	}
	return out.Exists.Bool(), nil
}

// UploadFile uploads a local file, hashing it first so that the dedupe branch
// can trigger.
//
// The hash costs one extra read of the file. That is the deliberate trade:
// upstream decides on deduplication at create_file time, so the hash has to be
// known before a single byte is sent, and a re-read of local disk is far
// cheaper than re-sending the content over the network (§2.4, §10.1). Callers
// who already know the digest can set UploadParams.Hash and skip it, and
// callers streaming from a pipe should use Upload directly.
func (s *UploadService) UploadFile(ctx context.Context, localPath string, p UploadParams) (*UploadResult, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return nil, invalidRequest("cannot read %s: %v", filepath.Base(localPath), err)
	}
	if info.IsDir() {
		return nil, invalidRequest("%s is a directory", filepath.Base(localPath))
	}
	if p.Name == "" {
		p.Name = info.Name()
	}
	p.Size = info.Size()
	if p.ModTime.IsZero() {
		p.ModTime = info.ModTime()
	}

	if p.Hash == "" {
		hash, err := hashFile(localPath)
		if err != nil {
			return nil, err
		}
		p.Hash = hash
	}

	f, err := os.Open(localPath) //nolint:gosec // the path is the caller's own
	if err != nil {
		return nil, invalidRequest("cannot open %s: %v", filepath.Base(localPath), err)
	}
	defer func() { _ = f.Close() }()

	return s.Upload(ctx, f, p)
}

// hashFile streams a file through MD5 without holding it in memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the caller's own
	if err != nil {
		return "", invalidRequest("cannot open %s: %v", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	sum := md5.New() //nolint:gosec // protocol requirement, not a security use
	if _, err := io.Copy(sum, f); err != nil {
		return "", invalidRequest("cannot read %s: %v", filepath.Base(path), err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// Upload streams content through the four-step handshake.
//
// Memory use is bounded by the chunk size regardless of how large the content
// is: one chunk is buffered at a time so that it can be re-sent after a
// transient failure without rewinding the source (§10.1). The MD5 is computed
// as the content is read, in the same pass, through an io.TeeReader.
func (s *UploadService) Upload(ctx context.Context, src io.Reader, p UploadParams) (*UploadResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, invalidRequest("upload needs a source reader")
	}
	folder := p.FolderID
	if folder == "" {
		folder = RootFolderID
	}

	created, err := s.createFile(ctx, folder, p)
	if err != nil {
		return nil, err
	}
	fileID := created.id()
	if fileID == "" {
		return nil, &APIError{Kind: KindInvalidResponse, Op: "POST " + EndpointUploadCreateFile,
			UpstreamMsg: "create_file returned no file id"}
	}
	if p.OnRecordCreated != nil {
		p.OnRecordCreated(fileID, created.TempLocation)
	}

	// From here on an upstream record exists. Every path out of transfer must
	// either complete it or take it back (see reclaimOrphan).
	result, err := s.transfer(ctx, src, fileID, created, p)
	if err != nil {
		s.reclaimOrphan(ctx, fileID, p)
		return nil, err
	}
	return result, nil
}

// transfer runs everything after create_file: the branch decision, the chunk
// loop and the close.
func (s *UploadService) transfer(
	ctx context.Context, src io.Reader, fileID string, created *createFileResponse, p UploadParams,
) (*UploadResult, error) {
	result := &UploadResult{Hash: p.Hash}

	// Branch 1 — deduplication. Upstream already holds this content, so the
	// transfer is skipped entirely and the file is closed on the strength of
	// its hash (§2.4). The close must omit temp_location (D36).
	if created.RequireHashOnly.Bool() {
		file, err := s.closeFile(ctx, fileID, "", p, false)
		if err != nil {
			return nil, err
		}
		result.File = file
		result.Deduplicated = true
		if p.Progress != nil {
			p.Progress(p.Size, p.Size)
		}
		return result, nil
	}

	opened, err := s.openFile(ctx, fileID, p)
	if err != nil {
		return nil, err
	}
	tempLocation := opened.TempLocation
	if tempLocation == "" {
		tempLocation = created.TempLocation
	}
	if tempLocation == "" {
		return nil, &APIError{Kind: KindInvalidResponse, Op: "POST " + EndpointUploadOpenFile,
			UpstreamMsg: "no temp location was returned, so there is nowhere to send the content"}
	}

	// Branch 2 — compression. Upstream asks for Zlib level 6 and is told at
	// close that the content was compressed (§2.4).
	result.Compressed = opened.RequireCompression.Bool() || created.RequireCompression.Bool()

	// The hash is computed while the content is read, so a caller that did not
	// supply one still gets a correct file_hash at close.
	digest := md5.New() //nolint:gosec // protocol requirement, not a security use
	reader := io.TeeReader(src, digest)

	sent, chunks, err := s.sendChunks(ctx, fileID, tempLocation, reader, p, result.Compressed)
	result.BytesSent = sent
	result.Chunks = chunks
	if err != nil {
		return nil, err
	}

	if p.Hash == "" {
		p.Hash = hex.EncodeToString(digest.Sum(nil))
		result.Hash = p.Hash
	}

	file, err := s.closeFile(ctx, fileID, tempLocation, p, result.Compressed)
	if err != nil {
		return nil, err
	}
	result.File = file
	return result, nil
}

// reclaimTimeout bounds the clean-up call. It is short on purpose: reclamation
// runs on a path that has already failed, and must not hold the caller.
const reclaimTimeout = 30 * time.Second

// reclaimOrphan takes back the record create_file made when the transfer that
// should have filled it did not finish.
//
// This is a product-level obligation, not tidiness. The record is created before
// any content moves and is invisible to the user afterwards, so a failed or
// cancelled bulk transfer silently leaves one per file in the destination
// folder. Enough of them and upstream begins refusing further writes to that
// folder with a message about permissions that is not about permissions —
// exactly the failure D39 took a full round to diagnose, arriving in production
// as "contact your administrator" for a user whose only mistake was pressing
// cancel.
//
// Two cases are left alone: OpenIfExists, where create_file may have handed back
// a file that already existed and was never ours to delete, and an explicit
// KeepOnFailure, where the caller intends to resume into the record.
func (s *UploadService) reclaimOrphan(ctx context.Context, fileID string, p UploadParams) {
	if fileID == "" || p.KeepOnFailure || p.OpenIfExists {
		return
	}
	// Cancellation is the case where reclamation matters most, so the clean-up
	// gets a context that outlives the one that was cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reclaimTimeout)
	defer cancel()

	if err := s.Reclaim(ctx, fileID, p.AccessFolderID, p.SharingID); err != nil {
		// The upload has already failed; this is reported, not propagated.
		s.c.Logger().Warn("could not reclaim the record left by a failed upload",
			slog.String("file_id", fileID), slog.String("name", p.Name),
			slog.String("error", RedactString(err.Error())))
		return
	}
	s.c.Logger().Debug("reclaimed the record left by a failed upload",
		slog.String("file_id", fileID), slog.String("name", p.Name))
}

// Reclaim deletes an upstream file record outright, without a trash step.
//
// It exists for records an upload created and did not fill. The job engine calls
// it when a transfer is abandoned for good, and Upload calls it for itself
// unless UploadParams.KeepOnFailure says otherwise.
//
// DELETE /file.json/{session_id}/{file_id} deletes a file that was never
// trashed (D29), which is what makes it the right call here and the wrong one
// for anything the user can see.
func (s *UploadService) Reclaim(ctx context.Context, fileID, accessFolderID, sharingID string) error {
	if fileID == "" {
		return invalidRequest("reclaiming an upload record needs a file id")
	}
	return s.c.Files().DeleteTrashed(ctx, fileID, accessFolderID, sharingID)
}

// ---------------------------------------------------------------- steps

// createFile is step one. Passing file_size and file_hash together is what
// lets upstream answer RequireHashOnly and skip the transfer (§2.4).
func (s *UploadService) createFile(ctx context.Context, folderID string, p UploadParams) (*createFileResponse, error) {
	body := map[string]any{
		"folder_id": folderID,
		"file_name": p.Name,
		"file_size": p.Size,
	}
	if p.Hash != "" {
		body["file_hash"] = p.Hash
	}
	if p.OpenIfExists {
		body["open_if_exists"] = 1
	} else {
		body["open_if_exists"] = 0
	}
	if p.AccessFolderID != "" {
		body["access_folder_id"] = p.AccessFolderID
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}

	var out createFileResponse
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointUploadCreateFile,
		SessionPlacement: SessionInBody,
		Body:             body,
		// The destination folder decides whether this may be written at all, so
		// it is what the success witness is keyed on (docs/error-taxonomy.md T2).
		Scope: folderID,
	}, &out); err != nil {
		return nil, err
	}
	// A new file changes the destination listing.
	if cache := s.c.pathCache; cache != nil {
		cache.InvalidateID(folderID)
	}
	return &out, nil
}

// openFile is step two.
func (s *UploadService) openFile(ctx context.Context, fileID string, p UploadParams) (*openFileResponse, error) {
	body := map[string]any{
		"file_id":   fileID,
		"file_size": p.Size,
	}
	if p.Hash != "" {
		body["file_hash"] = p.Hash
	}
	if p.AccessFolderID != "" {
		body["access_folder_id"] = p.AccessFolderID
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}

	var out openFileResponse
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointUploadOpenFile,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// offsetMismatch matches the refusal upstream sends for a chunk that does not
// continue where the previous one stopped:
//
//	Incorrect chunk offset: uploaded=320, chunk_offset=999
//
// The number is authoritative and is what makes resume possible (D37).
var offsetMismatch = regexp.MustCompile(`uploaded=(\d+)`)

// serverOffset extracts the resume point from an offset-mismatch error, or
// reports false when the error is something else.
func serverOffset(err error) (int64, bool) {
	var ae *APIError
	if !errors.As(err, &ae) {
		return 0, false
	}
	m := offsetMismatch.FindStringSubmatch(ae.UpstreamMsg)
	if m == nil {
		return 0, false
	}
	n, convErr := strconv.ParseInt(m[1], 10, 64)
	if convErr != nil {
		return 0, false
	}
	return n, true
}

// sendChunks is step three: the loop. It returns the number of source bytes
// accepted and how many chunk requests were made.
func (s *UploadService) sendChunks(
	ctx context.Context, fileID, tempLocation string, src io.Reader, p UploadParams, compress bool,
) (int64, int, error) {
	var (
		// sourceOffset counts the content read, wireOffset the bytes actually
		// sent. They differ only when upstream asked for compression, and
		// chunk_offset is a wire offset: it addresses the byte stream upstream
		// is assembling, not the original file.
		sourceOffset int64
		wireOffset   int64
		chunks       int
		buf          = make([]byte, p.chunkSize())
	)

	for {
		n, readErr := io.ReadFull(src, buf)
		if n == 0 {
			if readErr == nil || errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			return sourceOffset, chunks, invalidRequest("cannot read the upload source: %v", readErr)
		}

		payload := buf[:n]
		if compress {
			compressed, err := compressChunk(payload)
			if err != nil {
				return sourceOffset, chunks, err
			}
			payload = compressed
		}

		sent, err := s.sendChunkResuming(ctx, fileID, tempLocation, wireOffset, payload)
		chunks += sent.attempts
		if err != nil {
			return sourceOffset, chunks, err
		}

		sourceOffset += int64(n)
		wireOffset += int64(len(payload))
		if p.Progress != nil {
			p.Progress(sourceOffset, p.Size)
		}
		if readErr != nil {
			// io.ReadFull reports a short final chunk this way.
			break
		}
	}

	if p.Size > 0 && sourceOffset != p.Size {
		return sourceOffset, chunks, invalidRequest(
			"the source produced %d bytes but the declared file size is %d", sourceOffset, p.Size)
	}
	return wireOffset, chunks, nil
}

// chunkOutcome reports how much work one logical chunk took.
type chunkOutcome struct {
	attempts int
}

// sendChunkResuming posts one chunk, recovering from an offset disagreement.
//
// Upstream refuses a chunk that does not continue exactly where it left off,
// and says where that is (D37). Three cases follow from the offset it reports:
//
//   - it is already past this whole chunk, so the chunk landed and only the
//     reply was lost — nothing more to send;
//   - it is somewhere inside this chunk, so a partial write landed — the tail
//     is re-sent from there, which is possible because the chunk is still
//     buffered;
//   - it is behind the start of this chunk, which would need the source to
//     rewind. A plain reader cannot, so that is an error rather than a silent
//     truncation.
func (s *UploadService) sendChunkResuming(
	ctx context.Context, fileID, tempLocation string, offset int64, payload []byte,
) (chunkOutcome, error) {
	var out chunkOutcome

	written, err := s.sendOneChunk(ctx, fileID, tempLocation, offset, payload)
	out.attempts++
	if err == nil {
		// D35: TotalWritten is this chunk's byte count, not a running total.
		if want := int64(len(payload)); written != want {
			return out, &APIError{
				Kind: KindInvalidResponse, Op: "POST " + EndpointUploadChunk,
				UpstreamMsg: fmt.Sprintf("chunk at offset %d: upstream wrote %d bytes of %d",
					offset, written, want),
			}
		}
		return out, nil
	}

	at, ok := serverOffset(err)
	if !ok {
		return out, err
	}
	end := offset + int64(len(payload))
	switch {
	case at >= end:
		// The chunk is already there.
		return out, nil
	case at > offset:
		tail := payload[at-offset:]
		written, retryErr := s.sendOneChunk(ctx, fileID, tempLocation, at, tail)
		out.attempts++
		if retryErr != nil {
			return out, retryErr
		}
		if want := int64(len(tail)); written != want {
			return out, &APIError{
				Kind: KindInvalidResponse, Op: "POST " + EndpointUploadChunk,
				UpstreamMsg: fmt.Sprintf("resumed chunk at offset %d: upstream wrote %d bytes of %d",
					at, written, want),
			}
		}
		return out, nil
	default:
		return out, fmt.Errorf(
			"upload resume needs offset %d but this chunk starts at %d and the source cannot rewind: %w",
			at, offset, err)
	}
}

// sendOneChunk posts a single chunk and returns the byte count upstream
// reports for it.
//
// POST /upload/upload_file_chunk2.json/{session_id}/{file_id}: session and file
// id are path segments, temp_location, chunk_offset and chunk_size are query
// parameters, and the bytes travel in a multipart file_data field (§2.4).
func (s *UploadService) sendOneChunk(
	ctx context.Context, fileID, tempLocation string, offset int64, payload []byte,
) (int64, error) {
	body, contentType, err := multipartChunk(payload)
	if err != nil {
		return 0, err
	}

	var out chunkResponse
	err = s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointUploadChunk,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{fileID},
		Query: map[string][]string{
			"temp_location": {tempLocation},
			"chunk_offset":  {strconv.FormatInt(offset, 10)},
			"chunk_size":    {strconv.Itoa(len(payload))},
		},
		Body:        body,
		ContentType: contentType,
		// A chunk is safe to re-send: upstream keys it by offset, so a
		// duplicate is refused rather than appended (D37).
		Retryable: Retryable(true),
		// This endpoint refuses the OAUTH marker outright (D38).
		NeedsSessionID: true,
	}, &out)
	if err != nil {
		return 0, err
	}
	return out.TotalWritten.Int64(), nil
}

// multipartChunk wraps a chunk in the multipart body upstream expects.
func multipartChunk(payload []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file_data"; filename="chunk"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, "", invalidRequest("cannot build the chunk body: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		return nil, "", invalidRequest("cannot build the chunk body: %v", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", invalidRequest("cannot build the chunk body: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// compressChunk applies the Zlib level upstream asks for (§2.4).
func compressChunk(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := zlib.NewWriterLevel(&buf, zlibLevel)
	if err != nil {
		return nil, invalidRequest("cannot compress the chunk: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		return nil, invalidRequest("cannot compress the chunk: %v", err)
	}
	if err := w.Close(); err != nil {
		return nil, invalidRequest("cannot compress the chunk: %v", err)
	}
	return buf.Bytes(), nil
}

// closeFile is step four. It returns the completed file's metadata.
//
// deduplicated selects the branch: a dedupe close must omit temp_location
// entirely, or upstream answers "Invalid upload file size. Total uploaded=0"
// even though it holds the content (D36).
func (s *UploadService) closeFile(
	ctx context.Context, fileID, tempLocation string, p UploadParams, compressedOrDeduped bool,
) (*FileInfo, error) {
	body := map[string]any{
		"file_id":   fileID,
		"file_size": p.Size,
	}
	if tempLocation != "" {
		body["temp_location"] = tempLocation
	}
	if p.Hash != "" {
		body["file_hash"] = p.Hash
	}
	if !p.ModTime.IsZero() {
		body["file_time"] = p.ModTime.Unix()
	}
	if tempLocation != "" && compressedOrDeduped {
		// Only meaningful when content was actually transferred.
		body["file_compressed"] = 1
	}
	if p.AccessFolderID != "" {
		body["access_folder_id"] = p.AccessFolderID
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}

	var out FileInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointUploadCloseFile,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
