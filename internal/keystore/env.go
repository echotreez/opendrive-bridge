package keystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The credential file, and the one path credentials take (§9.2.2).
//
// Two files sit next to the programs, in the directory the user unpacked:
//
//	.env       the credentials and the Bridge API key, encrypted, 0600
//	.env.key   32 random bytes that decrypt it, generated on first run, 0600
//
// A user deploys by copying .env.example to .env and typing their OpenDrive
// username and password into it in the clear. The first run of the daemon reads
// that, generates .env.key and a random Bridge API key, writes everything back
// encrypted, and the plaintext password is gone from disk. From then on nothing
// reads a credential except through this store.
//
// Keeping the key beside the ciphertext is a deliberate consequence of the
// product requirement that the daemon start unattended (§2.2): any scheme that
// asks for a passphrase at boot cannot restart itself after a crash or a reboot.
// §9.2.3 states plainly what that does and does not protect against, and
// docs/first-run.md repeats it to users rather than burying it.
const (
	// EnvFileName is the encrypted credential file.
	EnvFileName = ".env"
	// KeyFileName holds the key that decrypts it.
	KeyFileName = ".env.key"
	// ExampleFileName is the template shipped in the release archive.
	ExampleFileName = ".env.example"
)

// Keys used inside the file. They are the same names the daemon accepts as
// environment variables, so that a reader of .env can guess right.
//
// #nosec G101 -- reviewed: these are the *names* of fields, not values. gosec
// flags an identifier containing "password" or "token" that is assigned a
// string, which is exactly what a constant naming a field looks like. Renaming
// them to appease it would make the file harder to read for no gain.
const (
	envUsername     = "ODB_USERNAME"
	envPassword     = "ODB_PASSWORD"
	envAPIKey       = "ODB_API_KEY"
	envAuthMode     = "ODB_AUTH_MODE"
	envAccessToken  = "ODB_ACCESS_TOKEN"
	envRefreshToken = "ODB_REFRESH_TOKEN"
	envTokenExpiry  = "ODB_TOKEN_EXPIRY"
	envSessionID    = "ODB_SESSION_ID"
	envUserID       = "ODB_USER_ID"
	envAccType      = "ODB_ACC_TYPE"
	envUpdatedAt    = "ODB_UPDATED_AT"
	// envChecksum makes tampering with the ciphertext visible. CBC is
	// malleable, so the integrity check lives in the plaintext: alter the
	// ciphertext and this stops matching, or the file stops parsing.
	envChecksum = "ODB_CHECKSUM"
)

// envStore is the only credential backend. It replaced three OS keyring
// implementations in v1.2; the reasoning is in whitepaper §9.2.1, and the short
// version is that two of the three were never available where the bridge most
// often runs, so the file path had to exist anyway and was being maintained as
// the second-class one.
type envStore struct {
	path    string // .env
	keyPath string // .env.key

	mu  sync.Mutex
	key []byte
}

func newEnvStore(path string, key []byte) (*envStore, error) {
	if path == "" {
		return nil, errors.New("keystore: the credential file needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("keystore: cannot create the state directory: %w", err)
	}
	return &envStore{
		path:    path,
		keyPath: filepath.Join(filepath.Dir(path), KeyFileName),
		key:     key,
	}, nil
}

// Backend implements Store.
func (s *envStore) Backend() Backend { return BackendFile }

// Available implements Store: it is the "no persistence = configuration error"
// gate (§9.2.4). It answers without touching the network, and every failure it
// reports names the file that is missing or unreadable and what to do about it.
func (s *envStore) Available(ctx context.Context) error {
	if _, err := s.Load(ctx); err != nil && !errors.Is(err, opendrive.ErrNoCredentials) {
		return err
	}
	return nil
}

// Load implements opendrive.CredentialStore.
func (s *envStore) Load(ctx context.Context) (*opendrive.StoredCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	if fields[envUsername] == "" {
		return nil, opendrive.ErrNoCredentials
	}
	return credentialsFromFields(fields)
}

// Save implements opendrive.CredentialStore.
func (s *envStore) Save(ctx context.Context, cred *opendrive.StoredCredentials) error {
	if cred == nil {
		return errors.New("keystore: refusing to store nil credentials")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read first so that anything the user put in the file which is not ours —
	// a comment, a setting we do not know about — survives a write.
	fields, err := s.read(ctx)
	if err != nil && !errors.Is(err, opendrive.ErrNoCredentials) {
		return err
	}
	if fields == nil {
		fields = map[string]string{}
	}
	applyCredentials(fields, cred)
	return s.write(fields)
}

// Delete implements opendrive.CredentialStore.
//
// The file itself stays, with the API key in it: removing it would take the
// Bridge's own key with it, and a user who runs `odctl logout` has not asked to
// re-key their API clients. Only the account credentials go.
func (s *envStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, err := s.read(ctx)
	if err != nil {
		if errors.Is(err, opendrive.ErrNoCredentials) {
			return nil
		}
		return err
	}
	for _, k := range []string{
		envUsername, envPassword, envAuthMode, envAccessToken,
		envRefreshToken, envTokenExpiry, envSessionID, envUserID, envAccType,
	} {
		delete(fields, k)
	}
	return s.write(fields)
}

// APIKey returns the Bridge's own API key, generating and storing one on first
// use. §9.2.2: the user never types or manages this.
func (s *envStore) APIKey(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, err := s.read(ctx)
	if err != nil && !errors.Is(err, opendrive.ErrNoCredentials) {
		return "", err
	}
	if fields == nil {
		fields = map[string]string{}
	}
	if key := fields[envAPIKey]; key != "" {
		return key, nil
	}
	key, err := randomHex(32)
	if err != nil {
		return "", err
	}
	fields[envAPIKey] = key
	if err := s.write(fields); err != nil {
		return "", err
	}
	return key, nil
}

// ---------------------------------------------------------------- file access

// read returns the file's fields, decrypting it and, on a first run, sealing it.
//
// The first run is the only moment a plaintext credential exists on disk, and it
// exists because the user put it there. Reading it also removes it: the file is
// written back encrypted before this function returns, so a daemon that starts
// once has no plaintext password on disk afterwards even if it never signs in.
func (s *envStore) read(ctx context.Context) (map[string]string, error) {
	raw, err := os.ReadFile(s.path) // #nosec G304 -- the configured credential file
	if errors.Is(err, os.ErrNotExist) {
		return nil, opendrive.ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot read %s: %w", s.path, err)
	}

	if !isEncrypted(raw) {
		fields := parseEnv(raw)
		if len(fields) == 0 {
			return nil, opendrive.ErrNoCredentials
		}
		// First run: seal it, which also creates .env.key if there is none.
		if err := s.writeLocked(ctx, fields); err != nil {
			return nil, err
		}
		return fields, nil
	}

	key, err := s.loadKey()
	if err != nil {
		return nil, err
	}
	plain, err := open(raw, key)
	if err != nil {
		return nil, fmt.Errorf("keystore: %s could not be decrypted with %s. "+
			"If you replaced the key file, restore it from your backup; if you no longer "+
			"have it, delete both files, copy %s to %s again and sign in once more: %w",
			EnvFileName, KeyFileName, ExampleFileName, EnvFileName, err)
	}
	fields := parseEnv(plain)
	if want, ok := fields[envChecksum]; ok && want != checksumOf(fields) {
		return nil, fmt.Errorf("keystore: %s decrypted but its contents do not match their "+
			"checksum, so the file has been altered since the bridge wrote it. Restore it "+
			"from a backup, or delete both files and sign in again", EnvFileName)
	}
	return fields, nil
}

func (s *envStore) write(fields map[string]string) error {
	return s.writeLocked(context.Background(), fields)
}

func (s *envStore) writeLocked(_ context.Context, fields map[string]string) error {
	key, err := s.ensureKey()
	if err != nil {
		return err
	}
	fields[envChecksum] = checksumOf(fields)
	sealed, err := seal(renderEnv(fields), key)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, sealed)
}

// loadKey reads .env.key, or the environment variable that stands in for it in
// a container.
func (s *envStore) loadKey() ([]byte, error) {
	if len(s.key) > 0 {
		return s.key, nil
	}
	raw, err := os.ReadFile(s.keyPath) // #nosec G304 -- the configured key file
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("keystore: %s is encrypted but %s is missing. "+
			"That file is the only thing that can decrypt it — restore it from your "+
			"backup, or delete both and start again with %s",
			EnvFileName, s.keyPath, ExampleFileName)
	}
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot read %s: %w", s.keyPath, err)
	}
	key := trimKey(raw)
	if len(key) == 0 {
		return nil, fmt.Errorf("keystore: %s is empty", s.keyPath)
	}
	s.key = key
	return key, nil
}

// ensureKey returns the key, creating .env.key when this is the first run.
func (s *envStore) ensureKey() ([]byte, error) {
	if key, err := s.loadKey(); err == nil {
		return key, nil
	}
	if len(s.key) > 0 {
		return s.key, nil
	}
	material := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, material); err != nil {
		return nil, fmt.Errorf("keystore: cannot generate a key: %w", err)
	}
	// Base64 so that `openssl enc -pass file:.env.key` reads it as one line,
	// which is what makes the documented recovery command work.
	encoded := []byte(base64.StdEncoding.EncodeToString(material) + "\n")
	if err := writeFileAtomic(s.keyPath, encoded); err != nil {
		return nil, err
	}
	s.key = trimKey(encoded)
	return s.key, nil
}

func trimKey(raw []byte) []byte {
	s := string(raw)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return []byte(strings.TrimSpace(s))
}

// ---------------------------------------------------------------- .env format

// parseEnv reads KEY=VALUE lines, ignoring blanks and comments. Values may be
// quoted, because a password can contain anything.
func parseEnv(raw []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// A double-quoted value may carry escapes, and they have to be undone
		// here or the value that comes back is not the value that went in — the
		// checksum notices, which is how this was found.
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = unescapeQuoted(v[1 : len(v)-1])
		} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

// renderEnv writes the fields back out, sorted so that the ciphertext of an
// unchanged credential set does not churn between writes for no reason.
func renderEnv(fields map[string]string) []byte {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		v := fields[k]
		if needsQuote(v) {
			v = `"` + escapeQuoted(v) + `"`
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// needsQuote decides whether a value has to be written between quotes.
//
// The rule is not "does it contain a space". A fuzzer produced "\v0", which
// survived rendering unquoted and then lost its first character on the way back
// in, because the parser trims each value and Go counts a vertical tab as space.
// So anything that trimming would change is quoted, whatever the character.
func needsQuote(v string) bool {
	if v == "" || v != strings.TrimSpace(v) {
		return true
	}
	if strings.ContainsAny(v, " \t\"'#=\\") {
		return true
	}
	for _, r := range v {
		if unicode.IsSpace(r) || r < 0x20 {
			return true
		}
	}
	return false
}

// escapeQuoted and unescapeQuoted are inverses. Backslash first on the way in
// and last on the way out, or an escaped backslash eats the quote after it.
func escapeQuoted(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

func unescapeQuoted(v string) string {
	v = strings.ReplaceAll(v, `\"`, `"`)
	return strings.ReplaceAll(v, `\\`, `\`)
}

// checksumOf covers every field except the checksum itself.
func checksumOf(fields map[string]string) string {
	copyOf := make(map[string]string, len(fields))
	for k, v := range fields {
		if k != envChecksum {
			copyOf[k] = v
		}
	}
	sum := sha256.Sum256(renderEnv(copyOf))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- mapping

func credentialsFromFields(f map[string]string) (*opendrive.StoredCredentials, error) {
	cred := &opendrive.StoredCredentials{
		Username:  f[envUsername],
		Password:  f[envPassword],
		SessionID: f[envSessionID],
		UserID:    f[envUserID],
		AuthMode:  opendrive.AuthMode(f[envAuthMode]),
	}
	if cred.AuthMode == "" {
		cred.AuthMode = opendrive.AuthModeOAuth2
	}
	if v := f[envAccType]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			cred.AccType = n
		}
	}
	if v := f[envUpdatedAt]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cred.UpdatedAt = t
		}
	}
	if access := f[envAccessToken]; access != "" {
		tok := &opendrive.Token{AccessToken: access, RefreshToken: f[envRefreshToken]}
		if v := f[envTokenExpiry]; v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				tok.Expiry = t
			}
		}
		cred.Token = tok
	}
	return cred, nil
}

func applyCredentials(f map[string]string, cred *opendrive.StoredCredentials) {
	set := func(k, v string) {
		if v == "" {
			delete(f, k)
			return
		}
		f[k] = v
	}
	set(envUsername, cred.Username)
	set(envPassword, cred.Password)
	set(envSessionID, cred.SessionID)
	set(envUserID, cred.UserID)
	set(envAuthMode, string(cred.AuthMode))
	if cred.AccType != 0 {
		set(envAccType, fmt.Sprintf("%d", cred.AccType))
	} else {
		delete(f, envAccType)
	}
	updated := cred.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	set(envUpdatedAt, updated.UTC().Format(time.RFC3339))

	if cred.Token != nil {
		set(envAccessToken, cred.Token.AccessToken)
		set(envRefreshToken, cred.Token.RefreshToken)
		if !cred.Token.Expiry.IsZero() {
			set(envTokenExpiry, cred.Token.Expiry.UTC().Format(time.RFC3339))
		} else {
			delete(f, envTokenExpiry)
		}
	} else {
		delete(f, envAccessToken)
		delete(f, envRefreshToken)
		delete(f, envTokenExpiry)
	}
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("keystore: cannot generate a key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// writeFileAtomic writes data through a temporary file in the same directory, so
// the replacement is atomic and the old content survives a crash.
//
// This is not tidiness: §9.2.4 requires it because a token rotation that is
// interrupted halfway would otherwise leave a truncated file, and a truncated
// credential file locks the user out permanently — the one failure this system
// must not have.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keystore: cannot create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".odb-*.tmp")
	if err != nil {
		// Name the file the user cares about, not the temporary one they will
		// never see. This is the message a container with a read-only /data
		// gets, and "cannot create a temporary file in /data:
		// open /data/.odb-419478592.tmp: permission denied" told them nothing
		// about which file the bridge wanted or what to change.
		//
		// The PathError is unwrapped to its errno before wrapping, because
		// otherwise the temporary name comes back anyway in the tail of the
		// message — which a test here noticed. errors.Is(err, fs.ErrPermission)
		// still works, since that is what the errno answers.
		cause := err
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			cause = pathErr.Err
		}
		return fmt.Errorf("keystore: cannot write %s: %s has to be writable, because the "+
			"bridge keeps your credentials there and creates %s in it on first run: %w",
			filepath.Base(path), dir, KeyFileName, cause)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // a no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("keystore: cannot restrict the file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("keystore: cannot write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("keystore: cannot flush %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keystore: cannot close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("keystore: cannot replace %s: %w", path, err)
	}
	// The mode set above means nothing on Windows, so the access control list
	// is tightened here, after the file is in place (§9.2.2).
	if err := restrictToOwner(path); err != nil {
		return err
	}
	// Flush the rename itself, so a power cut cannot lose the new file.
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- the directory just written
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
