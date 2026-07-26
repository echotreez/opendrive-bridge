// Package keystore persists the credentials that let the bridge run silently
// after its initial setup: username, password, OAuth tokens and session id
// (whitepaper §9.2, the four credential kinds of the CredentialStore).
//
// Two real backends are available. The default is the operating system's own
// credential vault — macOS Keychain, Linux Secret Service — which unlocks with
// the user's login session and stores its contents encrypted on disk. Where no
// vault exists (Docker, headless servers) an AES-256-GCM encrypted file is used
// instead, and its key must be supplied explicitly.
//
// Open refuses to hand back a store that forgets everything on restart unless
// the caller asks for that in so many words: a bridge that looks configured and
// then loses its credentials on reboot is worse than one that never started
// (§9.2, "no persistence = configuration error").
package keystore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// Defaults for the keyring item and the encrypted file.
const (
	// DefaultService is the keyring service name credentials are filed under.
	DefaultService = "opendrive-bridge"
	// DefaultAccount separates several bridge profiles in one keyring.
	DefaultAccount = "default"
	// DefaultKeyEnv is the environment variable holding the encrypted file key.
	DefaultKeyEnv = "ODB_STATE_KEY"
	// DefaultFileName is the encrypted credential file name.
	DefaultFileName = "credentials.enc"
)

// Backend selects where credentials live (config keystore, §3.4).
type Backend string

// Backends.
const (
	// BackendAuto prefers the OS keyring and falls back to the encrypted file
	// only when a key was configured explicitly.
	BackendAuto Backend = "auto"
	// BackendKeyring is the OS credential vault.
	BackendKeyring Backend = "keyring"
	// BackendFile is the AES-256-GCM encrypted file.
	BackendFile Backend = "encrypted_file"
	// BackendEphemeral keeps credentials in memory only. It must be requested
	// explicitly and is meant for tests and one-off runs.
	BackendEphemeral Backend = "ephemeral"
)

// Errors returned by Open. They are deliberately actionable: whichever one the
// operator sees, it tells them what to configure.
var (
	// ErrNoBackend means nothing can persist credentials on this machine, and
	// the caller did not opt into an ephemeral run.
	ErrNoBackend = errors.New("keystore: no credential store is available; " +
		"unlock the OS keyring, or set keystore: encrypted_file together with " +
		DefaultKeyEnv + ", or start with --ephemeral to accept losing credentials on restart")
	// ErrEphemeralNotAllowed means an in-memory store was requested without the
	// explicit opt-in.
	ErrEphemeralNotAllowed = errors.New("keystore: the ephemeral backend keeps credentials in memory only " +
		"and must be enabled explicitly (--ephemeral)")
	// ErrNoKey means the encrypted file backend has no key to work with.
	ErrNoKey = errors.New("keystore: the encrypted file backend needs a key in " + DefaultKeyEnv +
		" (32 random bytes, base64 encoded, or any passphrase)")
	// ErrUnsupportedBackend is returned for an unknown backend name.
	ErrUnsupportedBackend = errors.New("keystore: unknown backend")
)

// Config describes where and how credentials are stored.
type Config struct {
	// Backend selects the store. The zero value means BackendAuto.
	Backend Backend
	// Service and Account name the keyring item.
	Service string
	Account string
	// Path is the encrypted file location; empty means the per-OS default.
	Path string
	// Key is the encrypted file key. When empty the value of KeyEnv is used.
	Key []byte
	// KeyEnv names the environment variable holding the key; empty means
	// DefaultKeyEnv.
	KeyEnv string
	// AllowEphemeral must be true for BackendEphemeral to be honoured. It maps
	// to the daemon's --ephemeral flag.
	AllowEphemeral bool
}

func (c Config) withDefaults() Config {
	if c.Backend == "" {
		c.Backend = BackendAuto
	}
	if c.Service == "" {
		c.Service = DefaultService
	}
	if c.Account == "" {
		c.Account = DefaultAccount
	}
	if c.KeyEnv == "" {
		c.KeyEnv = DefaultKeyEnv
	}
	return c
}

// Store is a credential store the SDK can use, plus the two things the daemon
// needs on top: which backend answered, and whether it is readable right now.
type Store interface {
	opendrive.CredentialStore

	// Backend reports which implementation is in use, for /v1/auth/status.
	Backend() Backend
	// Available reports whether the store can be read at this moment. A locked
	// keyring returns an error here, which the daemon surfaces as
	// keystore_unavailable without touching the network (§4.5).
	Available(ctx context.Context) error
}

// keyringFactory builds the OS vault backend. It is a variable so that tests
// can simulate a machine without a vault, which is the case the "no persistence
// = configuration error" gate exists for.
var keyringFactory = newKeyring

// Open selects and prepares a credential store.
//
// It is the "no persistence = configuration error" gate of §9.2: with the
// default configuration it returns ErrNoBackend rather than quietly handing
// back a store that evaporates on restart.
func Open(cfg Config) (Store, error) {
	cfg = cfg.withDefaults()

	switch cfg.Backend {
	case BackendEphemeral:
		if !cfg.AllowEphemeral {
			return nil, ErrEphemeralNotAllowed
		}
		return newEphemeral(), nil

	case BackendKeyring:
		k := keyringFactory(cfg.Service, cfg.Account)
		if err := k.Available(context.Background()); err != nil {
			return nil, fmt.Errorf("keystore: the OS keyring is not usable: %w", err)
		}
		return k, nil

	case BackendFile:
		key, err := resolveKey(cfg)
		if err != nil {
			return nil, err
		}
		return newFileStore(filePath(cfg), key)

	case BackendAuto:
		k := keyringFactory(cfg.Service, cfg.Account)
		if err := k.Available(context.Background()); err == nil {
			return k, nil
		}
		// No vault. The encrypted file is only acceptable when the operator
		// configured a key for it, never as a silent fallback.
		if key, err := resolveKey(cfg); err == nil {
			return newFileStore(filePath(cfg), key)
		}
		return nil, ErrNoBackend

	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedBackend, cfg.Backend)
	}
}

// filePath returns the configured encrypted file path or the per-OS default.
func filePath(cfg Config) string {
	if cfg.Path != "" {
		return cfg.Path
	}
	return filepath.Join(defaultStateDir(), DefaultFileName)
}

// defaultStateDir mirrors the configuration locations of §3.4.
func defaultStateDir() string {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "opendrive-bridge")
		}
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
}

func newEphemeral() *ephemeralStore {
	return &ephemeralStore{MemoryTokenStore: opendrive.NewMemoryTokenStore()}
}

func (s *ephemeralStore) Backend() Backend                { return BackendEphemeral }
func (s *ephemeralStore) Available(context.Context) error { return nil }
