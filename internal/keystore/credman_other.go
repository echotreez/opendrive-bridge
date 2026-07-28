//go:build !windows

package keystore

import "errors"

// The Credential Manager exists only on Windows. Everywhere else these stubs
// keep keyring.go free of build tags; the platform switch inside it never
// reaches them.

var errCredManUnavailable = errors.New("keystore: the Windows Credential Manager is only available on Windows")

func credManAvailable() error { return errCredManUnavailable }

func credManLoad(string) (string, error) { return "", errCredManUnavailable }

func credManSave(string, string) error { return errCredManUnavailable }

func credManDelete(string) error { return errCredManUnavailable }
