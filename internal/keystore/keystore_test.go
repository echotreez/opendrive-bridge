package keystore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	s, err := newEnvStore(filepath.Join(t.TempDir(), CredentialsFileName))
	if err != nil {
		t.Fatalf("newEnvStore: %v", err)
	}
	return s
}

// ---------------------------------------------------------------- nothing to prepare

// No file is the normal first state: nobody has signed in yet. It is not an
// error the daemon has to refuse over, and nothing is created by looking.
func TestNoFileMeansNobodyHasSignedIn(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Load(context.Background()); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("Load = %v, want ErrNoCredentials", err)
	}
	if err := s.Available(context.Background()); err != nil {
		t.Fatalf("Available = %v; an absent file is not a fault", err)
	}
	if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading created %s", s.path)
	}
}

// Signing in writes the one file, in a directory that did not have to exist, and
// a second store — the next start — reads it back.
func TestSigningInWritesOneFileTheNextStartReads(t *testing.T) {
	cheapKDF(t)
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	path := filepath.Join(dir, CredentialsFileName)
	s, _ := newEnvStore(path)
	want := sampleCredentials()
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != CredentialsFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the directory holds %v; want only %s", names, CredentialsFileName)
	}

	again, _ := newEnvStore(path)
	got, err := again.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if got.Username != want.Username || got.Password != want.Password ||
		got.Token == nil || got.Token.RefreshToken != want.Token.RefreshToken ||
		!got.Token.Expiry.Equal(want.Token.Expiry) || got.AccType != want.AccType {
		t.Fatalf("round trip lost something:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestTheFileIsEncryptedAndPrivate(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(context.Background(), sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.path)
	for _, secret := range []string{"correct horse", "access-token-value", "refresh-token-value", "derek@example.com"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("%q is readable in the file", secret)
		}
	}
	if !strings.HasPrefix(string(raw), "# opendrive-bridge credentials") {
		t.Errorf("the file does not say what it is:\n%s", raw)
	}
	fi, _ := os.Stat(s.path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// The recovery procedure written at the top of the file is run as written, with
// the real openssl, so the promise that the user is not locked in to this
// program stays true.
func TestTheRecoveryCommandInTheFileWorks(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	// The documented command uses the shipped iteration count.
	s, _ := newEnvStore(filepath.Join(t.TempDir(), CredentialsFileName))
	if err := s.Save(context.Background(), sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.path)
	keyFile := filepath.Join(t.TempDir(), "k")
	script := "grep -v '^#' credentials.key | head -1 > " + keyFile + " && " +
		"grep -v '^#' credentials.key | tail -n +2 | " +
		openssl + " enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a -pass file:" + keyFile
	cmd := exec.Command("sh", "-c", script) // #nosec G204 -- a fixed script over test files
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the documented recovery failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ODB_PASSWORD=") || !strings.Contains(string(out), "correct horse") {
		t.Fatalf("recovered text does not hold the password:\n%s", out)
	}
	// And the header in the file is the command this test ran.
	raw, _ := os.ReadFile(s.path)
	if !strings.Contains(string(raw), "-iter 600000 -a -pass file:/tmp/k") {
		t.Errorf("the header no longer documents the command that works:\n%s", raw)
	}
}

// ---------------------------------------------------------------- when it goes wrong

// A damaged file is reported, and signing in again replaces it: that is the
// whole of the recovery a user needs.
func TestADamagedFileIsReportedAndReplacedBySigningIn(t *testing.T) {
	s := newTestStore(t)
	if err := os.WriteFile(s.path, []byte("this is not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Available(context.Background()); err == nil || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("Available = %v, want a refusal that says to sign in again", err)
	}
	if err := s.Save(context.Background(), sampleCredentials()); err != nil {
		t.Fatalf("signing in over a damaged file: %v", err)
	}
	if _, err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load after signing in again: %v", err)
	}
}

func TestAnAlteredFileIsRefused(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(context.Background(), sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.path)
	lines := strings.Split(string(raw), "\n")
	// Flip a character in the middle of the ciphertext.
	for i := len(lines) - 1; i >= 0; i-- {
		if len(lines[i]) > 20 && !strings.HasPrefix(lines[i], "#") {
			b := []byte(lines[i])
			if b[10] == 'A' {
				b[10] = 'B'
			} else {
				b[10] = 'A'
			}
			lines[i] = string(b)
			break
		}
	}
	_ = os.WriteFile(s.path, []byte(strings.Join(lines, "\n")), 0o600)
	fresh, _ := newEnvStore(s.path)
	if _, err := fresh.Load(context.Background()); err == nil {
		t.Fatal("an altered file was accepted")
	}
}

// Signing out removes the file; the next start asks to be signed in again.
func TestSigningOutRemovesTheFile(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(context.Background(), sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file survived signing out: %v", err)
	}
	if err := s.Delete(context.Background()); err != nil {
		t.Fatalf("signing out twice: %v", err)
	}
}

func TestValuesSurviveTheirAwkwardCharacters(t *testing.T) {
	s := newTestStore(t)
	cred := sampleCredentials()
	cred.Password = `p"a s#s='w\ord` + "\t"
	if err := s.Save(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != cred.Password {
		t.Fatalf("password = %q, want %q", got.Password, cred.Password)
	}
}

// A directory that cannot be written is reported naming the file and the
// directory, never the temporary file the atomic write uses.
func TestAnUnwritableDirectoryNamesTheCredentialFileNotATempFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this test depends on")
	}
	cheapKDF(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	s, _ := newEnvStore(filepath.Join(dir, CredentialsFileName))
	err := s.Save(context.Background(), sampleCredentials())
	if err == nil {
		t.Fatal("a read-only directory accepted a credential write")
	}
	for _, want := range []string{CredentialsFileName, dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), ".tmp") {
		t.Errorf("the message names a temporary file: %v", err)
	}
}

// ---------------------------------------------------------------- Open

func TestOpenEphemeralRequiresOptIn(t *testing.T) {
	if _, err := Open(Config{Backend: BackendEphemeral}); !errors.Is(err, ErrEphemeralNotAllowed) {
		t.Fatalf("Open = %v, want ErrEphemeralNotAllowed", err)
	}
	if st, err := Open(Config{Backend: BackendEphemeral, AllowEphemeral: true}); err != nil || st.Backend() != BackendEphemeral {
		t.Fatalf("Open = %v, %v", st, err)
	}
}

func TestOpenRejectsAnUnknownBackendAndNamesTheChoices(t *testing.T) {
	_, err := Open(Config{Backend: "keyring"})
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("Open = %v", err)
	}
	for _, want := range []string{string(BackendAuto), string(BackendFile), string(BackendEphemeral)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

func TestAutoAndFileAreTheSameStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), CredentialsFileName)
	for _, b := range []Backend{BackendAuto, BackendFile} {
		st, err := Open(Config{Backend: b, Path: path})
		if err != nil || st.Backend() != BackendFile {
			t.Fatalf("%s: %v, %v", b, st, err)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	if got := (Config{}).withDefaults(); got.Backend != BackendAuto {
		t.Errorf("defaults = %+v", got)
	}
}

// Beside the program, because the bridge runs in place: everything a user backs
// up or deletes is in the folder they unpacked (§8.2).
func TestTheDefaultPathIsBesideTheProgram(t *testing.T) {
	got := filePath(Config{}.withDefaults())
	if filepath.Base(got) != CredentialsFileName {
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
