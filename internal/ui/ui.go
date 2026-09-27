// Package ui is the embedded web interface of whitepaper §12.1.1: static files
// compiled into opendrived and served at /ui.
//
// # Why this instead of a desktop application
//
// The roadmap called for native clients on macOS and Windows until v1.2 replaced
// them with this, and the reason was arithmetic rather than taste. A desktop client
// means a toolchain and a packaging story per platform, a signing certificate per
// platform, and almost certainly cgo — which would end the CGO_ENABLED=0
// cross-compilation the whole release matrix rests on. This is `go:embed` into a
// binary that already exists: no build chain, no signing, no cgo, no new release
// artefact, and it works on every platform including the Linux one nobody would have
// built a desktop client for.
//
// # The iron rule
//
// **The interface is another client of the Bridge REST API, level with odctl, and it
// never reaches past the daemon into the SDK.** §12.1.1 is unambiguous about it, and
// the reason is not architectural tidiness: a second path to OpenDrive would be a
// second place credentials are handled and a second set of error wordings. Six
// phases of work went into making the Bridge boundary the one place upstream's
// misleading answers are translated; a GUI that went around it would undo that in the
// one component a user actually looks at.
//
// This package therefore contains no OpenDrive code at all. It serves files. Every
// number on the screen arrives over HTTP from the same endpoints odctl uses, and
// every error shown is the `message` field of §4.5's envelope, displayed verbatim —
// rewording it here would be a third translation of something already translated
// once, and the one furthest from the code that knows what happened.
//
// # Nothing from the network
//
// No CDN, no fonts, no analytics, no source maps fetched on demand. A local service
// holding somebody's cloud credentials has no business making outbound requests
// because a page was opened, and a machine with no internet has no business having a
// broken interface. The chart is drawn on a canvas by hand for the same reason: a
// charting library would be either a vendored blob nobody reads or a script tag
// pointing somewhere else.
//
// A Content-Security-Policy enforces it at run time rather than by convention, and a
// test greps the assets for the shapes of a remote reference — because the convention
// is exactly the kind of thing that erodes when somebody needs a quick icon.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// Path is where the interface is served. It is also what the daemon prints at
// startup, so the two cannot drift.
const Path = "/ui"

//go:embed assets
var assets embed.FS

// Handler serves the interface, rooted so that /ui and /ui/ both work.
//
// The assets are served **without** requiring an API key, and that is deliberate:
// they are a page, not data. Every number the page displays comes from an API call
// that is authenticated normally, so an unauthenticated visitor to a non-loopback
// bridge gets an empty interface asking for a key — which is §12.1.1's design, and
// the only way to have somewhere to type the key in.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the directory is embedded at compile time. Answering rather
		// than panicking, because a broken interface should not take the daemon with
		// it — the API is what matters.
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "the embedded interface is unavailable in this build", http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		// Strip the mount point, and send /ui to /ui/ so relative asset paths in the
		// document resolve against the right directory.
		rest := strings.TrimPrefix(r.URL.Path, Path)
		if rest == "" {
			http.Redirect(w, r, Path+"/", http.StatusMovedPermanently)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		files.ServeHTTP(w, r2)
	})
}

// setSecurityHeaders is the "nothing from the network" rule, enforced.
//
// A comment saying "do not add a CDN" is a request. A Content-Security-Policy is the
// browser refusing. Both are here, because the policy also documents the intent to
// whoever opens the developer tools and wonders why their script tag did nothing.
func setSecurityHeaders(w http.ResponseWriter) {
	// 'self' everywhere, and no 'unsafe-inline' for scripts: the page's behaviour
	// lives in app.js, which keeps it reviewable and keeps an injected string from
	// becoming code. Styles do allow inline, because the chart sets a few pixel
	// dimensions from measurements and a nonce for that would be ceremony.
	//
	// connect-src is 'self' as well, which is the one that matters most here: even
	// if a script were somehow introduced, it could not send anything anywhere.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'self'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"font-src 'self'; "+
			"connect-src 'self'; "+
			"form-action 'none'; "+
			"base-uri 'none'; "+
			"frame-ancestors 'none'")
	// No referrer at all. The API key is never in a URL (§9.4), but a path can still
	// say which account somebody is looking at, and nothing here needs a referrer.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The interface is for the person running the bridge, not for embedding.
	w.Header().Set("X-Frame-Options", "DENY")
	// Nothing here needs a camera, a microphone or a location.
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// Files exposes the embedded filesystem for tests, so they can assert what is
// actually shipped rather than what is in the source tree.
func Files() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return assets
	}
	return sub
}
