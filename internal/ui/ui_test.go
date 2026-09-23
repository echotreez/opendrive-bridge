package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests are mostly about what the interface does *not* do.
//
// A status page is easy to test for "does it render" and that is not where the risk
// is. The risks §12.1.1 names are a page that fetches something from the internet, a
// page that puts an API key somewhere it will be logged, and a page that reaches past
// the daemon. None of the three is visible by looking at it, and all three are the
// kind of thing that arrives later as a small convenient change — so they are
// asserted here rather than left to a comment.

func assetText(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(Files(), name)
	if err != nil {
		t.Fatalf("cannot read the embedded %s: %v", name, err)
	}
	return string(data)
}

func allAssets(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(Files(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		out[path] = assetText(t, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no assets were embedded, so nothing below proves anything")
	}
	return out
}

// ---------------------------------------------------------------- serving

func TestTheInterfaceIsServed(t *testing.T) {
	h := Handler()

	// /ui redirects to /ui/ so that relative asset paths resolve.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET %s = %d, want a redirect to %s/", Path, rec.Code, Path)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s/ = %d", Path, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>OpenDrive Bridge</title>") {
		t.Errorf("the page does not look like the interface:\n%s", body[:min(400, len(body))])
	}

	// And the assets it references.
	for _, name := range []string{"app.js", "style.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s/%s = %d", Path, name, rec.Code)
		}
	}
}

func TestSomethingThatIsNotThereIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/nope.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing asset = %d, want 404", rec.Code)
	}
}

// The five panels of §12.1.1 are all present. A list in a document is not a
// deliverable; the markup is.
func TestAllFivePanelsArePresent(t *testing.T) {
	html := assetText(t, "index.html")
	for _, panel := range []struct{ id, what string }{
		{"panel-status", "service status"},
		{"panel-auth", "authentication"},
		{"panel-rate", "the transfer rate curve"},
		{"panel-jobs", "job monitoring"},
		{"panel-cache", "the cache"},
	} {
		if !strings.Contains(html, `id="`+panel.id+`"`) {
			t.Errorf("there is no panel for %s (expected id %q)", panel.what, panel.id)
		}
	}
}

// ---------------------------------------------------------------- nothing remote

// No CDN, no fonts, no analytics. §12.1.1: a local service holding somebody's cloud
// credentials should not make outbound requests because a page was opened, and a
// machine with no internet should not have a broken interface.
//
// Greps for the shapes rather than for a list of known hosts, because the next one
// will be a host nobody listed.
func TestNothingIsFetchedFromTheNetwork(t *testing.T) {
	remote := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)

	for name, text := range allAssets(t) {
		for _, m := range remote.FindAllString(text, -1) {
			// A link the user may click is not a fetch. The document links to
			// SECURITY.md on GitHub, which is a destination rather than a resource
			// the page loads, so only attributes that *load* something are a problem.
			if isLinkHref(text, m) {
				continue
			}
			t.Errorf("%s refers to %q, which would be fetched from the network", name, m)
		}
	}
}

// isLinkHref reports whether a URL appears only as an <a href>, which a browser
// follows when a person clicks it and never loads on its own.
func isLinkHref(text, url string) bool {
	anchor := regexp.MustCompile(`<a\s[^>]*href="` + regexp.QuoteMeta(url))
	// Every occurrence has to be inside an anchor; one that is not is a fetch.
	occurrences := strings.Count(text, url)
	return len(anchor.FindAllString(text, -1)) >= occurrences
}

// The tags that load a resource must all point at something relative.
func TestEveryLoadedResourceIsLocal(t *testing.T) {
	html := assetText(t, "index.html")
	loaders := regexp.MustCompile(`(?i)<(script|link|img|iframe|source|video|audio)\s[^>]*>`)
	attr := regexp.MustCompile(`(?i)\b(src|href)="([^"]*)"`)

	for _, tag := range loaders.FindAllString(html, -1) {
		if strings.HasPrefix(strings.ToLower(tag), "<link") && !strings.Contains(tag, "stylesheet") {
			continue // a <link rel="icon"> or similar is still checked below
		}
		for _, m := range attr.FindAllStringSubmatch(tag, -1) {
			value := m[2]
			if strings.HasPrefix(value, "//") || strings.Contains(value, "://") {
				t.Errorf("%s loads %q from somewhere else", tag, value)
			}
		}
	}
}

// A Content-Security-Policy makes it the browser's rule rather than a convention,
// and the convention is exactly the sort of thing that erodes when somebody wants a
// quick icon.
func TestTheContentSecurityPolicyForbidsOutsideSources(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path+"/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("there is no Content-Security-Policy")
	}
	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		// The one that matters most: even an injected script could not send anything
		// anywhere.
		"connect-src 'self'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy does not contain %q: %s", want, csp)
		}
	}
	// Inline script would make an injected string executable.
	if strings.Contains(csp, "script-src") && strings.Contains(csp, "'unsafe-inline' 'self'") {
		t.Error("the policy allows inline script")
	}

	for header, want := range map[string]string{
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// ---------------------------------------------------------------- the key

// sessionStorage, not localStorage, and never in a URL.
//
// §12.1.1 asks for sessionStorage by name: it is gone when the tab closes, which for
// a key that opens somebody's cloud storage is the right lifetime. A URL is out of
// the question — history, Referer, and every log in between — which is the whole of
// what §9.4 exists to prevent.
func TestTheAPIKeyIsKeptForTheTabAndNeverInAURL(t *testing.T) {
	js := assetText(t, "app.js")

	if !strings.Contains(js, "sessionStorage") {
		t.Error("the key is not kept in sessionStorage")
	}
	// Usage, not the word: the file explains in a comment why localStorage is the
	// wrong choice, and a test that forbade mentioning it would forbid the
	// explanation along with the mistake.
	for _, use := range []string{"localStorage.", "localStorage["} {
		if strings.Contains(js, use) {
			t.Errorf("app.js uses %s; a key that outlives the tab is not what §12.1.1 asked for", use)
		}
	}

	// The key travels as a header. Anything that looked like putting it in a query
	// string is a defect, so the shapes are checked rather than the intent.
	for _, bad := range []string{
		"api_key=", "apikey=", "?key=", "&key=", "token=",
		"searchParams.set", "encodeURIComponent(key",
	} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js contains %q, which looks like putting a credential in a URL", bad)
		}
	}
	if !strings.Contains(js, "'Authorization'") && !strings.Contains(js, `"Authorization"`) {
		t.Error("the key is not sent as an Authorization header")
	}
}

// ---------------------------------------------------------------- the iron rule

// The interface is a REST client and nothing else. §12.1.1: it must not bypass the
// daemon, because a second path to OpenDrive is a second place credentials are
// handled and a second set of error wordings.
//
// In Go terms that means this package imports nothing from pkg/opendrive, and in
// browser terms it means every request goes to this daemon's own API.
func TestTheInterfaceOnlyTalksToTheBridge(t *testing.T) {
	// No SDK in the package. Checked against the source rather than the embedded
	// assets, because this is about what the Go side could reach.
	src := goSourceOfThisPackage(t)
	for _, forbidden := range []string{
		"pkg/opendrive",
		"internal/keystore",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("internal/ui imports %s; the interface must go through the REST API "+
				"like any other client", forbidden)
		}
	}

	// And every fetch in the browser is a relative path under /v1.
	js := assetText(t, "app.js")
	fetches := regexp.MustCompile(`fetch\(([^,)]+)`)
	if len(fetches.FindAllString(js, -1)) == 0 {
		t.Fatal("no fetch call was found, so this test is not checking anything")
	}
	paths := regexp.MustCompile(`call\('(?:GET|POST|DELETE|PUT)',\s*'([^']+)'`)
	found := paths.FindAllStringSubmatch(js, -1)
	if len(found) == 0 {
		t.Fatal("no API calls were found, so this test is not checking anything")
	}
	for _, m := range found {
		p := m[1]
		if !strings.HasPrefix(p, "/v1/") {
			t.Errorf("the interface calls %q, which is not this bridge's API", p)
		}
	}
}

func goSourceOfThisPackage(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("cannot read ui.go: %v", err)
	}
	return string(data)
}

// ---------------------------------------------------------------- the verdict

// The cache panel's shutdown verdict comes from the daemon, and the page must not
// compute its own. §3.5.2 rule 3 puts the answer in one place precisely so that the
// web page, odctl and a script cannot reach three different conclusions from the same
// numbers.
func TestTheShutdownVerdictComesFromTheDaemon(t *testing.T) {
	js := assetText(t, "app.js")
	if !strings.Contains(js, "safe_to_shut_down") {
		t.Error("the page does not read safe_to_shut_down; it must not derive its own answer")
	}
	// The wording has to be unambiguous in the unsafe direction, because that is the
	// case where being vague hurts somebody.
	if !strings.Contains(js, "NOT safe to stop the bridge") {
		t.Error("the unsafe verdict is not stated plainly")
	}
	// §8.3.1's container warning is shown in full rather than summarised.
	if !strings.Contains(js, "durability_note") {
		t.Error("the durability warning from the daemon is not displayed")
	}

	html := assetText(t, "index.html")
	if !strings.Contains(html, `id="cache-verdict"`) {
		t.Error("there is no element for the verdict")
	}
}

// The rate chart does not invent a peak. Floored arithmetic to avoid dividing by
// zero once produced "peak 1.0 B/s" on an idle bridge — a figure describing a
// transfer that never happened, which is the same class of thing this project spends
// its time catching upstream. Found by looking at the page rather than by a test,
// which is why there is now a test.
func TestTheRateChartSaysNothingWhenNothingHasMoved(t *testing.T) {
	js := assetText(t, "app.js")
	if !strings.Contains(js, "no transfers yet") {
		t.Error("an idle chart does not say it is idle")
	}
	if !strings.Contains(js, "rate.some(") {
		t.Error("the chart does not check whether anything actually moved before drawing a peak")
	}
}

// Errors are the daemon's own words, inserted as text rather than as markup. §4.5's
// message field is already written for a person, and it may quote a filename somebody
// else chose.
func TestErrorsAreShownVerbatimAndAsText(t *testing.T) {
	js := assetText(t, "app.js")
	if !strings.Contains(js, "parsed.error.message") {
		t.Error("the daemon's message field is not the source of the error text")
	}
	if !strings.Contains(js, "line.textContent = err.message") {
		t.Error("the error is not inserted as text; markup from a message would be executable")
	}
	// Usage, not the word — the same distinction as the localStorage check above.
	// The file says in a comment why textContent is used instead, and a test that
	// banned the word would ban the explanation with it. This is the second time
	// that caught me, which is why both now check for a property access.
	for _, use := range []string{".innerHTML", "insertAdjacentHTML", "outerHTML"} {
		if strings.Contains(js, use) {
			t.Errorf("app.js uses %s; every value it displays comes from outside this file", use)
		}
	}
}
