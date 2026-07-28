//go:build windows

package keystore

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// restrictToOwner makes the credential file readable by its owner only.
//
// NTFS ignores the POSIX mode bits Go's Chmod pretends to set, so on Windows a
// freshly written file simply inherits the parent directory's access control
// list — which on a default profile includes Administrators, and in a shared or
// redirected location can include Users. Since this file holds the account
// password and the refresh token, that is not good enough (§9.2).
//
// icacls is used rather than a hand-built DACL through SetNamedSecurityInfo:
// it needs no additional dependency, it is present on every supported Windows
// version, and the resulting ACL is the one an administrator would inspect with
// the same tool.
func restrictToOwner(path string) error {
	me, err := user.Current()
	if err != nil {
		return fmt.Errorf("keystore: cannot determine the current user: %w", err)
	}

	// /inheritance:r drops every inherited entry, /grant:r replaces this
	// account's entries with full control. Together they leave an ACL with one
	// principal on it.
	if out, err := exec.Command("icacls", path,
		"/inheritance:r",
		"/grant:r", me.Username+":(F)",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("keystore: cannot restrict the credential file ACL: %w: %s",
			err, strings.TrimSpace(string(out)))
	}

	// Any explicit entry for a broad principal is removed as well. A file
	// written by an older build, or inside a directory an administrator has
	// customised, can carry one of these directly rather than by inheritance.
	for _, principal := range []string{
		`BUILTIN\Users`,
		`Everyone`,
		`NT AUTHORITY\Authenticated Users`,
		`BUILTIN\Administrators`,
	} {
		// A principal that has no entry makes this a no-op, so the error is
		// deliberately ignored.
		_ = exec.Command("icacls", path, "/remove:g", principal).Run()
	}
	return nil
}
