package keystore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The vault backends are chosen at compile time, so each platform's CI runner
// covers a different set of files. These tests drive the parts that can be
// exercised anywhere — the command dialects, the not-found mapping, the
// unsupported-platform stubs — so that a gate passing on one runner is not an
// accident of which files happened to compile.

// Available has to distinguish three things a caller reacts to differently: the
// tool is missing, the tool is there but the vault will not answer, and all is
// well. Only the last may report success, because a locked vault that reports
// itself available becomes a login attempt with no credentials (§4.5).
func TestAvailableSeparatesAMissingToolFromALockedVault(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			t.Run("tool missing", func(t *testing.T) {
				k, _ := newFakeKeyring(platform)
				k.lookPath = func(string) (string, error) { return "", errors.New("not in PATH") }

				err := k.Available(context.Background())
				if err == nil {
					t.Fatal("a missing vault tool was reported as available")
				}
				// The message has to name the tool, or an operator cannot fix it.
				want := map[string]string{"darwin": "security", "linux": "secret-tool"}[platform]
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to name %q", err, want)
				}
			})

			t.Run("vault refuses", func(t *testing.T) {
				k, _ := newFakeKeyring(platform)
				k.run = func(context.Context, []string, string) (string, error) {
					return "", errors.New("the vault is locked")
				}
				if err := k.Available(context.Background()); err == nil {
					t.Fatal("a locked vault was reported as available")
				}
			})

			t.Run("all is well", func(t *testing.T) {
				k, _ := newFakeKeyring(platform)
				if err := k.Available(context.Background()); err != nil {
					t.Errorf("a working vault reported %v", err)
				}
			})
		})
	}
}

// On Linux "nothing stored yet" arrives as exit status 1 with no stderr, which
// must read as an empty vault rather than as a broken one — the difference
// between "log in once" and "something is wrong with your machine".
func TestLinuxEmptyVaultIsNotAFailure(t *testing.T) {
	k, _ := newFakeKeyring("linux")
	k.run = func(_ context.Context, args []string, _ string) (string, error) {
		if args[1] == "lookup" {
			return "", errItemNotFound
		}
		return "", nil
	}

	// The contract is the sentinel, not a nil credential: a caller has to be
	// able to tell "nothing stored yet" from "the vault would not answer", and
	// only one of those means "ask the user to log in".
	_, err := k.Load(context.Background())
	if !errors.Is(err, opendrive.ErrNoCredentials) {
		t.Fatalf("an empty vault reported %v, want ErrNoCredentials", err)
	}
}

// A vault that answers with something we cannot read is a real failure and must
// not be mistaken for an empty one — that would silently discard a working
// login and ask the user to sign in again.
func TestUnreadablePayloadIsAnError(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			k, _ := newFakeKeyring(platform)
			k.run = func(context.Context, []string, string) (string, error) {
				return "this is not the payload", nil
			}
			if _, err := k.Load(context.Background()); err == nil {
				t.Fatal("an unreadable payload was reported as no credentials")
			}
		})
	}
}

// Deleting something that was never there is not an error: logging out twice is
// an ordinary thing to do.
func TestDeleteIsIdempotent(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			k, _ := newFakeKeyring(platform)
			k.run = func(context.Context, []string, string) (string, error) {
				return "", errItemNotFound
			}
			if err := k.Delete(context.Background()); err != nil {
				t.Errorf("deleting an absent item reported %v", err)
			}
		})
	}
}

// The Windows dialect is selected on the same code path as the other two, so
// the selection is worth asserting from any runner even though the API calls
// themselves are only reachable there.
func TestCredManTargetNamesTheItem(t *testing.T) {
	k, _ := newFakeKeyring("windows")
	target := k.credManTarget()
	for _, want := range []string{k.service, k.account} {
		if !strings.Contains(target, want) {
			t.Errorf("target %q does not contain %q", target, want)
		}
	}
	// Two profiles must not collide in one credential store.
	other := &keyringStore{service: k.service, account: "second-profile"}
	if other.credManTarget() == target {
		t.Error("two accounts produced the same credential target")
	}
}

// Off Windows the Credential Manager stubs must refuse clearly rather than
// pretend to work — a store that silently does nothing is the "no persistence"
// failure §9.2 exists to prevent.
func TestCredManStubsRefuseOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this asserts the behaviour of the non-Windows stubs")
	}
	if err := credManAvailable(); err == nil {
		t.Error("credManAvailable claimed to work off Windows")
	}
	if _, err := credManLoad("t"); err == nil {
		t.Error("credManLoad claimed to work off Windows")
	}
	if err := credManSave("t", "x"); err == nil {
		t.Error("credManSave claimed to work off Windows")
	}
	if err := credManDelete("t"); err == nil {
		t.Error("credManDelete claimed to work off Windows")
	}
}

// runCommand is the thin layer over exec that every vault call goes through: it
// must return stdout, feed stdin without putting it in the arguments, and carry
// a failure with enough of stderr to act on.
func TestRunCommandBehaviour(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these use POSIX shell utilities; the Windows path is the Credential Manager")
	}
	ctx := context.Background()

	t.Run("stdout comes back", func(t *testing.T) {
		out, err := runCommand(ctx, []string{"echo", "hello"}, "")
		if err != nil {
			t.Fatalf("echo: %v", err)
		}
		if strings.TrimSpace(out) != "hello" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("stdin is delivered", func(t *testing.T) {
		// cat reads the secret from stdin, which is the whole point: an
		// argument would be visible to every process on the machine (§9.2).
		out, err := runCommand(ctx, []string{"cat"}, "the secret")
		if err != nil {
			t.Fatalf("cat: %v", err)
		}
		if strings.TrimSpace(out) != "the secret" {
			t.Errorf("stdin did not reach the command: %q", out)
		}
	})

	t.Run("a failure carries something to act on", func(t *testing.T) {
		_, err := runCommand(ctx, []string{"sh", "-c", "echo boom >&2; exit 3"}, "")
		if err == nil {
			t.Fatal("a failing command reported success")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("error = %v, want it to carry stderr", err)
		}
	})

	t.Run("a cancelled context stops it", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := runCommand(cancelled, []string{"sleep", "5"}, ""); err == nil {
			t.Error("a cancelled command reported success")
		}
	})
}

// The default state directory has to land somewhere the user owns, and must not
// be the current working directory — a credential file written next to whatever
// the daemon was started from is a file somebody commits by accident.
func TestDefaultStateDirIsUnderTheUsersOwnDirectory(t *testing.T) {
	dir := defaultStateDir()
	if dir == "" {
		t.Fatal("no default state directory")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("state dir %q is not absolute", dir)
	}
	if dir == "." || dir == string(filepath.Separator) {
		t.Errorf("state dir %q is not a place to keep credentials", dir)
	}
	if !strings.Contains(strings.ToLower(dir), "opendrive") {
		t.Errorf("state dir %q is not named for this application", dir)
	}
	// It must not be inside the repository the daemon happens to run from.
	if wd, err := os.Getwd(); err == nil && strings.HasPrefix(dir, wd) {
		t.Errorf("state dir %q is inside the working directory", dir)
	}
}
