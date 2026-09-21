package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// fakeAuth is an Authenticator whose state the tests set directly, so that
// /v1/auth/status can be checked without a network — which is the property the
// endpoint is supposed to have.
type fakeAuth struct {
	identity  opendrive.Identity
	state     opendrive.AuthState
	token     opendrive.Token
	hasToken  bool
	loginErr  error
	logoutErr error
	logins    int
	logouts   int
}

func (f *fakeAuth) Identity() opendrive.Identity   { return f.identity }
func (f *fakeAuth) AuthState() opendrive.AuthState { return f.state }
func (f *fakeAuth) Token() (opendrive.Token, bool) { return f.token, f.hasToken }
func (f *fakeAuth) Logout(context.Context) error   { f.logouts++; return f.logoutErr }
func (f *fakeAuth) Login(_ context.Context, u, p string) error {
	f.logins++
	if f.loginErr != nil {
		return f.loginErr
	}
	f.identity = opendrive.Identity{Username: u, UserID: "42", AccType: 1,
		AuthMode: opendrive.AuthModeOAuth2, Seamless: p != ""}
	f.state = opendrive.StateAuthenticated
	return nil
}

// fakeStore reports a backend and whether it can be read.
type fakeStore struct {
	keystore.Store
	backend   keystore.Backend
	available error
	probes    int
}

func (f *fakeStore) Backend() keystore.Backend { return f.backend }
func (f *fakeStore) Available(context.Context) error {
	f.probes++
	return f.available
}

func newTestServer(t *testing.T, auth Auth, opts ...Option) *Server {
	t.Helper()
	srv, err := New(Config{Addr: "127.0.0.1:0"}, auth, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func do(t *testing.T, srv *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, out
}

// ---------------------------------------------------------------- binding

// A daemon that listens beyond loopback without a key would hand an OpenDrive
// account to the network. Refusing to start is the only safe answer, and the
// refusal has to say what to do about it.
func TestNonLoopbackWithoutAKeyRefusesToStart(t *testing.T) {
	_, err := New(Config{Addr: "0.0.0.0:7777"}, &fakeAuth{})
	if err == nil {
		t.Fatal("a public listener with no API key was accepted")
	}
	for _, want := range []string{"API key", DefaultAddr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}

	if _, err := New(Config{
		Addr: "0.0.0.0:7777", APIKey: "k", APIKeyConfigured: true,
	}, &fakeAuth{}); err != nil {
		t.Errorf("a public listener with a key the user chose was refused: %v", err)
	}
}

// The key the daemon generates for itself does not open a public listener.
//
// This is the test the old one should have been. It asserted that any non-empty
// APIKey was enough — which was true when the only way to have a key was to
// configure one. Since v1.1 the daemon always has a key, because it makes one on
// first run, so the refusal above silently stopped firing for every deployment
// that had not set ODB_API_KEY. The container job in CI would have caught it,
// except that a daemon which starts instead of exiting does not fail a test that
// runs it in the foreground: it hangs, and six hours later the runner is killed.
func TestAGeneratedKeyDoesNotOpenAPublicListener(t *testing.T) {
	_, err := New(Config{Addr: "0.0.0.0:7777", APIKey: "generated-into-dot-env"}, &fakeAuth{})
	if err == nil {
		t.Fatal("a public listener was opened on the strength of a key the daemon " +
			"generated; nothing prints that key, so the address would be reachable " +
			"by a secret its owner has never seen")
	}
	// And it must point at the thing the user has to do, naming the ways in.
	for _, want := range []string{"--api-key", "ODB_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A key the *daemon* generated for itself must not shut the local CLI out.
// Since v1.1 the first run puts an API key in .env so that clients which need
// one have it (§9.2.2); odctl, in the same folder, does not know it. Enforcing
// that key on loopback meant every local command came back 401 — found by the
// systemd job, which drives the daemon the way a user would.
//
// A wrong key is still a wrong key: presenting one that does not match is
// refused rather than waved through.
func TestAGeneratedKeyDoesNotLockOutLoopback(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", APIKey: "generated-for-clients"}, &fakeAuth{})
	if err != nil {
		t.Fatal(err)
	}

	rec, _ := do(t, srv, http.MethodGet, "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a key the daemon made for itself must not "+
			"lock the user out of their own loopback socket", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Header.Set("Authorization", "Bearer the-wrong-key")
	wrong := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wrong, req)
	if wrong.Code != http.StatusUnauthorized {
		t.Errorf("a wrong key was accepted: status = %d", wrong.Code)
	}

	right := httptest.NewRecorder()
	reqOK := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	reqOK.Header.Set("Authorization", "Bearer generated-for-clients")
	srv.Handler().ServeHTTP(right, reqOK)
	if right.Code != http.StatusOK {
		t.Errorf("the right key was refused: status = %d", right.Code)
	}
}

func TestLoopbackNeedsNoKey(t *testing.T) {
	srv := newTestServer(t, &fakeAuth{state: opendrive.StateNotConfigured})
	rec, _ := do(t, srv, http.MethodGet, "/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on loopback with no key", rec.Code)
	}
}

// A key the user configured is enforced everywhere, loopback included: setting
// one is a decision, and the bridge honours it.
func TestAKeyIsEnforcedWhenConfigured(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0", APIKey: "secret", APIKeyConfigured: true}, &fakeAuth{})
	if err != nil {
		t.Fatal(err)
	}

	rec, body := do(t, srv, http.MethodGet, "/v1/health", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a key", rec.Code)
	}
	// Even this message has to tell the caller what to do.
	msg := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "Bearer") {
		t.Errorf("the 401 does not say how to send the key: %q", msg)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, r)
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d with the right key", rec2.Code)
	}

	r = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, r)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("status = %d with a wrong key", rec3.Code)
	}
}

func TestRequestIDIsAssignedAndEchoed(t *testing.T) {
	srv := newTestServer(t, &fakeAuth{})
	rec, _ := do(t, srv, http.MethodGet, "/v1/health", "")
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("no request id was returned")
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.Header.Set(RequestIDHeader, "caller-supplied")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, r)
	if got := rec2.Header().Get(RequestIDHeader); got != "caller-supplied" {
		t.Errorf("request id = %q, want the caller's own", got)
	}
}

func TestUnknownRoutesAnswerInTheEnvelope(t *testing.T) {
	srv := newTestServer(t, &fakeAuth{})
	rec, body := do(t, srv, http.MethodGet, "/v1/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if body["error"] == nil {
		t.Fatalf("an unknown route did not answer in the standard envelope: %s", rec.Body)
	}

	rec2, _ := do(t, srv, http.MethodDelete, "/v1/auth/status", "")
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec2.Code)
	}
}

// ---------------------------------------------------------------- status

// The endpoint has to answer when nothing is configured — that is when somebody
// runs it — and it must not go near the network to do it.
func TestStatusWithNothingConfigured(t *testing.T) {
	srv := newTestServer(t, &fakeAuth{state: opendrive.StateNotConfigured,
		identity: opendrive.Identity{AuthMode: opendrive.AuthModeOAuth2}})

	rec, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body["account"] != nil {
		t.Errorf("account = %v, want null before anything is configured", body["account"])
	}
	if body["state"] != "not_configured" {
		t.Errorf("state = %v", body["state"])
	}
	if body["quota"] != nil {
		t.Errorf("quota = %v, want null with no client", body["quota"])
	}
	// Every §4.1 field is present, even when null, so a client can rely on it.
	for _, field := range []string{"account", "auth_mode", "state", "seamless",
		"token_expires_at", "keystore", "quota"} {
		if _, ok := body[field]; !ok {
			t.Errorf("%q missing from the status response; §4.1 requires it", field)
		}
	}
}

func TestStatusReportsTheConfiguredAccount(t *testing.T) {
	auth := &fakeAuth{
		state: opendrive.StateAuthenticated,
		identity: opendrive.Identity{Username: "derek@example.com", UserID: "2125533",
			AccType: 1, AuthMode: opendrive.AuthModeOAuth2, Seamless: true},
	}
	srv := newTestServer(t, auth, WithKeystore(&fakeStore{backend: keystore.BackendFile}))

	_, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	account := body["account"].(map[string]any)
	if account["username"] != "derek@example.com" || account["user_id"] != "2125533" {
		t.Errorf("account = %v", account)
	}
	if body["seamless"] != true || body["state"] != "authenticated" {
		t.Errorf("seamless = %v state = %v", body["seamless"], body["state"])
	}
	ks := body["keystore"].(map[string]any)
	if ks["backend"] != "encrypted_file" || ks["available"] != true {
		t.Errorf("keystore = %v", ks)
	}
}

// An unreadable credential store outranks whatever the authenticator last
// managed: nothing can be renewed until it comes back, and no upstream request
// may be attempted meanwhile (§4.5). Since v1.1 that means a missing .env.key or
// an undecryptable .env rather than a locked vault.
func TestStatusReportsALockedKeystoreAndMakesNoRequest(t *testing.T) {
	auth := &fakeAuth{
		state:    opendrive.StateAuthenticated,
		identity: opendrive.Identity{Username: "derek@example.com", AuthMode: opendrive.AuthModeOAuth2},
	}
	store := &fakeStore{backend: keystore.BackendFile, available: errors.New("the keyring is locked")}
	srv := newTestServer(t, auth, WithKeystore(store))

	_, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	if body["state"] != "keystore_unavailable" {
		t.Errorf("state = %v, want keystore_unavailable", body["state"])
	}
	if ks := body["keystore"].(map[string]any); ks["available"] != false {
		t.Errorf("keystore.available = %v", ks["available"])
	}
	if store.probes == 0 {
		t.Error("the store was never asked whether it is readable")
	}
}

func TestStatusReportsTokenExpiry(t *testing.T) {
	expiry := mustTime(t, "2026-07-26T10:00:00Z")
	auth := &fakeAuth{
		state:    opendrive.StateAuthenticated,
		identity: opendrive.Identity{Username: "d@example.com", AuthMode: opendrive.AuthModeOAuth2},
		token:    opendrive.Token{AccessToken: "x", Expiry: expiry},
		hasToken: true,
	}
	srv := newTestServer(t, auth)

	_, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	if body["token_expires_at"] != "2026-07-26T10:00:00Z" {
		t.Errorf("token_expires_at = %v", body["token_expires_at"])
	}
	// The token itself must never appear anywhere in the response.
	if strings.Contains(strings.ToLower(toJSON(t, body)), "access_token") {
		t.Error("the status response mentions the access token")
	}
}

// ---------------------------------------------------------------- login

func TestLoginStoresAndReportsStatus(t *testing.T) {
	auth := &fakeAuth{state: opendrive.StateNotConfigured}
	srv := newTestServer(t, auth, WithKeystore(&fakeStore{backend: keystore.BackendFile}))

	rec, body := do(t, srv, http.MethodPost, "/v1/auth/login",
		`{"username":"derek@example.com","password":"hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if auth.logins != 1 {
		t.Errorf("login was called %d times", auth.logins)
	}
	if body["state"] != "authenticated" {
		t.Errorf("state = %v", body["state"])
	}
	// The password must not come back in the response.
	if strings.Contains(toJSON(t, body), "hunter2") {
		t.Error("the login response echoes the password")
	}
}

// The one that must never be confused with a wrong password: an unreadable
// credential store stops the bridge *before* it asks upstream anything, because
// retrying blindly is how an account meets a captcha lock.
func TestLoginWithALockedKeystoreMakesNoUpstreamRequest(t *testing.T) {
	auth := &fakeAuth{state: opendrive.StateNotConfigured}
	store := &fakeStore{backend: keystore.BackendFile, available: errors.New("the keyring is locked")}
	srv := newTestServer(t, auth, WithKeystore(store))

	rec, body := do(t, srv, http.MethodPost, "/v1/auth/login",
		`{"username":"derek@example.com","password":"hunter2"}`)

	if auth.logins != 0 {
		t.Fatal("a sign-in was attempted with an unreadable credential store")
	}
	e := body["error"].(map[string]any)
	if e["code"] != "keystore_unavailable" {
		t.Errorf("code = %v, want keystore_unavailable", e["code"])
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rec.Code)
	}
	msg := e["message"].(string)
	assertUserReadable(t, msg)
	if !strings.Contains(strings.ToLower(msg), "keychain") {
		t.Errorf("the message does not tell the user what to unlock: %q", msg)
	}
}

func TestLoginValidatesItsBody(t *testing.T) {
	srv := newTestServer(t, &fakeAuth{})
	for _, body := range []string{`{}`, `{"username":"x"}`, `{"password":"y"}`, `not json`} {
		rec, _ := do(t, srv, http.MethodPost, "/v1/auth/login", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestLoginSurfacesAWrongPasswordAsSomethingActionable(t *testing.T) {
	auth := &fakeAuth{loginErr: &opendrive.APIError{
		Kind: opendrive.KindReauthRequired, HTTPCode: 401,
		UpstreamMsg: "Invalid username or password",
	}}
	srv := newTestServer(t, auth)

	rec, body := do(t, srv, http.MethodPost, "/v1/auth/login", `{"username":"a","password":"b"}`)
	e := body["error"].(map[string]any)
	if e["code"] != "reauth_required" {
		t.Errorf("code = %v", e["code"])
	}
	if rec.Code != http.StatusPreconditionRequired {
		t.Errorf("status = %d", rec.Code)
	}
	assertUserReadable(t, e["message"].(string))
}

func TestLogoutClearsAndSaysSo(t *testing.T) {
	auth := &fakeAuth{state: opendrive.StateAuthenticated}
	srv := newTestServer(t, auth)

	rec, body := do(t, srv, http.MethodPost, "/v1/auth/logout", "")
	if rec.Code != http.StatusOK || auth.logouts != 1 {
		t.Fatalf("status = %d logouts = %d", rec.Code, auth.logouts)
	}
	if !strings.Contains(body["detail"].(string), "password") {
		t.Errorf("logout does not tell the user the password was forgotten: %v", body["detail"])
	}
}

// A panicking handler must not drop the connection or leak the panic.
func TestPanicsBecomeTheStandardEnvelope(t *testing.T) {
	srv := newTestServer(t, &panicAuth{})
	rec, body := do(t, srv, http.MethodGet, "/v1/auth/status", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	if strings.Contains(msg, "runtime error") || strings.Contains(msg, "nil pointer") {
		t.Errorf("the panic reached the client: %q", msg)
	}
	assertUserReadable(t, msg)
}

type panicAuth struct{ fakeAuth }

func (p *panicAuth) Identity() opendrive.Identity { panic("boom") }

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// D45: upstream reports usage in bytes and the limits in megabytes, in the same
// object. Passed through unchanged, a user is told they are 180 times over a
// one-megabyte quota on an unlimited plan. The Bridge normalises to bytes.
func TestQuotaLimitsAreNormalisedToBytes(t *testing.T) {
	info := &opendrive.AccountInfo{}
	if err := json.Unmarshal([]byte(`{
		"StorageUsed":"188726232","MaxStorage":"1048576",
		"BwUsed":"6901021","BwMax":"10240"}`), info); err != nil {
		t.Fatal(err)
	}

	q := quotaFrom(info)
	if q.StorageMax <= q.StorageUsed {
		t.Errorf("storage_max (%d) is not above storage_used (%d); the units are still mixed",
			q.StorageMax, q.StorageUsed)
	}
	if q.StorageMax != 1048576*bytesPerMB {
		t.Errorf("storage_max = %d, want the megabyte figure converted to bytes", q.StorageMax)
	}
	if q.BWMax != 10240*bytesPerMB {
		t.Errorf("bw_max = %d, want the megabyte figure converted to bytes", q.BWMax)
	}
	// The used figures were already bytes and must not be touched.
	if q.StorageUsed != 188726232 || q.BWUsed != 6901021 {
		t.Errorf("a used figure was converted: storage=%d bw=%d", q.StorageUsed, q.BWUsed)
	}
}
