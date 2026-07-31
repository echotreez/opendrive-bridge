package opendrive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzzing here is aimed at the two layers that read input the project does not
// control: whatever upstream puts in a response body, and whatever a caller puts
// in a path. Everything else in the SDK is reached through one of those two.
//
// "No panic" is the floor. A panic in the decode layer is a denial of service
// reachable by an upstream that answers oddly for a minute, which D39 and D40
// show it does. The properties asserted below are the part worth more than the
// floor: they say what must still be true for any input at all, including the
// ones nobody thought of.

// seedFromFixtures adds every recorded response as a starting point. Fuzzing
// from real shapes finds more than fuzzing from "{}" because the mutator starts
// somewhere structurally interesting — numbers upstream sends as strings, empty
// strings where a number belongs (D19), and so on.
func seedFromFixtures(f *testing.F) {
	root := filepath.Join("..", "..", "testdata", "fixtures")
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil //nolint:nilerr // a missing fixture tree is not a test failure
		}
		if b, err := os.ReadFile(p); err == nil { // #nosec G304 -- test data, path from Walk
			f.Add(b)
		}
		return nil
	})
}

// FuzzDecodeUpstreamJSON pushes arbitrary bytes through every response type the
// SDK decodes into. json.Unmarshal is safe on its own; the custom
// UnmarshalJSON methods on the Flex types are what this is really testing,
// because they exist precisely to accept the shapes upstream sends that a plain
// decoder rejects.
func FuzzDecodeUpstreamJSON(f *testing.F) {
	seedFromFixtures(f)
	f.Add([]byte(`{"FolderID":"1","Name":"x","DateModified":"1785122523"}`))
	f.Add([]byte(`{"Access":"0","Encrypted":1,"BWExceeded":"true"}`))
	f.Add([]byte(`{"DirUpdateTime":-1,"Size":"999999999999999999999"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Each of these is a type a live response is decoded into somewhere.
		targets := []any{
			new(FolderListing), new(FolderEntry), new(FileEntry),
			new(SessionLogin), new(SessionInfo), new(UserInfo),
			new(CaptchaStatus), new(Pagination),
			new(FlexBool), new(StringBool), new(FlexInt), new(FlexString),
			new(UnixTime), new(BoolResult),
		}
		for _, out := range targets {
			// The contract is only that it must not panic. Malformed input is
			// expected to produce an error, and an error is a fine outcome.
			_ = json.Unmarshal(data, out)
		}
	})
}

// FuzzFlexRoundTrip checks the one thing the Flex types owe beyond not
// panicking: a value that decodes must re-encode and decode again to the same
// value. Without this a mutation could quietly change on its way through, which
// is worse than an error because nothing reports it.
func FuzzFlexRoundTrip(f *testing.F) {
	for _, s := range []string{`"1"`, `1`, `"true"`, `true`, `""`, `"0"`, `null`, `"-12"`, `"1785122523"`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var i FlexInt
		if err := json.Unmarshal(data, &i); err != nil {
			return
		}
		encoded, err := json.Marshal(i)
		if err != nil {
			t.Fatalf("a decoded FlexInt would not encode: %v", err)
		}
		var again FlexInt
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("re-decoding %s failed: %v", encoded, err)
		}
		if again != i {
			t.Fatalf("FlexInt changed on the way round: %d -> %s -> %d", i, encoded, again)
		}
	})
}

// FuzzClassifyResponse feeds arbitrary bodies to the classification layer and
// asserts the invariant that CLAUDE.md rule 3 calls non-negotiable: a body that
// is not API-shaped can never produce a credential verdict, and nothing
// ambiguous or non-API-shaped may drive the auth state machine.
//
// That rule exists because of D38 — an HTML 401 from a proxy that had nothing to
// do with the token. A fuzzer is the right tool for it: the rule has to hold for
// every body upstream could ever return, not just the ones already recorded.
func FuzzClassifyResponse(f *testing.F) {
	// No seedFromFixtures here: this target takes three values, and a corpus
	// entry has to match the signature.
	f.Add(401, []byte(`<html><head><title>401 Authorization Required</title></head></html>`), "text/html")
	f.Add(403, []byte(`{"error":{"code":403,"message":"You don't have permission"}}`), "application/json")
	f.Add(200, []byte(`{"error":{"code":401,"message":"expired"}}`), "application/json")
	f.Add(429, []byte(``), "")
	f.Add(500, []byte(`Internal Server Error`), "text/plain")

	f.Fuzz(func(t *testing.T, status int, body []byte, contentType string) {
		// Statuses outside the HTTP range are not what this is about.
		if status < 100 || status > 599 {
			return
		}
		ev := Evidence{
			Status:      status,
			Body:        body,
			ContentType: contentType,
			Op:          "GET /folder/list.json",
			Path:        "/folder/list.json",
			URL:         "https://dev.opendrive.com/api/v1/folder/list.json",
		}
		err := classifyResponse(ev)
		if err == nil {
			return
		}

		shape := err.BodyShape()
		credential := err.Kind == KindKeystoreUnavailable ||
			err.Kind == KindReauthRequired ||
			err.Kind == KindTokenExpired
		if shape != ShapeJSON && credential {
			t.Fatalf("a %s body produced the credential verdict %v:\nbody: %q\ncontent-type: %q",
				shape, err.Kind, body, contentType)
		}
		if !err.drivesAuth() {
			return
		}
		// ShapeUnset means the SDK built the error itself rather than reading it
		// off the wire, which classifyResponse never does.
		if shape != ShapeJSON {
			t.Fatalf("a %s body was allowed to drive the auth state machine:\nbody: %q", shape, body)
		}
		if err.Ambiguous() {
			t.Fatalf("an ambiguous verdict was allowed to drive the auth state machine:\nbody: %q", body)
		}
	})
}

// FuzzErrorInBody covers the 200-with-an-error-envelope path, which reads a body
// upstream chose while the status says everything is fine.
func FuzzErrorInBody(f *testing.F) {
	seedFromFixtures(f)
	f.Add([]byte(`{"error":{"code":403,"message":"no"}}`))
	f.Add([]byte(`{"error":null}`))
	f.Add([]byte(`{"error":""}`))
	f.Add([]byte(`{"error":[]}`))
	f.Add([]byte(`{`))

	f.Fuzz(func(t *testing.T, data []byte) {
		err := errorInBody(data, "GET /folder/list.json", "/folder/list.json", "", "https://x/y")
		if err == nil {
			return
		}
		// Whatever it decides, the message must be safe to show and to log: the
		// body it came from is upstream's, and §9.4 does not stop applying
		// because the input was strange.
		if strings.Contains(err.Error(), "access_token=") {
			t.Fatalf("a credential survived into the error text: %q", err.Error())
		}
	})
}

// FuzzNormalizeFolderPath is the security-relevant one. §9.3 says a caller must
// never escape the account namespace, so the properties are stated as
// consequences of that rather than as "it returns something".
func FuzzNormalizeFolderPath(f *testing.F) {
	for _, s := range []string{
		"/", "", ".", "..", "/..", "/a/../b", "//a//b//", "/a/./b", " /a ",
		"/a\\b", "/a b/c", strings.Repeat("/a", 500), "/\x00", "/a\x00b",
		"/日本語/ファイル", "/a/..\x2f..", "/....//", "/.hidden",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		got, err := NormalizeFolderPath(p)
		if err != nil {
			// A refusal must still say nothing surprising, and must be a client
			// mistake rather than something a caller could mistake for an
			// upstream failure.
			var api *APIError
			if !asAPIError(err, &api) {
				t.Fatalf("%q was refused with a %T rather than an APIError: %v", p, err, err)
			}
			return
		}

		if !strings.HasPrefix(got, "/") {
			t.Fatalf("%q normalised to %q, which is not rooted", p, got)
		}
		if got != "/" && strings.HasSuffix(got, "/") {
			t.Fatalf("%q normalised to %q, which has a trailing slash", p, got)
		}
		if strings.Contains(got, "\x00") {
			t.Fatalf("%q normalised to %q, which carries a null byte", p, got)
		}
		for _, seg := range strings.Split(strings.TrimPrefix(got, "/"), "/") {
			if seg == ".." {
				t.Fatalf("%q normalised to %q, which can leave the account", p, got)
			}
			if seg == "" && got != "/" {
				t.Fatalf("%q normalised to %q, which has an empty segment", p, got)
			}
			if seg == "." {
				t.Fatalf("%q normalised to %q, which still has a dot segment", p, got)
			}
		}

		// Normalising is idempotent. If it were not, a path that had already
		// been through the cache could resolve to something different the
		// second time, which is the shape of bug that only appears in
		// production.
		twice, err := NormalizeFolderPath(got)
		if err != nil {
			t.Fatalf("%q normalised to %q, which then failed: %v", p, got, err)
		}
		if twice != got {
			t.Fatalf("normalising is not idempotent: %q -> %q -> %q", p, got, twice)
		}

		// The parent of a normalised path is itself normalised, and joining it
		// back to the name reproduces the original.
		parent, name := ParentPath(got)
		if got != "/" {
			if rebuilt := strings.TrimSuffix(parent, "/") + "/" + name; rebuilt != got {
				t.Fatalf("%q split into %q + %q, which rebuilds as %q", got, parent, name, rebuilt)
			}
		}
	})
}

// asAPIError is errors.As without importing errors into every assertion.
func asAPIError(err error, target **APIError) bool {
	if e, ok := err.(*APIError); ok {
		*target = e
		return true
	}
	return false
}
