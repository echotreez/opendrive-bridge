package keystore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// fileFormat is the header of the encrypted credential file. It is versioned so
// a future key rotation scheme can recognise old files.
const fileFormat = "ODBKS1"

// fileStore keeps credentials in an AES-256-GCM encrypted file, for the
// environments that have no OS credential vault: Docker images and headless
// Linux servers (§9.2). The key never lives in the file — it comes from the
// environment or a mounted secret.
//
// Writes are atomic: the new content lands in a temporary file in the same
// directory, is flushed, and only then replaces the old one. A crash therefore
// leaves either the previous credentials or the new ones, never a truncated
// file that would lock the user out permanently (§9.2).
type fileStore struct {
	path string
	key  []byte

	mu sync.Mutex
}

func newFileStore(path string, key []byte) (*fileStore, error) {
	if len(key) != 32 {
		return nil, ErrNoKey
	}
	if path == "" {
		return nil, errors.New("keystore: the encrypted file needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("keystore: cannot create the state directory: %w", err)
	}
	return &fileStore{path: path, key: key}, nil
}

// resolveKey turns the configured key material into a 32-byte AES key. A
// base64 encoded 32-byte value is used as-is; anything else is hashed, so a
// passphrase works too.
func resolveKey(cfg Config) ([]byte, error) {
	raw := cfg.Key
	if len(raw) == 0 {
		if v := os.Getenv(cfg.KeyEnv); v != "" {
			raw = []byte(v)
		}
	}
	if len(raw) == 0 {
		return nil, ErrNoKey
	}
	if decoded, err := base64.StdEncoding.DecodeString(string(raw)); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

// Backend implements Store.
func (s *fileStore) Backend() Backend { return BackendFile }

// Available implements Store: the directory must be writable, and an existing
// file must be decryptable with the configured key.
func (s *fileStore) Available(ctx context.Context) error {
	if _, err := s.Load(ctx); err != nil && !errors.Is(err, opendrive.ErrNoCredentials) {
		return err
	}
	return nil
}

// Load implements opendrive.CredentialStore.
func (s *fileStore) Load(context.Context) (*opendrive.StoredCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, opendrive.ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot read the credential file: %w", err)
	}
	plain, err := s.decrypt(raw)
	if err != nil {
		return nil, err
	}
	var cred opendrive.StoredCredentials
	if err := json.Unmarshal(plain, &cred); err != nil {
		return nil, fmt.Errorf("keystore: the stored credentials are unreadable: %w", err)
	}
	return &cred, nil
}

// Save implements opendrive.CredentialStore.
func (s *fileStore) Save(_ context.Context, cred *opendrive.StoredCredentials) error {
	if cred == nil {
		return errors.New("keystore: refusing to store nil credentials")
	}
	plain, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("keystore: cannot encode credentials: %w", err)
	}
	sealed, err := s.encrypt(plain)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.path, sealed)
}

// Delete implements opendrive.CredentialStore.
func (s *fileStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keystore: cannot remove the credential file: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- crypto

func (s *fileStore) encrypt(plain []byte) ([]byte, error) {
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("keystore: cannot generate a nonce: %w", err)
	}
	out := make([]byte, 0, len(fileFormat)+len(nonce)+len(plain)+gcm.Overhead())
	out = append(out, fileFormat...)
	out = append(out, nonce...)
	// The format header is authenticated, so a file from another version
	// cannot be silently reinterpreted.
	return gcm.Seal(out, nonce, plain, []byte(fileFormat)), nil
}

func (s *fileStore) decrypt(raw []byte) ([]byte, error) {
	if len(raw) < len(fileFormat) || string(raw[:len(fileFormat)]) != fileFormat {
		return nil, errors.New("keystore: the credential file is not in a known format")
	}
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	body := raw[len(fileFormat):]
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("keystore: the credential file is truncated")
	}
	nonce, ciphertext := body[:gcm.NonceSize()], body[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(fileFormat))
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot decrypt the credential file; "+
			"is %s the same key that wrote it? %w", DefaultKeyEnv, err)
	}
	return plain, nil
}

func (s *fileStore) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("keystore: invalid key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot initialise AES-GCM: %w", err)
	}
	return gcm, nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, so the replacement is atomic and the old content survives a crash.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keystore: cannot create the state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("keystore: cannot create a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once the rename succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("keystore: cannot restrict the file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("keystore: cannot write the credential file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("keystore: cannot flush the credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keystore: cannot close the credential file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("keystore: cannot replace the credential file: %w", err)
	}
	// Flush the rename itself, so a power cut cannot lose the new file.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
