//go:build windows

package keystore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// The Windows Credential Manager backend.
//
// Unlike macOS and Linux there is no command line tool that can read a secret
// back — cmdkey writes and lists but never prints — so this goes through the
// Win32 credential API directly. That also keeps the secret out of any process
// argument list, which is the same property the other two backends get from
// passing the payload on stdin (§9.2).
//
// The calls are made through a lazily loaded DLL rather than cgo, so the
// binaries stay CGO_ENABLED=0 and cross-compile from any host (§8.1).
var (
	advapi32        = syscall.NewLazyDLL(systemDLL("advapi32.dll"))
	procCredReadW   = advapi32.NewProc("CredReadW")
	procCredWriteW  = advapi32.NewProc("CredWriteW")
	procCredDeleteW = advapi32.NewProc("CredDeleteW")
	procCredFree    = advapi32.NewProc("CredFree")
)

// systemDLL resolves a system library by absolute path so that a DLL of the
// same name sitting in the working directory cannot be loaded instead
// (§9.5, supply chain).
func systemDLL(name string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", name)
}

// Win32 constants from wincred.h.
const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	// CRED_MAX_CREDENTIAL_BLOB_SIZE.
	credMaxBlobSize = 5 * 512
	// ERROR_NOT_FOUND.
	errorNotFound syscall.Errno = 1168
)

// credentialW mirrors the Win32 CREDENTIALW structure. The field order and
// widths are load-bearing: the API reads this memory directly.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// credManAvailable reports whether the credential API can be reached at all.
func credManAvailable() error {
	for _, proc := range []*syscall.LazyProc{procCredReadW, procCredWriteW, procCredDeleteW, procCredFree} {
		if err := proc.Find(); err != nil {
			return fmt.Errorf("%s is unavailable: %w", proc.Name, err)
		}
	}
	// Reading an entry that cannot exist proves the vault answers. "Not found"
	// is the healthy reply; anything else means the service is unreachable.
	if _, err := credManLoad(`opendrive-bridge:availability-probe`); err != nil &&
		!errors.Is(err, errItemNotFound) {
		return err
	}
	return nil
}

// credManLoad reads the payload stored under a target name.
func credManLoad(target string) (string, error) {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return "", fmt.Errorf("keystore: invalid credential target: %w", err)
	}

	var cred *credentialW
	ret, _, callErr := procCredReadW.Call(
		uintptr(unsafe.Pointer(name)),
		credTypeGeneric,
		0,
		uintptr(unsafe.Pointer(&cred)),
	)
	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorNotFound {
			return "", errItemNotFound
		}
		return "", fmt.Errorf("keystore: CredReadW failed: %w", callErr)
	}
	defer func() { _, _, _ = procCredFree.Call(uintptr(unsafe.Pointer(cred))) }()

	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return "", errItemNotFound
	}
	// The blob belongs to the API until CredFree, so it is copied out.
	payload := string(unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize))
	return payload, nil
}

// credManSave writes the payload, replacing any existing entry. CredWriteW
// replaces atomically, which is what the rolling refresh token needs (§9.2).
func credManSave(target, payload string) error {
	if payload == "" {
		return errors.New("keystore: refusing to store an empty credential")
	}
	blob := []byte(payload)
	if len(blob) > credMaxBlobSize {
		return fmt.Errorf("keystore: the credential is %d bytes, over the Credential Manager limit of %d",
			len(blob), credMaxBlobSize)
	}

	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("keystore: invalid credential target: %w", err)
	}
	comment, err := syscall.UTF16PtrFromString("OpenDrive Bridge credentials")
	if err != nil {
		return err
	}
	// The user name is what the Credential Manager UI shows; the secret itself
	// is the blob.
	userName, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}

	cred := credentialW{
		Type:               credTypeGeneric,
		TargetName:         name,
		Comment:            comment,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachine,
		UserName:           userName,
	}
	ret, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	// The blob must outlive the call; without this the collector may move or
	// free it while the API is reading.
	runtime.KeepAlive(blob)
	runtime.KeepAlive(cred)
	if ret == 0 {
		return fmt.Errorf("keystore: CredWriteW failed: %w", callErr)
	}
	return nil
}

// credManDelete removes the entry. A missing entry reports errItemNotFound,
// which the caller treats as success.
func credManDelete(target string) error {
	name, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("keystore: invalid credential target: %w", err)
	}
	ret, _, callErr := procCredDeleteW.Call(uintptr(unsafe.Pointer(name)), credTypeGeneric, 0)
	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorNotFound {
			return errItemNotFound
		}
		return fmt.Errorf("keystore: CredDeleteW failed: %w", callErr)
	}
	return nil
}
