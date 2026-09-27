//go:build integration

// This file is a probe, not a feature test, and the distinction matters.
//
// Whitepaper §10.2 asks two questions that the mock suite cannot answer and that
// no amount of reading the documentation will settle, because the documentation
// does not discuss either one:
//
//	(b) will one TempLocation accept concurrent, out-of-order chunks?
//	(c) will N concurrent bounded Range requests reassemble into the right file?
//
// The answers decide whether `parallel_chunks` and `parallel_ranges` are real
// configuration or decoration, and §10.2 forbids writing any concurrent transfer
// code before they are known. So this file measures and reports; it asserts only
// the things that would make a *measurement* untrustworthy (that the bytes it
// compares are the bytes it uploaded, say), and never that upstream behaves one
// way rather than the other. Whatever it finds goes into docs/discrepancies.md.
//
// It lives in package opendrive rather than opendrive_test because the upload
// protocol's steps — createFile, openFile, sendOneChunk — are unexported, and
// probing the real protocol is the whole point. Exporting them to make a probe
// possible would widen the SDK's surface for the convenience of a test.
//
// Run it with:
//
//	scripts/integration-test.sh -run 'TestProbe' -v
package opendrive

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement, mirrors the code under test
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- harness

// probeClient logs in with retries switched off.
//
// Retrying would defeat the measurement. A chunk refused for a bad offset is a
// permanent refusal and would not be retried anyway, but the sandbox also
// refuses writes at random (D39/D40), and a probe that silently retried could
// not tell "upstream rejected my concurrency" from "upstream was busy". Every
// response here is a first response.
func probeClient(t *testing.T) (*Client, context.Context) {
	t.Helper()

	user, pass := os.Getenv("ODB_SPEC_USER"), os.Getenv("ODB_SPEC_PASS")
	if user == "" || pass == "" {
		t.Skip("set ODB_SPEC_USER and ODB_SPEC_PASS (see scripts/integration-test.sh)")
	}

	c, err := New(WithRetryPolicy(RetryPolicy{Max: 0}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	auth := NewOAuth2(c)
	c.SetAuthenticator(auth)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	if err := auth.Login(ctx, user, pass); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c, ctx
}

// probeScratch makes a folder to work in and removes it afterwards, along with
// anything left inside it. Rule: never leave an upstream artefact behind — a
// create_file record nobody filled is invisible to the user and enough of them
// in one folder make upstream refuse further writes there (D39).
func probeScratch(t *testing.T, ctx context.Context, c *Client) string {
	t.Helper()
	root, err := c.Folders().List(ctx, "0", ListOptions{})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	parent := "0"
	// Prefer a writable subfolder if the account's root is read-only, which is
	// how the test account is set up.
	for i := range root.Folders {
		if strings.EqualFold(root.Folders[i].Name, "odb-scratch") {
			parent = root.Folders[i].FolderID.String()
			break
		}
	}

	name := fmt.Sprintf("odb-probe-%s-%d", time.Now().UTC().Format("20060102-150405"), os.Getpid())
	created, err := c.Folders().Create(ctx, CreateFolderParams{
		Name: name, ParentID: parent, Access: FolderPrivate,
		Description: "opendrive-bridge concurrency probe; safe to delete",
	})
	if err != nil {
		t.Skipf("cannot create a scratch folder under %s, so there is nothing to probe: %v", parent, err)
	}
	id := created.FolderID.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = c.Folders().Trash(ctx, []string{id})
		if err := c.Folders().Remove(ctx, []string{id}); err != nil {
			t.Logf("cleanup: scratch folder %s may remain: %v", id, err)
		}
	})
	return id
}

func probePayload(n int, seed string) []byte {
	block := []byte("opendrive-bridge concurrency probe " + seed + " ")
	out := make([]byte, 0, n+len(block))
	for len(out) < n {
		out = append(out, block...)
	}
	return out[:n]
}

func probeMD5(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol requirement
	return hex.EncodeToString(sum[:])
}

// report prints a block that is meant to be read by a person and transcribed
// into docs/discrepancies.md. A probe whose findings are buried in `-v` output
// nobody reads has not recorded anything.
func report(t *testing.T, title string, lines ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("┌─ PROBE RESULT ─────────────────────────────────────────────\n")
	b.WriteString("│ " + title + "\n")
	b.WriteString("├────────────────────────────────────────────────────────────\n")
	for _, l := range lines {
		b.WriteString("│ " + l + "\n")
	}
	b.WriteString("└────────────────────────────────────────────────────────────")
	t.Log(b.String())
}

// ---------------------------------------------------------------- (b) upload

// The first version of this probe fired four out-of-order chunks at once and
// concluded from how many were accepted. On 2026-09-27 it saw 4 of 4 accepted
// and a matching hash, and its conclusion branch said to go and implement
// parallel_chunks. The next three runs showed why that was luck: the requests
// had simply arrived in offset order. Acceptance depends on arrival order, so
// counting acceptances measures the network, not upstream. TestProbeChunksReadBack
// below asks the question in a form timing cannot answer for it.

// ---------------------------------------------------------------- (c) download

// rangeOutcome is one concurrent bounded-range request's fate.
type rangeOutcome struct {
	from, to     int64
	status       int
	contentRange string
	body         []byte
	err          error
	duration     time.Duration
}

// fetchRange asks for one bounded range. The public Download only sends an
// open-ended `bytes=N-`, because resuming is all it has ever needed; a segmented
// parallel download needs `bytes=N-M`, and whether upstream honours a bounded
// range at all is part of what this probe is for.
func fetchRange(ctx context.Context, c *Client, fileID string, from, to int64) rangeOutcome {
	out := rangeOutcome{from: from, to: to}
	header := http.Header{}
	header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))

	t0 := time.Now()
	var buf bytes.Buffer
	out.err = c.DoStream(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointDownloadFile,
		SessionPlacement: SessionInQuery,
		PathSegments:     []string{fileID},
		Header:           header,
		Accept:           "*/*",
	}, func(resp *StreamResponse) error {
		out.status = resp.Status
		out.contentRange = resp.Header.Get("Content-Range")
		// An exhausted allowance arrives as JSON where bytes were expected, and
		// the status says nothing about it (T14). Under concurrency this is one
		// of the things worth watching for, so it is captured rather than hidden.
		if isJSONContentType(resp.Header.Get("Content-Type")) {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("JSON where bytes were expected: %s", strings.TrimSpace(string(body)))
		}
		_, err := io.Copy(&buf, resp.Body)
		return err
	})
	out.body = buf.Bytes()
	out.duration = time.Since(t0)
	return out
}

// TestProbeConcurrentRangeDownloads answers §10.2(c).
//
// D41 established that the Range header works where upstream's own `offset`
// parameter is off by one at the last byte, so single-stream resume is settled.
// What is not settled is the three things a segmented download depends on:
// whether a *bounded* range is honoured, whether N of them in flight at once
// reassemble to the right bytes, and what upstream does when asked for more
// connections than it wants to give.
func TestProbeConcurrentRangeDownloads(t *testing.T) {
	c, ctx := probeClient(t)
	folder := probeScratch(t, ctx, c)

	// 6 MiB: large enough to segment meaningfully, small enough to stay under
	// ProbeThreshold's 8 MiB so the probe is not measuring the pre-flight too.
	const size = 6 << 20
	content := probePayload(size, "parallel-ranges")
	want := probeMD5(content)

	up := c.Uploads()
	res, err := up.Upload(ctx, bytes.NewReader(content), UploadParams{
		FolderID: folder,
		Name:     fmt.Sprintf("odb-probe-ranges-%d.bin", time.Now().Unix()),
		Size:     int64(len(content)),
		Hash:     want,
	})
	if err != nil {
		t.Skipf("cannot upload the file to read back, so there is nothing to probe: %v", err)
	}
	if res.File == nil {
		t.Fatal("upload returned no file")
	}
	fileID := res.File.FileID.String()
	t.Logf("uploaded %d bytes as %s (dedupe=%v)", size, fileID, res.Deduplicated)

	// --- does a bounded range work at all, on its own? ---
	single := fetchRange(ctx, c, fileID, 1024, 2047)
	singleLines := []string{
		fmt.Sprintf("Range: bytes=1024-2047  ->  status %d", single.status),
		fmt.Sprintf("Content-Range: %q", single.contentRange),
		fmt.Sprintf("bytes returned: %d (asked for 1024)", len(single.body)),
	}
	if single.err != nil {
		singleLines = append(singleLines, fmt.Sprintf("error: %v", single.err))
	}
	exact := single.err == nil && len(single.body) == 1024 &&
		bytes.Equal(single.body, content[1024:2048])
	singleLines = append(singleLines,
		fmt.Sprintf("the bytes are the right bytes: %v", exact))
	if single.status == http.StatusOK && len(single.body) == size {
		singleLines = append(singleLines, "",
			"upstream ignored the bound and sent the whole file. A segmented",
			"download is then impossible: N segments would each cost a full",
			"transfer. parallel_ranges must go.")
	}
	report(t, "§10.2(c) is a bounded Range honoured?", singleLines...)

	// --- N concurrent segments, reassembled ---
	for _, n := range []int{4, 8, 16} {
		probeSegments(t, ctx, c, fileID, content, want, n)
	}
}

// probeSegments splits the file into n parts, fetches them all at once, and
// reassembles. Three things are measured: whether every segment arrived, whether
// the result hashes to the original, and how upstream behaved as n grew — the
// connection limit §10.2(c) asks about shows up as failures that start at some n
// and not before.
func probeSegments(t *testing.T, ctx context.Context, c *Client, fileID string,
	content []byte, want string, n int,
) {
	t.Helper()
	size := int64(len(content))
	seg := size / int64(n)

	outcomes := make([]rangeOutcome, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from := int64(i) * seg
			to := from + seg - 1
			if i == n-1 {
				to = size - 1 // the last segment takes the remainder
			}
			<-start
			outcomes[i] = fetchRange(ctx, c, fileID, from, to)
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		lines     []string
		failures  int
		assembled = make([]byte, 0, size)
		slowest   time.Duration
	)
	for i, o := range outcomes {
		if o.duration > slowest {
			slowest = o.duration
		}
		wantLen := int(o.to - o.from + 1)
		switch {
		case o.err != nil:
			failures++
			lines = append(lines, fmt.Sprintf("  seg %2d [%8d-%8d]  FAILED  %v  [retryable=%v retry-after=%s after %s]",
				i, o.from, o.to, o.err, IsTemporary(o.err), RetryAfter(o.err), o.duration.Round(time.Millisecond)))
		case len(o.body) != wantLen:
			failures++
			lines = append(lines, fmt.Sprintf("  seg %2d [%8d-%8d]  status %d, %d bytes, wanted %d",
				i, o.from, o.to, o.status, len(o.body), wantLen))
		default:
			lines = append(lines, fmt.Sprintf("  seg %2d [%8d-%8d]  status %d, %d bytes  (%s)",
				i, o.from, o.to, o.status, len(o.body), o.duration.Round(time.Millisecond)))
		}
	}
	// Reassemble in offset order regardless of the order they came back in,
	// which is the whole idea of a segmented download.
	ordered := make([]rangeOutcome, len(outcomes))
	copy(ordered, outcomes)
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].from < ordered[b].from })
	for _, o := range ordered {
		assembled = append(assembled, o.body...)
	}
	got := probeMD5(assembled)

	head := []string{
		fmt.Sprintf("%d concurrent bounded ranges over %d bytes", n, size),
		fmt.Sprintf("slowest segment: %s", slowest.Round(time.Millisecond)),
		"",
	}
	tail := []string{
		"",
		fmt.Sprintf("failures: %d of %d", failures, n),
		fmt.Sprintf("reassembled %d bytes, md5 %s", len(assembled), got),
		fmt.Sprintf("expected            %s", want),
		fmt.Sprintf("MATCH: %v", strings.EqualFold(got, want)),
	}
	if failures == 0 && strings.EqualFold(got, want) {
		tail = append(tail, "",
			fmt.Sprintf("At n=%d a segmented download reassembles correctly.", n))
	} else if failures > 0 {
		tail = append(tail, "",
			fmt.Sprintf("At n=%d upstream refused %d segment(s). If lower n succeeded,", n, failures),
			"this is the concurrent-connection limit §10.2(c) asks about; record the",
			"largest n that worked and set parallel_ranges below it.")
	}
	report(t, fmt.Sprintf("§10.2(c) %d concurrent segments", n), append(append(head, lines...), tail...)...)
}

// ---------------------------------------------------------------- (b) verified

// probeReadBack downloads what upstream actually stored. close_file_upload's
// FileHash may be nothing more than the hash the client declared, and a 200 has
// never been evidence of anything here (D42, D43), so the only proof that
// concurrent chunks assembled correctly is the stored bytes. D44: a just-closed
// file can be briefly undownloadable, so it waits.
func probeReadBack(t *testing.T, ctx context.Context, c *Client, fileID string, size int64) []byte {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		out := fetchRange(ctx, c, fileID, 0, size-1)
		if out.err == nil && int64(len(out.body)) == size {
			return out.body
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not read the stored file back: status %d, %d bytes, %v",
				out.status, len(out.body), out.err)
		}
		time.Sleep(3 * time.Second)
	}
}

// probeUpload runs create/open, hands the temp location to send, closes and
// reads back. It reports what upstream stored, not what it said.
func probeUpload(t *testing.T, ctx context.Context, c *Client, folder, label string,
	content []byte, send func(fileID, temp string) []string) {
	t.Helper()
	up := c.Uploads()
	hash := probeMD5(content)
	created, err := up.createFile(ctx, folder, UploadParams{
		Name: fmt.Sprintf("odb-probe-%s-%d.bin", label, time.Now().UnixNano()),
		Size: int64(len(content)), Hash: hash,
	})
	if err != nil {
		t.Skipf("create_file refused: %v", err)
	}
	fileID := created.FileID.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := up.Reclaim(ctx, fileID, "", ""); err != nil {
			t.Logf("cleanup: upstream record %s may remain: %v", fileID, err)
		}
	})
	opened, err := up.openFile(ctx, fileID, UploadParams{Size: int64(len(content)), Hash: hash})
	if err != nil || opened.TempLocation == "" {
		t.Skipf("open_file_upload refused: %v", err)
	}
	lines := send(fileID, opened.TempLocation)

	closed, err := up.closeFile(ctx, fileID, opened.TempLocation,
		UploadParams{Size: int64(len(content)), Hash: hash}, false)
	if err != nil {
		lines = append(lines, "", fmt.Sprintf("close_file_upload FAILED: %v", err))
		report(t, label, lines...)
		return
	}
	said := ""
	if closed != nil {
		said = closed.FileHash
	}
	stored := probeReadBack(t, ctx, c, fileID, int64(len(content)))
	got := probeMD5(stored)
	lines = append(lines, "",
		"close said:     "+said,
		"stored bytes:   "+got,
		"expected:       "+hash,
		fmt.Sprintf("STORED CORRECTLY: %v", got == hash))
	if got != hash {
		first := -1
		for i := range stored {
			if stored[i] != content[i] {
				first = i
				break
			}
		}
		lines = append(lines, fmt.Sprintf("first differing byte: %d", first))
	}
	report(t, label, lines...)
}

func chunkLine(offset int64, written int64, err error, d time.Duration) string {
	if err != nil {
		return fmt.Sprintf("  offset %8d  REFUSED  %v  (%s)", offset, err, d.Round(time.Millisecond))
	}
	return fmt.Sprintf("  offset %8d  accepted written=%d  (%s)", offset, written, d.Round(time.Millisecond))
}

// TestProbeChunksReadBack repeats (b) and then checks the stored bytes, and adds
// the case that tells "random access" apart from "held until the cursor catches
// up": chunks 3, 1, 2 are sent concurrently and must have *answered* before
// chunk 0 is sent at all. A server that serialises on a cursor either refuses
// them or makes them wait, and the timings show which.
func TestProbeChunksReadBack(t *testing.T) {
	c, ctx := probeClient(t)
	folder := probeScratch(t, ctx, c)
	const chunkSize = 256 << 10
	up := c.Uploads()

	concurrent := func(content []byte, order []int) func(string, string) []string {
		return func(fileID, temp string) []string {
			lines := make([]string, len(order))
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i, idx := range order {
				wg.Add(1)
				go func(i, idx int) {
					defer wg.Done()
					off := int64(idx * chunkSize)
					<-start
					t0 := time.Now()
					w, err := up.sendOneChunk(ctx, fileID, temp, off, content[off:off+chunkSize])
					lines[i] = chunkLine(off, w, err, time.Since(t0))
				}(i, idx)
			}
			close(start)
			wg.Wait()
			return lines
		}
	}

	t.Run("concurrent-3102", func(t *testing.T) {
		content := probePayload(4*chunkSize, "rb-3102")
		probeUpload(t, ctx, c, folder, "§10.2(b) 4 concurrent chunks (3,1,0,2), read back",
			content, concurrent(content, []int{3, 1, 0, 2}))
	})
	t.Run("concurrent-8", func(t *testing.T) {
		content := probePayload(8*chunkSize, "rb-8")
		probeUpload(t, ctx, c, folder, "§10.2(b) 8 concurrent chunks (7..0), read back",
			content, concurrent(content, []int{7, 6, 5, 4, 3, 2, 1, 0}))
	})
	t.Run("zero-last", func(t *testing.T) {
		content := probePayload(4*chunkSize, "rb-zero-last")
		probeUpload(t, ctx, c, folder, "§10.2(b) chunks 3,1,2 answered BEFORE chunk 0 is sent",
			content, func(fileID, temp string) []string {
				lines := []string{"phase 1: offsets 3,1,2 concurrently, no chunk 0 yet"}
				lines = append(lines, concurrent(content, []int{3, 1, 2})(fileID, temp)...)
				lines = append(lines, "phase 2: offset 0, alone, after the others answered")
				t0 := time.Now()
				w, err := up.sendOneChunk(ctx, fileID, temp, 0, content[:chunkSize])
				return append(lines, chunkLine(0, w, err, time.Since(t0)))
			})
	})
	t.Run("sequential-reverse", func(t *testing.T) {
		content := probePayload(4*chunkSize, "rb-seq-rev")
		probeUpload(t, ctx, c, folder, "§10.2(b) one at a time, last chunk first (3,2,1,0)",
			content, func(fileID, temp string) []string {
				var lines []string
				for _, idx := range []int{3, 2, 1, 0} {
					off := int64(idx * chunkSize)
					t0 := time.Now()
					w, err := up.sendOneChunk(ctx, fileID, temp, off, content[off:off+chunkSize])
					lines = append(lines, chunkLine(off, w, err, time.Since(t0)))
				}
				return lines
			})
	})
}

// ---------------------------------------------------------------- (d) files

// TestProbeConcurrentFileUploads measures what one TempLocation cannot give:
// several whole files in flight at once. If each connection is throttled (the
// range probe measured ~200 KB/s per stream), this is where upload throughput
// comes from, and the question is how many upstream allows from one IP before
// it refuses — the downloads stop at six (D49).
func TestProbeConcurrentFileUploads(t *testing.T) {
	c, ctx := probeClient(t)
	folder := probeScratch(t, ctx, c)
	const size = 512 << 10
	for _, n := range []int{1, 4, 8, 12} {
		type result struct {
			err error
			d   time.Duration
		}
		results := make([]result, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				content := probePayload(size, fmt.Sprintf("files-%d-%d", n, i))
				<-start
				s := time.Now()
				res, err := c.Uploads().Upload(ctx, bytes.NewReader(content), UploadParams{
					FolderID: folder, Name: fmt.Sprintf("odb-probe-files-%d-%d.bin", n, i),
					Size: size, Hash: probeMD5(content),
				})
				if err == nil && res != nil && res.File != nil {
					fid := res.File.FileID.String()
					t.Cleanup(func() {
						ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
						defer cancel()
						_ = c.Uploads().Reclaim(ctx, fid, "", "")
					})
				}
				results[i] = result{err, time.Since(s)}
			}(i)
		}
		t0 := time.Now()
		close(start)
		wg.Wait()
		wall := time.Since(t0)
		ok := 0
		lines := []string{}
		for i, r := range results {
			if r.err == nil {
				ok++
				lines = append(lines, fmt.Sprintf("  file %2d  ok      (%s)", i, r.d.Round(time.Millisecond)))
			} else {
				lines = append(lines, fmt.Sprintf("  file %2d  FAILED  %v", i, r.err))
			}
		}
		rate := float64(ok*size) / wall.Seconds() / 1024
		lines = append(lines, "", fmt.Sprintf("%d of %d stored; wall %s; aggregate %.0f KB/s",
			ok, n, wall.Round(time.Millisecond), rate))
		report(t, fmt.Sprintf("§10.2(d) %d files of %d KiB uploaded at once", n, size>>10), lines...)
		time.Sleep(5 * time.Second)
	}
}

// TestProbeAccountLimits records what the account says its limits are, so the
// throughput the other probes measure can be read against the plan rather than
// taken as a property of upstream in general.
func TestProbeAccountLimits(t *testing.T) {
	c, ctx := probeClient(t)
	info, err := c.Users().Info(ctx)
	if err != nil {
		t.Fatalf("users info: %v", err)
	}
	report(t, "account limits the throughput probes ran under",
		"plan:                 "+info.UserPlan,
		fmt.Sprintf("UploadSpeedLimit:     %d", info.UploadSpeedLimit.Int64()),
		fmt.Sprintf("DownloadSpeedLimit:   %d", info.DownloadSpeedLimit.Int64()),
		fmt.Sprintf("BwMax / BwUsed:       %d / %d", info.BwMax.Int64(), info.BwUsed.Int64()),
		fmt.Sprintf("MaxFileSize:          %d", info.MaxFileSize.Int64()),
		fmt.Sprintf("account user:         %v", info.IsAccountUser()))
}
