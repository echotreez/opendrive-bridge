package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// FuzzS3KeyEncoding asserts properties, not just the absence of a panic (rule 5):
//
//   - every key objectPath accepts comes back unchanged from keyOf;
//   - the path it produces has no character OpenDrive cannot hold, and no
//     segment starting or ending with a space OpenDrive would trim (D46);
//   - the SDK's own path normalisation leaves it exactly as it is, so the path
//     the gateway stores under is the path every later lookup asks for.
func FuzzS3KeyEncoding(f *testing.F) {
	for _, seed := range []string{
		"a", "a/b/c", `x:y*z?"<>|\`, " lead", "trail ", " both ", ".", "..", "a/./b", "a/../b",
		"‛", "‛．", "．", "．．", "␠", "\t", "\x00", "\x7f", "日本", "dir/", "a b", "a ",
		"＼", "‛‛x", "a\x1fb",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, key string) {
		p, err := objectPath("bucket", key)
		if err != nil {
			return
		}
		// The folder-marker stand-in (markFolderKey) is a trailing "/".
		if strings.HasSuffix(key, folderKeySuffix) {
			key = strings.TrimSuffix(key, folderKeySuffix) + "/"
		}
		if !utf8.ValidString(key) {
			t.Fatalf("invalid UTF-8 %q was accepted", key)
		}
		trailing := strings.HasSuffix(p, "/")
		body := strings.TrimSuffix(p, "/")
		if got := keyOf("/bucket", body); got+map[bool]string{true: "/", false: ""}[trailing] != key {
			t.Fatalf("%q → %q → %q", key, p, got)
		}
		for _, seg := range strings.Split(strings.TrimPrefix(body, "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q produced the segment %q", key, seg)
			}
			if strings.TrimSpace(seg) != seg {
				t.Fatalf("%q produced %q, which OpenDrive would trim", key, seg)
			}
			for _, r := range seg {
				if r < 0x20 || r == 0x7f || strings.ContainsRune(`\:*?"<>|`, r) {
					t.Fatalf("%q produced %q, which holds %q", key, seg, r)
				}
			}
		}
		norm, err := opendrive.NormalizeFolderPath(body)
		if err != nil {
			t.Fatalf("the SDK refuses %q (from %q): %v", body, key, err)
		}
		if norm != body {
			t.Fatalf("the SDK rewrites %q as %q (from key %q)", body, norm, key)
		}
	})
}

func TestS3KeysThatCannotBeStoredAreRefusedInWords(t *testing.T) {
	for key, want := range map[string]string{
		"":                        "empty",
		"/":                       "names nothing",
		"a//b":                    "empty part",
		"/a":                      "empty part",
		"a ":                      "space character",
		"\xfe":                    "UTF-8",
		strings.Repeat("k", 1025): "1024",
		strings.Repeat("k", 256):  "limited to 255",
		strings.Repeat(":", 100):  "3 bytes each",
	} {
		_, err := objectPath("b", key)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("objectPath(%q) = %v; want an error mentioning %q", key, err, want)
		}
	}
}
