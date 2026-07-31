package opendrive

import (
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrored here to measure it
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Whitepaper §10 asks for a 1 GB transfer baseline. These benchmarks run the
// real upload and download pipelines against a local upstream that discards
// what it is given, so what is measured is our own cost — hashing, chunking,
// buffering and the multipart wrapper — with the network taken out.
//
// The point is not the absolute number. It is the ratio to BenchmarkReferenceMD5,
// which does the one piece of work the pipeline cannot avoid: an MD5 over the
// same bytes. A fixed MB/s threshold on shared CI runners mostly measures which
// runner you were given that morning, and would cry wolf until someone turned it
// off. The ratio is a property of the code. scripts/check-bench.sh is what
// enforces it.

// benchSize is 1 GB by default and can be lowered for a quick local run:
//
//	ODB_BENCH_SIZE=64MiB go test -bench=Transfer ./pkg/opendrive/
func benchSize(tb testing.TB) int64 {
	tb.Helper()
	raw := os.Getenv("ODB_BENCH_SIZE")
	if raw == "" {
		return 1 << 30
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(raw, "GiB"):
		mult, raw = 1<<30, strings.TrimSuffix(raw, "GiB")
	case strings.HasSuffix(raw, "MiB"):
		mult, raw = 1<<20, strings.TrimSuffix(raw, "MiB")
	case strings.HasSuffix(raw, "KiB"):
		mult, raw = 1<<10, strings.TrimSuffix(raw, "KiB")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		tb.Fatalf("ODB_BENCH_SIZE=%q is not a size", os.Getenv("ODB_BENCH_SIZE"))
	}
	return n * mult
}

// zeroes is a source of n bytes that costs nothing to produce, so the benchmark
// measures the pipeline rather than the generator. The buffer is reused and
// never re-filled: its contents do not matter, only its size.
type zeroes struct {
	left int64
	buf  []byte
}

func newZeroes(n int64) *zeroes { return &zeroes{left: n, buf: make([]byte, 256<<10)} }

func (z *zeroes) Read(p []byte) (int, error) {
	if z.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > z.left {
		n = int(z.left)
	}
	z.left -= int64(n)
	return n, nil
}

// benchUpstream answers the four upload calls and the download stream without
// holding a chunk in memory. The production mock in testing_test.go reads every
// body with io.ReadAll, which is right for asserting on requests and wrong here:
// at 50 MB a chunk it would measure the test harness.
//
// It is a RoundTripper rather than an httptest.Server, and that change is the
// difference between a gate that works and one that does not. Over a real
// loopback socket the upload benchmark also measured the kernel, net/http's
// server goroutines and the garbage they make, and those dominate on a shared
// four-core runner: the same unchanged code came out at 1.41x, 1.61x and 1.67x
// of the reference on consecutive CI runs while measuring 1.28x on a laptop.
// A regression gate cannot live with a 19% spread it does not control.
//
// What remains is ours: reading the source, hashing it, chunking it, and
// building the multipart body. Those are the things a change to the transfer
// path would make slower, and they are what the gate is for.
type benchTransport struct {
	size    int64
	handler http.HandlerFunc
}

func (t benchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The file content is streamed rather than recorded. httptest.NewRecorder
	// buffers everything written to it, which for a 1 GB download would mean
	// holding the whole file in memory and measuring the allocator instead of
	// the pipeline.
	if strings.Contains(r.URL.Path, EndpointDownloadFile) && r.URL.Query().Get("test") != "1" {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			Header:        http.Header{"Content-Type": {"application/octet-stream"}},
			ContentLength: t.size,
			Body:          io.NopCloser(newZeroes(t.size)),
			Request:       r,
		}, nil
	}
	rec := httptest.NewRecorder()
	t.handler(rec, r)
	if r.Body != nil {
		_ = r.Body.Close()
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func benchUpstream(tb testing.TB, size int64) benchTransport {
	tb.Helper()
	return benchTransport{size: size, handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, EndpointUploadCreateFile):
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"FileId":"bench-file","DirUpdateTime":1,"TempLocation":"/tmp/bench","RequireCompression":false,"RequireHashOnly":false,"SpeedLimit":0}`)

		case strings.HasSuffix(path, EndpointUploadOpenFile):
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"TempLocation":"/tmp/bench","RequireCompression":false,"RequireHashOnly":false,"SpeedLimit":0}`)

		case strings.Contains(path, EndpointUploadChunk):
			// Drained, not buffered. This is the hot path.
			n, _ := io.Copy(io.Discard, r.Body)
			// The wire body is multipart, so the payload is a little smaller
			// than what arrived; the client checks TotalWritten against the
			// chunk it sent (D35), so answer with that rather than with n.
			size := r.URL.Query().Get("chunk_size")
			if size == "" {
				size = strconv.FormatInt(n, 10)
			}
			_, _ = fmt.Fprintf(w, `{"TotalWritten":%s}`, size)

		case strings.HasSuffix(path, EndpointUploadCloseFile):
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, `{"FileId":"bench-file","Name":"bench.bin","Size":"0","DateModified":"1785122523"}`)

		case strings.Contains(path, EndpointDownloadFile):
			// Anything at or above ProbeThreshold is preceded by a test=1
			// pre-flight, which answers JSON rather than content. Serving the
			// file for it would hand the decoder a gigabyte of zeros to parse.
			if r.URL.Query().Get("test") == "1" {
				_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true,"BWExceeded":false}`)
				return
			}
			// Content is handled in RoundTrip, above, so that it streams.
			w.WriteHeader(http.StatusInternalServerError)

		default:
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = io.WriteString(w, `{"error":{"code":501,"message":"not part of the benchmark"}}`)
		}
	}}
}

func benchClient(tb testing.TB, upstream benchTransport) *Client {
	tb.Helper()
	c, err := New(
		WithBaseURL("http://bench.invalid/api/v1"),
		WithHTTPClient(&http.Client{Transport: upstream}),
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "bench-session"}}),
	)
	if err != nil {
		tb.Fatalf("client: %v", err)
	}
	return c
}

// BenchmarkReferenceMD5 is the yardstick: the hash the upload pipeline has to
// compute anyway, over the same number of bytes, with nothing else happening.
// Every other number here is reported against it.
func BenchmarkReferenceMD5(b *testing.B) {
	size := benchSize(b)
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := md5.New() //nolint:gosec // measuring the protocol's hash, not securing anything
		if _, err := io.CopyN(h, newZeroes(size), size); err != nil {
			b.Fatal(err)
		}
		h.Sum(nil)
	}
}

func BenchmarkTransferUpload(b *testing.B) {
	size := benchSize(b)
	srv := benchUpstream(b, size)
	c := benchClient(b, srv)
	ctx := context.Background()

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.Uploads().Upload(ctx, newZeroes(size), UploadParams{
			FolderID: "bench-folder",
			Name:     "bench.bin",
			Size:     size,
		})
		if err != nil {
			b.Fatalf("upload: %v", err)
		}
		if res.BytesSent != size {
			b.Fatalf("sent %d bytes of %d", res.BytesSent, size)
		}
	}
}

func BenchmarkTransferDownload(b *testing.B) {
	size := benchSize(b)
	srv := benchUpstream(b, size)
	c := benchClient(b, srv)
	ctx := context.Background()

	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.Downloads().Download(ctx, io.Discard, DownloadParams{
			FileID: "bench-file",
			Size:   size,
		})
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		if res.Written != size {
			b.Fatalf("wrote %d bytes of %d", res.Written, size)
		}
	}
}
