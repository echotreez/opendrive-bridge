package opendrive

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewRejectsARelativeBaseURL(t *testing.T) {
	if _, err := New(WithBaseURL("not-a-url")); err == nil {
		t.Fatal("expected an error")
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL() != DefaultBaseURL {
		t.Fatalf("BaseURL = %q", c.BaseURL())
	}
	if c.Logger() == nil {
		t.Fatal("Logger must never be nil")
	}
}

func TestDoValidatesTheRequest(t *testing.T) {
	c, _ := New()
	if err := c.Do(context.Background(), Request{Path: "/x.json"}, nil); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing method: %v", err)
	}
	if err := c.Do(context.Background(), Request{Method: http.MethodGet}, nil); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing path: %v", err)
	}
	//nolint:staticcheck // deliberately passing a nil context
	if err := c.Do(nil, Request{Method: http.MethodGet, Path: "/x.json"}, nil); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("nil context: %v", err)
	}
}

// §2.2: in session mode the id travels in the body, the query or the path,
// depending on the endpoint.
func TestSessionPlacement(t *testing.T) {
	t.Run("body by default for a request with a body", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `{"result":true}`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
		var out BoolResult
		if err := c.Do(context.Background(), Request{
			Method: http.MethodPost, Path: "/folder/rename.json",
			Body: map[string]string{"folder_id": "42", "folder_name": "x"},
		}, &out); err != nil {
			t.Fatal(err)
		}
		call := m.lastCall()
		if call.Body["session_id"] != "SESSION" {
			t.Fatalf("body = %s", call.RawBody)
		}
		if call.Query.Has("session_id") {
			t.Fatal("session must not be duplicated into the query string")
		}
		if call.Body["folder_id"] != "42" {
			t.Fatal("the caller's fields must survive session injection")
		}
	})

	t.Run("query by default for a bodyless request", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `{"result":true}`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
		if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/session/info.json"}, nil); err != nil {
			t.Fatal(err)
		}
		if got := m.lastCall().Query.Get("session_id"); got != "SESSION" {
			t.Fatalf("query session_id = %q", got)
		}
	})

	t.Run("path segments", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `{"DirUpdateTime":1}`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
		err := c.Do(context.Background(), Request{
			Method: http.MethodGet, Path: "/folder/list.json",
			SessionPlacement: SessionInPath, PathSegments: []string{"12345"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.lastCall().Path; got != "/api/v1/folder/list.json/SESSION/12345" {
			t.Fatalf("path = %q", got)
		}
	})

	t.Run("omitted for login", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `{"SessionID":"new"}`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
		err := c.Do(context.Background(), Request{
			Method: http.MethodPost, Path: "/session/login.json",
			SessionPlacement: SessionOmit, Body: map[string]string{"username": "u"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		call := m.lastCall()
		if _, ok := call.Body["session_id"]; ok {
			t.Fatalf("login must not carry a session: %s", call.RawBody)
		}
		if call.Query.Has("access_token") {
			t.Fatal("login must not carry a token")
		}
	})

	t.Run("explicit query placement for a request with a body", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `true`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
		err := c.Do(context.Background(), Request{
			Method: http.MethodPost, Path: "/x.json",
			SessionPlacement: SessionInQuery, Body: map[string]string{"a": "b"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.lastCall().Query.Get("session_id") != "SESSION" {
			t.Fatal("explicit query placement ignored")
		}
	})
}

// §2.6 #2: download/all.json calls the session parameter session_key.
func TestSessionParamOverride(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"result":true}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
	err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "/download/all.json",
		SessionParam: "session_key",
		Body:         map[string]string{"files": "1,2"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := m.lastCall()
	if call.Body["session_key"] != "SESSION" {
		t.Fatalf("session_key missing: %s", call.RawBody)
	}
	if _, ok := call.Body["session_id"]; ok {
		t.Fatal("download/all.json must not receive session_id")
	}
}

// §2.2 B and §2.6 #8: OAuth calls send session_id=OAUTH plus the token in the
// query string, for every verb.
func TestOAuthCredentialPlacement(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		m := newMockUpstream(t)
		m.push(200, `{"result":true}`)
		c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "tok-abc"}}))
		req := Request{Method: method, Path: "/file/info.json"}
		if method != http.MethodGet {
			req.Body = map[string]string{"file_id": "7"}
		}
		if err := c.Do(context.Background(), req, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		call := m.lastCall()
		if got := call.Query.Get("access_token"); got != "tok-abc" {
			t.Errorf("%s: access_token in query = %q", method, got)
		}
		if got := call.SessionValue("session_id"); got != OAuthSessionID {
			t.Errorf("%s: session_id = %q, want %q", method, got, OAuthSessionID)
		}
	}
}

// §2.6 #12: the PDF writes some endpoints with a /v1 prefix the base URL
// already carries.
func TestPathJoinDeduplicatesTheVersionPrefix(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"result":true}`)
	m.push(200, `{"result":true}`)
	c := m.client()
	for _, p := range []string{"/file/expiringlink.json", "/v1/file/expiringlink.json"} {
		if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: p}, nil); err != nil {
			t.Fatal(err)
		}
	}
	calls := m.calls()
	if calls[0].Path != "/api/v1/file/expiringlink.json" || calls[1].Path != calls[0].Path {
		t.Fatalf("paths = %q and %q", calls[0].Path, calls[1].Path)
	}

	if got := joinPath("/api/v1", "/v1"); got != "/api/v1/" {
		t.Errorf("joinPath collapsing to the base = %q", got)
	}
	if got := joinPath("/api/v1", "v1files/x.json"); got != "/api/v1/v1files/x.json" {
		t.Errorf("joinPath must only strip a whole segment, got %q", got)
	}
	if got := joinPath("", "/x.json"); got != "/x.json" {
		t.Errorf("joinPath with an empty base = %q", got)
	}
}

func TestPathSegmentsAreEscaped(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `true`)
	c := m.client()
	err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Path: "/folder/list.json",
		PathSegments: []string{"a b/c", ""},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The separator inside the segment must be escaped, so it cannot forge an
	// extra path element (§9.3).
	if got := m.lastCall().Path; got != "/api/v1/folder/list.json/a%20b%2Fc" {
		t.Fatalf("path = %q", got)
	}
}

// §10.1: temporary failures back off and retry, permanent ones do not.
func TestRetryBehaviour(t *testing.T) {
	t.Run("retries a temporary failure on a GET", func(t *testing.T) {
		m := newMockUpstream(t)
		m.handle(func(w http.ResponseWriter, _ *http.Request, n int) {
			if n < 2 {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":true}`))
		})
		c := m.client()
		var out BoolResult
		if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, &out); err != nil {
			t.Fatal(err)
		}
		if m.callCount() != 3 {
			t.Fatalf("calls = %d, want 3", m.callCount())
		}
	})

	t.Run("gives up after the configured maximum", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(500, `{"error":{"code":500,"message":"boom"}}`)
		c := m.client()
		err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
		if ErrorKind(err) != KindUpstreamError {
			t.Fatalf("err = %v", err)
		}
		if m.callCount() != 4 { // 1 attempt + 3 retries
			t.Fatalf("calls = %d, want 4", m.callCount())
		}
	})

	t.Run("does not retry a POST by default", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(500, `{"error":{"code":500,"message":"boom"}}`)
		c := m.client()
		err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/x.json",
			Body: map[string]string{"a": "b"}}, nil)
		if err == nil {
			t.Fatal("expected an error")
		}
		if m.callCount() != 1 {
			t.Fatalf("a non-idempotent call was replayed %d times", m.callCount())
		}
	})

	t.Run("retries a POST that opts in", func(t *testing.T) {
		m := newMockUpstream(t)
		m.handle(func(w http.ResponseWriter, _ *http.Request, n int) {
			if n == 0 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"code":503,"message":"try later"}}`))
				return
			}
			_, _ = w.Write([]byte(`true`))
		})
		c := m.client()
		err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/x.json",
			Retryable: Retryable(true), Body: map[string]string{"a": "b"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if m.callCount() != 2 {
			t.Fatalf("calls = %d", m.callCount())
		}
	})

	t.Run("never retries a permanent failure", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(404, `{"error":{"code":404,"message":"File not exists"}}`)
		c := m.client()
		err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
		if ErrorKind(err) != KindNotFound {
			t.Fatalf("err = %v", err)
		}
		if m.callCount() != 1 {
			t.Fatalf("calls = %d, want 1", m.callCount())
		}
	})

	t.Run("honours Retry-After", func(t *testing.T) {
		m := newMockUpstream(t)
		m.pushHeader(429, `{"error":{"code":429,"message":"slow down"}}`,
			http.Header{"Retry-After": []string{"2"}})
		m.push(200, `true`)

		var delays []time.Duration
		c := m.client(WithSleepFunc(func(_ context.Context, d time.Duration) error {
			delays = append(delays, d)
			return nil
		}))
		if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil); err != nil {
			t.Fatal(err)
		}
		if len(delays) != 1 || delays[0] != 2*time.Second {
			t.Fatalf("delays = %v, want one 2s wait", delays)
		}
	})

	t.Run("a cancelled context stops the retry loop", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(500, `{"error":{"code":500}}`)
		ctx, cancel := context.WithCancel(context.Background())
		c := m.client(WithSleepFunc(func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}))
		err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/x.json"}, nil)
		if ErrorKind(err) != KindNetwork {
			t.Fatalf("err = %v, want a network error carrying the cancellation", err)
		}
	})
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{Max: 10, Base: time.Second, Cap: 8 * time.Second,
		Jitter: func(d time.Duration) time.Duration { return d }}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := p.backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i, got, w)
		}
	}
	// Defaults and full jitter stay inside the expected band.
	def := DefaultRetryPolicy()
	for i := 0; i < 6; i++ {
		d := def.backoff(i)
		if d < 0 || d > def.Cap {
			t.Fatalf("jittered backoff(%d) = %v", i, d)
		}
	}
	zero := RetryPolicy{}
	if d := zero.backoff(0); d <= 0 || d > time.Second {
		t.Fatalf("zero-value policy backoff = %v", d)
	}
}

// §2.2 and §11: a 401 invalid_token triggers exactly one refresh and one replay.
func TestTokenExpiryTriggersRefreshAndReplayOnce(t *testing.T) {
	t.Run("refresh then replay succeeds", func(t *testing.T) {
		m := newMockUpstream(t)
		m.handle(func(w http.ResponseWriter, r *http.Request, n int) {
			if r.URL.Query().Get("access_token") == "fresh" {
				_, _ = w.Write([]byte(`{"result":true}`))
				return
			}
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":401,"error":"invalid_token","error_description":"The access token provided has expired"}}`))
		})
		auth := &stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "stale"}}
		auth.refreshFn = func() (Credentials, error) {
			return Credentials{SessionID: OAuthSessionID, AccessToken: "fresh"}, nil
		}
		c := m.client(WithAuthenticator(auth))
		var out BoolResult
		if err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/file/info.json",
			Body: map[string]string{"file_id": "1"}}, &out); err != nil {
			t.Fatal(err)
		}
		if !out.OK() {
			t.Fatal("expected a successful result after the replay")
		}
		if auth.refreshCount() != 1 {
			t.Fatalf("refreshes = %d, want 1", auth.refreshCount())
		}
		if m.callCount() != 2 {
			t.Fatalf("calls = %d, want the original plus one replay", m.callCount())
		}
	})

	t.Run("a second expiry is not replayed again", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(401, `{"error":{"code":401,"error":"invalid_token","error_description":"expired"}}`)
		auth := &stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "stale"}}
		auth.refreshFn = func() (Credentials, error) {
			return Credentials{SessionID: OAuthSessionID, AccessToken: "also-stale"}, nil
		}
		c := m.client(WithAuthenticator(auth))
		err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/file/info.json"}, nil)
		if ErrorKind(err) != KindTokenExpired {
			t.Fatalf("err = %v", err)
		}
		if auth.refreshCount() != 1 {
			t.Fatalf("refreshes = %d, want exactly 1", auth.refreshCount())
		}
		if m.callCount() != 2 {
			t.Fatalf("calls = %d, want 2", m.callCount())
		}
	})

	t.Run("a failed refresh surfaces the refresh error", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(401, `{"error":{"code":401,"error":"invalid_token"}}`)
		auth := &stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "stale"},
			refreshErr: &APIError{Kind: KindRefreshTokenFailed, UpstreamMsg: "log in again"}}
		c := m.client(WithAuthenticator(auth))
		err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
		if ErrorKind(err) != KindRefreshTokenFailed {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a stale-token aware authenticator is told which token failed", func(t *testing.T) {
		m := newMockUpstream(t)
		m.handle(func(w http.ResponseWriter, r *http.Request, n int) {
			if n == 0 {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":401,"error":"invalid_token"}}`))
				return
			}
			_, _ = w.Write([]byte(`true`))
		})
		auth := &staleAwareAuth{stubAuth: stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "stale"}}}
		c := m.client(WithAuthenticator(auth))
		if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil); err != nil {
			t.Fatal(err)
		}
		if auth.staleSeen != "stale" {
			t.Fatalf("RefreshStale saw %q", auth.staleSeen)
		}
	})
}

type staleAwareAuth struct {
	stubAuth
	staleSeen string
}

func (a *staleAwareAuth) RefreshStale(ctx context.Context, stale string) error {
	a.staleSeen = stale
	return a.Refresh(ctx)
}

func TestCredentialFailureIsSurfaced(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{credsErr: &APIError{Kind: KindUnauthorized, UpstreamMsg: "not logged in"}}))
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
	if ErrorKind(err) != KindUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatal("a request without credentials must not reach the network")
	}

	m2 := newMockUpstream(t)
	c2 := m2.client(WithAuthenticator(&stubAuth{credsErr: errNotAPI{}}))
	err = c2.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
	if ErrorKind(err) != KindUnauthorized {
		t.Fatalf("a foreign credential error should still be unauthorized, got %v", err)
	}
}

type errNotAPI struct{}

func (errNotAPI) Error() string { return "keyring locked" }

// §2.6 #7: some endpoints answer 200 with an error envelope.
func TestErrorEnvelopeInASuccessfulResponse(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"error":{"code":403,"message":"Bandwidth limit exceeded"}}`)
	c := m.client()
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/download/file.json"}, nil)
	if ErrorKind(err) != KindBandwidthExceeded {
		t.Fatalf("err = %v", err)
	}

	// A null or empty error field is not an error.
	for _, body := range []string{`{"error":null,"FileId":"1"}`, `{"error":"","FileId":"1"}`, `[1,2]`, `true`} {
		m2 := newMockUpstream(t)
		m2.push(200, body)
		if err := m2.client().Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil); err != nil {
			t.Errorf("body %s treated as an error: %v", body, err)
		}
	}
}

func TestResponseDecoding(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, `{"FileId":`)
		var out struct{ FileID string }
		err := m.client().Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, &out)
		if ErrorKind(err) != KindInvalidResponse {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("empty body with an expected result", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, ``)
		var out struct{ FileID string }
		err := m.client().Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, &out)
		if ErrorKind(err) != KindInvalidResponse {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("empty body when nothing is expected", func(t *testing.T) {
		m := newMockUpstream(t)
		m.push(200, ``)
		if err := m.client().Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTransportFailureIsANetworkError(t *testing.T) {
	c, err := New(WithBaseURL("https://127.0.0.1:1/api/v1"),
		WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}),
		WithRetryPolicy(RetryPolicy{}))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil)
	if ErrorKind(err) != KindNetwork {
		t.Fatalf("err = %v", err)
	}
	if !IsTemporary(err) {
		t.Fatal("a transport failure should be retryable")
	}
}

func TestBodyMustBeAJSONObjectWhenTheSessionGoesIntoIt(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
	err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "/x.json",
		SessionPlacement: SessionInBody, Body: []string{"an", "array"},
	}, nil)
	if ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}

	// An unencodable body is rejected before any network traffic.
	err = c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/x.json", Body: make(chan int)}, nil)
	if ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatal("an invalid request must not reach the network")
	}
}

func TestCallerSuppliedSessionFieldWins(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `true`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))
	err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "/x.json",
		Body: map[string]string{"session_id": "explicit"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.lastCall().Body["session_id"]; got != "explicit" {
		t.Fatalf("session_id = %v", got)
	}
}

func TestQueryParametersArePreserved(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `true`)
	c := m.client()
	err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Path: "/folder/list.json",
		Query: map[string][]string{"offset": {"100"}, "search_query": {"财务 report"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := m.lastCall()
	if call.Query.Get("offset") != "100" || call.Query.Get("search_query") != "财务 report" {
		t.Fatalf("query = %v", call.Query)
	}
}

// §9.4: nothing that reaches a log line may carry a credential.
func TestLoggingRedactsCredentials(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&syncWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	m := newMockUpstream(t)
	m.push(200, `{"access_token":"super-secret-token","result":true}`)
	c := m.client(
		WithLogger(logger),
		WithBodyLogging(true),
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "super-secret-token"}}),
	)
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/users/info.json"}, nil); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if logged == "" {
		t.Fatal("nothing was logged")
	}
	mustNotContain(t, logged, "super-secret-token", "log output")
	mustContain(t, logged, "REDACTED", "log output")
	mustContain(t, logged, "users/info.json", "log output")
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// §9.4: the URL kept on the error must be redacted too.
func TestErrorURLIsRedacted(t *testing.T) {
	m := newMockUpstream(t)
	m.push(404, `{"error":{"code":404,"message":"File not exists"}}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "secret-token"}}))
	err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/file/info.json"}, nil)
	var ae *APIError
	if !as(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	mustNotContain(t, ae.URL, "secret-token", "error URL")
	mustContain(t, ae.URL, "REDACTED", "error URL")
	mustNotContain(t, ae.Error(), "secret-token", "error message")
}

func as(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

func TestConcurrentRequestsAreSafe(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"result":true}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SESSION"}}))

	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out BoolResult
			errs[i] = c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, &out)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
}

func TestOptionsAreDefensive(t *testing.T) {
	c, err := New(
		WithHTTPClient(nil), WithUserAgent(""), WithLogger(nil), WithSleepFunc(nil),
		WithUserAgent("custom/1.0"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.hc == nil || c.log == nil || c.sleep == nil {
		t.Fatal("nil options must be ignored")
	}
	if c.ua != "custom/1.0" {
		t.Fatalf("user agent = %q", c.ua)
	}

	m := newMockUpstream(t)
	m.push(200, `true`)
	c2 := m.client(WithUserAgent("odctl-test/9"))
	if err := c2.Do(context.Background(), Request{Method: http.MethodGet, Path: "/x.json"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("a cancelled context must abort the sleep")
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("a zero sleep returned %v", err)
	}
}

func TestNewRequestIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if len(id) != 16 {
			t.Fatalf("id = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

func TestInjectBodyFieldKeepsExistingFields(t *testing.T) {
	out, err := injectBodyField([]byte(`{"a":1}`), "session_id", "S")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["a"] != float64(1) || got["session_id"] != "S" {
		t.Fatalf("got %v", got)
	}
	if out, err = injectBodyField(nil, "session_id", "S"); err != nil || !strings.Contains(string(out), "session_id") {
		t.Fatalf("empty body: %s %v", out, err)
	}
}

func TestDiscardHandlerStaysSilent(t *testing.T) {
	h := discardHandler{}
	if h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("the discard handler must report itself disabled")
	}
	if err := h.Handle(context.Background(), slog.Record{}); err != nil {
		t.Fatal(err)
	}
	if h.WithAttrs(nil) != h || h.WithGroup("x") != h {
		t.Fatal("the discard handler must be its own derivative")
	}
}
