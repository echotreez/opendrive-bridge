//go:build !windows

package keystore

import (
	"fmt"
	"os"
)

// restrictToOwner makes the credential file readable by its owner only.
//
// On a POSIX system the mode bits say it all: 0600, set when the temporary file
// is created and reasserted here in case an existing file was created by an
// older build with a laxer mode (§9.2).
func restrictToOwner(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("keystore: cannot restrict the credential file: %w", err)
	}
	return nil
}
