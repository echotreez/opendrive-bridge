//go:build windows

package keystore

import (
	"context"
	"os/exec"
	"os/user"
	"strings"
	"testing"
)

// TestCredentialFileACLIsOwnerOnly is the Windows replacement for the POSIX
// 0600 assertion. NTFS ignores the mode bits Go's Chmod pretends to set, so
// without an explicit ACL the credential file inherits whatever the parent
// directory allows — on a default profile that includes Administrators, and in
// a shared location it can include Users (§9.2).
func TestCredentialFileACLIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := exec.Command("icacls", s.path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v\n%s", s.path, err, out)
	}
	acl := string(out)
	t.Logf("icacls:\n%s", acl)

	// Inheritance must be broken, otherwise the entries below are only half
	// the story.
	if strings.Contains(acl, "(I)") {
		t.Errorf("the credential file still inherits ACEs:\n%s", acl)
	}

	// The account that wrote the file must be able to read it.
	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	shortName := me.Username
	if i := strings.LastIndex(shortName, `\`); i >= 0 {
		shortName = shortName[i+1:]
	}
	if !strings.Contains(acl, me.Username) && !strings.Contains(acl, shortName) {
		t.Errorf("the owner %q has no entry in the ACL:\n%s", me.Username, acl)
	}

	// And nobody else should. BUILTIN\Users and Everyone are the two that a
	// default inherited ACL would bring along.
	for _, unwanted := range []string{`BUILTIN\Users`, "Everyone", `NT AUTHORITY\Authenticated Users`} {
		if strings.Contains(acl, unwanted) {
			t.Errorf("%s can still reach the credential file:\n%s", unwanted, acl)
		}
	}
}

// Rewriting an existing file must leave it just as locked down: rotation
// happens through a temporary file and a rename, which is exactly where an ACL
// can quietly come back (§9.2).
func TestCredentialFileACLSurvivesRotation(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	rotated := sampleCredentials()
	rotated.Token.RefreshToken = "second-refresh-token"
	if err := s.Save(ctx, rotated); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	out, err := exec.Command("icacls", s.path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "(I)") {
		t.Errorf("the rotated file inherits ACEs again:\n%s", out)
	}
}
