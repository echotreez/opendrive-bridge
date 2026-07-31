package server

import (
	"strings"
	"testing"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The Bridge's own path layer is fuzzed separately from the SDK's because they
// are not the same code and do not have the same job. The SDK normalises paths
// it is about to send upstream; this one normalises paths that arrived over
// HTTP, from a caller the daemon does not control. It is the outer edge, so its
// properties are the ones that matter for §9.3.

func FuzzNormalisePath(f *testing.F) {
	for _, s := range []string{
		"/", "", ".", "..", "/..", "/a/../b", "//a//b//", "/a/./b",
		"/Documents/report.pdf", "/a\\b", strings.Repeat("/a", 400),
		"/\x00", "/a\x00b", "/日本語", "/a/..", "/..%2f..", "/....//",
		"/a/b/../../..", "/./././", "/ ", " ",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, p string) {
		got, err := normalisePath(p)
		if err != nil {
			// A refusal is a client mistake and must be reported as one. If this
			// ever produced a bare error the caller would get a 500 for a bad
			// path, which blames the wrong party.
			var e *RequestError
			if !errorsAsBridge(err, &e) {
				t.Fatalf("%q was refused with a %T rather than a bridge error: %v", p, err, err)
			}
			if e.HTTP < 400 || e.HTTP >= 500 {
				t.Fatalf("%q was refused with HTTP %d, which is not the caller's fault", p, e.HTTP)
			}
			assertUserReadable(t, e.Message)
			return
		}

		if !strings.HasPrefix(got, "/") {
			t.Fatalf("%q normalised to %q, which is not rooted", p, got)
		}
		if got != "/" && strings.HasSuffix(got, "/") {
			t.Fatalf("%q normalised to %q, which has a trailing slash", p, got)
		}
		for _, seg := range strings.Split(strings.TrimPrefix(got, "/"), "/") {
			if seg == ".." {
				t.Fatalf("%q normalised to %q, which can leave the account", p, got)
			}
			if seg == "." {
				t.Fatalf("%q normalised to %q, which still has a dot segment", p, got)
			}
		}

		// Idempotent, for the same reason as in the SDK: a path that has already
		// been through the cache must resolve to the same thing the second time.
		twice, err := normalisePath(got)
		if err != nil {
			t.Fatalf("%q normalised to %q, which then failed: %v", p, got, err)
		}
		if twice != got {
			t.Fatalf("normalising is not idempotent: %q -> %q -> %q", p, got, twice)
		}

		// splitPath and joinPath are inverses on anything normalisePath accepts.
		// The path cache invalidates subtrees by rebuilding paths this way, so a
		// pair that did not round trip would leave stale entries behind — which
		// would show up as a file that was just deleted still being listed.
		parent, name := splitPath(got)
		if got == "/" {
			if parent != "/" || name != "" {
				t.Fatalf("the root split into %q + %q", parent, name)
			}
			return
		}
		if rebuilt := joinPath(parent, name); rebuilt != got {
			t.Fatalf("%q split into %q + %q, which joins back as %q", got, parent, name, rebuilt)
		}
	})
}

// FuzzJoinPath checks the other direction: whatever joinPath builds must be
// something normalisePath accepts and splits back, or the cache and the resolver
// would disagree about what a path is.
//
// The directory is normalised first, on purpose. The first version of this
// target fed joinPath raw fuzzer output and duly reported that joining "0/" and
// "0" gives "0//0" — true, and about an input joinPath never receives, since
// every caller passes a path that normalisePath or splitPath produced. A
// property that only holds for inputs the function is not given is not a
// property of the function; it is a bug in the test, and asserting it would have
// bought a defensive branch nothing could reach.
func FuzzJoinPath(f *testing.F) {
	f.Add("/", "a")
	f.Add("/Documents", "report.pdf")
	f.Add("/a/b", "c")
	f.Add("/a", "b c")
	f.Add("/a", "..")

	f.Fuzz(func(t *testing.T, dir, name string) {
		parent, err := normalisePath(dir)
		if err != nil {
			return // not a directory this function is ever handed
		}
		// Nor is this a name it is ever handed. Every name reaching joinPath has
		// already been through ValidateName — the rename handlers validate
		// before calling upstream, and a listing entry is a name upstream
		// stored, which it would not have accepted either. The fuzzer duly
		// reported that joining "/" and "." gives "/.", which normalises to the
		// parent; true, and about an input the function cannot receive.
		if opendrive.ValidateName(name) != nil {
			return
		}
		got := joinPath(parent, name)
		if got == "" {
			t.Fatalf("joining %q and %q produced an empty path", parent, name)
		}

		// If the result is a path the bridge would accept, it must survive the
		// round trip back to the pieces it was built from.
		clean, err := normalisePath(got)
		if err != nil {
			return // a name that cannot appear in a path; the caller is told so
		}
		if clean != got {
			t.Fatalf("joining %q and %q produced %q, which normalises to %q",
				parent, name, got, clean)
		}
		// The round trip is over canonical names, not over the exact bytes that
		// went in: joinPath trims the name, because upstream does (D46) and a
		// path built from " report" would name a folder that cannot exist. So
		// what must come back is the name upstream would have stored.
		back, backName := splitPath(got)
		if want := strings.TrimSpace(name); back != parent || backName != want {
			t.Fatalf("%q + %q joined to %q, which splits back to %q + %q, want %q + %q",
				parent, name, got, back, backName, parent, want)
		}
	})
}

func errorsAsBridge(err error, target **RequestError) bool {
	if e, ok := err.(*RequestError); ok {
		*target = e
		return true
	}
	return false
}
