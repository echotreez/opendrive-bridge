package keystore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

func sampleCredentials() *opendrive.StoredCredentials {
	return &opendrive.StoredCredentials{
		Username: "derek@example.com",
		Password: "correct horse battery staple",
		AuthMode: opendrive.AuthModeOAuth2,
		UserID:   "2125533",
		AccType:  1,
		Token: &opendrive.Token{
			AccessToken:  "access-token-value",
			RefreshToken: "refresh-token-value",
			Expiry:       time.Unix(1753531200, 0).UTC(),
		},
	}
}

func newTestStore(t *testing.T) *envStore {
	t.Helper()
	cheapKDF(t)
	s, err := newEnvStore(filepath.Join(t.TempDir(), EnvFileName), nil)
	if err != nil {
		t.Fatalf("newEnvStore: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	want := sampleCredentials()

	if err := s.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Username != want.Username || got.Password != want.Password {
		t.Errorf("account = %q/%q", got.Username, got.Password)
	}
	if got.Token == nil || got.Token.AccessToken != want.Token.AccessToken ||
		got.Token.RefreshToken != want.Token.RefreshToken {
		t.Errorf("token = %+v", got.Token)
	}
	if !got.Token.Expiry.Equal(want.Token.Expiry) {
		t.Errorf("expiry = %v, want %v", got.Token.Expiry, want.Token.Expiry)
	}
	if got.AuthMode != want.AuthMode || got.UserID != want.UserID || got.AccType != want.AccType {
		t.Errorf("metadata = %+v", got)
	}
}

// The file must be ciphertext *and* readable only by its owner. Both halves of
// §9.2.2 in one test, because a file that is encrypted but world-readable and
// one that is 0600 but plaintext are both failures.
func TestTheFileIsEncryptedAndPrivate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "correct horse") || strings.Contains(string(raw), "derek") {
		t.Fatalf("the file holds readable credentials:\n%s", raw)
	}
	if !isEncrypted(raw) {
		t.Fatal("the file is not in the encrypted format")
	}

	if runtime.GOOS != "windows" {
		for _, p := range []string{s.path, s.keyPath} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("%s mode = %04o, want 0600", filepath.Base(p), mode)
			}
		}
	}
}

// The first run is the point of the design: a user copies the example, types
// their password in the clear, and starting the daemon once takes it away again.
func TestFirstRunSealsThePlaintextTheUserWrote(t *testing.T) {
	ctx := context.Background()
	cheapKDF(t)
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)

	plaintext := "# copied from .env.example\n" +
		"ODB_USERNAME=derek@example.com\n" +
		"ODB_PASSWORD=correct horse battery staple\n"
	if err := os.WriteFile(path, []byte(plaintext), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := newEnvStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cred.Username != "derek@example.com" || cred.Password != "correct horse battery staple" {
		t.Fatalf("credentials = %q/%q", cred.Username, cred.Password)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "correct horse") {
		t.Fatal("the plaintext password is still on disk after the first run")
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatalf("the key file was not created: %v", err)
	}

	// And a second store, as a restarted daemon would be, reads it back.
	again, err := newEnvStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred2, err := again.Load(ctx)
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if cred2.Password != "correct horse battery staple" {
		t.Fatalf("password after restart = %q", cred2.Password)
	}
}

// The API key is generated once and then stays. One that changed on every start
// would break every client the user had configured.
func TestTheAPIKeyIsGeneratedOnceAndKept(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, err := s.APIKey(ctx)
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if len(first) < 32 {
		t.Fatalf("API key is %d characters, not enough to be worth having", len(first))
	}
	second, err := s.APIKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the API key changed between calls")
	}

	reopened, err := newEnvStore(s.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := reopened.APIKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatal("the API key changed across a restart")
	}
}

// Signing out clears the account and keeps the API key: a user logging out of
// OpenDrive has not asked to re-key the programs talking to their bridge.
func TestLogoutKeepsTheAPIKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	key, err := s.APIKey(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Errorf("Load after delete = %v, want ErrNoCredentials", err)
	}
	after, err := s.APIKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != key {
		t.Error("the API key was regenerated by a logout")
	}
}

// Losing .env.key must be reported as what it is. This is the "no persistence =
// configuration error" gate: name the missing file, say what to do, and do it
// without touching the network.
func TestAMissingKeyFileSaysWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.keyPath); err != nil {
		t.Fatal(err)
	}

	reopened, err := newEnvStore(s.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reopened.Load(ctx)
	if err == nil {
		t.Fatal("a missing key file was not noticed")
	}
	msg := err.Error()
	for _, want := range []string{KeyFileName, ExampleFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %s: %s", want, msg)
		}
	}
	if err := reopened.Available(ctx); err == nil {
		t.Error("Available said the store was usable")
	}
}

func TestTheWrongKeyIsReportedNotGuessedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.keyPath, []byte("a completely different key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := newEnvStore(s.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(ctx); err == nil {
		t.Fatal("the wrong key produced credentials")
	} else if !strings.Contains(err.Error(), KeyFileName) {
		t.Errorf("the message does not name the key file: %v", err)
	}
}

// Altering the sealed file must not go unnoticed. CBC is malleable, so this is
// the second half of the argument in envelope.go: the plaintext carries a
// checksum, and an edited file fails to load rather than loading something
// subtly different.
func TestAnAlteredFileIsRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	key, err := s.loadKey()
	if err != nil {
		t.Fatal(err)
	}

	tampered := map[string]string{
		envUsername: "someone-else",
		envPassword: "not the real one",
		envChecksum: strings.Repeat("0", 64),
	}
	sealed, err := seal(renderEnv(tampered), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, sealed, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := newEnvStore(s.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(ctx); err == nil {
		t.Fatal("an altered file was accepted")
	} else if !strings.Contains(err.Error(), "altered") {
		t.Errorf("the message does not say the file was altered: %v", err)
	}
}

// A key supplied through the environment is used as it is and no key file is
// written. That is how a container gets one (§8.3) without a writable directory.
func TestAKeyFromTheEnvironmentNeedsNoKeyFile(t *testing.T) {
	ctx := context.Background()
	cheapKDF(t)
	dir := t.TempDir()
	t.Setenv(DefaultKeyEnv, "a configured passphrase")

	s, err := Open(Config{Backend: BackendFile, Path: filepath.Join(dir, EnvFileName)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a key file was written even though the key came from the environment")
	}
	if _, err := s.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
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
		t.Errorf("backend = %q", s.Backend())
	}
	if err := s.Available(context.Background()); err != nil {
		t.Errorf("Available: %v", err)
	}
}

func TestOpenRejectsAnUnknownBackendAndNamesTheChoices(t *testing.T) {
	_, err := Open(Config{Backend: "file"})
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("err = %v, want ErrUnsupportedBackend", err)
	}
	// "file" is what a person reaches for; "encrypted_file" is what it is
	// called, so the message has to say so.
	if !strings.Contains(err.Error(), string(BackendFile)) {
		t.Errorf("the message does not name the alternatives: %v", err)
	}
}

func TestAutoAndFileAreTheSameStore(t *testing.T) {
	cheapKDF(t)
	dir := t.TempDir()
	for _, backend := range []Backend{BackendAuto, BackendFile, ""} {
		s, err := Open(Config{Backend: backend, Path: filepath.Join(dir, EnvFileName)})
		if err != nil {
			t.Fatalf("Open(%q): %v", backend, err)
		}
		if s.Backend() != BackendFile {
			t.Errorf("Open(%q) gave backend %q", backend, s.Backend())
		}
	}
}

// Nothing there yet is not an error; it is a bridge waiting to be set up.
func TestAnAbsentFileReadsAsNoCredentials(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("Load = %v, want ErrNoCredentials", err)
	}
	if err := s.Available(ctx); err != nil {
		t.Errorf("Available = %v, want nil for a store that is merely empty", err)
	}
}

func TestValuesSurviveTheirAwkwardCharacters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cred := sampleCredentials()
	cred.Password = `a "quoted" pass#word with spaces = and equals`
	if err := s.Save(ctx, cred); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != cred.Password {
		t.Errorf("password came back as %q, want %q", got.Password, cred.Password)
	}
}

// Anything in the file that is not ours must survive a write, or the first token
// refresh would silently delete the user's own lines.
func TestUnknownLinesSurviveAWrite(t *testing.T) {
	ctx := context.Background()
	cheapKDF(t)
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)
	if err := os.WriteFile(path, []byte("ODB_USERNAME=derek\nSOMETHING_ELSE=keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newEnvStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	fields, err := s.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fields["SOMETHING_ELSE"] != "keep me" {
		t.Errorf("an unrelated value was lost: %q", fields["SOMETHING_ELSE"])
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Backend != BackendAuto || got.KeyEnv != DefaultKeyEnv {
		t.Errorf("defaults = %+v", got)
	}
}

// The default location is beside the program, because v1.2 runs in place: a user
// who unpacks the archive finds their credentials in the same folder (§8.2).
func TestTheDefaultPathIsBesideTheProgram(t *testing.T) {
	got := filePath(Config{}.withDefaults())
	if filepath.Base(got) != EnvFileName {
		t.Errorf("default path = %q", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip("this platform cannot report the executable path")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if filepath.Dir(got) != filepath.Dir(exe) {
		t.Errorf("default path %q is not beside the program at %q", got, exe)
	}
}

// A directory the bridge cannot write to is the v1.2 shape of "no persistence =
// configuration error" (§9.2.4), and it has a specific way of going wrong: the
// failure surfaces from the atomic-write temp file, so the message used to read
// "cannot create a temporary file in /data" and name .odb-419478592.tmp. That
// tells the person nothing. A container with /data mounted read-only — an easy
// mistake, and the one you would make on purpose after reading that .env holds
// secrets — is exactly who receives it.
func TestAnUnwritableDirectoryNamesTheCredentialFileNotATempFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not restrict writes on NTFS")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test depends on")
	}
	cheapKDF(t)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s, err := newEnvStore(filepath.Join(dir, EnvFileName), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Save(context.Background(), sampleCredentials())
	if err == nil {
		t.Fatal("a read-only directory accepted a credential write")
	}
	msg := err.Error()
	// The file the user is looking for, the directory to fix, and the key file
	// they will need to know about — all three, because the point of the message
	// is that somebody can act on it without reading the source.
	for _, want := range []string{EnvFileName, dir, KeyFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, ".tmp") {
		t.Errorf("the message names a temporary file the user will never see: %s", msg)
	}
}
