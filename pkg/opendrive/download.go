package opendrive

import (
	"context"
	"crypto/md5" //nolint:gosec // upstream's protocol requires MD5; not a security use
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DownloadService is the download half of the transfer pipeline (whitepaper
// §2.5, §10.1). It streams to disk, never holding a file in memory, and resumes
// from a byte offset.
//
// Three things about this endpoint were settled on the wire rather than from the
// spec, and each one silently corrupts a resume if assumed the other way:
//
//   - the `offset` query parameter is off by one at the very last byte: asking
//     for offset == size-1 answers 200 with the *whole file* instead of a
//     one-byte tail. The Range header is correct at every offset, so that is
//     what the pipeline sends (docs/discrepancies.md D41).
//   - whatever is asked for, a 200 means the offset was not honoured and the
//     body starts at zero. Only 206 means it was. The status is the sole honest
//     signal, so it is checked rather than trusted.
//   - `download/all.json` accepts `files` and answers 200 with a valid but
//     empty archive. Only `folders` produces content (D42).
type DownloadService struct {
	c *Client
}

// Downloads returns the download pipeline bound to this client.
func (c *Client) Downloads() *DownloadService { return &DownloadService{c: c} }

// ProbeThreshold is the size above which Download checks with test=1 before
// opening a transfer. Below it the probe costs more than it saves.
const ProbeThreshold = 8 << 20

// DownloadProbe is the reply of download/file.json?test=1: a cheap, JSON answer
// to "could this be downloaded right now?".
type DownloadProbe struct {
	Result     FlexBool `json:"result"`
	StreamOK   FlexBool `json:"dl_stream_status"`
	BWExceeded FlexBool `json:"BWExceeded"`
}

// DownloadParams describes one download.
type DownloadParams struct {
	// FileID is the file to fetch. Required.
	FileID string
	// Offset is where to resume from. Zero downloads the whole file.
	Offset int64
	// Size, when known, lets Download decide whether the test=1 probe is worth
	// making and lets it verify what arrived.
	Size int64
	// Hash is the expected hex MD5. When set, the download is verified against
	// it and a mismatch is an error rather than a silently corrupt file.
	Hash string
	// SharingID scopes the call to a share.
	SharingID string
	// Inline asks for an inline Content-Disposition instead of an attachment.
	Inline bool
	// Probe forces the test=1 pre-flight on or off. When nil it is made for
	// anything at or above ProbeThreshold.
	Probe *bool
	// Progress, when set, is called as bytes land with the total when known.
	Progress func(written, total int64)
}

func (p *DownloadParams) validate() error {
	if p.FileID == "" {
		return invalidRequest("download needs a file id")
	}
	if p.Offset < 0 {
		return invalidRequest("download offset must not be negative")
	}
	if p.Hash != "" && len(p.Hash) != 32 {
		return invalidRequest("file_hash must be a 32 character hex MD5, got %d characters", len(p.Hash))
	}
	return nil
}

func (p *DownloadParams) wantsProbe() bool {
	if p.Probe != nil {
		return *p.Probe
	}
	return p.Size >= ProbeThreshold
}

// DownloadResult reports what a download did.
type DownloadResult struct {
	// Written counts the bytes handed to the destination in this call.
	Written int64
	// Offset is where this transfer started.
	Offset int64
	// Resumed reports whether upstream honoured the offset. When false and
	// Offset was non-zero, upstream sent the file from the beginning and the
	// leading bytes were discarded — see Download.
	Resumed bool
	// Total is the file size upstream reported, or -1 when it did not.
	Total int64
	// Name is the file name from Content-Disposition, when upstream sent one.
	Name string
	// Hash is the MD5 of the byte stream upstream sent. It covers the whole
	// file whenever the stream did — either the download started at zero, or
	// upstream ignored the resume range and sent everything anyway — and only
	// the fetched tail otherwise.
	Hash string
	// HashVerified is true when Hash covered the whole file and was compared
	// against the expected digest. A partial resume cannot be verified this
	// way, and says so rather than implying it was.
	HashVerified bool
}

// Probe asks whether a file can be downloaded right now, without transferring
// it.
//
// GET /download/file.json/{file_id}?test=1. The reply is JSON —
// {"result":true,"dl_stream_status":true} — which makes it a cheap pre-flight
// for a large transfer, and the one place an exhausted bandwidth allowance can
// be seen before committing to it.
func (s *DownloadService) Probe(ctx context.Context, fileID string, sharingID ...string) (*DownloadProbe, error) {
	if fileID == "" {
		return nil, invalidRequest("download probe needs a file id")
	}
	q := url.Values{"test": {"1"}}
	if len(sharingID) > 0 && sharingID[0] != "" {
		q.Set("sharing_id", sharingID[0])
	}

	var out DownloadProbe
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointDownloadFile,
		SessionPlacement: SessionInQuery,
		PathSegments:     []string{fileID},
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	if out.BWExceeded.Bool() {
		return &out, bandwidthExceeded(EndpointDownloadFile)
	}
	return &out, nil
}

// DownloadFile writes a file to a local path, resuming automatically when a
// partial file is already there.
//
// The local file's size is the resume offset, which is the only offset that can
// be trusted: it is what actually reached the disk. The file is opened for
// append, so an interrupted download continues rather than starting over.
func (s *DownloadService) DownloadFile(ctx context.Context, localPath string, p DownloadParams) (*DownloadResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if info, err := os.Stat(localPath); err == nil && !info.IsDir() {
		p.Offset = info.Size()
	}
	if p.Size > 0 && p.Offset == p.Size {
		// Already complete. Saying so is more useful than asking upstream for
		// zero bytes, which it answers by sending the whole file again (D41).
		return &DownloadResult{Offset: p.Offset, Total: p.Size, Resumed: true}, nil
	}

	f, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the path is the caller's own
	if err != nil {
		return nil, invalidRequest("cannot open %s: %v", filepath.Base(localPath), err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(p.Offset, io.SeekStart); err != nil {
		return nil, invalidRequest("cannot seek %s: %v", filepath.Base(localPath), err)
	}

	// Download writes at the current position, and it skips forward through the
	// body itself when upstream ignores the range, so the file is correct
	// whichever way upstream answers.
	res, err := s.Download(ctx, f, p)
	if err != nil {
		return res, err
	}
	if err := f.Close(); err != nil {
		return res, invalidRequest("cannot flush %s: %v", filepath.Base(localPath), err)
	}
	return res, nil
}

// Download streams a file to dst.
//
// GET /download/file.json/{file_id}, session in the query string. The resume
// point travels as a Range header rather than as the documented offset
// parameter, because upstream's own parameter is off by one at the last byte
// (D41).
//
// When a resume is asked for and upstream answers 200 instead of 206, it has
// ignored the range and is sending the file from the start. That is not treated
// as an error: the leading bytes are discarded and the transfer continues, so
// the caller still ends up with the right content. The cost is bandwidth, and
// DownloadResult.Resumed reports which of the two happened.
func (s *DownloadService) Download(ctx context.Context, dst io.Writer, p DownloadParams) (*DownloadResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if dst == nil {
		return nil, invalidRequest("download needs a destination writer")
	}

	if p.wantsProbe() {
		if _, err := s.Probe(ctx, p.FileID, p.SharingID); err != nil {
			return nil, err
		}
	}

	q := url.Values{}
	if p.SharingID != "" {
		q.Set("sharing_id", p.SharingID)
	}
	if p.Inline {
		q.Set("inline", "1")
	}

	header := http.Header{}
	if p.Offset > 0 {
		header.Set("Range", fmt.Sprintf("bytes=%d-", p.Offset))
	}

	result := &DownloadResult{Offset: p.Offset, Total: -1}
	digest := md5.New() //nolint:gosec // protocol requirement, not a security use

	err := s.c.DoStream(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointDownloadFile,
		SessionPlacement: SessionInQuery,
		PathSegments:     []string{p.FileID},
		Query:            q,
		Header:           header,
		Accept:           "*/*",
	}, func(resp *StreamResponse) error {
		// Upstream answers an exhausted allowance with JSON where bytes were
		// expected. Content-Type is what gives it away, since a 200 says
		// nothing (docs/error-taxonomy.md T14).
		if isJSONContentType(resp.Header.Get("Content-Type")) {
			return s.readInsteadOfBytes(resp)
		}

		result.Resumed = resp.Status == http.StatusPartialContent
		result.Total = totalFromResponse(resp, result.Resumed, p.Offset)
		result.Name = filenameFromResponse(resp)

		// The digest is taken from the raw stream, before anything is skipped,
		// so that a transfer upstream restarted from zero still yields a digest
		// of the whole file rather than of the tail.
		body := io.TeeReader(resp.Body, digest)
		if p.Offset > 0 && !result.Resumed {
			// The range was ignored and the body starts at zero. Skip forward
			// rather than corrupting the destination with a second copy of the
			// leading bytes.
			s.c.Logger().Debug("upstream ignored the resume range; skipping forward instead",
				slog.Int64("offset", p.Offset))
			if _, err := io.CopyN(io.Discard, body, p.Offset); err != nil {
				return &APIError{Kind: KindInvalidResponse, Op: "GET " + EndpointDownloadFile,
					UpstreamMsg: fmt.Sprintf("upstream ignored the resume range and then ended "+
						"before offset %d", p.Offset)}
			}
		}

		written, err := copyWithProgress(dst, body, result.Total, p.Progress)
		result.Written = written
		return err
	})
	if err != nil {
		return result, err
	}

	result.Hash = hex.EncodeToString(digest.Sum(nil))
	// The digest covers the whole file whenever the stream did: either nothing
	// was asked to be skipped, or upstream ignored the range and sent all of it.
	if (p.Offset == 0 || !result.Resumed) && p.Hash != "" {
		result.HashVerified = true
		if !strings.EqualFold(result.Hash, p.Hash) {
			return result, &APIError{
				Kind: KindInvalidResponse, Op: "GET " + EndpointDownloadFile,
				UpstreamMsg: fmt.Sprintf("downloaded content hashes to %s but %s was expected",
					result.Hash, p.Hash),
			}
		}
	}
	return result, nil
}

// Archive downloads folders as a single ZIP.
//
// POST /download/all.json, session_id in the JSON body — not session_key, whose
// only mention is in the PDF and which upstream ignores (D1, verified live).
//
// Only folders are honoured. The endpoint accepts a `files` list, answers 200
// and produces a structurally valid archive containing nothing at all, which is
// why this method does not offer the parameter: an empty ZIP presented as a
// successful download is worse than no feature (D42).
func (s *DownloadService) Archive(ctx context.Context, dst io.Writer, folderIDs []string) (int64, error) {
	if len(folderIDs) == 0 {
		return 0, invalidRequest("an archive download needs at least one folder id")
	}
	ids, err := joinIDs(folderIDs)
	if err != nil {
		return 0, err
	}

	var written int64
	err = s.c.DoStream(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointDownloadAll,
		SessionPlacement: SessionInBody,
		Body:             map[string]string{"folders": ids},
		Accept:           "*/*",
	}, func(resp *StreamResponse) error {
		if isJSONContentType(resp.Header.Get("Content-Type")) {
			return s.readInsteadOfBytes(resp)
		}
		n, copyErr := io.Copy(dst, resp.Body)
		written = n
		return copyErr
	})
	return written, err
}

// ---------------------------------------------------------------- helpers

// readInsteadOfBytes handles a 2xx that carries JSON where content was expected.
// Upstream reports an exhausted bandwidth allowance this way, and the
// classification layer decides what the body means — nothing here interprets it
// (docs/error-taxonomy.md).
func (s *DownloadService) readInsteadOfBytes(resp *StreamResponse) error {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return &APIError{Kind: KindInvalidResponse, Op: "GET " + EndpointDownloadFile, Err: err}
	}
	var probe DownloadProbe
	if json.Unmarshal(raw, &probe) == nil && probe.BWExceeded.Bool() {
		return bandwidthExceeded(EndpointDownloadFile)
	}
	if apiErr := errorInBody(raw, "GET "+EndpointDownloadFile, EndpointDownloadFile, "", ""); apiErr != nil {
		return apiErr
	}
	// A success-shaped JSON body where bytes belong is not a download.
	return &APIError{
		Kind: KindInvalidResponse, Op: "GET " + EndpointDownloadFile,
		UpstreamMsg: "upstream answered with JSON where file content was expected: " +
			truncate(strings.TrimSpace(string(raw)), 200),
	}
}

// bandwidthExceeded is the one download verdict that is never retried: the
// allowance does not return within any retry window, and hammering it is how an
// account gets throttled further (docs/error-taxonomy.md T14).
func bandwidthExceeded(endpoint string) *APIError {
	return &APIError{
		Kind: KindBandwidthExceeded, Op: "GET " + endpoint,
		UpstreamMsg: "the account's download bandwidth allowance is exhausted",
		retry:       retryNo,
	}
}

func isJSONContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "json")
}

// totalFromResponse works out the file size. On a 206 the Content-Range carries
// it; otherwise Content-Length does, and it is the whole file rather than the
// remainder.
func totalFromResponse(resp *StreamResponse, resumed bool, offset int64) int64 {
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
				return n
			}
		}
	}
	if resp.ContentLength < 0 {
		return -1
	}
	if resumed {
		return resp.ContentLength + offset
	}
	return resp.ContentLength
}

// filenameFromResponse reads the name upstream suggests. It arrives RFC 5987
// encoded — attachment; filename*=UTF-8”probe.bin — which mime.ParseMediaType
// decodes for us.
func filenameFromResponse(resp *StreamResponse) string {
	cd := resp.Header.Get("Content-Disposition")
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// copyWithProgress streams src to dst, reporting progress as it goes. The buffer
// is what bounds memory: the file size never enters into it.
func copyWithProgress(dst io.Writer, src io.Reader, total int64, progress func(written, total int64)) (int64, error) {
	if progress == nil {
		return io.Copy(dst, src)
	}
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return written, writeErr
			}
			written += int64(n)
			progress(written, total)
		}
		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}
			return written, readErr
		}
	}
}
