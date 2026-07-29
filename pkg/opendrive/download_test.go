package opendrive

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrors the code under test
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func md5of(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol requirement
	return hex.EncodeToString(sum[:])
}

func downloadClient(m *mockUpstream) *Client {
	return m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}),
		WithAccessProbe(nil))
}

// serveContent answers download/file.json the way the live endpoint does:
// 206 with a Content-Range for an honoured Range, 200 with the whole file
// otherwise, and JSON for test=1.
func serveContent(t *testing.T, content []byte, honourRange bool) *mockUpstream {
	t.Helper()
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Query().Get("test") == "1" {
			_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true}`)
			return
		}
		rng := r.Header.Get("Range")
		if rng != "" && honourRange {
			var off int64
			_, _ = fmt.Sscanf(rng, "bytes=%d-", &off)
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", off, len(content)-1, len(content)))
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[off:])
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''probe.bin`)
		_, _ = w.Write(content)
	})
	return m
}

func TestDownloadStreamsTheWholeFile(t *testing.T) {
	content := bytes.Repeat([]byte("opendrive "), 500)
	m := serveContent(t, content, true)

	var buf bytes.Buffer
	res, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{
		FileID: "F1", Hash: md5of(content),
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatal("downloaded content does not match")
	}
	if res.Written != int64(len(content)) {
		t.Errorf("written = %d, want %d", res.Written, len(content))
	}
	if !res.HashVerified || res.Hash != md5of(content) {
		t.Errorf("hash = %q verified=%v, want the content digest checked", res.Hash, res.HashVerified)
	}
	if res.Name != "probe.bin" {
		t.Errorf("name = %q, want the RFC 5987 filename decoded", res.Name)
	}
}

// A wrong digest must be an error, not a quietly corrupt file.
func TestDownloadRejectsAHashMismatch(t *testing.T) {
	content := []byte("the real content")
	m := serveContent(t, content, true)

	var buf bytes.Buffer
	_, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{
		FileID: "F1", Hash: md5of([]byte("something else entirely")),
	})
	if err == nil {
		t.Fatal("want a hash mismatch to fail")
	}
	if ErrorKind(err) != KindInvalidResponse {
		t.Errorf("kind = %q, want %q", ErrorKind(err), KindInvalidResponse)
	}
}

func TestDownloadResumesFromAnOffset(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100)
	m := serveContent(t, content, true)

	var buf bytes.Buffer
	res, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{
		FileID: "F1", Offset: 400,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !res.Resumed {
		t.Error("upstream sent a 206 but the result does not report a resume")
	}
	if !bytes.Equal(buf.Bytes(), content[400:]) {
		t.Fatal("resumed content is not the tail")
	}
	if res.Total != int64(len(content)) {
		t.Errorf("total = %d, want %d from the Content-Range", res.Total, len(content))
	}
	// The digest covers only what was fetched, so it must not claim a check.
	if res.HashVerified {
		t.Error("a partial download reported its hash as verified")
	}
}

// D41: upstream answers 200 with the whole file when it will not honour the
// range — at the last byte it does this even though the request is valid. The
// pipeline must notice and skip forward, because appending the body to a partial
// file would produce a file with its opening bytes repeated in the middle.
func TestDownloadSkipsForwardWhenUpstreamIgnoresTheRange(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 100)
	m := serveContent(t, content, false) // never honours Range

	var buf bytes.Buffer
	res, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{
		FileID: "F1", Offset: 400,
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if res.Resumed {
		t.Error("a 200 was reported as a resume")
	}
	if !bytes.Equal(buf.Bytes(), content[400:]) {
		t.Fatalf("want the tail after skipping, got %d bytes", buf.Len())
	}
	// The digest is taken before the skip, so it still covers the whole file and
	// can still be checked.
	if res.Hash != md5of(content) {
		t.Errorf("hash = %q, want the digest of the whole file", res.Hash)
	}
}

// The same case on disk: the file must end up correct, not longer than the
// original with a duplicated middle.
func TestDownloadFileResumesOnDisk(t *testing.T) {
	content := bytes.Repeat([]byte("abcdefghij"), 100)
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.bin")
	if err := os.WriteFile(path, content[:350], 0o600); err != nil {
		t.Fatal(err)
	}

	for _, honour := range []bool{true, false} {
		t.Run(map[bool]string{true: "upstream honours the range", false: "upstream ignores it"}[honour],
			func(t *testing.T) {
				if err := os.WriteFile(path, content[:350], 0o600); err != nil {
					t.Fatal(err)
				}
				m := serveContent(t, content, honour)
				res, err := downloadClient(m).Downloads().DownloadFile(context.Background(), path,
					DownloadParams{FileID: "F1", Size: int64(len(content)), Hash: md5of(content)})
				if err != nil {
					t.Fatalf("download: %v", err)
				}
				got, err := os.ReadFile(path) //nolint:gosec // test temp file
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, content) {
					t.Fatalf("file is %d bytes, want %d and identical", len(got), len(content))
				}
				if honour && !res.Resumed {
					t.Error("a 206 was not reported as a resume")
				}
			})
	}
}

// A complete local file needs no request at all: asking upstream for a zero
// length tail is exactly the case where it sends everything again (D41).
func TestDownloadFileSkipsACompleteFile(t *testing.T) {
	content := []byte("already complete")
	dir := t.TempDir()
	path := filepath.Join(dir, "done.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	m := serveContent(t, content, true)

	res, err := downloadClient(m).Downloads().DownloadFile(context.Background(), path,
		DownloadParams{FileID: "F1", Size: int64(len(content))})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if res.Written != 0 {
		t.Errorf("wrote %d bytes for a complete file", res.Written)
	}
	if m.callCount() != 0 {
		t.Errorf("made %d upstream calls for a file already on disk", m.callCount())
	}
}

// T14: an exhausted allowance arrives as JSON where bytes belong, and is never
// retried.
func TestDownloadReportsBandwidthExceeded(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"result":false,"BWExceeded":1}`)
	})

	var buf bytes.Buffer
	_, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{FileID: "F1"})
	if ErrorKind(err) != KindBandwidthExceeded {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindBandwidthExceeded)
	}
	if IsTemporary(err) {
		t.Error("bandwidth exhaustion must never be retried")
	}
	if buf.Len() != 0 {
		t.Error("a JSON refusal was written to the destination as if it were content")
	}
}

func TestDownloadProbeReportsBandwidthExceeded(t *testing.T) {
	m := newMockUpstream(t)
	m.push(http.StatusOK, `{"result":false,"BWExceeded":1}`)

	_, err := downloadClient(m).Downloads().Probe(context.Background(), "F1")
	if ErrorKind(err) != KindBandwidthExceeded {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindBandwidthExceeded)
	}
}

func TestDownloadProbesLargeFilesFirst(t *testing.T) {
	content := []byte("small enough")
	m := serveContent(t, content, true)
	c := downloadClient(m)

	var buf bytes.Buffer
	if _, err := c.Downloads().Download(context.Background(), &buf,
		DownloadParams{FileID: "F1", Size: 10}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got := m.callCount(); got != 1 {
		t.Errorf("made %d calls for a small file, want 1: no probe was warranted", got)
	}

	buf.Reset()
	if _, err := c.Downloads().Download(context.Background(), &buf,
		DownloadParams{FileID: "F1", Size: ProbeThreshold}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got := m.callCount(); got != 3 {
		t.Errorf("made %d calls in total, want 3: a large file is probed first", got)
	}
}

// An error body must reach the classification layer rather than the destination.
func TestDownloadClassifiesAnErrorResponse(t *testing.T) {
	m := newMockUpstream(t)
	m.push(http.StatusNotFound, `{"error":{"code":404,"message":"File is deleted"}}`)

	var buf bytes.Buffer
	_, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{FileID: "F1"})
	if ErrorKind(err) != KindNotFound {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindNotFound)
	}
	if buf.Len() != 0 {
		t.Error("an error body was written to the destination")
	}
}

func TestDownloadProgressIsMonotonic(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 700<<10)
	m := serveContent(t, content, true)

	var last int64
	var calls int
	var buf bytes.Buffer
	_, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{
		FileID: "F1",
		Progress: func(written, total int64) {
			calls++
			if written < last {
				t.Errorf("progress went backwards: %d after %d", written, last)
			}
			last = written
		},
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if calls < 2 {
		t.Errorf("progress was reported %d times for a multi-buffer file", calls)
	}
	if last != int64(len(content)) {
		t.Errorf("final progress = %d, want %d", last, len(content))
	}
}

// D42: only folders produce an archive, so files are not offered at all.
func TestArchiveSendsFoldersOnly(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK\x03\x04 pretend archive"))
	})

	var buf bytes.Buffer
	n, err := downloadClient(m).Downloads().Archive(context.Background(), &buf, []string{"D1", "D2"})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n != int64(buf.Len()) || !bytes.HasPrefix(buf.Bytes(), []byte("PK")) {
		t.Errorf("wrote %d bytes, buffer holds %d", n, buf.Len())
	}
	call := m.lastCall()
	if call.Body["folders"] != "D1,D2" {
		t.Errorf("folders = %v, want the ids joined", call.Body["folders"])
	}
	if _, ok := call.Body["files"]; ok {
		t.Error("a files parameter was sent; upstream answers it with an empty archive (D42)")
	}
	// D1: the session parameter is session_id, not the PDF's session_key.
	if call.SessionValue("session_id") == "" {
		t.Error("no session_id in the archive request body (D1)")
	}
	if _, ok := call.Body["session_key"]; ok {
		t.Error("session_key was sent; upstream ignores it and answers 403 (D1, verified live)")
	}
}

func TestArchiveNeedsAFolder(t *testing.T) {
	m := newMockUpstream(t)
	if _, err := downloadClient(m).Downloads().Archive(context.Background(), io.Discard, nil); err == nil {
		t.Fatal("want an error with no folder ids")
	}
}

func TestDownloadValidatesItsParameters(t *testing.T) {
	m := newMockUpstream(t)
	d := downloadClient(m).Downloads()
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		p    DownloadParams
	}{
		{"no file id", DownloadParams{}},
		{"negative offset", DownloadParams{FileID: "F1", Offset: -1}},
		{"short hash", DownloadParams{FileID: "F1", Hash: "abc"}},
	} {
		if _, err := d.Download(ctx, io.Discard, tc.p); ErrorKind(err) != KindInvalidRequest {
			t.Errorf("%s: kind = %q, want %q", tc.name, ErrorKind(err), KindInvalidRequest)
		}
	}
	if _, err := d.Download(ctx, nil, DownloadParams{FileID: "F1"}); err == nil {
		t.Error("want an error for a nil destination")
	}
	if m.callCount() != 0 {
		t.Error("a caller mistake reached the network")
	}
}

// A JSON body where content belongs, with nothing in it we recognise, is not a
// download — and must not be written to disk as though it were.
func TestDownloadRefusesJSONWhereContentBelongs(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true}`)
	})

	var buf bytes.Buffer
	_, err := downloadClient(m).Downloads().Download(context.Background(), &buf, DownloadParams{FileID: "F1"})
	if ErrorKind(err) != KindInvalidResponse {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindInvalidResponse)
	}
	if buf.Len() != 0 {
		t.Error("JSON was written to the destination")
	}
	var ae *APIError
	if errors.As(err, &ae) && !strings.Contains(ae.UpstreamMsg, "JSON where file content") {
		t.Errorf("message = %q, want it to name the problem", ae.UpstreamMsg)
	}
}
