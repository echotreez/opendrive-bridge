package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretSession = "SESSION-1234567890abcdef"

// newSpecServer emulates the Restler explorer: anonymous callers only see the
// public endpoints, authenticated callers see everything (whitepaper §2.1).
func newSpecServer(t *testing.T) *httptest.Server {
	t.Helper()
	authed := func(r *http.Request) bool {
		return r.URL.Query().Get("session_id") == secretSession ||
			strings.HasSuffix(r.URL.Path, "/"+secretSession)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/resources.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if authed(r) {
			_, _ = w.Write([]byte(`{"apiVersion":"1","swaggerVersion":"1.1","apis":[{"path":"/session"},{"path":"/file"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"1","swaggerVersion":"1.1","apis":[{"path":"/session"}]}`))
	})
	mux.HandleFunc("/api/v1/resources/session.json", func(w http.ResponseWriter, r *http.Request) {
		if authed(r) {
			// Session id echoed back in the body: must be redacted on archive.
			_, _ = w.Write([]byte(`{"apis":[{"path":"/session/login.json","operations":[{"httpMethod":"POST"}]},` +
				`{"path":"/session/info.json/` + secretSession + `","operations":[{"httpMethod":"GET"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"apis":[{"path":"/session/login.json","operations":[{"httpMethod":"POST"}]}]}`))
	})
	mux.HandleFunc("/api/v1/resources/file.json", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, `{"error":{"code":403}}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"apis":[{"path":"/file/info.json","operations":[{"httpMethod":"GET"},{"httpMethod":"PUT"}]}]}`))
	})
	mux.HandleFunc("/api/v1/session/login.json", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["username"] != "user" || body["passwd"] != "pass" {
			http.Error(w, `{"error":{"code":401,"message":"Invalid username or password"}}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"SessionID":"` + secretSession + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchAllAnonymousSeesPublicSubsetOnly(t *testing.T) {
	srv := newSpecServer(t)
	a, err := fetchAll(context.Background(), srv.Client(), srv.URL+"/api/v1", strategy{name: "anonymous"})
	if err != nil {
		t.Fatalf("fetchAll: %v", err)
	}
	if got, want := len(a.Files), 2; got != want { // resources.json + session.json
		t.Fatalf("files = %d, want %d", got, want)
	}
	if a.Operations != 1 {
		t.Fatalf("operations = %d, want 1", a.Operations)
	}
}

func TestFetchAllAuthenticatedSeesMore(t *testing.T) {
	srv := newSpecServer(t)
	s := strategy{name: "query-session_id", session: secretSession,
		query: map[string]string{"session_id": secretSession}}
	a, err := fetchAll(context.Background(), srv.Client(), srv.URL+"/api/v1", s)
	if err != nil {
		t.Fatalf("fetchAll: %v", err)
	}
	if a.Operations != 4 { // session: 2, file: 2
		t.Fatalf("operations = %d, want 4", a.Operations)
	}
	if a.Endpoints != 3 {
		t.Fatalf("endpoints = %d, want 3", a.Endpoints)
	}
}

// §9.4: no session id may survive into the archive on disk.
func TestArchiveRedactsSessionID(t *testing.T) {
	srv := newSpecServer(t)
	s := strategy{name: "query-session_id", session: secretSession,
		query: map[string]string{"session_id": secretSession}}
	a, err := fetchAll(context.Background(), srv.Client(), srv.URL+"/api/v1", s)
	if err != nil {
		t.Fatalf("fetchAll: %v", err)
	}
	dir := t.TempDir()
	if err := a.write(dir); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 { // resources.json, session.json, file.json, manifest.json
		t.Fatalf("archived %d files, want 4", len(entries))
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secretSession) {
			t.Fatalf("%s leaks the session id", e.Name())
		}
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got archive
	if err := json.Unmarshal(manifest, &got); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(got.Files) != 3 || got.Files[0].SHA256 == "" {
		t.Fatalf("manifest missing checksums: %+v", got.Files)
	}
}

// The live listing advertises modules as "/resources/file.{format}" (Restler),
// which must not be requested verbatim (whitepaper §2.6 #1: trust the wire).
func TestModuleName(t *testing.T) {
	cases := map[string]string{
		"/resources/file.{format}": "file",
		"/resources/oauth2.json":   "oauth2",
		"/session":                 "session",
		"/resources/a/b.{format}":  "",
		"/":                        "",
	}
	for in, want := range cases {
		if got := moduleName(in); got != want {
			t.Errorf("moduleName(%q) = %q, want %q", in, got, want)
		}
	}
}

// An unreadable module is a documented gap, not a fatal error: the rest of the
// archive still has to land on disk.
func TestUnreadableModuleIsRecordedNotFatal(t *testing.T) {
	srv := newSpecServer(t)
	// Anonymous callers get a 403 on /resources/file.json, but the listing we
	// force here still advertises it.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/resources.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"apis":[{"path":"/resources/session.{format}"},{"path":"/resources/file.{format}"}]}`))
	})
	mux.HandleFunc("/api/v1/resources/session.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"apis":[{"path":"/v1/session/login.json","operations":[{"httpMethod":"POST"}]}]}`))
	})
	mux.HandleFunc("/api/v1/resources/file.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":403}}`, http.StatusForbidden)
	})
	gated := httptest.NewServer(mux)
	defer gated.Close()
	_ = srv

	a, err := fetchAll(context.Background(), gated.Client(), gated.URL+"/api/v1", strategy{name: "anonymous"})
	if err != nil {
		t.Fatalf("fetchAll: %v", err)
	}
	if len(a.Missing) != 1 || !strings.HasPrefix(a.Missing[0], "file:") {
		t.Fatalf("missing = %v, want one entry for file", a.Missing)
	}
	if a.Operations != 1 {
		t.Fatalf("operations = %d, want 1", a.Operations)
	}
}

func TestLoginRejectsBadCredentialsWithoutEchoingThem(t *testing.T) {
	srv := newSpecServer(t)
	_, err := login(context.Background(), srv.Client(), srv.URL+"/api/v1", "user", "hunter2")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error message leaks the password: %v", err)
	}
}

func TestLoginReturnsSessionID(t *testing.T) {
	srv := newSpecServer(t)
	got, err := login(context.Background(), srv.Client(), srv.URL+"/api/v1", "user", "pass")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if got != secretSession {
		t.Fatalf("session = %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ in, wantAbsent, wantPresent string }{
		{"https://x/api/v1/file/info.json?session_id=OAUTH&access_token=tok123", "tok123", "session_id=OAUTH"},
		{"https://x/api/v1/session/info.json?session_id=abc123", "abc123", "REDACTED"},
	}
	for _, c := range cases {
		got := redactURL(c.in)
		if strings.Contains(got, c.wantAbsent) {
			t.Errorf("redactURL(%q) = %q, still contains %q", c.in, got, c.wantAbsent)
		}
		if !strings.Contains(got, c.wantPresent) {
			t.Errorf("redactURL(%q) = %q, want it to contain %q", c.in, got, c.wantPresent)
		}
	}
	if got := redactURL("://bad url"); got != "<unparseable url>" {
		t.Errorf("redactURL(bad) = %q", got)
	}
}

func TestRedactSecret(t *testing.T) {
	if got := redactSecret("abc"); got != "***" {
		t.Errorf("short secret = %q", got)
	}
	got := redactSecret("abcdefghij")
	if strings.Contains(got, "cdefgh") || !strings.HasPrefix(got, "ab") || !strings.HasSuffix(got, "ij") {
		t.Errorf("redactSecret = %q", got)
	}
}

func TestTruncateAndEnvOr(t *testing.T) {
	if got := truncate("  hello  ", 10); got != "hello" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate = %q", got)
	}
	if got := envOr("ODB_SPEC_TEST_UNSET_KEY", "fallback"); got != "fallback" {
		t.Errorf("envOr = %q", got)
	}
	t.Setenv("ODB_SPEC_TEST_KEY", "set")
	if got := envOr("ODB_SPEC_TEST_KEY", "fallback"); got != "set" {
		t.Errorf("envOr = %q", got)
	}
}

func TestFetchJSONRejectsNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>login page</html>"))
	}))
	defer srv.Close()
	_, err := fetchJSON(context.Background(), srv.Client(), srv.URL+"/resources.json", strategy{})
	if err == nil || !strings.Contains(err.Error(), "non-JSON") {
		t.Fatalf("err = %v, want a non-JSON complaint", err)
	}
}
