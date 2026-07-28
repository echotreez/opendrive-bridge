//go:build windows

package keystore

import "errors"

// restrictToOwner makes the credential file readable by its owner only.
//
// NTFS ignores the POSIX mode bits Go's Chmod pretends to set, so on Windows
// the file keeps whatever the parent directory hands down — typically readable
// by Users and Administrators. This replaces the inherited ACL with a single
// entry for the current account (§9.2).
//
// Not implemented in this commit: the test in perm_windows_test.go is written
// first and must fail on the windows-latest runner.
func restrictToOwner(path string) error {
	return errors.New("keystore: tightening the credential file ACL is not implemented yet")
}
