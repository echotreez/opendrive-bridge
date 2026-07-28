//go:build windows

package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// These tests run against the real Windows Credential Manager on the
// windows-latest runner. They are the Windows half of what
// TestKeychainOnRealMacOS does for macOS, and they are what closes the
// three-platform gap recorded in CLAUDE.md.

func newRealCredManStore(t *testing.T) *keyringStore {
	t.Helper()
	k := newKeyring("odb-keystore-selftest", fmt.Sprintf("test-%d-%d", os.Getpid(), testRunID()))
	if err := k.Available(context.Background()); err != nil {
		t.Fatalf("the Credential Manager must be usable on a Windows runner: %v", err)
	}
	t.Cleanup(func() { _ = k.Delete(context.Background()) })
	return k
}

// TestCredentialManagerRoundTrip is the contract every backend satisfies,
// exercised against the real vault.
func TestCredentialManagerRoundTrip(t *testing.T) {
	assertRoundTrip(t, newRealCredManStore(t))
}

// A credential the bridge actually stores is around a kilobyte of base64 once
// the token pair and the password are in it, which is well inside the Win32
// blob limit but worth proving rather than assuming.
func TestCredentialManagerHandlesAFullCredential(t *testing.T) {
	ctx := context.Background()
	k := newRealCredManStore(t)

	cred := sampleCredentials()
	cred.SessionID = "SESSION-abcdef0123456789"
	cred.Password = strings.Repeat("p", 128)
	if err := k.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := k.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Password != cred.Password || got.SessionID != cred.SessionID {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Token == nil || got.Token.RefreshToken != cred.Token.RefreshToken {
		t.Fatalf("token round trip failed: %+v", got.Token)
	}
}

// A missing item is ErrNoCredentials, not an error, exactly as on the other
// two platforms.
func TestCredentialManagerMissingItem(t *testing.T) {
	ctx := context.Background()
	k := newKeyring("odb-keystore-selftest", fmt.Sprintf("absent-%d-%d", os.Getpid(), testRunID()))
	if _, err := k.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("Load of a missing item = %v, want ErrNoCredentials", err)
	}
	// Deleting something that is not there is not an error either.
	if err := k.Delete(ctx); err != nil {
		t.Fatalf("Delete of a missing item: %v", err)
	}
}

// Rotation must replace the stored credential rather than accumulate entries,
// which is what keeps the refresh-token rotation of §9.2 atomic.
func TestCredentialManagerRotationReplaces(t *testing.T) {
	ctx := context.Background()
	k := newRealCredManStore(t)

	first := sampleCredentials()
	if err := k.Save(ctx, first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := first.Clone()
	second.Token.AccessToken = "second-access-token"
	second.Token.RefreshToken = "second-refresh-token"
	if err := k.Save(ctx, second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := k.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Token.RefreshToken != "second-refresh-token" {
		t.Fatalf("rotation did not replace the stored credential: %+v", got.Token)
	}
}

// Open must now choose the Credential Manager on Windows, which is the whole
// point of the exercise: the encrypted file stops being the default there.
func TestOpenPrefersTheCredentialManagerOnWindows(t *testing.T) {
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	s, err := Open(Config{
		Backend: BackendAuto,
		Service: "odb-keystore-selftest",
		Account: fmt.Sprintf("auto-%d-%d", os.Getpid(), testRunID()),
		Path:    fmt.Sprintf("%s\\credentials.enc", t.TempDir()),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Backend() != BackendKeyring {
		t.Fatalf("backend = %q, want the keyring: Windows should no longer fall back to a file", s.Backend())
	}
	t.Cleanup(func() { _ = s.Delete(context.Background()) })
	assertRoundTrip(t, s)
}
