//go:build integration

// Integration tests run against the real OpenDrive sandbox (whitepaper §6.2).
// They are excluded from the default build and only run with:
//
//	scripts/integration-test.sh          # credentials from the OS keychain
//	go test -tags=integration ./pkg/opendrive/ -run TestSandbox -v
//
// Requirements: ODB_SPEC_USER / ODB_SPEC_PASS name the test account. When that
// account is an *account user* rather than an account owner it cannot write to
// the account root, so every artefact is created under a granted base folder —
// ODB_TEST_FOLDER_ID when set, otherwise the first folder the root listing
// offers. Artefacts are named odb-test-<timestamp> and cleaned up as far as the
// account's rights allow (§6.2, docs/discrepancies.md D25).
package opendrive_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// sandboxOnce guards the shared client. Every test reuses one login, as §6.2
// asks, to stay clear of captcha throttling. (A login burst was once blamed for
// the spurious permission errors in D39; the real cause turned out to be leaked
// test artefacts, but sharing the login is still the right thing to do.)
var (
	sandboxOnce   sync.Once
	sandboxClient *opendrive.Client
	sandboxErr    error
)

// newSandboxClient returns the shared client, logging in once per test binary.
// It skips rather than fails when credentials are absent, so a plain
// `go test -tags=integration ./...` on a machine without them is quiet.
func newSandboxClient(t *testing.T) (*opendrive.Client, context.Context) {
	t.Helper()

	user, pass := os.Getenv("ODB_SPEC_USER"), os.Getenv("ODB_SPEC_PASS")
	if user == "" || pass == "" {
		t.Skip("set ODB_SPEC_USER and ODB_SPEC_PASS (see scripts/integration-test.sh)")
	}

	sandboxOnce.Do(func() {
		c, err := opendrive.New()
		if err != nil {
			sandboxErr = err
			return
		}
		auth := opendrive.NewOAuth2(c)
		c.SetAuthenticator(auth)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := auth.Login(ctx, user, pass); err != nil {
			sandboxErr = err
			return
		}
		sandboxClient = c
	})
	if sandboxErr != nil {
		t.Fatalf("login: %v", sandboxErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return sandboxClient, ctx
}

// writableBase returns the folder new artefacts belong under. An account user
// gets a 403 on the account root, so tests never write there.
func writableBase(t *testing.T, ctx context.Context, c *opendrive.Client) string {
	t.Helper()

	if id := os.Getenv("ODB_TEST_FOLDER_ID"); id != "" {
		return id
	}
	root, err := c.Folders().List(ctx, "0", opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if len(root.Folders) == 0 {
		t.Skip("no folder available to write into; set ODB_TEST_FOLDER_ID")
	}
	base := string(root.Folders[0].FolderID)
	t.Logf("writing under %q (%s); override with ODB_TEST_FOLDER_ID",
		root.Folders[0].Name, base)
	return base
}

// TestSandboxReadOperations exercises the read half of the folder module
// against the live account.
func TestSandboxReadOperations(t *testing.T) {
	c, ctx := newSandboxClient(t)
	f := c.Folders()

	root, err := f.List(ctx, "0", opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	t.Logf("root: %d folders, %d files, DirUpdateTime=%v",
		len(root.Folders), len(root.Files), root.DirUpdateTime)

	if root.DirUpdateTime.IsZero() {
		t.Error("root listing carries no DirUpdateTime; the path cache depends on it (§10.3)")
	}
	if _, err := f.IDByPath(ctx, "/"); err != nil {
		t.Errorf("idbypath root: %v", err)
	}

	base := writableBase(t, ctx, c)
	if _, err := f.Info(ctx, base); err != nil {
		t.Errorf("info %s: %v", base, err)
	}
	if _, err := f.Breadcrumb(ctx, base, false); err != nil {
		t.Errorf("breadcrumb %s: %v", base, err)
	}
}

// TestSandboxWriteLifecycle is the full mutation path against the live API —
// the half that was unreachable while the account was read-only
// (docs/discrepancies.md D25): create, rename, copy, move, trash, restore and
// permanent delete, each asserted against a follow-up read.
func TestSandboxWriteLifecycle(t *testing.T) {
	c, ctx := newSandboxClient(t)
	f := c.Folders()
	base := writableBase(t, ctx, c)

	name := fmt.Sprintf("odb-test-%s", time.Now().UTC().Format("20060102-150405"))
	created, err := f.Create(ctx, opendrive.CreateFolderParams{
		Name:        name,
		ParentID:    base,
		Access:      opendrive.FolderPrivate,
		Description: "opendrive-bridge integration test; safe to delete",
	})
	if err != nil {
		t.Fatalf("create under %s: %v", base, err)
	}
	id := string(created.FolderID)
	t.Logf("created %s (%s)", created.Name, id)

	// Belt and braces: whatever the test leaves behind goes for good.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = f.Trash(ctx, []string{id})
		if err := f.Remove(ctx, []string{id}); err != nil {
			t.Logf("cleanup: %s may still be in the trash: %v", id, err)
		}
	})

	// The new folder must be visible in its parent, with the name we chose.
	listing, err := f.List(ctx, base, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list base: %v", err)
	}
	var found bool
	for _, sub := range listing.Folders {
		if string(sub.FolderID) == id {
			found = true
			if sub.Name != name {
				t.Errorf("created folder name = %q, want %q", sub.Name, name)
			}
		}
	}
	if !found {
		t.Errorf("created folder %s missing from the listing of %s", id, base)
	}

	// Round-trip the live path resolver against a folder we just made.
	info, err := f.Info(ctx, id)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if string(info.FolderID) != id {
		t.Errorf("info returned %s, want %s", info.FolderID, id)
	}
	path, err := f.Path(ctx, id)
	if err != nil {
		t.Errorf("path: %v", err)
	} else {
		resolved, err := f.IDByPath(ctx, path)
		if err != nil {
			t.Errorf("idbypath %q: %v", path, err)
		} else if resolved != id {
			t.Errorf("idbypath %q = %s, want %s", path, resolved, id)
		}
	}

	// Rename, and confirm the new name through a fresh read.
	renamed := name + "-renamed"
	if _, err := f.Rename(ctx, id, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if info, err := f.Info(ctx, id); err != nil {
		t.Errorf("info after rename: %v", err)
	} else if info.Name != renamed {
		t.Errorf("name after rename = %q, want %q", info.Name, renamed)
	}

	// Copy — the operation the boolean encoding used to make impossible (D26).
	copyName := name + "-copy"
	copied, err := f.MoveCopy(ctx, opendrive.MoveCopyParams{
		FolderID: id, DstFolderID: base, Move: false, NewName: copyName,
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	copyID := string(copied.FolderID)
	if copyID == "" || copyID == id {
		t.Errorf("copy returned folder %q; want a new id distinct from %s", copyID, id)
	}

	// Move the copy inside the original, then verify it landed there.
	if _, err := f.MoveCopy(ctx, opendrive.MoveCopyParams{
		FolderID: copyID, DstFolderID: id, Move: true,
	}); err != nil {
		t.Fatalf("move: %v", err)
	}
	inner, err := f.List(ctx, id, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list after move: %v", err)
	}
	var moved bool
	for _, sub := range inner.Folders {
		if string(sub.FolderID) == copyID {
			moved = true
		}
	}
	if !moved {
		t.Errorf("moved folder %s is not under %s", copyID, id)
	}

	// Trash, restore, then delete for good.
	if err := f.Trash(ctx, []string{copyID}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := f.Restore(ctx, []string{copyID}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := f.Trash(ctx, []string{copyID}); err != nil {
		t.Fatalf("trash after restore: %v", err)
	}
	if err := f.Remove(ctx, []string{copyID}); err != nil {
		t.Fatalf("permanent delete: %v", err)
	}

	// Existence is decided by the parent listing, not by info.json: upstream
	// keeps answering info for a permanently deleted folder (D27), so anything
	// that treats an info hit as proof of existence is wrong.
	gone, err := f.List(ctx, id, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list after permanent delete: %v", err)
	}
	for _, sub := range gone.Folders {
		if string(sub.FolderID) == copyID {
			t.Errorf("permanently deleted folder %s is still listed under %s", copyID, id)
		}
	}
	if _, err := f.Info(ctx, copyID); err == nil {
		t.Logf("note: info still resolves the deleted %s — expected, see D27", copyID)
	}
}

// TestSandboxWriteCapabilities reports which mutations the test account may
// perform. It never fails on a refusal — the point is a readable matrix in the
// log, so the state of the account is visible before P3 depends on it. It does
// fail if a refusal arrives as something other than upstream_error, because
// that would let the silent re-login state machine mistake a permission problem
// for a changed password (v1.1 §4.5).
func TestSandboxWriteCapabilities(t *testing.T) {
	c, ctx := newSandboxClient(t)
	f := c.Folders()
	base := writableBase(t, ctx, c)

	name := fmt.Sprintf("odb-probe-%s", time.Now().UTC().Format("20060102-150405"))
	created, err := f.Create(ctx, opendrive.CreateFolderParams{
		Name: name, ParentID: base, Access: opendrive.FolderPrivate,
	})
	if err != nil {
		t.Fatalf("create under %s: %v (the account cannot write at all)", base, err)
	}
	id := string(created.FolderID)

	report := func(op string, err error) bool {
		t.Helper()
		if err == nil {
			t.Logf("  %-16s granted", op)
			return true
		}
		if !errors.Is(err, opendrive.ErrUpstream) {
			t.Errorf("  %-16s refused as %v, want upstream_error (§4.5)", op, err)
			return false
		}
		t.Logf("  %-16s refused: %v", op, err)
		return false
	}

	t.Log("write capability matrix for this account:")
	report("create", nil)
	_, renameErr := f.Rename(ctx, id, name+"-renamed")
	report("rename", renameErr)
	copied, copyErr := f.MoveCopy(ctx, opendrive.MoveCopyParams{
		FolderID: id, DstFolderID: base, Move: false, NewName: name + "-copy",
	})
	report("copy", copyErr)
	// The copy is a second artefact and has to go too: leaked probe folders
	// accumulate in the base folder and eventually make writes fail with a
	// misleading 403 (docs/discrepancies.md D39).
	if copyErr == nil && copied != nil {
		if copyID := string(copied.FolderID); copyID != "" && copyID != id {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				_ = f.Trash(ctx, []string{copyID})
				_ = f.Remove(ctx, []string{copyID})
			})
		}
	}
	report("settings", f.UpdateSettings(ctx, id, opendrive.FolderSettings{
		Description: "probe",
	}))

	if report("trash", f.Trash(ctx, []string{id})) {
		report("restore", f.Restore(ctx, []string{id}))
		report("remove", f.Remove(ctx, []string{id}))
	}
}

// TestSandboxRejectsInvalidNamesLocally proves the local validation in §2.6 #10
// fires before a request is sent.
func TestSandboxRejectsInvalidNamesLocally(t *testing.T) {
	c, ctx := newSandboxClient(t)

	_, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{Name: `bad/name`})
	if !errors.Is(err, opendrive.ErrInvalidName) {
		t.Fatalf("create with an illegal name = %v, want ErrInvalidName", err)
	}
}

// ---------------------------------------------------------------- file module

// fileScratch creates a throwaway folder for one test and removes it, and
// everything in it, afterwards.
// scratchSeq makes scratch folder names unique even when two are created in
// the same clock tick, which macOS timestamps happily do.
var scratchSeq atomic.Int64

func fileScratch(t *testing.T, ctx context.Context, c *opendrive.Client) string {
	t.Helper()
	base := writableBase(t, ctx, c)
	name := fmt.Sprintf("odb-test-%s-%d-%d",
		time.Now().UTC().Format("20060102-150405"), os.Getpid(), scratchSeq.Add(1))
	created, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{
		Name: name, ParentID: base, Access: opendrive.FolderPrivate,
		Description: "opendrive-bridge integration test; safe to delete",
	})
	if err != nil {
		t.Fatalf("create scratch folder: %v", err)
	}
	id := created.FolderID.String()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.Folders().Trash(ctx, []string{id})
		if err := c.Folders().Remove(ctx, []string{id}); err != nil {
			t.Logf("cleanup: scratch folder %s may remain: %v", id, err)
		}
	})
	return id
}

// newSandboxFile creates an empty file in folder and returns its id.
func newSandboxFile(t *testing.T, ctx context.Context, c *opendrive.Client, folder string) string {
	t.Helper()
	created, err := c.Files().CreateEmpty(ctx, opendrive.CreateEmptyFileParams{
		FolderID: folder, FileType: "txt",
	})
	if err != nil {
		t.Fatalf("create empty file: %v", err)
	}
	id := created.FileID.String()
	if id == "" {
		t.Fatalf("create empty file returned no id: %+v", created)
	}
	return id
}

// fileInListing reports whether a file id appears in a folder listing. D27
// forbids using Info as an existence check, so every liveness assertion in this
// file goes through the parent listing instead.
func fileInListing(t *testing.T, ctx context.Context, c *opendrive.Client, folder, fileID string) bool {
	t.Helper()
	listing, err := c.Folders().List(ctx, folder, opendrive.ListOptions{})
	if err != nil {
		t.Fatalf("list %s: %v", folder, err)
	}
	for _, f := range listing.Files {
		if f.FileID.String() == fileID {
			return true
		}
	}
	return false
}

// TestSandboxFileLifecycle walks a file through every state transition the
// module offers and checks each one against a follow-up read: create, rename,
// settings, access, trash, restore, trash again, permanent removal.
func TestSandboxFileLifecycle(t *testing.T) {
	c, ctx := newSandboxClient(t)
	files := c.Files()
	scratch := fileScratch(t, ctx, c)

	id := newSandboxFile(t, ctx, c, scratch)
	t.Logf("created file %s", id)
	if !fileInListing(t, ctx, c, scratch, id) {
		t.Fatalf("the new file is absent from its parent listing")
	}

	info, err := files.Info(ctx, id)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.FileID.String() != id {
		t.Errorf("info reports id %q, want %q", info.FileID, id)
	}
	// An empty file has the MD5 of no bytes at all; the upload pipeline of P3
	// relies on this field being the real hash (§2.4).
	if info.FileHash != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("FileHash = %q, want the MD5 of an empty file", info.FileHash)
	}

	path, err := files.Path(ctx, id)
	if err != nil || path == "" {
		t.Fatalf("path = %q, %v", path, err)
	}
	full, err := files.FullPath(ctx, id)
	if err != nil {
		t.Fatalf("full path: %v", err)
	}
	// D30: upstream answers with backslashes in a field called DownloadLink;
	// the binding normalises both.
	if strings.Contains(full, `\`) || full == "" {
		t.Errorf("FullPath = %q, want a normalised forward-slash path", full)
	}

	// The id must be resolvable from the path, which is the existence check
	// D27 permits.
	resolved, err := files.IDByPath(ctx, "/"+path)
	if err != nil {
		t.Errorf("idbypath %q: %v", path, err)
	} else if resolved != id {
		t.Errorf("idbypath resolved %q, want %q", resolved, id)
	}

	renamed := fmt.Sprintf("odb-renamed-%d.txt", time.Now().Unix())
	if _, err := files.Rename(ctx, id, renamed, "", ""); err != nil {
		t.Fatalf("rename: %v", err)
	}
	after, err := files.Info(ctx, id)
	if err != nil || after.Name != renamed {
		t.Fatalf("after rename the name is %q, want %q (%v)", after.Name, renamed, err)
	}

	description := "set by the integration suite"
	editOnline := false
	if err := files.UpdateSettings(ctx, id, opendrive.FileSettings{
		Description: &description, EditOnline: &editOnline,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if err := files.SetAccess(ctx, id, opendrive.FilePublic, "", ""); err != nil {
		t.Fatalf("set access public: %v", err)
	}
	if err := files.SetAccess(ctx, id, opendrive.FilePrivate, "", ""); err != nil {
		t.Fatalf("set access private: %v", err)
	}

	if err := files.Trash(ctx, []string{id}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if fileInListing(t, ctx, c, scratch, id) {
		t.Error("a trashed file is still listed in its parent")
	}
	if err := files.Restore(ctx, []string{id}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !fileInListing(t, ctx, c, scratch, id) {
		t.Error("a restored file is missing from its parent listing")
	}

	if err := files.Trash(ctx, []string{id}); err != nil {
		t.Fatalf("second trash: %v", err)
	}
	if err := files.Remove(ctx, []string{id}, "", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if fileInListing(t, ctx, c, scratch, id) {
		t.Error("a permanently removed file is still listed")
	}
	// Unlike a folder (D27), a removed file does report itself gone.
	if _, err := files.Info(ctx, id); !errors.Is(err, opendrive.ErrNotFound) {
		t.Errorf("info after remove = %v, want not_found", err)
	}
}

// TestSandboxFileCopyAndMove is the D26 guard for the file module: the copy
// path is the one a wrong boolean encoding silently destroys.
func TestSandboxFileCopyAndMove(t *testing.T) {
	c, ctx := newSandboxClient(t)
	files := c.Files()
	src := fileScratch(t, ctx, c)
	dst := fileScratch(t, ctx, c)

	id := newSandboxFile(t, ctx, c, src)

	// Copy: the source must survive and the destination must gain a file.
	copied, err := files.MoveCopy(ctx, opendrive.FileMoveCopyParams{
		SourceFileID: id, DestinationFolder: dst, Move: false, NewName: "copied.txt",
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	copyID := copied.FileID.String()
	if copyID == "" || copyID == id {
		t.Fatalf("copy returned id %q for source %q", copyID, id)
	}
	if !fileInListing(t, ctx, c, src, id) {
		t.Error("the source file disappeared after a copy")
	}
	if !fileInListing(t, ctx, c, dst, copyID) {
		t.Error("the copy is missing from the destination")
	}

	// Move: the source must be gone from where it was.
	moved, err := files.MoveCopy(ctx, opendrive.FileMoveCopyParams{
		SourceFileID: id, DestinationFolder: dst, Move: true, NewName: "moved.txt",
	})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if fileInListing(t, ctx, c, src, id) {
		t.Error("the source file is still in place after a move")
	}
	if !fileInListing(t, ctx, c, dst, moved.FileID.String()) {
		t.Error("the moved file is missing from the destination")
	}
}

// TestSandboxFileBooleanEncoding pins down what upstream actually accepts for
// the two string booleans of file/move_copy.json. The mock tests can only
// assert what the SDK sends; this asserts that upstream agrees, and it is the
// test that would have caught D26 in the folder module.
func TestSandboxFileBooleanEncoding(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)
	dst := fileScratch(t, ctx, c)

	send := func(move any) error {
		id := newSandboxFile(t, ctx, c, scratch)
		var out opendrive.FileInfo
		return c.Do(ctx, opendrive.Request{
			Method:           http.MethodPost,
			Path:             opendrive.EndpointFileMoveCopy,
			SessionPlacement: opendrive.SessionInBody,
			Body: map[string]any{
				"src_file_id":         id,
				"dst_folder_id":       dst,
				"move":                move,
				"overwrite_if_exists": "true",
			},
		}, &out)
	}

	// The encoding the SDK uses must work for both values.
	for _, v := range []any{opendrive.StringBool(true), opendrive.StringBool(false)} {
		if err := send(v); err != nil {
			t.Errorf("move=%v (the encoding the SDK sends) was rejected: %v", v, err)
		}
	}

	// Real JSON booleans must still be refused. If this ever starts passing,
	// upstream has changed and docs/discrepancies.md D24/D26 need revisiting.
	for _, v := range []any{true, false} {
		err := send(v)
		if err == nil {
			t.Errorf("move=%v (a JSON boolean) is now accepted; revisit D24/D26", v)
			continue
		}
		t.Logf("move=%v rejected as expected: %v", v, err)
	}
}

// TestSandboxFileAccessFolderIDIsRequired documents D28: the empty string does
// not satisfy access_folder_id, which made the first cut of CreateEmpty fail
// every time.
func TestSandboxFileAccessFolderIDIsRequired(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	var out opendrive.FileInfo
	err := c.Do(ctx, opendrive.Request{
		Method:           http.MethodPost,
		Path:             opendrive.EndpointFile,
		SessionPlacement: opendrive.SessionInBody,
		Body: map[string]string{
			"access_folder_id": "",
			"folder_id":        scratch,
			"file_type":        "txt",
		},
	}, &out)
	if err == nil {
		t.Fatal("an empty access_folder_id is now accepted; revisit D28")
	}
	t.Logf("empty access_folder_id rejected as expected: %v", err)

	// The binding's default of "0" is the value that works.
	if id := newSandboxFile(t, ctx, c, scratch); id == "" {
		t.Fatal("CreateEmpty produced no file")
	}
}

// TestSandboxFileDeleteWithoutTrash covers the second, multi-verb file resource
// and records that it does not require the file to be trashed first (D29).
func TestSandboxFileDeleteWithoutTrash(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)

	id := newSandboxFile(t, ctx, c, scratch)
	if err := c.Files().DeleteTrashed(ctx, id, "", ""); err != nil {
		t.Fatalf("DELETE /file.json on a file that was never trashed: %v", err)
	}
	if fileInListing(t, ctx, c, scratch, id) {
		t.Error("the file is still listed after DELETE /file.json")
	}

	// A trashed file goes the same way.
	other := newSandboxFile(t, ctx, c, scratch)
	if err := c.Files().Trash(ctx, []string{other}); err != nil {
		t.Fatalf("trash: %v", err)
	}
	if err := c.Files().DeleteTrashed(ctx, other, "", ""); err != nil {
		t.Fatalf("delete a trashed file: %v", err)
	}
}

// TestSandboxFileExpiringLink covers both halves of the expiring-link pair and
// the shape recorded in D31.
func TestSandboxFileExpiringLink(t *testing.T) {
	c, ctx := newSandboxClient(t)
	files := c.Files()
	scratch := fileScratch(t, ctx, c)
	id := newSandboxFile(t, ctx, c, scratch)

	expires := time.Now().AddDate(0, 1, 0).Format("2006-01-02")
	link, err := files.CreateExpiringLink(ctx, id, expires, 5, true)
	if err != nil {
		t.Fatalf("create expiring link: %v", err)
	}
	if link.URL() == "" {
		t.Fatalf("expiring link carries no URL: %+v", link)
	}
	// D31: a file answers with DownloadLink, not the folder module's Link.
	if link.DownloadLink == "" {
		t.Errorf("no DownloadLink in %+v", link)
	}

	listed, err := files.ExpiringLinks(ctx, id)
	if err != nil {
		t.Fatalf("list expiring links: %v", err)
	}
	if listed.ExpiringDate != expires {
		t.Errorf("ExpiringDate = %q, want %q", listed.ExpiringDate, expires)
	}
	if listed.CounterMax.Int() != 5 {
		t.Errorf("CounterMax = %d, want 5", listed.CounterMax.Int())
	}
}

// TestSandboxFileThumbnail proves the raw-response transport path returns image
// bytes rather than trying to decode them as JSON.
func TestSandboxFileThumbnail(t *testing.T) {
	c, ctx := newSandboxClient(t)
	scratch := fileScratch(t, ctx, c)
	id := newSandboxFile(t, ctx, c, scratch)

	data, err := c.Files().Thumbnail(ctx, id, opendrive.ThumbnailOptions{})
	if err != nil {
		t.Skipf("no thumbnail for an empty text file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("thumbnail returned no bytes")
	}
	if len(data) > 8 && string(data[1:4]) == "PNG" {
		t.Logf("thumbnail is a %d byte PNG", len(data))
	}
}

// TestSandboxFileVerifyPassword records D32: upstream answers false whether the
// password is right or wrong, and never returns a TempKey.
func TestSandboxFileVerifyPassword(t *testing.T) {
	c, ctx := newSandboxClient(t)
	files := c.Files()
	scratch := fileScratch(t, ctx, c)
	id := newSandboxFile(t, ctx, c, scratch)

	const password = "odb-integration-password"
	pw := password
	if err := files.UpdateSettings(ctx, id, opendrive.FileSettings{Password: &pw}); err != nil {
		t.Fatalf("set a file password: %v", err)
	}

	correct, err := files.VerifyPassword(ctx, id, password, "")
	if err != nil {
		t.Fatalf("verify the correct password: %v", err)
	}
	wrong, err := files.VerifyPassword(ctx, id, "definitely-not-it", "")
	if err != nil {
		t.Fatalf("verify a wrong password: %v", err)
	}
	if correct.OK() != wrong.OK() {
		t.Logf("upstream now distinguishes the two passwords (correct=%v wrong=%v); revisit D32",
			correct.OK(), wrong.OK())
	}
	if correct.TempKey != "" {
		t.Logf("upstream now returns a TempKey; revisit D32 and wire it into downloads")
	}
}

// TestSandboxPasswordGatesThePublicRoute settles D32 against the live API: a
// file password stops the anonymous download route and is invisible to the
// authenticated owner, which is why the P3 pipeline needs no TempKey.
func TestSandboxPasswordGatesThePublicRoute(t *testing.T) {
	c, ctx := newSandboxClient(t)
	files := c.Files()
	scratch := fileScratch(t, ctx, c)
	id := newSandboxFile(t, ctx, c, scratch)

	const password = "odb-public-route-password"
	pw := password
	if err := files.SetAccess(ctx, id, opendrive.FilePublic, "", ""); err != nil {
		t.Fatalf("make the file public: %v", err)
	}
	if err := files.UpdateSettings(ctx, id, opendrive.FileSettings{Password: &pw}); err != nil {
		t.Fatalf("set the password: %v", err)
	}

	// The owner's session downloads without ever being asked for it.
	var probe opendrive.BoolResult
	if err := c.Do(ctx, opendrive.Request{
		Method:           http.MethodGet,
		Path:             opendrive.EndpointDownloadFile,
		SessionPlacement: opendrive.SessionInQuery,
		PathSegments:     []string{id},
		Query:            map[string][]string{"test": {"1"}},
	}, &probe); err != nil {
		t.Fatalf("the owner should be able to download a password-protected file: %v", err)
	}

	// Anonymously the same request is refused, which is where the password
	// actually lives.
	anon, err := opendrive.New()
	if err != nil {
		t.Fatal(err)
	}
	err = anon.Do(ctx, opendrive.Request{
		Method:           http.MethodGet,
		Path:             opendrive.EndpointDownloadFile,
		SessionPlacement: opendrive.SessionOmit,
		PathSegments:     []string{id},
		Query:            map[string][]string{"test": {"1"}},
	}, &probe)
	if err == nil {
		t.Fatal("the anonymous route accepted a password-protected file; revisit D32")
	}
	// The wording depends on which gate upstream reaches first — "File requires
	// password" when the file is otherwise downloadable, "Download permissions
	// are not enabled for this file" when public download is off. Either way it
	// is a plain refusal, not a credential problem the SDK should try to fix by
	// re-authenticating (v1.1 §4.5).
	if opendrive.ErrorKind(err) != opendrive.KindUpstreamError {
		t.Errorf("anonymous refusal kind = %q, want upstream_error", opendrive.ErrorKind(err))
	}
	if opendrive.IsTemporary(err) {
		t.Error("a permission refusal must not be retryable")
	}
	t.Logf("anonymous download refused as expected: %v", err)

	// And verifypassword remains inert on the API route.
	got, err := files.VerifyPassword(ctx, id, password, "")
	if err != nil {
		t.Fatalf("verifypassword: %v", err)
	}
	if got.OK() || got.TempKey != "" {
		t.Errorf("verifypassword now works (%+v); revisit D32 and the P3 download plan", got)
	}
}

// ---------------------------------------------------------------- users

// TestSandboxUserInfo reads the account information the Bridge reports through
// /v1/auth/status (§4.1).
func TestSandboxUserInfo(t *testing.T) {
	c, ctx := newSandboxClient(t)

	info, err := c.Users().Info(ctx)
	if err != nil {
		t.Fatalf("users info: %v", err)
	}
	if info.UserName == "" || info.UserID == "" {
		t.Fatalf("info carries no identity: %+v", info)
	}
	if info.MaxStorage.Int64() <= 0 {
		t.Errorf("MaxStorage = %d, want the account quota", info.MaxStorage.Int64())
	}
	t.Logf("account %s, plan %q, storage %d/%d, account user: %v",
		info.UserID, info.UserPlan, info.StorageUsed.Int64(), info.MaxStorage.Int64(),
		info.IsAccountUser())

	// §9.4: the account information must not spill into logs through its own
	// String method.
	if s := info.String(); strings.Contains(s, info.UserName) {
		t.Errorf("AccountInfo.String leaks the username: %q", s)
	}

	withBW, err := c.Users().Info(ctx, opendrive.AccountInfoOptions{ApplyBW: true, Branding: true})
	if err != nil {
		t.Fatalf("users info with options: %v", err)
	}
	if withBW.UserID != info.UserID {
		t.Errorf("the optional parameters changed the identity: %v vs %v", withBW.UserID, info.UserID)
	}
}

// TestSandboxUserLogs covers both paging styles, including the cursor endpoint
// the PDF omits (D34).
func TestSandboxUserLogs(t *testing.T) {
	c, ctx := newSandboxClient(t)
	users := c.Users()

	page, err := users.Logs(ctx, 1, opendrive.ActivityLogFilter{})
	if err != nil {
		t.Fatalf("user logs: %v", err)
	}
	t.Logf("numbered paging: page %d of %d, %d entries",
		page.CurrentPage.Int(), page.TotalPages.Int(), len(page.Logs))
	if len(page.Logs) == 0 {
		t.Skip("the account has no activity to page through")
	}
	if page.Logs[0].Time.IsZero() || page.Logs[0].LogType == "" {
		t.Errorf("first log entry looks empty: %+v", page.Logs[0])
	}

	first, err := users.LogsCursor(ctx, "", opendrive.ActivityLogFilter{})
	if err != nil {
		t.Fatalf("user logs cursor: %v", err)
	}
	if len(first.Logs) == 0 {
		t.Fatal("the cursor endpoint returned no entries while the numbered one did")
	}
	if first.NextCursor == "" {
		t.Skip("the whole log fits in one cursor page")
	}

	second, err := users.LogsCursor(ctx, first.NextCursor, opendrive.ActivityLogFilter{})
	if err != nil {
		t.Fatalf("second cursor page: %v", err)
	}
	if second.NextCursor == first.NextCursor {
		t.Error("the cursor did not advance between pages")
	}
	// The invariant of keyset paging over a newest-first log: page two never
	// contains an entry newer than the oldest of page one. Comparing whole
	// entries would be wrong — two distinct events can share a timestamp and
	// carry no detail to tell them apart.
	if len(first.Logs) > 0 && len(second.Logs) > 0 {
		oldestOfFirst := first.Logs[len(first.Logs)-1].Time
		newestOfSecond := second.Logs[0].Time
		if newestOfSecond.After(oldestOfFirst.Time) {
			t.Errorf("page two starts at %v, newer than page one's oldest entry %v",
				newestOfSecond, oldestOfFirst)
		}
	}
	t.Logf("keyset paging: %d then %d entries", len(first.Logs), len(second.Logs))
}

// ---------------------------------------------------------------- sharing

// TestSandboxSharing runs the full share lifecycle when the credentials belong
// to an account owner, and asserts the documented refusal when they belong to
// an account user (docs/discrepancies.md D33).
//
// Supplying owner credentials is the only step needed to turn the constructed
// sharing fixtures into recorded ones.
func TestSandboxSharing(t *testing.T) {
	c, ctx := newSandboxClient(t)
	sharing := c.Sharing()

	info, err := c.Users().Info(ctx)
	if err != nil {
		t.Fatalf("users info: %v", err)
	}

	if info.IsAccountUser() {
		// D33: every operation is refused with the same message, writes
		// included, so the gate is the account type rather than the call.
		_, err := sharing.ListSharedUsers(ctx)
		if err == nil {
			t.Fatal("an account user was allowed to list shared users; revisit D33")
		}
		if opendrive.ErrorKind(err) != opendrive.KindUpstreamError {
			t.Errorf("refusal kind = %q, want upstream_error: a permission problem "+
				"must not look like a credential problem (v1.1 §4.5)",
				opendrive.ErrorKind(err))
		}
		if opendrive.IsTemporary(err) {
			t.Error("a permission refusal must not be retryable")
		}
		t.Logf("account user refused as documented: %v", err)

		// The write half is refused the same way, which is what makes this an
		// account-type gate rather than a per-call check.
		if _, err := sharing.Share(ctx, "0", info.UserName, opendrive.ShareViewOnly); err == nil {
			t.Error("an account user was allowed to share; revisit D33")
		}
		t.Skip("sharing needs an account-owner login; see docs/discrepancies.md D33")
	}

	// From here on the account owns itself, so the real lifecycle runs.
	scratch := fileScratch(t, ctx, c)
	peer := os.Getenv("ODB_TEST_SHARE_USER")
	if peer == "" {
		t.Skip("set ODB_TEST_SHARE_USER to a second OpenDrive account to test sharing")
	}

	created, err := sharing.Share(ctx, scratch, peer, opendrive.ShareViewOnly)
	if err != nil {
		t.Fatalf("share with %s: %v", peer, err)
	}
	shareID := created.SharingID.String()
	if shareID == "" {
		t.Fatalf("share returned no sharing id: %+v", created)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := sharing.Revoke(ctx, shareID); err != nil {
			t.Logf("cleanup: share %s may remain: %v", shareID, err)
		}
	})

	users, err := sharing.ListFolderUsers(ctx, scratch)
	if err != nil {
		t.Fatalf("list the folder's users: %v", err)
	}
	var found bool
	for _, u := range users {
		if u.SharingID.String() == shareID {
			found = true
		}
	}
	if !found {
		t.Errorf("the new share %s is absent from the folder's user list", shareID)
	}

	if err := sharing.SetMode(ctx, shareID, opendrive.ShareFullAccess); err != nil {
		t.Fatalf("set share mode: %v", err)
	}
	if err := sharing.Revoke(ctx, shareID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := sharing.ListFolderUsers(ctx, scratch)
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	for _, u := range after {
		if u.SharingID.String() == shareID {
			t.Errorf("the revoked share %s is still listed", shareID)
		}
	}
}
