package server

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// S3 keys and OpenDrive names (whitepaper §3.6.3).
//
// An S3 key is any UTF-8 string up to 1024 bytes; an OpenDrive name is not. The
// mapping below is reversible, so that a key written through the gateway lists
// back exactly as it was written — restic, Kopia and Hyper Backup all compare
// what they list with what they wrote, and a key that comes back different is a
// repository that looks corrupt.
//
// The set of characters replaced is rclone's for its OpenDrive backend, which
// rclone arrived at by measuring the same API; it is re-measured by the
// integration probe TestSandboxS3NameEncoding rather than trusted. Each is
// swapped for a look-alike from a Unicode block nobody types by accident, the
// way rclone does it, so a file browsed on the OpenDrive web page still reads
// sensibly. A key that already contains one of the look-alikes has it escaped
// with a quote mark, which is what makes the mapping reversible rather than
// merely usually right.

const nameQuote = '‛' // U+201B, marks a look-alike that was in the key itself

// maxNameBytes is OpenDrive's limit on a file or folder name, as the SDK
// enforces it.
const maxNameBytes = 255

// nameReplacements maps each character OpenDrive cannot hold to its stand-in.
var nameReplacements = map[rune]rune{
	'\\': '＼', ':': '：', '*': '＊', '?': '？', '"': '＂',
	'<': '＜', '>': '＞', '|': '｜',
	0x7f: '␡',
}

// nameRestorations is nameReplacements the other way round, plus the control
// characters (U+0000–U+001F ↔ U+2400–U+241F) and the edge space.
var nameRestorations = func() map[rune]rune {
	m := map[rune]rune{}
	for k, v := range nameReplacements {
		m[v] = k
	}
	for c := rune(0); c < 0x20; c++ {
		m[0x2400+c] = c
	}
	m['␠'] = ' '
	return m
}()

// isStandIn reports a character the encoding uses in place of another. It has
// to be the comma-ok form: U+2400 stands in for NUL, whose value is zero, and a
// lookup that tested the value for zero let a key holding a literal "␀" come
// back as a NUL (found by FuzzS3KeyEncoding).
func isStandIn(r rune) bool {
	_, ok := nameRestorations[r]
	return ok
}

// encodeSegment turns one "/"-separated part of an S3 key into an OpenDrive
// name. The caller has already refused empty segments.
func encodeSegment(seg string) string {
	switch seg {
	case ".":
		return "．"
	case "..":
		return "．．"
	}
	var b strings.Builder
	runes := []rune(seg)
	for i, r := range runes {
		switch {
		case r == utf8.RuneError:
			// Invalid UTF-8 cannot be stored or given back; the key was refused
			// before this point, so this is unreachable in practice.
			b.WriteRune('�')
		case r == nameQuote || isStandIn(r) || r == '．' && (seg == "．" || seg == "．．"):
			b.WriteRune(nameQuote)
			b.WriteRune(r)
		case r < 0x20:
			b.WriteRune(0x2400 + r)
		case nameReplacements[r] != 0:
			b.WriteRune(nameReplacements[r])
		case r == ' ' && (i == 0 || i == len(runes)-1):
			// OpenDrive trims surrounding whitespace from names (D46).
			b.WriteRune('␠')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decodeSegment is encodeSegment's inverse.
func decodeSegment(name string) string {
	switch name {
	case "．":
		return "."
	case "．．":
		return ".."
	}
	var b strings.Builder
	quoted := false
	for _, r := range name {
		if quoted {
			b.WriteRune(r)
			quoted = false
			continue
		}
		if r == nameQuote {
			quoted = true
			continue
		}
		if orig, ok := nameRestorations[r]; ok {
			b.WriteRune(orig)
			continue
		}
		b.WriteRune(r)
	}
	if quoted {
		// A name ending in a bare quote mark was not written by this gateway;
		// give it back as it is.
		b.WriteRune(nameQuote)
	}
	return b.String()
}

// s3KeyError says why a key cannot be stored. It is a sentence for the client's
// error message, not a code.
type s3KeyError string

func (e s3KeyError) Error() string { return string(e) }

// objectPath maps a bucket and key to the path of the file in the account.
//
// A trailing "/" is allowed and preserved as a trailing "/" in the result, for
// the zero-byte "folder marker" objects some clients create.
func objectPath(bucket, key string) (string, error) {
	if strings.HasSuffix(key, folderKeySuffix) {
		key = strings.TrimSuffix(key, folderKeySuffix) + "/" // see markFolderKey
	}
	if key == "" {
		return "", s3KeyError("the key is empty")
	}
	if len(key) > 1024 {
		return "", s3KeyError("keys are limited to 1024 bytes")
	}
	if !utf8.ValidString(key) {
		return "", s3KeyError("the key is not valid UTF-8")
	}
	trailing := strings.HasSuffix(key, "/")
	body := strings.TrimSuffix(key, "/")
	if body == "" {
		return "", s3KeyError("a key of only \"/\" names nothing")
	}
	segs := strings.Split(body, "/")
	out := make([]string, 0, len(segs)+1)
	out = append(out, "", bucketName(bucket))
	for _, s := range segs {
		if s == "" {
			return "", s3KeyError("keys with an empty part (\"a//b\", or a leading \"/\") " +
				"cannot be stored: OpenDrive folders need names")
		}
		if edgeSpace(s) {
			return "", s3KeyError("a part of the key begins or ends with a space character other " +
				"than an ordinary space; OpenDrive would remove it and the key could not be given back")
		}
		enc := encodeSegment(s)
		if len(enc) > maxNameBytes {
			// Found by fuzzing. Accepted, it would have been refused by
			// OpenDrive at flush time and sat unsendable in the cache for ever.
			return "", s3KeyError(fmt.Sprintf("a part of the key is %d bytes as stored "+
				"(characters OpenDrive cannot hold take 3 bytes each); OpenDrive names are limited to %d",
				len(enc), maxNameBytes))
		}
		out = append(out, enc)
	}
	p := strings.Join(out, "/")
	if trailing {
		p += "/"
	}
	return p, nil
}

// edgeSpace reports a segment that starts or ends with Unicode white space the
// encoding does not cover. Ordinary spaces and control characters are encoded;
// anything else there (a no-break space, say) may be trimmed by OpenDrive or by
// the path handling in front of it (D46), and a key that silently loses a
// character is worse than one refused in words.
func edgeSpace(seg string) bool {
	first, _ := utf8.DecodeRuneInString(seg)
	last, _ := utf8.DecodeLastRuneInString(seg)
	odd := func(r rune) bool { return unicode.IsSpace(r) && r != ' ' && r >= 0x20 && r != 0x7f }
	return odd(first) || odd(last)
}

// bucketName is the top-level folder a bucket is (§3.6.3: bucket = folder).
func bucketName(bucket string) string { return encodeSegment(bucket) }

// keyOf is objectPath's inverse for a path inside the bucket's folder.
func keyOf(bucketPath, p string) string {
	rest := strings.TrimPrefix(p, bucketPath+"/")
	segs := strings.Split(rest, "/")
	for i, s := range segs {
		segs[i] = decodeSegment(s)
	}
	return strings.Join(segs, "/")
}
