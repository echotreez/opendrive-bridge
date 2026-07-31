package keystore

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The format's whole justification is that a user can recover their credentials
// with openssl and without us (§9.2.2). That claim is only worth making if it is
// checked against the real openssl, in both directions, so these tests shell out
// to it. They skip where it is absent rather than pretending to have run.
//
// This is also what makes the hand-written PBKDF2 safe to keep: a mistake in it
// would show up here as openssl and this package disagreeing.

// cheapKDF lowers the iteration count for tests that are about the .env format
// rather than about the KDF. The openssl compatibility tests do not call it:
// they have to derive the key the same way the documented command does.
func cheapKDF(t *testing.T) {
	t.Helper()
	previous := pbkdf2Iterations
	pbkdf2Iterations = 1000
	t.Cleanup(func() { pbkdf2Iterations = previous })
}

// The number in the documentation and the number in the code have to be the
// same, or the recovery command in .env.example and docs/first-run.md would not
// work — and that command is the whole reason for this file format.
func TestTheShippedIterationCountIsWhatWeDocument(t *testing.T) {
	if shippedIterations != 600000 {
		t.Fatalf("the shipped iteration count is %d; every documented openssl "+
			"command says -iter 600000", shippedIterations)
	}
	if pbkdf2Iterations != shippedIterations {
		t.Fatalf("pbkdf2Iterations is %d outside a test", pbkdf2Iterations)
	}
}

func opensslPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed; the compatibility claim cannot be checked here")
	}
	return path
}

func TestOpensslCanDecryptWhatWeWrite(t *testing.T) {
	ssl := opensslPath(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, ".env.key")
	encFile := filepath.Join(dir, ".env")

	password := []byte("Zm9vYmFyLWtleS1tYXRlcmlhbC0zMi1ieXRlcy0hIQ==")
	plain := []byte("ODB_USERNAME=derek\nODB_PASSWORD=correct horse\n")

	if err := os.WriteFile(keyFile, append(password, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, err := seal(plain, password)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := os.WriteFile(encFile, sealed, 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly the command the documentation tells a user to run.
	cmd := exec.Command(ssl, "enc", "-d", "-aes-256-cbc", "-pbkdf2",
		"-iter", "600000", "-a", "-pass", "file:"+keyFile, "-in", encFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("openssl could not read our file: %v\n%s", err, stderr.String())
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("openssl decrypted to %q, want %q", out, plain)
	}
}

func TestWeCanDecryptWhatOpensslWrites(t *testing.T) {
	ssl := opensslPath(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, ".env.key")
	encFile := filepath.Join(dir, ".env")

	password := []byte("a passphrase openssl was given")
	plain := []byte("ODB_USERNAME=derek\nODB_API_KEY=abc123\n")

	if err := os.WriteFile(keyFile, append(password, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ssl, "enc", "-aes-256-cbc", "-pbkdf2",
		"-iter", "600000", "-a", "-pass", "file:"+keyFile, "-out", encFile)
	cmd.Stdin = bytes.NewReader(plain)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("openssl: %v\n%s", err, stderr.String())
	}

	sealed, err := os.ReadFile(encFile) // #nosec G304 -- a path this test just made
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(sealed, password)
	if err != nil {
		t.Fatalf("we could not read openssl's file: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypted to %q, want %q", got, plain)
	}
}

// A note on why the key file's trailing newline does not matter: openssl's
// `-pass file:` reads the first line and drops the newline, and this package is
// given the same bytes. If that ever stopped being true the two tests above
// would disagree, which is the point of having both.

func TestRoundTripWithoutOpenssl(t *testing.T) {
	cheapKDF(t)
	password := []byte("key material")
	for _, plain := range []string{
		"", "a", "exactly-sixteen!", "ODB_PASSWORD=p\n", strings.Repeat("x", 5000),
	} {
		sealed, err := seal([]byte(plain), password)
		if err != nil {
			t.Fatalf("seal(%d bytes): %v", len(plain), err)
		}
		got, err := open(sealed, password)
		if err != nil {
			t.Fatalf("open(%d bytes): %v", len(plain), err)
		}
		if string(got) != plain {
			t.Fatalf("round trip changed %d bytes", len(plain))
		}
	}
}

func TestTheWrongKeyIsRefusedRatherThanGuessedAt(t *testing.T) {
	cheapKDF(t)
	sealed, err := seal([]byte("ODB_PASSWORD=secret\n"), []byte("the right key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(sealed, []byte("the wrong key")); err == nil {
		t.Fatal("the wrong key produced a result")
	}
}

// Altering the ciphertext must not yield a plausible-looking document. CBC is
// malleable, which is exactly why the plaintext is checked after decryption
// rather than trusted; this test pins the first half of that argument.
func TestAlteredCiphertextDoesNotDecryptCleanly(t *testing.T) {
	cheapKDF(t)
	password := []byte("key material")
	sealed, err := seal([]byte("ODB_USERNAME=derek\nODB_PASSWORD=secret\n"), password)
	if err != nil {
		t.Fatal(err)
	}
	decoded := stripSpace(string(sealed))
	// Flip a character in the middle of the armoured body.
	i := len(decoded) / 2
	swap := byte('A')
	if decoded[i] == 'A' {
		swap = 'B'
	}
	tampered := decoded[:i] + string(swap) + decoded[i+1:]

	got, err := open([]byte(tampered), password)
	if err != nil {
		return // refused outright, which is the better outcome
	}
	if bytes.Contains(got, []byte("ODB_PASSWORD=secret")) {
		t.Fatal("tampering left the credentials intact and readable")
	}
}

func TestIsEncryptedTellsAFreshEnvFromASealedOne(t *testing.T) {
	cheapKDF(t)
	sealed, err := seal([]byte("ODB_USERNAME=derek\n"), []byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	if !isEncrypted(sealed) {
		t.Error("a sealed file was not recognised")
	}
	for _, plain := range []string{
		"ODB_USERNAME=derek\nODB_PASSWORD=hunter2\n",
		"# just a comment\n",
		"",
		"not base64 at all !!!",
	} {
		if isEncrypted([]byte(plain)) {
			t.Errorf("%q was mistaken for a sealed file", plain)
		}
	}
}

// The .env format has to round trip anything a password can contain. The
// awkward-characters test found one bug here already — quoting escaped a
// character that unquoting did not put back, so the value changed on its way
// through and only the checksum noticed. A fuzzer is the right tool for the rest
// of that space.
func FuzzEnvRoundTrip(f *testing.F) {
	for _, v := range []string{
		"simple", "with spaces", `a "quoted" value`, "tab\there", "hash#inside",
		`back\slash`, `\"`, "=equals=", "'single'", "", "  padded  ",
	} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// A newline ends a line in this format and is not part of a value; the
		// store never writes one, since no credential field can contain one.
		if strings.ContainsAny(value, "\n\r") || strings.TrimSpace(value) == "" {
			return
		}
		fields := map[string]string{"ODB_PASSWORD": value}
		got := parseEnv(renderEnv(fields))
		if got["ODB_PASSWORD"] != value {
			t.Fatalf("%q came back as %q", value, got["ODB_PASSWORD"])
		}
		// And the checksum must agree with itself, which is what makes an
		// altered file detectable rather than merely different.
		if checksumOf(fields) != checksumOf(got) {
			t.Fatalf("checksum changed for %q", value)
		}
	})
}
