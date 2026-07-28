//go:build windows

package keystore

import "errors"

// This file is the Windows Credential Manager backend. It is deliberately
// unimplemented in this commit: the tests in keyring_windows_test.go are
// written first and must fail on the windows-latest runner before the syscalls
// below are filled in (whitepaper §6.1, test-first).

var errCredManNotImplemented = errors.New("keystore: the Windows Credential Manager backend is not implemented yet")

func credManAvailable() error { return errCredManNotImplemented }

func credManLoad(string) (string, error) { return "", errCredManNotImplemented }

func credManSave(string, string) error { return errCredManNotImplemented }

func credManDelete(string) error { return errCredManNotImplemented }
