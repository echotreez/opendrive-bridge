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

// probeChunkOutcome is one concurrent chunk's fate.
type probeChunkOutcome struct {
	seq      int   // the order this chunk was fired in
	offset   int64 // the offset it claimed
	size     int
	written  int64
	err      error
	duration time.Duration
}

// TestProbeConcurrentChunksToOneTempLocation answers §10.2(b).
//
// The existing evidence says this will be refused: D37 shows upstream comparing
// the requested chunk_offset against the number of bytes it has already received
// ("uploaded=0, chunk_offset=999999"), and D35 shows TotalWritten reporting the
// current chunk rather than a running total. Both describe a single linear write
// cursor per TempLocation. If that is right, only one of a set of concurrent
// out-of-order chunks can be at the cursor, and the rest must fail.
//
// Being fairly sure is not the same as knowing, which is why §10.2 asks for this
// rather than letting someone delete the option on the strength of an inference.
func TestProbeConcurrentChunksToOneTempLocation(t *testing.T) {
	c, ctx := probeClient(t)
	folder := probeScratch(t, ctx, c)

	const (
		chunkSize = 256 << 10 // 256 KiB, small enough to be quick and big enough to be real
		chunks    = 4
	)
	content := probePayload(chunkSize*chunks, "parallel-chunks")
	hash := probeMD5(content)

	up := c.Uploads()
	created, err := up.createFile(ctx, folder, UploadParams{
		Name: fmt.Sprintf("odb-probe-parallel-%d.bin", time.Now().Unix()),
		Size: int64(len(content)),
		Hash: hash,
	})
	if err != nil {
		t.Skipf("create_file refused, so there is nothing to probe: %v", err)
	}
	fileID := created.FileID.String()
	// The record must not survive this test whatever happens next, filled or not.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := up.Reclaim(ctx, fileID, "", ""); err != nil {
			t.Logf("cleanup: upstream record %s may remain: %v", fileID, err)
		}
	})

	opened, err := up.openFile(ctx, fileID, UploadParams{Size: int64(len(content)), Hash: hash})
	if err != nil {
		t.Skipf("open_file_upload refused, so there is nothing to probe: %v", err)
	}
	temp := opened.TempLocation
	if temp == "" {
		t.Fatal("open_file_upload returned no temp_location; the probe cannot proceed")
	}

	// Deliberately scrambled: the last chunk first, then the second, the first,
	// the third. If a linear cursor is what upstream keeps, exactly one of these
	// (offset 0) can possibly succeed.
	order := []int{3, 1, 0, 2}
	outcomes := make([]probeChunkOutcome, len(order))

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, idx := range order {
		wg.Add(1)
		go func(seq, idx int) {
			defer wg.Done()
			offset := int64(idx * chunkSize)
			payload := content[offset : offset+chunkSize]
			<-start // fire together, so the requests genuinely overlap
			t0 := time.Now()
			written, err := up.sendOneChunk(ctx, fileID, temp, offset, payload)
			outcomes[seq] = probeChunkOutcome{
				seq: seq, offset: offset, size: len(payload),
				written: written, err: err, duration: time.Since(t0),
			}
		}(i, idx)
	}
	close(start)
	wg.Wait()

	lines := []string{
		fmt.Sprintf("%d chunks of %d bytes fired simultaneously at one temp_location,", chunks, chunkSize),
		"offsets presented out of order (3, 1, 0, 2):",
		"",
	}
	accepted := 0
	for _, o := range outcomes {
		switch {
		case o.err == nil:
			accepted++
			lines = append(lines, fmt.Sprintf("  offset %8d  ACCEPTED  written=%d  (%s)",
				o.offset, o.written, o.duration.Round(time.Millisecond)))
		default:
			kind := "?"
			var apiErr *APIError
			if asAPIError(o.err, &apiErr) {
				kind = string(apiErr.Kind)
				if apiErr.UpstreamMsg != "" {
					kind += ": " + apiErr.UpstreamMsg
				}
			} else {
				kind = o.err.Error()
			}
			lines = append(lines, fmt.Sprintf("  offset %8d  REFUSED   %s  (%s)",
				o.offset, kind, o.duration.Round(time.Millisecond)))
		}
	}
	lines = append(lines, "", fmt.Sprintf("accepted %d of %d", accepted, len(outcomes)))
	switch {
	case accepted <= 1:
		lines = append(lines,
			"",
			"CONCLUSION: one temp_location does not accept concurrent out-of-order",
			"chunks, which matches D35 and D37 — the server keeps a single linear",
			"write cursor. parallel_chunks cannot be implemented and must be removed",
			"rather than left in the configuration describing something impossible.")
	default:
		lines = append(lines,
			"",
			"CONCLUSION: more than one concurrent chunk was accepted, which contradicts",
			"the inference from D35/D37. Record the exact conditions before writing any",
			"code: which offsets, in what order, and whether close_file_upload then",
			"produced the right hash.")
	}
	report(t, "§10.2(b) concurrent out-of-order chunks to one temp_location", lines...)

	// Whether the assembled file is correct is a separate question from whether
	// the chunks were accepted, and it is the one that matters: upstream could
	// accept everything and still produce rubbish. Only ask if it is worth asking.
	if accepted == len(outcomes) {
		closed, err := up.closeFile(ctx, fileID, temp, UploadParams{
			Size: int64(len(content)), Hash: hash,
		}, false)
		if err != nil {
			report(t, "§10.2(b) follow-up: close_file_upload after concurrent chunks",
				fmt.Sprintf("every chunk was accepted but the close failed: %v", err),
				"so the acceptances did not amount to a stored file.")
			return
		}
		got := ""
		if closed != nil {
			got = closed.FileHash
		}
		report(t, "§10.2(b) follow-up: close_file_upload after concurrent chunks",
			fmt.Sprintf("upstream hash: %s", got),
			fmt.Sprintf("expected:      %s", hash),
			fmt.Sprintf("match: %v", strings.EqualFold(got, hash)),
			"",
			"A mismatch here is the dangerous outcome: every chunk reported success",
			"and the stored file is wrong. If that is what happened, parallel_chunks",
			"must be removed even though the writes were accepted.")
	}
}

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
			lines = append(lines, fmt.Sprintf("  seg %2d [%8d-%8d]  FAILED  %v", i, o.from, o.to, o.err))
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
