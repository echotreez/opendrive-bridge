package keystore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Seal is the daemon's first act. It must turn the plaintext the user wrote into
// the encrypted file, and — the reason it exists — refuse loudly when it cannot,
// rather than leave the password in the clear behind a daemon that looks fine.

func writePlain(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, EnvFileName)
	if err := os.WriteFile(path, []byte("ODB_USERNAME=u@example.com\nODB_PASSWORD=pw-in-the-clear\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSealEncryptsAPlaintextFile(t *testing.T) {
	dir := t.TempDir()
	path := writePlain(t, dir)
	s, err := newEnvStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Seal(context.Background(), s); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !isEncrypted(raw) || strings.Contains(string(raw), "pw-in-the-clear") {
		t.Fatalf("the file was not sealed: %q", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatalf("no key file: %v", err)
	}
	// And it is a no-op the second time.
	if err := Seal(context.Background(), s); err != nil {
		t.Fatalf("Seal on an encrypted file: %v", err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(raw) {
		t.Fatal("sealing an already encrypted file rewrote it")
	}
}

// Absent and empty are not Seal's business: /v1/auth/status explains those, and
// the daemon must stay up to answer it.
func TestSealLeavesAbsentAndEmptyFilesToStatus(t *testing.T) {
	dir := t.TempDir()
	s, _ := newEnvStore(filepath.Join(dir, EnvFileName), nil)
	if err := Seal(context.Background(), s); err != nil {
		t.Fatalf("absent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnvFileName), []byte("# nothing yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Seal(context.Background(), s); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

// Docker, asked to mount a .env.key that does not exist yet, makes a directory
// of that name. Measured in CI on 2026-09-27 with the compose file as shipped.
func TestSealRefusesAKeyThatIsADirectory(t *testing.T) {
	dir := t.TempDir()
	path := writePlain(t, dir)
	if err := os.Mkdir(filepath.Join(dir, KeyFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newEnvStore(path, nil)
	err := Seal(context.Background(), s)
	if err == nil {
		t.Fatal("sealed with a directory where the key should be")
	}
	for _, want := range []string{"is a directory", "mount the folder", "still in", "unencrypted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
}

// Somewhere the key cannot be written is a refusal too, and names the files the
// user is dealing with rather than a temporary one.
func TestSealRefusesADirectoryItCannotWrite(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("POSIX permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits")
	}
	dir := t.TempDir()
	path := writePlain(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s, _ := newEnvStore(path, nil)
	err := Seal(context.Background(), s)
	if err == nil {
		t.Fatal("sealed into a read-only directory")
	}
	if strings.Contains(err.Error(), ".tmp") {
		t.Errorf("the refusal names a temporary file: %v", err)
	}
	if !strings.Contains(err.Error(), "unencrypted") {
		t.Errorf("the refusal does not say the password is still in the clear: %v", err)
	}
}

// Only the file backend has anything to seal.
func TestSealIgnoresOtherBackends(t *testing.T) {
	if err := Seal(context.Background(), newEphemeral()); err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
}
