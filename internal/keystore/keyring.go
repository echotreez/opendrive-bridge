package keystore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// keyringStore keeps credentials in the operating system's credential vault:
// macOS Keychain or Linux Secret Service (GNOME Keyring / KWallet). Both unlock
// with the user's login session and store their contents encrypted, which is
// exactly the balance §9.2 asks for — silent while the user is logged in,
// unreadable to anybody else.
//
// The vaults are driven through their command line tools rather than through
// cgo bindings, so the binaries stay CGO_ENABLED=0 and cross-compile cleanly
// (§8.1). Secrets are handed over on stdin, never as command arguments, because
// arguments are visible to every process on the machine (§9.2).
type keyringStore struct {
	service string
	account string

	// run executes a vault command. It is a field so tests can drive the
	// parsing without touching a real keychain.
	run func(ctx context.Context, args []string, stdin string) (stdout string, err error)
	// lookPath resolves the vault binary; a field for the same reason.
	lookPath func(string) (string, error)
	// platform selects the command dialect; defaults to runtime.GOOS.
	platform string

	mu sync.Mutex
}

func newKeyring(service, account string) *keyringStore {
	return &keyringStore{
		service:  service,
		account:  account,
		run:      runCommand,
		lookPath: exec.LookPath,
		platform: runtime.GOOS,
	}
}

// notFound marks "the vault works, but holds nothing for us".
var errItemNotFound = errors.New("keystore: no item in the vault")

// Backend implements Store.
func (s *keyringStore) Backend() Backend { return BackendKeyring }

// Available reports whether the vault can be reached. A locked or missing vault
// is an error here, which becomes keystore_unavailable upstream (§4.5).
func (s *keyringStore) Available(ctx context.Context) error {
	switch s.platform {
	case "darwin":
		if _, err := s.lookPath("security"); err != nil {
			return fmt.Errorf("keystore: the macOS security tool is unavailable: %w", err)
		}
		// Listing keychains touches the vault without needing an item.
		if _, err := s.run(ctx, []string{"security", "list-keychains"}, ""); err != nil {
			return fmt.Errorf("keystore: the macOS keychain is not readable: %w", err)
		}
		return nil
	case "linux":
		if _, err := s.lookPath("secret-tool"); err != nil {
			return fmt.Errorf("keystore: secret-tool (libsecret) is not installed: %w", err)
		}
		// A lookup for our own item proves the Secret Service is answering; a
		// missing item is a fine answer, a D-Bus failure is not.
		_, err := s.run(ctx, []string{"secret-tool", "lookup", "service", s.service, "account", s.account}, "")
		if err != nil && !errors.Is(err, errItemNotFound) {
			return fmt.Errorf("keystore: the Secret Service is not answering: %w", err)
		}
		return nil
	case "windows":
		// The Credential Manager has no usable command line tool — cmdkey
		// cannot read a secret back — so this one goes through the Win32
		// credential API directly (credman_windows.go).
		if err := credManAvailable(); err != nil {
			return fmt.Errorf("keystore: the Windows Credential Manager is not usable: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("keystore: no OS keyring integration for %s", s.platform)
	}
}

// credManTarget is the Credential Manager target name for this store. The
// service and account are folded into one string because the Win32 API keys
// generic credentials by target name alone.
func (s *keyringStore) credManTarget() string {
	return s.service + ":" + s.account
}

// Load implements opendrive.CredentialStore.
func (s *keyringStore) Load(ctx context.Context) (*opendrive.StoredCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var (
		out string
		err error
	)
	switch s.platform {
	case "darwin":
		out, err = s.run(ctx, []string{"security", "find-generic-password",
			"-s", s.service, "-a", s.account, "-w"}, "")
	case "linux":
		out, err = s.run(ctx, []string{"secret-tool", "lookup",
			"service", s.service, "account", s.account}, "")
	case "windows":
		out, err = credManLoad(s.credManTarget())
	default:
		return nil, fmt.Errorf("keystore: no OS keyring integration for %s", s.platform)
	}
	if errors.Is(err, errItemNotFound) {
		return nil, opendrive.ErrNoCredentials
	}
	if err != nil {
		return nil, err
	}

	payload := strings.TrimSpace(out)
	if payload == "" {
		return nil, opendrive.ErrNoCredentials
	}
	return decodePayload(payload)
}

// Save implements opendrive.CredentialStore. The vaults replace an item
// atomically, so a crash cannot leave a half-written credential (§9.2).
func (s *keyringStore) Save(ctx context.Context, cred *opendrive.StoredCredentials) error {
	if cred == nil {
		return s.Delete(ctx)
	}
	payload, err := encodePayload(cred)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.platform {
	case "darwin":
		// security -i reads commands from stdin, which keeps the secret out of
		// the process arguments. -U replaces an existing item in one step.
		cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -j %s -w %s\n",
			quoteArg(s.service), quoteArg(s.account), quoteArg(s.service),
			quoteArg("OpenDrive Bridge credentials"), quoteArg(payload))
		if _, err := s.run(ctx, []string{"security", "-i"}, cmd); err != nil {
			return fmt.Errorf("keystore: cannot write to the keychain: %w", err)
		}
		return nil
	case "linux":
		// secret-tool store reads the secret from stdin by design.
		if _, err := s.run(ctx, []string{"secret-tool", "store",
			"--label=OpenDrive Bridge credentials",
			"service", s.service, "account", s.account}, payload); err != nil {
			return fmt.Errorf("keystore: cannot write to the Secret Service: %w", err)
		}
		return nil
	case "windows":
		// The payload never becomes a command argument here either: it is
		// passed as a byte blob through the credential API (§9.2).
		if err := credManSave(s.credManTarget(), payload); err != nil {
			return fmt.Errorf("keystore: cannot write to the Credential Manager: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("keystore: no OS keyring integration for %s", s.platform)
	}
}

// Delete implements opendrive.CredentialStore.
func (s *keyringStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	switch s.platform {
	case "darwin":
		_, err = s.run(ctx, []string{"security", "delete-generic-password",
			"-s", s.service, "-a", s.account}, "")
	case "linux":
		_, err = s.run(ctx, []string{"secret-tool", "clear",
			"service", s.service, "account", s.account}, "")
	case "windows":
		err = credManDelete(s.credManTarget())
	default:
		return fmt.Errorf("keystore: no OS keyring integration for %s", s.platform)
	}
	if err != nil && !errors.Is(err, errItemNotFound) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- payload

// encodePayload renders credentials as base64 JSON. Base64 keeps the blob free
// of quotes and newlines, which matters because the macOS tool parses its input
// as a command line.
func encodePayload(cred *opendrive.StoredCredentials) (string, error) {
	raw, err := json.Marshal(cred)
	if err != nil {
		return "", fmt.Errorf("keystore: cannot encode credentials: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func decodePayload(payload string) (*opendrive.StoredCredentials, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Tolerate a plain JSON item, e.g. one written by an older build.
		raw = []byte(payload)
	}
	var cred opendrive.StoredCredentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("keystore: the stored credentials are unreadable: %w", err)
	}
	return &cred, nil
}

// quoteArg quotes a value for the macOS security tool's interactive parser.
func quoteArg(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// runCommand executes a vault command, mapping "no such item" onto
// errItemNotFound. Neither stdin nor stdout is ever logged (§9.2).
func runCommand(ctx context.Context, args []string, stdin string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("keystore: empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // fixed binaries, arguments are not user input
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// macOS security uses 44, secret-tool uses 1 for "not found".
			if exitErr.ExitCode() == 44 {
				return "", errItemNotFound
			}
			msg := strings.ToLower(stderr.String())
			if strings.Contains(msg, "could not be found") || strings.Contains(msg, "no such") {
				return "", errItemNotFound
			}
			if exitErr.ExitCode() == 1 && args[0] == "secret-tool" && strings.TrimSpace(stderr.String()) == "" {
				return "", errItemNotFound
			}
			return "", fmt.Errorf("keystore: %s failed: %s", args[0], strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("keystore: cannot run %s: %w", args[0], err)
	}
	// secret-tool reports a missing item with an empty result and exit code 0
	// on some versions.
	if args[0] == "secret-tool" && args[1] == "lookup" && strings.TrimSpace(stdout.String()) == "" {
		return "", errItemNotFound
	}
	return stdout.String(), nil
}
