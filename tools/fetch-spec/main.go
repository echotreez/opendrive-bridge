// Command fetch-spec logs into the OpenDrive API and downloads the full
// Swagger 1.1 specification set from https://dev.opendrive.com/api/v1/resources/*.json,
// archiving it under testdata/spec/ as the baseline for contract tests and for
// upstream API drift monitoring.
//
// Reference: OpenDrive-Bridge-Whitepaper.md §2.1 (machine readable spec),
// §5 P0 (exit criteria: spec archive committed), §12.2 (drift watch).
//
// The API explorer only exposes public endpoints (session/login,
// upload/checkfileexistsbyname, ...) to anonymous callers, so the tool tries a
// number of authentication strategies and keeps the archive produced by the one
// that reveals the most operations.
//
// Credentials are read from the environment only. They are never written to
// disk, never logged, and any session identifier that leaks into a response
// body is redacted before archiving.
//
// Usage:
//
//	ODB_SPEC_USER=... ODB_SPEC_PASS=... go run ./tools/fetch-spec
//	go run ./tools/fetch-spec -anonymous          # public subset only
//	go run ./tools/fetch-spec -out testdata/spec  # archive location
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultBase = "https://dev.opendrive.com/api/v1"
	userAgent   = "opendrive-bridge-fetch-spec/0.1 (+https://github.com/echotreez/opendrive-bridge)"

	// OAuthSessionMarker is the magic session_id value used with OAuth2; it is
	// not a secret and stays readable in logs.
	OAuthSessionMarker = "OAUTH"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-spec:", err)
		os.Exit(1)
	}
}

type options struct {
	base      string
	out       string
	anonymous bool
	timeout   time.Duration
}

func run() error {
	var opt options
	flag.StringVar(&opt.base, "base", envOr("ODB_SPEC_BASE", defaultBase), "upstream API base URL")
	flag.StringVar(&opt.out, "out", "testdata/spec", "archive directory")
	flag.BoolVar(&opt.anonymous, "anonymous", false, "skip login, archive the public subset only")
	flag.DurationVar(&opt.timeout, "timeout", 60*time.Second, "per-request timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: opt.timeout, Jar: jar}

	strategies := []strategy{{name: "anonymous"}}

	if !opt.anonymous {
		user, pass := os.Getenv("ODB_SPEC_USER"), os.Getenv("ODB_SPEC_PASS")
		if user == "" || pass == "" {
			return errors.New("set ODB_SPEC_USER and ODB_SPEC_PASS, or pass -anonymous")
		}
		sess, err := login(ctx, hc, opt.base, user, pass)
		if err != nil {
			return fmt.Errorf("session login: %w", err)
		}
		defer logout(hc, opt.base, sess)

		fmt.Printf("logged in, session %s\n", redactSecret(sess))

		// The explorer is a PHP/Restler app: the session may be honoured via a
		// query parameter, via the path segment style used by several
		// endpoints, or via the PHP cookie the login response set on our jar.
		strategies = append(strategies,
			strategy{name: "cookie", session: sess},
			strategy{name: "query-session_id", session: sess, query: map[string]string{"session_id": sess}},
			strategy{name: "path-session", session: sess, pathSuffix: sess},
		)

		if tok, err := oauthGrant(ctx, hc, opt.base, user, pass); err != nil {
			fmt.Printf("oauth2 grant unavailable (%v), continuing without it\n", err)
		} else {
			strategies = append(strategies, strategy{
				name:    "query-access_token",
				session: sess,
				query:   map[string]string{"session_id": "OAUTH", "access_token": tok},
				secrets: []string{tok},
			})
		}
	}

	var best *archive
	for _, s := range strategies {
		a, err := fetchAll(ctx, hc, opt.base, s)
		if err != nil {
			// §9.4: a session id in a path segment survives URL-parameter
			// redaction, so the whole message is scrubbed before printing.
			fmt.Printf("strategy %-20s failed: %s\n", s.name, redactText(err.Error(), s.secretValues()))
			continue
		}
		fmt.Printf("strategy %-20s -> %d resources, %d endpoints, %d operations, %d unreadable\n",
			s.name, len(a.Files), a.Endpoints, a.Operations, len(a.Missing))
		if best == nil || a.Operations > best.Operations {
			best = a
		}
	}
	if best == nil {
		return errors.New("every strategy failed; no spec archived")
	}

	if err := best.write(opt.out); err != nil {
		return err
	}
	fmt.Printf("\narchived %d files to %s using strategy %q (%d endpoints, %d operations)\n",
		len(best.Files), opt.out, best.Strategy, best.Endpoints, best.Operations)
	if best.Strategy == "anonymous" {
		fmt.Println("WARNING: archive is the PUBLIC subset only; authenticated endpoints are missing")
	}
	return nil
}

// ---------------------------------------------------------------- strategies

type strategy struct {
	name       string
	session    string
	query      map[string]string
	pathSuffix string   // appended as an extra path segment, e.g. /resources/file.json/{session}
	secrets    []string // extra values to redact from archived bodies
}

// secretValues lists every credential this strategy puts on the wire. A session
// id carried in a path segment survives URL-parameter redaction, so error text
// has to be scrubbed against this list too (§9.4).
func (s strategy) secretValues() []string {
	out := []string{s.session, s.pathSuffix}
	out = append(out, s.secrets...)
	for k, v := range s.query {
		if k == "session_id" && v == OAuthSessionMarker {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------- login

type loginResponse struct {
	SessionID string `json:"SessionID"`
}

func login(ctx context.Context, hc *http.Client, base, user, pass string) (string, error) {
	// Whitepaper §2.2 (A): POST /session/login.json.
	body := map[string]string{
		"username":         user,
		"passwd":           pass,
		"version":          "10",
		"partner_id":       "",
		"captcha_response": "",
	}
	var out loginResponse
	if err := postJSON(ctx, hc, base+"/session/login.json", body, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("login succeeded but no SessionID in response")
	}
	return out.SessionID, nil
}

func logout(hc *http.Client, base, sess string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = postJSON(ctx, hc, base+"/session/logout.json", map[string]string{"session_id": sess}, nil)
}

type grantResponse struct {
	AccessToken string `json:"access_token"`
}

func oauthGrant(ctx context.Context, hc *http.Client, base, user, pass string) (string, error) {
	// Whitepaper §2.2 (B): simplified resource-owner password credentials flow.
	body := map[string]string{
		"grant_type": "password",
		"client_id":  "OpenDrive",
		"username":   user,
		"password":   pass,
	}
	var out grantResponse
	if err := postJSON(ctx, hc, base+"/oauth2/grant.json", body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("no access_token in grant response")
	}
	return out.AccessToken, nil
}

func postJSON(ctx context.Context, hc *http.Client, endpoint string, in, out any) error {
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Never echo the request body (it holds the password).
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, redactURL(endpoint), truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---------------------------------------------------------------- fetching

// resourceListing is the Swagger 1.1 root document returned by resources.json.
type resourceListing struct {
	APIVersion     string `json:"apiVersion"`
	SwaggerVersion string `json:"swaggerVersion"`
	BasePath       string `json:"basePath"`
	APIs           []struct {
		Path        string `json:"path"`
		Description string `json:"description"`
	} `json:"apis"`
}

// apiDeclaration is a per-module Swagger 1.1 document.
type apiDeclaration struct {
	APIs []struct {
		Path       string `json:"path"`
		Operations []struct {
			HTTPMethod string `json:"httpMethod"`
			Method     string `json:"method"`
			Nickname   string `json:"nickname"`
		} `json:"operations"`
	} `json:"apis"`
}

type specFile struct {
	Name       string `json:"name"`
	SourceURL  string `json:"source_url"`
	SHA256     string `json:"sha256"`
	Bytes      int    `json:"bytes"`
	Endpoints  int    `json:"endpoints"`
	Operations int    `json:"operations"`

	data []byte
}

type archive struct {
	FetchedAt  time.Time  `json:"fetched_at"`
	Base       string     `json:"base"`
	Strategy   string     `json:"strategy"`
	Endpoints  int        `json:"endpoints"`
	Operations int        `json:"operations"`
	Files      []specFile `json:"files"`
	Missing    []string   `json:"missing,omitempty"`
}

func fetchAll(ctx context.Context, hc *http.Client, base string, s strategy) (*archive, error) {
	root, rootRaw, err := fetchListing(ctx, hc, base, s)
	if err != nil {
		return nil, err
	}

	a := &archive{FetchedAt: time.Now().UTC(), Base: base, Strategy: s.name}
	a.Files = append(a.Files, specFile{
		Name:      "resources.json",
		SourceURL: redactURL(base + "/resources.json"),
		data:      rootRaw,
	})

	for _, api := range root.APIs {
		module := moduleName(api.Path)
		if module == "" {
			continue
		}
		raw, err := fetchJSON(ctx, hc, base+"/resources/"+module+".json", s)
		if err != nil {
			// A module the current credentials may not read is a gap in the
			// archive, not a reason to discard the whole strategy.
			a.Missing = append(a.Missing, module+": "+truncate(err.Error(), 160))
			continue
		}
		var decl apiDeclaration
		if err := json.Unmarshal(raw, &decl); err != nil {
			a.Missing = append(a.Missing, module+": malformed spec document")
			continue
		}
		f := specFile{
			Name:      module + ".json",
			SourceURL: redactURL(base + "/resources/" + module + ".json"),
			data:      raw,
			Endpoints: len(decl.APIs),
		}
		for _, e := range decl.APIs {
			f.Operations += len(e.Operations)
		}
		a.Files = append(a.Files, f)
		a.Endpoints += f.Endpoints
		a.Operations += f.Operations
	}

	secrets := s.secretValues()
	for i := range a.Files {
		a.Files[i].data = redactAll(a.Files[i].data, secrets)
		sum := sha256.Sum256(a.Files[i].data)
		a.Files[i].SHA256 = hex.EncodeToString(sum[:])
		a.Files[i].Bytes = len(a.Files[i].data)
	}
	sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Name < a.Files[j].Name })
	return a, nil
}

// moduleName normalises an entry of the Swagger resource listing into a module
// name. Restler advertises resources as "/resources/file.{format}"; older
// deployments use "/file" or "/resources/file.json".
func moduleName(path string) string {
	p := strings.Trim(path, "/")
	p = strings.TrimPrefix(p, "resources/")
	p = strings.TrimSuffix(p, ".{format}")
	p = strings.TrimSuffix(p, ".json")
	if strings.ContainsAny(p, "/{}") {
		return ""
	}
	return p
}

func fetchListing(ctx context.Context, hc *http.Client, base string, s strategy) (*resourceListing, []byte, error) {
	raw, err := fetchJSON(ctx, hc, base+"/resources.json", s)
	if err != nil {
		return nil, nil, err
	}
	var root resourceListing
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("decode resources.json: %w", err)
	}
	if len(root.APIs) == 0 {
		return nil, nil, errors.New("resources.json listed no APIs")
	}
	return &root, raw, nil
}

func fetchJSON(ctx context.Context, hc *http.Client, endpoint string, s strategy) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if s.pathSuffix != "" {
		u.Path += "/" + s.pathSuffix
	}
	if len(s.query) > 0 {
		q := u.Query()
		for k, v := range s.query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, redactURL(u.String()), truncate(string(raw), 300))
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return nil, fmt.Errorf("%s returned non-JSON: %s", redactURL(u.String()), truncate(string(raw), 200))
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

// ---------------------------------------------------------------- archiving

func (a *archive) write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, f := range a.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.data, 0o600); err != nil {
			return err
		}
	}
	manifest, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')
	return os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600)
}

// ---------------------------------------------------------------- redaction

// redactSecret renders a secret safe for logs: whitepaper §9.4 forbids emitting
// session ids or tokens in full.
func redactSecret(s string) string {
	if len(s) <= 4 {
		return "***"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

var redactedParams = []string{"session_id", "access_token", "refresh_token", "passwd", "password"}

// redactURL removes credential-bearing query parameters from a URL.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	q := u.Query()
	for _, p := range redactedParams {
		if v := q.Get(p); v != "" && v != "OAUTH" {
			q.Set(p, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// redactAll replaces every occurrence of the given secrets in a body.
func redactAll(body []byte, secrets []string) []byte {
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		body = bytes.ReplaceAll(body, []byte(s), []byte("REDACTED"))
	}
	return body
}

// redactText is redactAll for strings: log lines and error messages.
func redactText(s string, secrets []string) string {
	return string(redactAll([]byte(s), secrets))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
