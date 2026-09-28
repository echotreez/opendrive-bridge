// Package keystore persists the credentials that let the bridge run silently
// after its initial setup: username, password, OAuth tokens and session id
// (whitepaper §9.2, the four credential kinds of the CredentialStore).
//
// There is one real backend: credentials.key beside the programs, written by the
// daemon when somebody signs in (§9.2.2 as revised for 1.3). Until v1.1 there were
// four — the three OS vaults and an encrypted .env — and 1.1 and 1.2 had the user
// prepare that .env by hand. §9.2.1 has the reasoning for one file, and §9.2.3 is
// honest about what it protects.
//
// Open never hands back a store that forgets everything on restart unless the
// caller asks for that in so many words (--ephemeral).
package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// DefaultFileName is the credential file's name.
const DefaultFileName = CredentialsFileName

// Backend selects where credentials live (config keystore, §3.4).
type Backend string

// Backends.
const (
	// BackendAuto is the encrypted file. The name is kept because it is what
	// existing configurations say, and because it still describes the
	// behaviour: the store sets itself up without being told how.
	BackendAuto Backend = "auto"
	// BackendFile is the encrypted .env, named for what §3.4 calls it.
	BackendFile Backend = "encrypted_file"
	// BackendEphemeral keeps credentials in memory only. It must be requested
	// explicitly and is meant for tests and one-off runs.
	BackendEphemeral Backend = "ephemeral"
)

// Errors returned by Open. They are deliberately actionable.
var (
	// ErrNoBackend means nothing can persist credentials on this machine, and
	// the caller did not opt into an ephemeral run.
	ErrNoBackend = errors.New("keystore: no credential store is available; " +
		"start with --ephemeral to accept signing in again after every restart")
	// ErrEphemeralNotAllowed means an in-memory store was requested without the
	// explicit opt-in.
	ErrEphemeralNotAllowed = errors.New("keystore: the ephemeral backend keeps credentials in memory only " +
		"and must be enabled explicitly (--ephemeral)")
	// ErrUnsupportedBackend is returned for an unknown backend name.
	ErrUnsupportedBackend = errors.New("keystore: unknown backend")
)

// Config describes where and how credentials are stored.
type Config struct {
	// Backend selects the store. The zero value means BackendAuto.
	Backend Backend
	// Path is the credential file; empty means credentials.key beside the program.
	Path string
	// AllowEphemeral must be true for BackendEphemeral to be honoured. It maps
	// to the daemon's --ephemeral flag.
	AllowEphemeral bool
}

func (c Config) withDefaults() Config {
	if c.Backend == "" {
		c.Backend = BackendAuto
	}
	return c
}

// Store is a credential store the SDK can use, plus the two things the daemon
// needs on top: which backend answered, and whether it is readable right now.
type Store interface {
	opendrive.CredentialStore

	// Backend reports which implementation is in use, for /v1/auth/status.
	Backend() Backend
	// Available reports whether the store can be read at this moment. A missing
	// file is not an error (nobody has signed in yet); a damaged one is, and the
	// daemon surfaces it as keystore_unavailable without touching the network.
	Available(ctx context.Context) error
}

// Secrets stores the bridge's own secrets — values it generated, such as S3
// access keys — in the same encrypted file as the sign-in, under the same rules
// (§9.2): nowhere else, never in a config file or a log. Both stores implement it.
type Secrets interface {
	// LoadSecret returns ErrNoSecret when the secret has never been saved.
	LoadSecret(ctx context.Context, name string) (string, error)
	// SaveSecret stores a secret; an empty value removes it.
	SaveSecret(ctx context.Context, name, value string) error
}

// ErrNoSecret reports a secret that has not been saved.
var ErrNoSecret = errors.New("keystore: no such secret")

func validSecretName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Open selects and prepares a credential store.
//
// With the default configuration it is the encrypted file, which need not exist
// yet: the daemon writes it when somebody signs in.
func Open(cfg Config) (Store, error) {
	cfg = cfg.withDefaults()

	switch cfg.Backend {
	case BackendEphemeral:
		if !cfg.AllowEphemeral {
			return nil, ErrEphemeralNotAllowed
		}
		return newEphemeral(), nil

	case BackendFile, BackendAuto:
		return newEnvStore(filePath(cfg))

	default:
		// Naming the alternatives is the difference between a message that ends
		// the problem and one that starts a search.
		return nil, fmt.Errorf("%w: %q (choose one of: %s, %s, %s)",
			ErrUnsupportedBackend, cfg.Backend,
			BackendAuto, BackendFile, BackendEphemeral)
	}
}

// filePath returns the configured credential file, or credentials.key in the
// directory the program was unpacked into.
//
// v1.1 runs in place (§8.2), so the default is beside the binary rather than in
// a per-OS configuration directory: everything a user needs to back up or delete
// is then in one folder. defaultStateDir remains the fallback for the case where
// the executable's location cannot be determined.
func filePath(cfg Config) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Join(filepath.Dir(exe), CredentialsFileName)
	}
	return filepath.Join(defaultStateDir(), DefaultFileName)
}

// defaultStateDir mirrors the configuration locations of §3.4.
func defaultStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "opendrive-bridge")
		}
	default:
		if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
			return filepath.Join(dir, "opendrive-bridge")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".config", "opendrive-bridge")
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "opendrive-bridge")
	}
	return filepath.Join(dir, "opendrive-bridge")
}

// ---------------------------------------------------------------- ephemeral

// ephemeralStore is the in-memory store, wrapped so that it reports a backend
// and is always available.
type ephemeralStore struct {
	*opendrive.MemoryTokenStore
	smu     sync.Mutex
	secrets map[string]string
}

func newEphemeral() *ephemeralStore {
	return &ephemeralStore{MemoryTokenStore: opendrive.NewMemoryTokenStore()}
}

func (s *ephemeralStore) Backend() Backend                { return BackendEphemeral }
func (s *ephemeralStore) Available(context.Context) error { return nil }

func (s *ephemeralStore) LoadSecret(_ context.Context, name string) (string, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	v, ok := s.secrets[name]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}

func (s *ephemeralStore) SaveSecret(_ context.Context, name, value string) error {
	if !validSecretName(name) {
		return fmt.Errorf("keystore: %q is not a usable secret name", name)
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.secrets == nil {
		s.secrets = map[string]string{}
	}
	if value == "" {
		delete(s.secrets, name)
	} else {
		s.secrets[name] = value
	}
	return nil
}
