package opendrive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one call the mock upstream received.
type recordedRequest struct {
	Method      string
	Path        string
	Query       url.Values
	RawBody     string
	ContentType string
	Body        map[string]any
}

// SessionValue returns the session parameter as it arrived, looking in the
// query string first and then in the JSON body.
func (r recordedRequest) SessionValue(param string) string {
	if v, ok := r.Query[param]; ok && len(v) > 0 {
		return v[0]
	}
	if v, ok := r.Body[param]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mockUpstream is an httptest server that records every request and answers
// from a queue of scripted responses.
type mockUpstream struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	requests    []recordedRequest
	responses   []mockResponse
	handler     func(w http.ResponseWriter, r *http.Request, n int)
	handlerFrom int
}

type mockResponse struct {
	status int
	body   string
	header http.Header
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{t: t}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.Query(),
			RawBody:     string(raw),
			ContentType: r.Header.Get("Content-Type"),
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.Body)
		}
		// Hand the body back so a custom handler can read it too.
		r.Body = io.NopCloser(bytes.NewReader(raw))
		m.mu.Lock()
		n := len(m.requests)
		m.requests = append(m.requests, rec)
		handler := m.handler
		if handler != nil && n < m.handlerFrom {
			handler = nil // the queue answers the scripted prefix
		}
		var resp mockResponse
		hasResp := false
		if len(m.responses) > 0 {
			resp = m.responses[0]
			if len(m.responses) > 1 {
				m.responses = m.responses[1:]
			}
			hasResp = true
		}
		m.mu.Unlock()

		if handler != nil {
			handler(w, r, n)
			return
		}
		if !hasResp {
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = io.WriteString(w, `{"error":{"code":501,"message":"no scripted response"}}`)
			return
		}
		for k, vs := range resp.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if resp.status != 0 {
			w.WriteHeader(resp.status)
		}
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// push queues a response. The last queued response is reused for any further
// requests.
func (m *mockUpstream) push(status int, body string) *mockUpstream {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, mockResponse{status: status, body: body})
	return m
}

func (m *mockUpstream) pushHeader(status int, body string, h http.Header) *mockUpstream {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, mockResponse{status: status, body: body, header: h})
	return m
}

// handle installs a custom handler, which takes precedence over the queue.
func (m *mockUpstream) handle(f func(w http.ResponseWriter, r *http.Request, n int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = f
}

// handleFrom installs a handler that takes over from the nth request onwards,
// leaving the queued responses to answer the ones before it. Multi-step
// protocols use it: a scripted handshake followed by an open-ended loop.
func (m *mockUpstream) handleFrom(after int, f func(w http.ResponseWriter, r *http.Request, n int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlerFrom = after
	m.handler = f
}

func (m *mockUpstream) calls() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *mockUpstream) callCount() int { return len(m.calls()) }

func (m *mockUpstream) lastCall() recordedRequest {
	c := m.calls()
	if len(c) == 0 {
		m.t.Fatal("upstream received no request")
	}
	return c[len(c)-1]
}

// client builds a Client wired to the mock, with instant backoff so retry
// tests do not sleep.
func (m *mockUpstream) client(opts ...Option) *Client {
	m.t.Helper()
	base := []Option{
		WithBaseURL(m.srv.URL + "/api/v1"),
		WithHTTPClient(m.srv.Client()),
		WithSleepFunc(func(context.Context, time.Duration) error { return nil }),
		WithRetryPolicy(RetryPolicy{Max: 3, Base: time.Millisecond, Cap: time.Millisecond,
			Jitter: func(d time.Duration) time.Duration { return d }}),
	}
	c, err := New(append(base, opts...)...)
	if err != nil {
		m.t.Fatalf("New: %v", err)
	}
	return c
}

// stubAuth is a minimal Authenticator for client tests.
type stubAuth struct {
	mu         sync.Mutex
	creds      Credentials
	credsErr   error
	refreshes  int
	refreshFn  func() (Credentials, error)
	refreshErr error
}

func (s *stubAuth) Credentials(context.Context) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creds, s.credsErr
}

func (s *stubAuth) Refresh(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	if s.refreshFn != nil {
		creds, err := s.refreshFn()
		if err != nil {
			return err
		}
		s.creds = creds
		return nil
	}
	return s.refreshErr
}

func (s *stubAuth) Identity() Identity {
	return Identity{Username: "stub", AuthMode: AuthModeOAuth2, Seamless: true}
}

func (s *stubAuth) AuthState() AuthState { return StateAuthenticated }

func (s *stubAuth) refreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// fakeClock is a controllable time source for token expiry tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// mustContain fails the test when haystack does not contain needle.
func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s: %q does not contain %q", what, haystack, needle)
	}
}

// mustNotContain fails the test when haystack contains needle.
func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s: %q must not contain %q", what, haystack, needle)
	}
}
