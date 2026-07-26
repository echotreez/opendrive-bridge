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
	"os"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// newSandboxClient logs in with the credentials in the environment. It skips
// the test rather than failing when they are absent, so a plain
// `go test -tags=integration ./...` on a machine without credentials is quiet.
func newSandboxClient(t *testing.T) (*opendrive.Client, context.Context) {
	t.Helper()

	user, pass := os.Getenv("ODB_SPEC_USER"), os.Getenv("ODB_SPEC_PASS")
	if user == "" || pass == "" {
		t.Skip("set ODB_SPEC_USER and ODB_SPEC_PASS (see scripts/integration-test.sh)")
	}

	c, err := opendrive.New()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	auth := opendrive.NewOAuth2(c)
	c.SetAuthenticator(auth)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	if err := auth.Login(ctx, user, pass); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c, ctx
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
	_, copyErr := f.MoveCopy(ctx, opendrive.MoveCopyParams{
		FolderID: id, DstFolderID: base, Move: false, NewName: name + "-copy",
	})
	report("copy", copyErr)
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
