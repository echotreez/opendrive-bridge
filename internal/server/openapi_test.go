package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The consistency check between docs/bridge-openapi.yaml and the routes the
// daemon actually serves.
//
// It exists because a specification nobody verifies is worse than none: it is
// believed. Once this runs in CI, the two cannot drift apart silently — adding a
// route without documenting it, or documenting one that was renamed, fails the
// build rather than waiting to be noticed while somebody writes deployment docs.
//
// The YAML is read with a small hand-rolled reader rather than a dependency.
// What is needed is the path/method/status skeleton, which is two levels of
// indentation, and the whitepaper's §3.3 asks for the dependency list to stay
// short.

const specPath = "../../docs/bridge-openapi.yaml"

// specOperation is one documented method on one path.
type specOperation struct {
	path     string
	method   string
	statuses map[int]bool
}

func loadSpec(t *testing.T) map[string]*specOperation {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("cannot read the OpenAPI spec: %v", err)
	}

	ops := map[string]*specOperation{}
	var (
		inPaths     bool
		currentPath string
		current     *specOperation
	)
	methods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "head": true, "options": true,
	}

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)

		switch {
		case indent == 0:
			inPaths = trimmed == "paths:"
			currentPath, current = "", nil

		case !inPaths:
			// not our business

		case indent == 2 && strings.HasPrefix(trimmed, "/"):
			currentPath = strings.TrimSuffix(trimmed, ":")
			current = nil

		case indent == 4 && currentPath != "":
			name := strings.TrimSuffix(trimmed, ":")
			if methods[name] {
				current = &specOperation{path: currentPath, method: strings.ToUpper(name),
					statuses: map[int]bool{}}
				ops[current.method+" "+current.path] = current
			} else {
				current = nil
			}

		case indent >= 8 && current != nil:
			// A status line looks like:  "200": { $ref: ... }  or  "200":
			if !strings.HasPrefix(trimmed, `"`) {
				continue
			}
			end := strings.Index(trimmed[1:], `"`)
			if end < 0 {
				continue
			}
			code, convErr := strconv.Atoi(trimmed[1 : end+1])
			if convErr != nil {
				continue
			}
			// Only the responses block declares three-digit keys at this depth.
			if code >= 100 && code < 600 {
				current.statuses[code] = true
			}
		}
	}

	if len(ops) == 0 {
		t.Fatal("no operations were read from the spec; the reader is broken")
	}
	return ops
}

// routeOperation is one method on one path the router actually serves.
type routeOperation struct {
	path   string
	method string
}

func liveRoutes(t *testing.T) map[string]routeOperation {
	t.Helper()
	srv := newTestServer(t, &fakeAuth{})

	out := map[string]routeOperation{}
	err := chi.Walk(srv.Handler().(chi.Router),
		func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			route = strings.TrimSuffix(route, "/")
			if route == "" || !strings.HasPrefix(route, "/v1") {
				return nil
			}
			out[method+" "+route] = routeOperation{path: route, method: method}
			return nil
		})
	if err != nil {
		t.Fatalf("cannot walk the routes: %v", err)
	}
	return out
}

// Neither side may have anything the other lacks.
func TestOpenAPIMatchesTheRoutes(t *testing.T) {
	spec := loadSpec(t)
	routes := liveRoutes(t)

	var undocumented, unimplemented []string
	for key := range routes {
		if _, ok := spec[key]; !ok {
			undocumented = append(undocumented, key)
		}
	}
	for key := range spec {
		if _, ok := routes[key]; !ok {
			unimplemented = append(unimplemented, key)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unimplemented)

	if len(undocumented) > 0 {
		t.Errorf("the daemon serves %d route(s) the spec does not document:\n  %s",
			len(undocumented), strings.Join(undocumented, "\n  "))
	}
	if len(unimplemented) > 0 {
		t.Errorf("the spec documents %d operation(s) the daemon does not serve:\n  %s",
			len(unimplemented), strings.Join(unimplemented, "\n  "))
	}
	t.Logf("%d operations, spec and router agree", len(routes))
}

// Every operation has to document the failure envelope, not just the happy
// path. A client that only knows the 200 shape will parse an error as a
// success, which is the failure this whole layer exists to prevent.
func TestEveryOperationDocumentsItsErrors(t *testing.T) {
	spec := loadSpec(t)
	for key, op := range spec {
		if key == "GET /v1/health" {
			continue // liveness has nothing to go wrong that is worth a schema
		}
		var hasSuccess, hasFailure bool
		for code := range op.statuses {
			switch {
			case code >= 200 && code < 300:
				hasSuccess = true
			case code >= 400:
				hasFailure = true
			}
		}
		if !hasSuccess {
			t.Errorf("%s documents no success status", key)
		}
		if !hasFailure {
			t.Errorf("%s documents no failure status; a client would parse an error as a success", key)
		}
	}
}

// The statuses the error mapping can actually produce must all be documented
// somewhere, or a client will meet one it has never heard of.
func TestEveryMappedStatusAppearsInTheSpec(t *testing.T) {
	spec := loadSpec(t)
	documented := map[int]bool{}
	for _, op := range spec {
		for code := range op.statuses {
			documented[code] = true
		}
	}

	for kind := range messages {
		status := statusFor(kind)
		if !documented[status] {
			t.Errorf("the error mapping can answer %d (for %s) but no operation documents it",
				status, kind)
		}
	}
}

// A stale enum is the quietest kind of drift: the code keeps working and the
// spec keeps looking right.
func TestTheSpecEnumeratesEveryErrorCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for kind := range messages {
		if !strings.Contains(text, "- "+string(kind)) {
			t.Errorf("error code %q is missing from the spec's enum", kind)
		}
	}
}

// The spec is the daemon's own documentation, so its promises about the two
// behaviours most likely to be misread should be there in words.
func TestTheSpecExplainsTheTrapsItProtectsAgainst(t *testing.T) {
	data, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace is collapsed first: YAML wraps prose across lines, and a check
	// that breaks when a sentence is re-wrapped would train people to delete it.
	text := strings.Join(strings.Fields(strings.ToLower(string(data))), " ")
	for _, promise := range []string{
		"dir_update_time",    // D7 paging cannot be guessed
		"info.json",          // D27: stat does not use it
		"containing nothing", // D42: the archive parameter that is not offered
		"megabytes",          // D45: the quota units
	} {
		if !strings.Contains(text, promise) {
			t.Errorf("the spec does not mention %q, which callers need to know about", promise)
		}
	}
}

func TestSpecReaderFindsTheExpectedShape(t *testing.T) {
	spec := loadSpec(t)
	// A few anchors, so a broken reader cannot make the comparison vacuous.
	for _, key := range []string{
		"GET /v1/auth/status", "POST /v1/auth/login", "GET /v1/ls", "GET /v1/stat",
		"POST /v1/upload", "PUT /v1/upload/stream", "GET /v1/download/stream",
		"GET /v1/jobs", "DELETE /v1/jobs/{id}", "DELETE /v1/share",
	} {
		if _, ok := spec[key]; !ok {
			t.Errorf("%s missing from the parsed spec", key)
		}
	}
	if got := spec["GET /v1/ls"]; got != nil && !got.statuses[200] {
		t.Errorf("the reader found no 200 for GET /v1/ls: %v", got.statuses)
	}
}

// readSpec and specSection let other tests assert against the document without
// re-reading it by hand.
func readSpec() (string, error) {
	data, err := os.ReadFile(filepath.Clean(specPath))
	return string(data), err
}

// specSection returns the block belonging to one top-level path key.
func specSection(spec, key string) string {
	start := strings.Index(spec, "\n  "+key)
	if start < 0 {
		return ""
	}
	rest := spec[start+1:]
	for i := 1; i < len(rest); i++ {
		if strings.HasPrefix(rest[i:], "\n  /") {
			return rest[:i]
		}
	}
	return rest
}
