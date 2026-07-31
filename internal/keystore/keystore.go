// Package keystore persists the credentials that let the bridge run silently
// after its initial setup: username, password, OAuth tokens and session id
// (whitepaper §9.2, the four credential kinds of the CredentialStore).
//
// There is one real backend: an encrypted .env file beside the programs, with
// its key in .env.key next to it (§9.2.2). Until v1.2 there were four — the
// three OS vaults and this — and the three were removed because two of them were
// unavailable on the machines where the bridge most often runs, so this path had
// to exist anyway and was being maintained as the second-class one. §9.2.1 has
// the full reasoning, and §9.2.3 is honest about what the change costs.
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
	// DefaultKeyEnv names an environment variable that stands in for .env.key.
	// It exists for containers and for tests: a key given this way is used as
	// it is and no key file is written.
	DefaultKeyEnv = "ODB_STATE_KEY"
	// DefaultFileName is the credential file's name.
	DefaultFileName = EnvFileName
)

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

// Errors returned by Open. They are deliberately actionable: whichever one the
// operator sees, it tells them what to configure.
var (
	// ErrNoBackend means nothing can persist credentials on this machine, and
	// the caller did not opt into an ephemeral run.
	ErrNoBackend = errors.New("keystore: no credential store is available; " +
		"copy " + ExampleFileName + " to " + EnvFileName + " and fill in your OpenDrive " +
		"username and password, or start with --ephemeral to accept losing credentials on restart")
	// ErrEphemeralNotAllowed means an in-memory store was requested without the
	// explicit opt-in.
	ErrEphemeralNotAllowed = errors.New("keystore: the ephemeral backend keeps credentials in memory only " +
		"and must be enabled explicitly (--ephemeral)")
	// ErrNoKey means a key was asked for and none could be read or created.
	ErrNoKey = errors.New("keystore: no key is available to encrypt " + EnvFileName +
		"; the bridge creates " + KeyFileName + " itself on first run, so this means the " +
		"directory could not be written to")
	// ErrUnsupportedBackend is returned for an unknown backend name.
	ErrUnsupportedBackend = errors.New("keystore: unknown backend")
)

// Config describes where and how credentials are stored.
type Config struct {
	// Backend selects the store. The zero value means BackendAuto.
	Backend Backend
	// Path is the credential file; empty means .env beside the program.
	Path string
	// Key overrides .env.key. When empty the value of KeyEnv is used, and
	// failing that the key file is read or created.
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
	// Available reports whether the store can be read at this moment. A missing
	// key file or an undecryptable .env returns an error here, which the daemon
	// surfaces as keystore_unavailable without touching the network (§4.5).
	Available(ctx context.Context) error
}

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

	case BackendFile, BackendAuto:
		// A key given explicitly, or through the environment, is used as it is
		// and no key file is written. That is how a container supplies one
		// (§8.3) and how tests avoid touching the filesystem twice.
		key := cfg.Key
		if len(key) == 0 {
			if v := os.Getenv(cfg.KeyEnv); v != "" {
				key = []byte(v)
			}
		}
		return newEnvStore(filePath(cfg), key)

	default:
		// Naming the alternatives is the difference between a message that ends
		// the problem and one that starts a search.
		return nil, fmt.Errorf("%w: %q (choose one of: %s, %s, %s)",
			ErrUnsupportedBackend, cfg.Backend,
			BackendAuto, BackendFile, BackendEphemeral)
	}
}

// filePath returns the configured credential file, or .env in the directory the
// program was unpacked into.
//
// v1.2 runs in place (§8.2), so the default is beside the binary rather than in
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
		return filepath.Join(filepath.Dir(exe), EnvFileName)
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
