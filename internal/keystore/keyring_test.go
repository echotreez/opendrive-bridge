package keystore

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// fakeVault records the commands a keyring backend issues and answers them the
// way the real tools do, so the command construction can be tested anywhere.
type fakeVault struct {
	mu       sync.Mutex
	item     string
	present  bool
	calls    [][]string
	stdins   []string
	failWith error
}

func (v *fakeVault) run(_ context.Context, args []string, stdin string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, args)
	v.stdins = append(v.stdins, stdin)

	if v.failWith != nil {
		return "", v.failWith
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "list-keychains"):
		return "keychain", nil
	case strings.Contains(joined, "find-generic-password"), strings.Contains(joined, "secret-tool lookup"):
		if !v.present {
			return "", errItemNotFound
		}
		return v.item + "\n", nil
	case strings.Contains(joined, "security -i"):
		// The payload arrives on stdin, quoted for the security parser.
		start := strings.Index(stdin, `-w "`)
		if start < 0 {
			return "", fmt.Errorf("no -w in %q", stdin)
		}
		rest := stdin[start+4:]
		end := strings.Index(rest, `"`)
		v.item, v.present = rest[:end], true
		return "", nil
	case strings.Contains(joined, "secret-tool store"):
		v.item, v.present = strings.TrimSpace(stdin), true
		return "", nil
	case strings.Contains(joined, "delete-generic-password"), strings.Contains(joined, "secret-tool clear"):
		if !v.present {
			return "", errItemNotFound
		}
		v.item, v.present = "", false
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %q", joined)
}

func (v *fakeVault) recorded() ([][]string, []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls, v.stdins
}

func newFakeKeyring(platform string) (*keyringStore, *fakeVault) {
	v := &fakeVault{}
	k := newKeyring("odb-test", "default")
	k.platform = platform
	k.run = v.run
	// The vault binaries are simulated, so their presence is too.
	k.lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	return k, v
}

func TestKeyringRoundTripOnBothPlatforms(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			k, _ := newFakeKeyring(platform)
			if k.Backend() != BackendKeyring {
				t.Fatalf("backend = %q", k.Backend())
			}
			assertRoundTrip(t, k)
		})
	}
}

// §9.2: a secret must never appear in command arguments, where every process on
// the machine can read it.
func TestKeyringNeverPutsSecretsInArguments(t *testing.T) {
	const password = "correct horse battery staple"
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			k, v := newFakeKeyring(platform)
			cred := sampleCredentials()
			cred.Password = password
			if err := k.Save(context.Background(), cred); err != nil {
				t.Fatalf("Save: %v", err)
			}

			calls, stdins := v.recorded()
			for _, args := range calls {
				for _, a := range args {
					if strings.Contains(a, password) || strings.Contains(a, "access-token-value") {
						t.Fatalf("a secret reached the command line: %q", a)
					}
				}
			}
			// It did travel, just on stdin.
			var sawPayload bool
			for _, in := range stdins {
				if strings.TrimSpace(in) != "" {
					sawPayload = true
				}
			}
			if !sawPayload {
				t.Fatal("the payload was never written to stdin")
			}
		})
	}
}

func TestKeyringReportsAMissingItemAsNoCredentials(t *testing.T) {
	k, _ := newFakeKeyring("darwin")
	if _, err := k.Load(context.Background()); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

// A vault that is present but locked must surface as an error, which becomes
// keystore_unavailable and stops all upstream traffic (§4.5).
func TestKeyringFailureIsSurfaced(t *testing.T) {
	k, v := newFakeKeyring("darwin")
	v.failWith = errors.New("User interaction is not allowed")

	if _, err := k.Load(context.Background()); err == nil ||
		errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("a locked vault returned %v", err)
	}
	if err := k.Available(context.Background()); err == nil {
		t.Fatal("Available must fail on a locked vault")
	}
	if err := k.Save(context.Background(), sampleCredentials()); err == nil {
		t.Fatal("Save must fail on a locked vault")
	}
	if err := k.Delete(context.Background()); err == nil {
		t.Fatal("Delete must report a locked vault")
	}
}

func TestKeyringUnsupportedPlatform(t *testing.T) {
	k, _ := newFakeKeyring("plan9")
	ctx := context.Background()
	if err := k.Available(ctx); err == nil {
		t.Fatal("an unsupported platform must not report itself available")
	}
	if _, err := k.Load(ctx); err == nil {
		t.Fatal("Load must fail")
	}
	if err := k.Save(ctx, sampleCredentials()); err == nil {
		t.Fatal("Save must fail")
	}
	if err := k.Delete(ctx); err == nil {
		t.Fatal("Delete must fail")
	}
}

func TestSaveNilDeletes(t *testing.T) {
	ctx := context.Background()
	k, _ := newFakeKeyring("darwin")
	if err := k.Save(ctx, sampleCredentials()); err != nil {
		t.Fatal(err)
	}
	if err := k.Save(ctx, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	if _, err := k.Load(ctx); !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestPayloadEncoding(t *testing.T) {
	cred := sampleCredentials()
	payload, err := encodePayload(cred)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "correct horse") || strings.Contains(payload, `"`) {
		t.Fatalf("the payload is not opaque: %q", payload)
	}
	got, err := decodePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != cred.Username || got.Token.RefreshToken != cred.Token.RefreshToken {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// A plain JSON item, as an older build might have written, still loads.
	if _, err := decodePayload(`{"username":"derek"}`); err != nil {
		t.Fatalf("plain JSON: %v", err)
	}
	if _, err := decodePayload("not a payload at all"); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestQuoteArg(t *testing.T) {
	if got := quoteArg(`a "b" \c`); got != `"a \"b\" \\c"` {
		t.Fatalf("quoteArg = %s", got)
	}
}

// The real thing: on macOS the backend must work against the actual Keychain.
// The item is namespaced and removed afterwards, so it cannot disturb anything.
func TestKeychainOnRealMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the macOS Keychain is only available on darwin")
	}
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("the security tool is unavailable")
	}

	k := newKeyring("odb-keystore-selftest", fmt.Sprintf("test-%d", testRunID()))
	ctx := context.Background()
	if err := k.Available(ctx); err != nil {
		t.Skipf("the keychain is not usable in this environment: %v", err)
	}
	t.Cleanup(func() { _ = k.Delete(ctx) })

	assertRoundTrip(t, k)

	// And once more with the exact record the bridge stores, to be sure a real
	// keychain accepts the payload size and characters.
	cred := sampleCredentials()
	cred.SessionID = "SESSION-abcdef0123456789"
	if err := k.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := k.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Password != cred.Password || got.SessionID != cred.SessionID {
		t.Fatalf("real keychain round trip failed: %+v", got)
	}
	if err := k.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

var runID int64

func testRunID() int64 {
	runID++
	return runID
}
