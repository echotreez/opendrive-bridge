package keystore

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

func sampleCredentials() *opendrive.StoredCredentials {
	return &opendrive.StoredCredentials{
		Username: "derek@example.com",
		Password: "correct horse battery staple",
		AuthMode: opendrive.AuthModeOAuth2,
		UserID:   "2125533",
		AccType:  1,
		Token: &opendrive.Token{
			AccessToken:   "access-token-value",
			RefreshToken:  "refresh-token-value",
			IssuedAt:      time.Unix(1753444800, 0).UTC(),
			Expiry:        time.Unix(1753531200, 0).UTC(),
			RefreshExpiry: time.Unix(1756036800, 0).UTC(),
		},
		UpdatedAt: time.Unix(1753444800, 0).UTC(),
	}
}

// assertRoundTrip is the contract every backend has to satisfy.
func assertRoundTrip(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := s.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("an empty store returned %v, want ErrNoCredentials", err)
	}
	if err := s.Available(ctx); err != nil {
		t.Fatalf("Available on an empty store: %v", err)
	}

	want := sampleCredentials()
	if err := s.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Username != want.Username || got.Password != want.Password {
		t.Fatalf("account round trip failed: %+v", got)
	}
	if got.Token == nil || got.Token.AccessToken != want.Token.AccessToken ||
		got.Token.RefreshToken != want.Token.RefreshToken {
		t.Fatalf("token round trip failed: %+v", got.Token)
	}
	if !got.Token.Expiry.Equal(want.Token.Expiry) || !got.Token.IssuedAt.Equal(want.Token.IssuedAt) {
		t.Fatalf("timestamps round trip failed: %+v", got.Token)
	}
	if got.AuthMode != want.AuthMode || got.UserID != want.UserID || got.AccType != want.AccType {
		t.Fatalf("metadata round trip failed: %+v", got)
	}

	// §9.2: rotation replaces the whole record atomically.
	rotated := got.Clone()
	rotated.Token.AccessToken = "second-access-token"
	rotated.Token.RefreshToken = "second-refresh-token"
	rotated.SessionID = "SESSION-1"
	if err := s.Save(ctx, rotated); err != nil {
		t.Fatalf("rotating Save: %v", err)
	}
	got, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load after rotation: %v", err)
	}
	if got.Token.RefreshToken != "second-refresh-token" || got.SessionID != "SESSION-1" {
		t.Fatalf("rotation did not stick: %+v", got)
	}

	if err := s.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("Load after Delete returned %v", err)
	}
	// Deleting twice is not an error.
	if err := s.Delete(ctx); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// ---------------------------------------------------------------- file backend

func newTestFileStore(t *testing.T) *fileStore {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s, err := newFileStore(filepath.Join(t.TempDir(), "credentials.enc"), key)
	if err != nil {
		t.Fatalf("newFileStore: %v", err)
	}
	return s
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := newTestFileStore(t)
	if s.Backend() != BackendFile {
		t.Fatalf("backend = %q", s.Backend())
	}
	assertRoundTrip(t, s)
}

// §9.2: the file is encrypted and readable only by its owner.
func TestFileStoreEncryptsAndRestrictsPermissions(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	for _, secret := range []string{"correct horse battery staple", "access-token-value",
		"refresh-token-value", "derek@example.com"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("the credential file leaks %q in clear text", secret)
		}
	}
	if !strings.HasPrefix(blob, fileFormat) {
		t.Fatalf("missing format header: %q", blob[:min(8, len(blob))])
	}

	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// POSIX permission bits are meaningless on Windows (NTFS uses ACLs; Go
	// reports synthetic modes there). Tightening the file via ACLs is part of
	// the owed Windows keystore work — see CLAUDE.md item 2.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("file mode = %04o, want 0600", perm)
		}
	}
}

func TestFileStoreRejectsTheWrongKey(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}

	other := make([]byte, 32)
	wrong, err := newFileStore(s.path, other)
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrong.Load(ctx)
	if err == nil {
		t.Fatal("a wrong key must not decrypt the file")
	}
	mustMention(t, err, DefaultKeyEnv)
	if wrong.Available(ctx) == nil {
		t.Fatal("Available must report an undecryptable file")
	}
}

func TestFileStoreRejectsForeignContent(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := os.WriteFile(s.path, []byte("just some text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); err == nil || !strings.Contains(err.Error(), "known format") {
		t.Fatalf("err = %v", err)
	}
	if err := os.WriteFile(s.path, []byte(fileFormat+"short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v", err)
	}
}

// A crash mid-write must never destroy the previous credentials (§9.2).
func TestFileStoreWritesAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}

	// A failed write leaves no partial file behind and no stray temporaries.
	if err := writeFileAtomic(filepath.Join(s.path, "impossible", "x"), []byte("data")); err == nil {
		t.Fatal("expected the nested write to fail")
	}
	after, err := os.ReadFile(s.path)
	if err != nil || string(after) != string(before) {
		t.Fatal("the previous credentials did not survive a failed write")
	}
	entries, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".credentials-") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestFileStoreRejectsNilAndBadKeys(t *testing.T) {
	ctx := context.Background()
	s := newTestFileStore(t)
	if err := s.Save(ctx, nil); err == nil {
		t.Fatal("storing nil must fail")
	}
	if _, err := newFileStore("x", []byte("short")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	if _, err := newFileStore("", make([]byte, 32)); err == nil {
		t.Fatal("an empty path must fail")
	}
}

func TestResolveKey(t *testing.T) {
	// A base64 32-byte value is used verbatim.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	got, err := resolveKey(Config{Key: []byte(encoded), KeyEnv: DefaultKeyEnv})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(key) {
		t.Fatal("a base64 key must be used as-is")
	}

	// A passphrase is hashed to 32 bytes.
	got, err = resolveKey(Config{Key: []byte("a passphrase"), KeyEnv: DefaultKeyEnv})
	if err != nil || len(got) != 32 {
		t.Fatalf("passphrase key = %d bytes, %v", len(got), err)
	}

	// The environment is the documented channel.
	t.Setenv(DefaultKeyEnv, "from the environment")
	got, err = resolveKey(Config{KeyEnv: DefaultKeyEnv})
	if err != nil || len(got) != 32 {
		t.Fatalf("env key = %d bytes, %v", len(got), err)
	}

	t.Setenv(DefaultKeyEnv, "")
	if _, err := resolveKey(Config{KeyEnv: DefaultKeyEnv}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

// ---------------------------------------------------------------- Open

// withoutAVault makes Open behave as it would on a machine with no OS
// credential vault: a headless Linux box or a Docker container.
func withoutAVault(t *testing.T) {
	t.Helper()
	previous := keyringFactory
	keyringFactory = func(service, account string) *keyringStore {
		k := newKeyring(service, account)
		k.platform = "nothing-here"
		return k
	}
	t.Cleanup(func() { keyringFactory = previous })
}

// §9.2: with nothing configured and no vault available, Open must fail rather
// than hand back a store that forgets everything on restart. This is the
// "no persistence = configuration error" gate.
func TestOpenRefusesToRunWithoutPersistence(t *testing.T) {
	withoutAVault(t)
	t.Setenv(DefaultKeyEnv, "")

	_, err := Open(Config{Backend: BackendAuto, Service: "odb-test-none"})
	if !errors.Is(err, ErrNoBackend) {
		t.Fatalf("err = %v, want ErrNoBackend", err)
	}
	// The message has to tell the operator all three ways out.
	mustMention(t, err, "--ephemeral")
	mustMention(t, err, DefaultKeyEnv)
	mustMention(t, err, "keyring")
}

// The keyring backend requested explicitly must fail loudly rather than fall
// back to anything.
func TestOpenKeyringFailsWhenThereIsNoVault(t *testing.T) {
	withoutAVault(t)
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	if _, err := Open(Config{Backend: BackendKeyring}); err == nil ||
		!strings.Contains(err.Error(), "not usable") {
		t.Fatalf("err = %v", err)
	}
}

// Without a vault but with a key configured, auto lands on the encrypted file:
// the Docker and headless case.
func TestOpenAutoFallsBackToTheFileWithoutAVault(t *testing.T) {
	withoutAVault(t)
	dir := t.TempDir()
	t.Setenv(DefaultKeyEnv, "a configured passphrase")

	s, err := Open(Config{Backend: BackendAuto, Path: filepath.Join(dir, "credentials.enc")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Backend() != BackendFile {
		t.Fatalf("backend = %q, want the encrypted file", s.Backend())
	}
	assertRoundTrip(t, s)
}

// On a machine that does have a vault, auto uses it in preference to a file.
func TestOpenAutoPrefersTheVault(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("this check needs a real OS keyring")
	}
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	s, err := Open(Config{Backend: BackendAuto, Service: "odb-test-auto-prefers",
		Path: filepath.Join(t.TempDir(), "credentials.enc")})
	if err != nil {
		t.Skipf("no usable keychain in this environment: %v", err)
	}
	if s.Backend() != BackendKeyring {
		t.Fatalf("backend = %q, want the keyring", s.Backend())
	}
}

func TestOpenEphemeralRequiresOptIn(t *testing.T) {
	if _, err := Open(Config{Backend: BackendEphemeral}); !errors.Is(err, ErrEphemeralNotAllowed) {
		t.Fatalf("err = %v, want ErrEphemeralNotAllowed", err)
	}
	s, err := Open(Config{Backend: BackendEphemeral, AllowEphemeral: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Backend() != BackendEphemeral {
		t.Fatalf("backend = %q", s.Backend())
	}
	if err := s.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	assertRoundTrip(t, s)

	// The daemon must be able to tell that this store forgets everything.
	eph, ok := s.(opendrive.EphemeralStore)
	if !ok || !eph.Ephemeral() {
		t.Fatal("the ephemeral store must announce itself")
	}
}

func TestOpenFileBackend(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	s, err := Open(Config{Backend: BackendFile, Path: filepath.Join(dir, "credentials.enc")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Backend() != BackendFile {
		t.Fatalf("backend = %q", s.Backend())
	}
	assertRoundTrip(t, s)

	t.Setenv(DefaultKeyEnv, "")
	if _, err := Open(Config{Backend: BackendFile, Path: filepath.Join(dir, "x.enc")}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

// With a key configured, auto falls back to the encrypted file on machines
// without a vault — the Docker and headless case of §9.2.
func TestOpenAutoUsesTheFileWhenAKeyIsConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	s, err := Open(Config{Backend: BackendAuto, Path: filepath.Join(dir, "credentials.enc"),
		Service: "odb-test-auto"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	switch s.Backend() {
	case BackendKeyring, BackendFile:
	default:
		t.Fatalf("backend = %q", s.Backend())
	}
}

func TestOpenRejectsAnUnknownBackend(t *testing.T) {
	if _, err := Open(Config{Backend: "sqlite"}); !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Backend != BackendAuto || got.Service != DefaultService ||
		got.Account != DefaultAccount || got.KeyEnv != DefaultKeyEnv {
		t.Fatalf("defaults = %+v", got)
	}
	if dir := defaultStateDir(); dir == "" || !filepath.IsAbs(dir) {
		t.Fatalf("default state dir = %q", dir)
	}
	if p := filePath(Config{Path: "/tmp/x.enc"}); p != "/tmp/x.enc" {
		t.Fatalf("explicit path = %q", p)
	}
	if p := filePath(Config{}.withDefaults()); filepath.Base(p) != DefaultFileName {
		t.Fatalf("default path = %q", p)
	}
}

func mustMention(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), what) {
		t.Fatalf("error %v does not mention %q", err, what)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
