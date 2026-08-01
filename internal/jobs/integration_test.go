//go:build integration

// Live job engine tests (whitepaper §6.2, §10.2). They run against the real
// sandbox with:
//
//	scripts/integration-test.sh -run TestSandboxJobs
package jobs_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // protocol requirement
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/jobs"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

func sandboxClient(t *testing.T) (*opendrive.Client, context.Context) {
	t.Helper()
	user, pass := os.Getenv("ODB_SPEC_USER"), os.Getenv("ODB_SPEC_PASS")
	if user == "" || pass == "" {
		t.Skip("set ODB_SPEC_USER and ODB_SPEC_PASS (see scripts/integration-test.sh)")
	}
	// The sandbox refuses writes intermittently (D39/D40), so the live engine
	// tests give the classifier the same headroom the SDK suite does.
	c, err := opendrive.New(opendrive.WithRetryPolicy(opendrive.RetryPolicy{
		Max: 10, Base: 2 * time.Second, Cap: 20 * time.Second,
	}))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	auth := opendrive.NewOAuth2(c)
	c.SetAuthenticator(auth)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if err := auth.Login(ctx, user, pass); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c, ctx
}

// scratch makes a folder for one test and removes it afterwards. Every artefact
// goes, intermediate ones included (D39).
func scratch(t *testing.T, ctx context.Context, c *opendrive.Client) string {
	t.Helper()
	root, err := c.Folders().List(ctx, "0", opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(root.Folders) == 0 {
		t.Skip("no folder available to write into")
	}
	base := string(root.Folders[0].FolderID)

	created, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{
		Name:     fmt.Sprintf("odb-jobs-%d", time.Now().UnixNano()),
		ParentID: base, Access: opendrive.FolderPrivate,
	})
	if err != nil {
		t.Skipf("cannot create a scratch folder (upstream may be refusing writes, D39): %v", err)
	}
	id := created.FolderID.String()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.Folders().Trash(cleanup, []string{id})
		if err := c.Folders().Remove(cleanup, []string{id}); err != nil {
			t.Logf("cleanup: scratch %s may remain: %v", id, err)
		}
	})
	return id
}

func payload(t *testing.T, n int) (string, []byte) {
	t.Helper()
	data := bytes.Repeat([]byte("opendrive job engine payload "), n/29+1)[:n]
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// mustOpen opens a file for the staged-crash upload.
func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // protocol requirement
	return hex.EncodeToString(sum[:])
}

func newEngine(t *testing.T, c *opendrive.Client, dir string) *jobs.Engine {
	t.Helper()
	store, err := jobs.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := jobs.New(c, jobs.WithWorkers(2), jobs.WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	return e
}

func waitTerminal(t *testing.T, e *jobs.Engine, id string, within time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		j, err := e.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if j.State.Terminal() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not finish within %s (state %s, %d/%d bytes)",
				id, within, j.State, j.BytesDone, j.BytesTotal)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// listFileIDs answers "what is actually in this folder" the only way D27
// permits: the parent listing.
func listFileIDs(t *testing.T, ctx context.Context, c *opendrive.Client, folder string) []string {
	t.Helper()
	listing, err := c.Folders().List(ctx, folder, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list %s: %v", folder, err)
	}
	out := make([]string, 0, len(listing.Files))
	for i := range listing.Files {
		out = append(out, listing.Files[i].FileID.String())
	}
	return out
}

// A round trip through the engine: upload, then download the same bytes back.
func TestSandboxJobsRoundTrip(t *testing.T) {
	c, ctx := sandboxClient(t)
	folder := scratch(t, ctx, c)
	e := newEngine(t, c, t.TempDir())
	e.Start(ctx)

	src, data := payload(t, 128<<10)
	up, err := e.Submit(jobs.Spec{
		Kind: jobs.KindUpload, LocalPath: src, FolderID: folder,
		Name: "roundtrip.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("submit upload: %v", err)
	}

	done := waitTerminal(t, e, up.ID, 3*time.Minute)
	if done.State != jobs.StateSucceeded {
		t.Fatalf("upload %s: %+v", done.State, done.Error)
	}
	if done.BytesDone != int64(len(data)) {
		t.Errorf("bytes_done = %d, want %d", done.BytesDone, len(data))
	}
	t.Logf("uploaded %d bytes", done.BytesDone)

	ids := listFileIDs(t, ctx, c, folder)
	if len(ids) != 1 {
		t.Fatalf("the folder holds %d files, want exactly the one uploaded", len(ids))
	}

	dst := filepath.Join(t.TempDir(), "back.bin")
	down, err := e.Submit(jobs.Spec{
		Kind: jobs.KindDownload, FileID: ids[0], LocalPath: dst,
		Size: int64(len(data)), Hash: md5hex(data),
	})
	if err != nil {
		t.Fatalf("submit download: %v", err)
	}
	got := waitTerminal(t, e, down.ID, 3*time.Minute)
	if got.State != jobs.StateSucceeded {
		t.Fatalf("download %s: %+v", got.State, got.Error)
	}

	back, err := os.ReadFile(dst) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, data) {
		t.Fatalf("round trip returned %d bytes, want %d identical", len(back), len(data))
	}
}

// The guarantee, on real hardware: cancelling an upload mid-flight leaves
// nothing behind in the destination folder. This is the failure D39 spent two
// diagnoses on, asserted rather than hoped for.
func TestSandboxJobsCancelLeavesNoOrphan(t *testing.T) {
	c, ctx := sandboxClient(t)
	folder := scratch(t, ctx, c)
	e := newEngine(t, c, t.TempDir())
	e.Start(ctx)

	before := listFileIDs(t, ctx, c, folder)
	if len(before) != 0 {
		t.Fatalf("the scratch folder is not empty: %v", before)
	}

	// Large enough that the transfer is still running when the cancel lands.
	src, data := payload(t, 6<<20)
	job, err := e.Submit(jobs.Spec{
		Kind: jobs.KindUpload, LocalPath: src, FolderID: folder,
		Name: "cancelled.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait until the record exists upstream — that is the window in which a
	// cancellation could leak one.
	deadline := time.Now().Add(90 * time.Second)
	for {
		j, err := e.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.UpstreamFileID != "" {
			t.Logf("create_file made %s; cancelling now", j.UpstreamFileID)
			break
		}
		if j.State.Terminal() {
			t.Skipf("the upload finished as %s before it could be cancelled", j.State)
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never created a record")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := e.Cancel(job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	done := waitTerminal(t, e, job.ID, 2*time.Minute)
	if done.State != jobs.StateCancelled {
		t.Fatalf("state = %s, want cancelled: %+v", done.State, done.Error)
	}

	// D27: existence is decided by the parent listing, never by info.json.
	after := listFileIDs(t, ctx, c, folder)
	if len(after) != 0 {
		t.Fatalf("cancelling left %d file(s) upstream: %v — enough of these and the folder "+
			"starts refusing writes (D39)", len(after), after)
	}

	// The record is gone for good, not merely unlisted. file/info.json does
	// report a removed *file* as gone, unlike the folder endpoint of D27.
	if _, err := c.Files().Info(ctx, done.UpstreamFileID); err == nil && done.UpstreamFileID != "" {
		t.Errorf("record %s still exists after cancellation", done.UpstreamFileID)
	}
}

// Stopping the engine is the crash path's civilised cousin: it too must leave
// nothing upstream.
func TestSandboxJobsStopReclaimsInFlightRecords(t *testing.T) {
	c, ctx := sandboxClient(t)
	folder := scratch(t, ctx, c)
	e := newEngine(t, c, t.TempDir())
	e.Start(ctx)

	src, data := payload(t, 6<<20)
	job, err := e.Submit(jobs.Spec{
		Kind: jobs.KindUpload, LocalPath: src, FolderID: folder,
		Name: "stopped.bin", Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		j, _ := e.Get(job.ID)
		if j != nil && j.UpstreamFileID != "" {
			break
		}
		if j != nil && j.State.Terminal() {
			t.Skipf("the upload finished as %s before Stop could interrupt it", j.State)
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never created a record")
		}
		time.Sleep(50 * time.Millisecond)
	}

	e.Stop()

	if got := listFileIDs(t, ctx, c, folder); len(got) != 0 {
		t.Fatalf("stopping the engine left %d file(s) upstream: %v", len(got), got)
	}
}

// Recovery after a crash: the persisted record is reclaimed and the transfer
// runs again cleanly, so a restart cannot accumulate records either.
func TestSandboxJobsRecoverAfterACrash(t *testing.T) {
	c, ctx := sandboxClient(t)
	folder := scratch(t, ctx, c)
	dir := t.TempDir()

	// A crash is not a Stop: nothing gets the chance to clean up, and the state
	// left on disk still says "running". Stopping the engine would reclaim the
	// record and mark the job cancelled, which is the opposite of what recovery
	// has to cope with — so the crash is staged directly.
	//
	// A real orphan first: an upload interrupted with KeepOnFailure set, which
	// is exactly the state a worker is in when its process dies.
	src, data := payload(t, 6<<20)
	upCtx, interrupt := context.WithCancel(ctx)
	var leaked, leakedTemp string
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		_, _ = c.Uploads().Upload(upCtx, mustOpen(t, src), opendrive.UploadParams{
			FolderID: folder, Name: "recovered.bin", Size: int64(len(data)),
			KeepOnFailure: true,
			OnRecordCreated: func(fileID, tempLocation string) {
				leaked, leakedTemp = fileID, tempLocation
				// Let a chunk or two land, then die.
				time.AfterFunc(300*time.Millisecond, interrupt)
			},
		})
	}()
	select {
	case <-uploadDone:
	case <-time.After(90 * time.Second):
		interrupt()
		t.Fatal("the staged upload never stopped")
	}
	interrupt()
	if leaked == "" {
		t.Skip("the staged upload never created a record")
	}
	// The temp location is an upload credential in all but name, so it is not
	// logged (§9.4).
	t.Logf("staged a crash with %s left upstream", leaked)

	// The state a crashed worker would have left on disk: still running, with
	// the record it had created.
	store, err := jobs.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := &jobs.Job{
		ID: "crashed-live", Kind: jobs.KindUpload, State: jobs.StateRunning,
		BytesTotal: int64(len(data)), CreatedAt: time.Now(),
		Spec: jobs.Spec{
			Kind: jobs.KindUpload, LocalPath: src, FolderID: folder,
			Name: "recovered.bin", Size: int64(len(data)),
		},
		UpstreamFileID:       leaked,
		UpstreamTempLocation: leakedTemp,
	}
	if err := store.Save(job); err != nil {
		t.Fatal(err)
	}

	// Second engine over the same directory, as a restarted daemon would be.
	second := newEngine(t, c, dir)
	if err := second.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	second.Start(ctx)

	done := waitTerminal(t, second, job.ID, 4*time.Minute)
	if done.State != jobs.StateSucceeded {
		t.Fatalf("the recovered job ended as %s: %+v", done.State, done.Error)
	}

	// Exactly one file: the completed upload. The record from before the crash
	// must not still be sitting there.
	after := listFileIDs(t, ctx, c, folder)
	if len(after) != 1 {
		t.Fatalf("after recovery the folder holds %d files, want 1: %v", len(after), after)
	}
	for _, id := range after {
		if id == leaked {
			t.Errorf("the pre-crash record %s was left in place", leaked)
		}
	}
}

// A failure the classifier calls permanent must not be retried, and the wording
// the user sees must be the classifier's.
func TestSandboxJobsReportTheClassifiersVerdict(t *testing.T) {
	c, ctx := sandboxClient(t)
	e := newEngine(t, c, t.TempDir())
	e.Start(ctx)

	// The account root is the one place this login genuinely may not write.
	src, data := payload(t, 4096)
	job, err := e.Submit(jobs.Spec{
		Kind: jobs.KindUpload, LocalPath: src, FolderID: "0",
		Name: fmt.Sprintf("odb-denied-%d.bin", time.Now().Unix()), Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	done := waitTerminal(t, e, job.ID, 3*time.Minute)
	if done.State == jobs.StateSucceeded {
		t.Skip("this account can write to the account root, so there is no denial to report")
	}
	if done.Error == nil {
		t.Fatal("a failed job reported no error")
	}
	t.Logf("reported: code=%s retryable=%v message=%q",
		done.Error.Code, done.Error.Retryable, done.Error.Message)

	if done.Error.Code != string(opendrive.KindUpstreamError) {
		t.Errorf("code = %q, want the classifier's stable enum", done.Error.Code)
	}
	// The message is the classifier's, so a user is never handed upstream's
	// misleading advice (docs/error-taxonomy.md T2).
	if done.Error.Message == "" {
		t.Error("no message was reported")
	}

	// And nothing was left behind by the attempt.
	if done.UpstreamFileID != "" {
		if _, err := c.Files().Info(ctx, done.UpstreamFileID); err == nil {
			t.Errorf("the failed job left record %s upstream", done.UpstreamFileID)
		} else if !errors.Is(err, opendrive.ErrNotFound) {
			t.Logf("could not confirm the record is gone: %v", err)
		}
	}
}
