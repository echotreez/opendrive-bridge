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
	"syscall"
	"time"
	"unicode"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The credential file (§9.2.2, as revised for 1.3).
//
// One file, credentials.key, beside the programs (or in /data in the container).
// The user never creates, edits or prepares it. The daemon writes it when someone
// signs in — on the web page or with `odctl login` — and again when a token is
// refreshed; it is read on every start. With no file, or with credentials
// OpenDrive no longer accepts, the daemon still starts and asks to be signed in.
//
// Inside, after a comment saying how to read it by hand, one line holds a random
// key and the rest is the credentials encrypted with it, in OpenSSL's own `enc`
// format (envelope.go). Key and ciphertext side by side keeps the password out
// of casual view, out of git and out of a cloud backup's plain
// text, and does not protect it from someone who can already read your files.
// §9.2.3 says so, and so does docs/first-run.md.
const (
	// CredentialsFileName is the one credential file.
	CredentialsFileName = "credentials.key"
)

// Keys used inside the encrypted document.
//
// #nosec G101 -- reviewed: these are the *names* of fields, not values. gosec
// flags an identifier containing "password" or "token" that is assigned a
// string, which is exactly what a constant naming a field looks like.
const (
	envUsername     = "ODB_USERNAME"
	envPassword     = "ODB_PASSWORD"
	envAuthMode     = "ODB_AUTH_MODE"
	envAccessToken  = "ODB_ACCESS_TOKEN"
	envRefreshToken = "ODB_REFRESH_TOKEN"
	envTokenExpiry  = "ODB_TOKEN_EXPIRY"
	envSessionID    = "ODB_SESSION_ID"
	envUserID       = "ODB_USER_ID"
	envAccType      = "ODB_ACC_TYPE"
	envUpdatedAt    = "ODB_UPDATED_AT"
	// envChecksum makes tampering with the ciphertext visible. CBC is
	// malleable, so the integrity check lives in the plaintext.
	envChecksum = "ODB_CHECKSUM"
)

// fileHeader is written above the key. It is for the person who opens the file
// wondering what it is, and it is the recovery procedure §9.2.2 promises.
const fileHeader = `# opendrive-bridge credentials. Written by the bridge when you sign in; do not edit.
# The next line is the key, the rest is your credentials encrypted with it. To read
# them without the bridge:
#   grep -v '^#' credentials.key | head -1 > /tmp/k
#   grep -v '^#' credentials.key | tail -n +2 | \
#     openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a -pass file:/tmp/k
# Anyone who can read this file can do the same. Keep it private; back it up.
`

// envStore is the only credential backend: one encrypted file.
type envStore struct {
	path string // credentials.key

	mu  sync.Mutex
	key []byte // cached after the first read or write
}

func newEnvStore(path string) (*envStore, error) {
	if path == "" {
		return nil, errors.New("keystore: the credential file needs a path")
	}
	return &envStore{path: path}, nil
}

// Backend implements Store.
func (s *envStore) Backend() Backend { return BackendFile }

// Path is where the credentials live, for messages that tell a user where to look.
func (s *envStore) Path() string { return s.path }

// Available implements Store. A file that is absent is not a fault — nobody has
// signed in yet — but one that is present and unreadable is, and says why.
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

	fields, err := s.read()
	if err != nil {
		return nil, err
	}
	if fields[envUsername] == "" {
		return nil, opendrive.ErrNoCredentials
	}
	return credentialsFromFields(fields)
}

// Save implements opendrive.CredentialStore. It is called when someone signs in
// and when a token is refreshed, and it creates the file and its directory the
// first time.
func (s *envStore) Save(ctx context.Context, cred *opendrive.StoredCredentials) error {
	if cred == nil {
		return errors.New("keystore: refusing to store nil credentials")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fields, err := s.read()
	if err != nil && !errors.Is(err, opendrive.ErrNoCredentials) {
		// An unreadable file is replaced: the user has just typed credentials
		// that work, which is better than whatever was there.
		fields = nil
	}
	if fields == nil {
		fields = map[string]string{}
	}
	applyCredentials(fields, cred)
	return s.write(fields)
}

// Delete implements opendrive.CredentialStore: signing out removes the
// OpenDrive sign-in.
//
// Only the sign-in. The same file holds the bridge's own secrets — the S3 access
// keys a NAS was set up with — and they are not OpenDrive's to revoke: signing
// out to switch accounts must not silently break every backup job pointed at the
// bridge. When nothing else is in the file, the file goes.
func (s *envStore) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fields, err := s.read()
	if err == nil {
		for _, k := range signInFields {
			delete(fields, k)
		}
		if hasSecrets(fields) {
			return s.write(fields)
		}
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keystore: cannot remove %s: %w", s.path, err)
	}
	s.key = nil
	return nil
}

// signInFields are the fields that describe the OpenDrive sign-in.
var signInFields = []string{
	envUsername, envPassword, envAuthMode, envAccessToken, envRefreshToken,
	envTokenExpiry, envSessionID, envUserID, envAccType, envUpdatedAt,
}

// secretPrefix marks the bridge's own secrets in the file (SaveSecret).
const secretPrefix = "ODB_SECRET_"

func hasSecrets(fields map[string]string) bool {
	for k := range fields {
		if strings.HasPrefix(k, secretPrefix) {
			return true
		}
	}
	return false
}

// LoadSecret implements Secrets.
func (s *envStore) LoadSecret(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fields, err := s.read()
	if errors.Is(err, opendrive.ErrNoCredentials) {
		return "", ErrNoSecret
	}
	if err != nil {
		return "", err
	}
	v, ok := fields[secretPrefix+name]
	if !ok || v == "" {
		return "", ErrNoSecret
	}
	return v, nil
}

// SaveSecret implements Secrets. It leaves the sign-in, if any, as it is.
func (s *envStore) SaveSecret(ctx context.Context, name, value string) error {
	if !validSecretName(name) {
		return fmt.Errorf("keystore: %q is not a usable secret name", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fields, err := s.read()
	if errors.Is(err, opendrive.ErrNoCredentials) {
		fields, err = map[string]string{}, nil
	}
	if err != nil {
		// Unlike Save, a damaged file is not replaced here: it may hold a
		// sign-in the user has no other copy of, and a secret is not worth that.
		return err
	}
	if value == "" {
		delete(fields, secretPrefix+name)
	} else {
		fields[secretPrefix+name] = value
	}
	return s.write(fields)
}

// ---------------------------------------------------------------- file access

// read returns the decrypted fields, or ErrNoCredentials when there is no file.
func (s *envStore) read() (map[string]string, error) {
	raw, err := os.ReadFile(s.path) // #nosec G304 -- the configured credential file
	if errors.Is(err, os.ErrNotExist) {
		return nil, opendrive.ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot read %s: %w", s.path, err)
	}
	key, sealed := splitCredentialFile(raw)
	if len(key) == 0 || !isEncrypted(sealed) {
		return nil, fmt.Errorf("keystore: %s is not a credentials file this bridge wrote. "+
			"Remove it and sign in again", s.path)
	}
	plain, err := open(sealed, key)
	if err != nil {
		return nil, fmt.Errorf("keystore: %s could not be decrypted: it has been damaged "+
			"or edited. Remove it and sign in again: %w", s.path, err)
	}
	fields := parseEnv(plain)
	// The checksum is required, not merely checked when present. The bridge always
	// writes one, and CBC has no authentication: a wrong key (or a key line from
	// another file) decrypts to noise that passes the padding check about one time
	// in 256, and noise parses to no fields at all — which, with an optional
	// checksum, read as "nobody has signed in" instead of "this file is damaged".
	if want, ok := fields[envChecksum]; !ok || want != checksumOf(fields) {
		return nil, fmt.Errorf("keystore: %s does not decrypt to what the bridge wrote: "+
			"it has been damaged or edited. Remove it and sign in again", s.path)
	}
	s.key = key
	return fields, nil
}

func (s *envStore) write(fields map[string]string) error {
	key := s.key
	if len(key) == 0 {
		material := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, material); err != nil {
			return fmt.Errorf("keystore: cannot generate a key: %w", err)
		}
		// Base64, so that it is one printable line openssl can read as a
		// passphrase file.
		key = []byte(base64.StdEncoding.EncodeToString(material))
	}
	fields[envChecksum] = checksumOf(fields)
	sealed, err := seal(renderEnv(fields), key)
	if err != nil {
		return err
	}
	var doc strings.Builder
	doc.WriteString(fileHeader)
	doc.Write(key)
	doc.WriteString("\n")
	doc.Write(sealed)
	if !strings.HasSuffix(string(sealed), "\n") {
		doc.WriteString("\n")
	}
	if err := writeFileAtomic(s.path, []byte(doc.String())); err != nil {
		return err
	}
	s.key = key
	return nil
}

// splitCredentialFile separates the key line from the ciphertext, skipping the
// comment header.
func splitCredentialFile(raw []byte) (key, sealed []byte) {
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines = append(lines, t)
	}
	if len(lines) < 2 {
		return nil, nil
	}
	return []byte(lines[0]), []byte(strings.Join(lines[1:], "\n") + "\n")
}

// ---------------------------------------------------------------- field format

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
			"bridge keeps your credentials there: %w",
			filepath.Base(path), dir, cause)
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
		// A file that is itself a mount point cannot be replaced, only written
		// into, and the atomic replacement above is not negotiable (§9.2.4). The
		// usual way to get here is mounting the credential file on its own.
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("keystore: cannot replace %s: it is mounted on its own, as a "+
				"single file, and a mounted file cannot be swapped for a new one. Mount the "+
				"folder that holds it instead (for Docker: -v \"$PWD:/data\", not "+
				"-v \"$PWD/data:/data\", not the file itself)", filepath.Base(path))
		}
		return fmt.Errorf("keystore: cannot replace %s: %w", path, err)
	}
	// The mode is reasserted after the rename, in case an existing file was
	// created by an older build with a laxer one (§9.2.2).
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

// restrictToOwner makes the credential file readable by its owner only.
//
// The mode bits say it all: 0600, set when the temporary file is created and
// reasserted after the rename (§9.2).
//
// Until v1.2 this had a second implementation. NTFS ignores the mode bits Go's
// Chmod pretends to set, so on Windows the file inherited its directory's access
// control list — which on a default profile includes Administrators — and a
// separate perm_windows.go rebuilt the ACL with icacls. That file, its test, and
// the reason for the build tags went with Windows support (§8.1): one platform
// needing a parallel implementation of something this basic is a fair sample of
// why the support surface narrowed.
func restrictToOwner(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("keystore: cannot restrict the credential file: %w", err)
	}
	return nil
}
