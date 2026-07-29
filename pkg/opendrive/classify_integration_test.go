//go:build integration

package opendrive_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The account root is the one place this login genuinely may not write, and it
// refuses with the exact message D39 saw for a transient condition. Classifying
// it correctly is the whole point of the classification layer: the wording is
// identical, so only the gathered evidence can tell them apart (D40).
func TestSandboxAmbiguousPermissionRefusal(t *testing.T) {
	c, ctx := newSandboxClient(t)

	_, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{
		Name:     fmt.Sprintf("odb-test-denied-%d", time.Now().Unix()),
		ParentID: "0", // the account root, where this login has no rights
	})
	if err == nil {
		t.Skip("this account can write to the account root, so there is no denial to classify")
	}

	var ae *opendrive.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %T: %v", err, err)
	}
	t.Logf("root refusal: kind=%s http=%d msg=%q diagnosis=%q",
		ae.Kind, ae.HTTPCode, ae.UpstreamMsg, ae.Diagnosis())

	// It is a refusal, not a credential problem. Mapping it onto a credential
	// kind would send the silent re-login machine chasing a password that was
	// never wrong (§4.5).
	if ae.Kind != opendrive.KindUpstreamError {
		t.Errorf("kind = %q, want %q", ae.Kind, opendrive.KindUpstreamError)
	}
	// The response came from the API, in JSON — that much is decidable without
	// any probe, and it is what separates this from D38's HTML 401.
	if ae.BodyShape() != opendrive.ShapeJSON {
		t.Errorf("body shape = %q, want json", ae.BodyShape())
	}
	// This login has never written to the root, so nothing licenses a retry.
	if ae.Temporary() {
		t.Error("a genuine denial was classified as retryable")
	}
	if strings.Contains(strings.ToLower(ae.Diagnosis()), "transient") {
		t.Errorf("diagnosis = %q, want it not to claim a real denial is transient", ae.Diagnosis())
	}
}

// The same operation in a folder this login owns must keep working, and must
// not be dragged down by the refusal above — the witness and the probe are per
// resource and shared respectively, and neither may leak a verdict sideways.
func TestSandboxClassificationDoesNotPoisonAWorkingFolder(t *testing.T) {
	c, ctx := newSandboxClient(t)
	base := writableBase(t, ctx, c)

	name := fmt.Sprintf("odb-test-classify-%d", time.Now().UnixNano())
	created, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{Name: name, ParentID: base})
	if err != nil {
		t.Fatalf("create in the granted folder: %v", err)
	}
	// D39: every artefact is removed, intermediate ones included.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.Folders().Trash(cleanupCtx, []string{created.FolderID.String()}); err != nil {
			t.Logf("cleanup trash: %v", err)
		}
		if err := c.Folders().Remove(cleanupCtx, []string{created.FolderID.String()}, "", ""); err != nil {
			t.Logf("cleanup remove: %v", err)
		}
	})

	if created.FolderID.String() == "" {
		t.Fatal("create returned no folder id")
	}
}

// A read that is refused for a reason upstream states plainly needs no probe and
// no retry: the sharing module answers every call the same way for an account
// user, and that is a property of the login (D33, taxonomy T3).
func TestSandboxAccountTypeGateIsPermanent(t *testing.T) {
	c, ctx := newSandboxClient(t)

	info, err := c.Users().Info(ctx)
	if err != nil {
		t.Fatalf("users info: %v", err)
	}
	if !info.IsAccountUser() {
		t.Skip("this login is an account owner, so the sharing gate does not apply (D33)")
	}

	_, err = c.Sharing().ListSharedUsers(ctx)
	if err == nil {
		t.Skip("sharing is open to this login after all")
	}

	var ae *opendrive.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %T: %v", err, err)
	}
	if ae.Kind != opendrive.KindUpstreamError {
		t.Errorf("kind = %q, want %q", ae.Kind, opendrive.KindUpstreamError)
	}
	if ae.Temporary() {
		t.Error("an account-type gate was classified as retryable; no retry can change it")
	}
	if ae.Ambiguous() {
		t.Error("a message this specific should not have needed disambiguation")
	}
}
