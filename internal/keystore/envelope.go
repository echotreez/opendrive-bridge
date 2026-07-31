package keystore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The encrypted .env is written in OpenSSL's own `enc` format so that a user can
// read their credentials back without this program:
//
//	openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a \
//	  -pass file:.env.key -in .env
//
// That property is the point of the format choice, and it cost something. The
// whitepaper asked for AES-256-GCM *and* for the file to be readable by
// `openssl enc`, and those are mutually exclusive: `openssl enc` refuses AEAD
// ciphers outright — "enc: AEAD ciphers not supported" — so a GCM file would be
// readable only by us. Given §9.2.3 already states that the threat model is file
// permissions and accidental leakage, and explicitly *not* an attacker who can
// already read the file as this user, AEAD's tamper resistance is guarding
// against someone the model has excluded, while "you are not locked in to our
// tool" protects the user against us. CBC keeps the promise that matters here.
//
// Integrity is not abandoned: the plaintext is a checksummed document (see
// envFile), so ciphertext that has been altered decrypts to something that fails
// to parse rather than to a subtly different password.
const (
	// opensslMagic is the 8-byte header OpenSSL writes before the salt.
	opensslMagic = "Salted__"
	// saltLen is what OpenSSL uses, and is not ours to change.
	saltLen = 8
	// pbkdf2Iterations is deliberately far above OpenSSL's default of 10000.
	// The cost is paid once per daemon start, and the key file is 32 random
	// bytes rather than a human passphrase, so this is belt and braces — but a
	// user who copies a weak passphrase into .env.key gets the benefit.
	//
	// It has to be passed to openssl explicitly (-iter 600000). Documented
	// everywhere the command appears.
	pbkdf2Iterations = 600000
	keyLen           = 32 // AES-256
	ivLen            = aes.BlockSize
)

var errNotEncrypted = errors.New("keystore: this is not an encrypted file")

// seal encrypts plaintext exactly as `openssl enc -aes-256-cbc -pbkdf2 -a` does,
// and returns the base64 armoured form. Armoured because .env is a file people
// open in editors and paste into issues; raw ciphertext invites a text editor to
// corrupt it silently.
func seal(plain, password []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("keystore: cannot generate a salt: %w", err)
	}
	key, iv := deriveKeyIV(password, salt)

	padded := pkcs7Pad(plain, aes.BlockSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot initialise AES: %w", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	raw := make([]byte, 0, len(opensslMagic)+saltLen+len(ciphertext))
	raw = append(raw, opensslMagic...)
	raw = append(raw, salt...)
	raw = append(raw, ciphertext...)

	// OpenSSL's -a output wraps at 64 characters and ends with a newline.
	encoded := base64.StdEncoding.EncodeToString(raw)
	var out bytes.Buffer
	for len(encoded) > 64 {
		out.WriteString(encoded[:64])
		out.WriteByte('\n')
		encoded = encoded[64:]
	}
	out.WriteString(encoded)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// open reverses seal. It accepts the armoured form with or without line breaks,
// which is what a user gets if they pass the file through anything at all.
func open(armoured, password []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(stripSpace(string(armoured)))
	if err != nil {
		return nil, errNotEncrypted
	}
	if len(raw) < len(opensslMagic)+saltLen || string(raw[:len(opensslMagic)]) != opensslMagic {
		return nil, errNotEncrypted
	}
	salt := raw[len(opensslMagic) : len(opensslMagic)+saltLen]
	ciphertext := raw[len(opensslMagic)+saltLen:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("keystore: the encrypted file is truncated")
	}

	key, iv := deriveKeyIV(password, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keystore: cannot initialise AES: %w", err)
	}
	padded := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(padded, ciphertext)
	return pkcs7Unpad(padded)
}

// isEncrypted reports whether content is the armoured form. It is how the first
// run tells a freshly copied .env.example from a file this program has already
// sealed.
func isEncrypted(content []byte) bool {
	raw, err := base64.StdEncoding.DecodeString(stripSpace(string(content)))
	return err == nil && len(raw) >= len(opensslMagic) &&
		string(raw[:len(opensslMagic)]) == opensslMagic
}

// deriveKeyIV is OpenSSL's -pbkdf2 derivation: one PBKDF2-HMAC-SHA256 stream
// split into the key and then the IV.
func deriveKeyIV(password, salt []byte) (key, iv []byte) {
	stream := pbkdf2SHA256(password, salt, pbkdf2Iterations, keyLen+ivLen)
	return stream[:keyLen], stream[keyLen:]
}

// pbkdf2SHA256 is RFC 8018 PBKDF2 with HMAC-SHA-256.
//
// Written out rather than imported. golang.org/x/crypto/pbkdf2 would do, but the
// version that provides it requires Go 1.25, and adding it moved this module's
// floor from 1.22 to 1.25 without anyone asking — the same silent drift as a
// hand-pinned version next to something that floats. Go 1.24's crypto/pbkdf2
// would cost the same. Twenty lines against a floor the whitepaper sets is a
// trade worth making, and this is not a primitive being invented: it is a KDF
// specified in an RFC, built out of the standard library's HMAC, and checked
// byte for byte against the real openssl in envelope_test.go.
func pbkdf2SHA256(password, salt []byte, iter, length int) []byte {
	mac := hmac.New(sha256.New, password)
	hashLen := mac.Size()
	blocks := (length + hashLen - 1) / hashLen

	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		mac.Reset()
		mac.Write(salt)
		// The block counter goes in big-endian, per RFC 8018. Written with
		// encoding/binary rather than four shifts so that the truncation is
		// the package's business rather than something a reader — or gosec —
		// has to convince themselves about.
		binary.BigEndian.PutUint32(buf, uint32(block))
		mac.Write(buf)
		acc := mac.Sum(nil)
		copy(u, acc)
		for i := 2; i <= iter; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range acc {
				acc[j] ^= u[j]
			}
		}
		out = append(out, acc...)
	}
	return out[:length]
}

// pkcs7Pad appends between 1 and size bytes, each holding the number added.
//
// The signature takes an int for readability at the call site, but the padding
// length is a byte by definition of PKCS#7, so the type is narrowed here where
// the bound is obvious rather than left for a reader to reason about.
func pkcs7Pad(data []byte, size int) []byte {
	if size <= 0 || size > 255 {
		panic("keystore: PKCS#7 is defined for block sizes of 1 to 255")
	}
	// #nosec G115 -- reviewed: the check above bounds size to 1..255 and the
	// modulus keeps the difference inside it, so this cannot truncate. The
	// analyser cannot follow that, and the alternative — carrying the value as
	// an int and converting at each use — would move the same conversion
	// somewhere less obvious.
	n := byte(size - len(data)%size)
	return append(data, bytes.Repeat([]byte{n}, int(n))...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("keystore: nothing to unpad")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > aes.BlockSize || n > len(data) {
		// Wrong key, or the file was altered. Both look like this.
		return nil, errWrongKey
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, errWrongKey
		}
	}
	return data[:len(data)-n], nil
}

var errWrongKey = errors.New("keystore: the file could not be decrypted with this key")

func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r != '\n' && r != '\r' && r != ' ' && r != '\t' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
