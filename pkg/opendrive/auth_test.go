package opendrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// oauthServer scripts a realistic upstream: it issues rolling tokens, rejects
// stale access tokens with the documented 401 body and rejects retired refresh
// tokens with invalid_grant (whitepaper §2.2 B).
type oauthServer struct {
	mu sync.Mutex

	access  string
	refresh string
	issued  int

	grants   int
	refreshs int
	calls    int

	expiresIn int
	password  string
}

func newOAuthServer() *oauthServer {
	return &oauthServer{password: "correct horse", expiresIn: 86400}
}

func (s *oauthServer) install(m *mockUpstream) {
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		s.mu.Lock()
		defer s.mu.Unlock()

		switch r.URL.Path {
		case "/api/v1/oauth2/grant.json":
			switch body["grant_type"] {
			case "password":
				s.grants++
				if body["password"] != s.password {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"Invalid username or password"}`))
					return
				}
				s.issue()
			case "refresh_token":
				s.refreshs++
				if body["refresh_token"] != s.refresh {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token is invalid or expired"}`))
					return
				}
				s.issue()
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unsupported_grant_type"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  s.access,
				"refresh_token": s.refresh,
				"token_type":    "Bearer",
				"expires_in":    s.expiresIn,
			})

		default: // any business endpoint
			s.calls++
			if r.URL.Query().Get("access_token") != s.access {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401,"error":"invalid_token","error_description":"The access token provided has expired"}}`))
				return
			}
			if r.URL.Query().Get("session_id") != OAuthSessionID {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"session_id must be OAUTH"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"UserName":"derek","StorageUsed":"1024"}`))
		}
	})
}

// issue rotates both tokens; the caller must hold s.mu.
func (s *oauthServer) issue() {
	s.issued++
	s.access = tokenName("access", s.issued)
	s.refresh = tokenName("refresh", s.issued)
}

func tokenName(kind string, n int) string {
	return kind + "-token-" + string(rune('0'+n%10)) + strings.Repeat("x", 8)
}

func (s *oauthServer) counts() (grants, refreshes, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants, s.refreshs, s.calls
}

func (s *oauthServer) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = "rotated-out-of-band"
}

// ---------------------------------------------------------------- login

func TestOAuth2LoginStoresTokens(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)

	clock := newFakeClock()
	c := m.client()
	store := NewMemoryTokenStore()
	auth := NewOAuth2(c, WithTokenStore(store), WithClock(clock.Now))
	c.SetAuthenticator(auth)

	if err := auth.Login(context.Background(), "derek", "correct horse"); err != nil {
		t.Fatalf("Login: %v", err)
	}

	tok, ok := auth.Token()
	if !ok || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token = %+v", tok)
	}
	if want := clock.Now().Add(DefaultAccessTokenTTL); !tok.Expiry.Equal(want) {
		t.Errorf("expiry = %v, want %v", tok.Expiry, want)
	}
	if want := clock.Now().Add(DefaultRefreshTokenTTL); !tok.RefreshExpiry.Equal(want) {
		t.Errorf("refresh expiry = %v, want %v", tok.RefreshExpiry, want)
	}
	if tok.Account != "derek" {
		t.Errorf("account = %q", tok.Account)
	}

	stored, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("the token was not persisted: %v", err)
	}
	if stored.AccessToken != tok.AccessToken {
		t.Error("the persisted token differs from the in-memory one")
	}

	// §9.2: the password must not be retained anywhere on the authenticator.
	if strings.Contains(tok.String(), "correct horse") {
		t.Fatal("the password leaked into the token")
	}
}

func TestOAuth2LoginRejectsBadCredentials(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)

	c := m.client()
	auth := NewOAuth2(c)
	err := auth.Login(context.Background(), "derek", "wrong")
	if err == nil {
		t.Fatal("expected an error")
	}
	if ErrorKind(err) != KindUnauthorized {
		t.Fatalf("kind = %q, want unauthorized", ErrorKind(err))
	}
	mustNotContain(t, err.Error(), "wrong", "login error")
	if _, ok := auth.Token(); ok {
		t.Fatal("a failed login must not leave a token behind")
	}
}

func TestOAuth2LoginRejectsAResponseWithoutAToken(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"token_type":"Bearer"}`)
	auth := NewOAuth2(m.client())
	if err := auth.Login(context.Background(), "u", "p"); ErrorKind(err) != KindInvalidResponse {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------- lifecycle

// P1 exit criterion: expiry, refresh and replay all work against a mock server.
func TestOAuth2FullTokenLifecycle(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)

	clock := newFakeClock()
	c := m.client()
	store := NewMemoryTokenStore()
	auth := NewOAuth2(c, WithTokenStore(store), WithClock(clock.Now))
	c.SetAuthenticator(auth)

	ctx := context.Background()
	if err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}
	first, _ := auth.Token()

	// 1. A call inside the token lifetime uses the token as-is.
	var info UserInfo
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if info.UserName != "derek" {
		t.Fatalf("info = %+v", info)
	}
	if _, refreshes, _ := srv.counts(); refreshes != 0 {
		t.Fatalf("no refresh was due, saw %d", refreshes)
	}

	// 2. Inside the refresh skew the client renews before sending anything.
	clock.Advance(DefaultAccessTokenTTL - DefaultRefreshSkew + time.Second)
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatalf("proactive refresh: %v", err)
	}
	_, refreshes, _ := srv.counts()
	if refreshes != 1 {
		t.Fatalf("proactive refreshes = %d, want 1", refreshes)
	}
	second, _ := auth.Token()
	if second.AccessToken == first.AccessToken {
		t.Fatal("the access token was not rotated")
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("§2.2 B: the refresh token must roll on every exchange")
	}
	stored, _ := store.Load(ctx)
	if stored.RefreshToken != second.RefreshToken {
		t.Fatal("§9.2: the rolled refresh token must be persisted")
	}

	// 3. A token invalidated server-side triggers the reactive path: one 401,
	//    one refresh, one replay.
	before := m.callCount()
	srv.expire()
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatalf("reactive refresh: %v", err)
	}
	if _, refreshes, _ = srv.counts(); refreshes != 2 {
		t.Fatalf("refreshes = %d, want 2", refreshes)
	}
	if got := m.callCount() - before; got != 3 { // 401, grant, replay
		t.Fatalf("the reactive path made %d calls, want 3", got)
	}

	// 4. When the refresh token itself is rejected, the user must log in again
	//    and the error says so.
	srv.mu.Lock()
	srv.refresh = "retired"
	srv.mu.Unlock()
	srv.expire()
	err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &info)
	if ErrorKind(err) != KindRefreshTokenFailed {
		t.Fatalf("err = %v, want refresh_token_failed", err)
	}
	if IsTemporary(err) {
		t.Fatal("a dead refresh token must not be retried")
	}
}

func TestOAuth2RefreshRequiresAUsableRefreshToken(t *testing.T) {
	m := newMockUpstream(t)
	clock := newFakeClock()
	auth := NewOAuth2(m.client(), WithClock(clock.Now))

	if err := auth.Refresh(context.Background()); ErrorKind(err) != KindUnauthorized {
		t.Fatalf("refresh without a login: %v", err)
	}

	// An expired refresh token fails locally, without burning a request.
	store := NewMemoryTokenStore()
	_ = store.Save(context.Background(), &Token{
		AccessToken:   "a",
		RefreshToken:  "r",
		Expiry:        clock.Now().Add(-time.Hour),
		RefreshExpiry: clock.Now().Add(-time.Minute),
	})
	auth2 := NewOAuth2(m.client(), WithTokenStore(store), WithClock(clock.Now))
	err := auth2.Refresh(context.Background())
	if ErrorKind(err) != KindRefreshTokenFailed {
		t.Fatalf("err = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatal("an expired refresh token must not reach the network")
	}
}

// §9.2: concurrent 401s must rotate the refresh token exactly once, otherwise
// the losers present a retired token and get logged out.
func TestConcurrentRefreshRotatesOnce(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)

	c := m.client()
	auth := NewOAuth2(c, WithClock(newFakeClock().Now))
	c.SetAuthenticator(auth)
	ctx := context.Background()
	if err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}
	srv.expire()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out UserInfo
			errs[i] = c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &out)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if _, refreshes, _ := srv.counts(); refreshes != 1 {
		t.Fatalf("refresh exchanges = %d, want exactly 1", refreshes)
	}
}

func TestRefreshStaleIgnoresAnAlreadyReplacedToken(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)
	c := m.client()
	auth := NewOAuth2(c, WithClock(newFakeClock().Now))
	c.SetAuthenticator(auth)
	if err := auth.Login(context.Background(), "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}

	if err := auth.RefreshStale(context.Background(), "some-older-token"); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if _, refreshes, _ := srv.counts(); refreshes != 0 {
		t.Fatalf("a stale trigger caused %d refreshes, want 0", refreshes)
	}

	current, _ := auth.Token()
	if err := auth.RefreshStale(context.Background(), current.AccessToken); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if _, refreshes, _ := srv.counts(); refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}

// §9.2: upstream has already rotated the credential, so a store failure must
// not cost the user their session.
func TestRefreshSurvivesAStoreFailure(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)

	store := &flakyStore{MemoryTokenStore: NewMemoryTokenStore()}
	c := m.client()
	auth := NewOAuth2(c, WithTokenStore(store), WithClock(newFakeClock().Now))
	c.SetAuthenticator(auth)

	store.failSave = true
	if err := auth.Login(context.Background(), "derek", "correct horse"); err != nil {
		t.Fatalf("a store failure must not fail the login: %v", err)
	}
	tok, ok := auth.Token()
	if !ok || tok.AccessToken == "" {
		t.Fatal("the session must survive in memory")
	}

	var info UserInfo
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatalf("call after an unpersisted login: %v", err)
	}
}

type flakyStore struct {
	*MemoryTokenStore
	failSave bool
	failLoad bool
}

func (s *flakyStore) Save(ctx context.Context, t *Token) error {
	if s.failSave {
		return errors.New("keyring is locked")
	}
	return s.MemoryTokenStore.Save(ctx, t)
}

func (s *flakyStore) Load(ctx context.Context) (*Token, error) {
	if s.failLoad {
		return nil, errors.New("keyring is locked")
	}
	return s.MemoryTokenStore.Load(ctx)
}

func TestCredentialsReportsAnUnreadableStore(t *testing.T) {
	m := newMockUpstream(t)
	store := &flakyStore{MemoryTokenStore: NewMemoryTokenStore(), failLoad: true}
	auth := NewOAuth2(m.client(), WithTokenStore(store))
	_, err := auth.Credentials(context.Background())
	if ErrorKind(err) != KindUnauthorized {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, err.Error(), "stored token", "store failure message")
}

func TestCredentialsResumesFromTheStore(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"UserName":"derek"}`)
	clock := newFakeClock()
	store := NewMemoryTokenStore()
	_ = store.Save(context.Background(), &Token{
		AccessToken:   "resumed-token",
		RefreshToken:  "resumed-refresh",
		Expiry:        clock.Now().Add(time.Hour),
		RefreshExpiry: clock.Now().Add(24 * time.Hour),
	})
	c := m.client()
	auth := NewOAuth2(c, WithTokenStore(store), WithClock(clock.Now))
	c.SetAuthenticator(auth)

	var info UserInfo
	if err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatal(err)
	}
	if got := m.lastCall().Query.Get("access_token"); got != "resumed-token" {
		t.Fatalf("access_token = %q, want the token from the store", got)
	}
}

func TestCredentialsWithoutALogin(t *testing.T) {
	m := newMockUpstream(t)
	auth := NewOAuth2(m.client())
	if _, err := auth.Credentials(context.Background()); ErrorKind(err) != KindUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestOAuth2Logout(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)
	c := m.client()
	store := NewMemoryTokenStore()
	auth := NewOAuth2(c, WithTokenStore(store))
	c.SetAuthenticator(auth)
	if err := auth.Login(context.Background(), "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.Token(); ok {
		t.Fatal("Logout must drop the token")
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrNoToken) {
		t.Fatal("Logout must clear the store")
	}
	if _, err := auth.Credentials(context.Background()); ErrorKind(err) != KindUnauthorized {
		t.Fatal("calls after a logout must be unauthorized")
	}
	// Logging out twice is not an error.
	if err := auth.Logout(context.Background()); err != nil {
		t.Fatalf("second logout: %v", err)
	}
}

func TestOAuth2OptionsAreDefensive(t *testing.T) {
	m := newMockUpstream(t)
	a := NewOAuth2(m.client(), WithTokenStore(nil), WithClientID(""), WithRefreshSkew(-1), WithClock(nil))
	if a.store == nil || a.clientID != DefaultClientID || a.skew != DefaultRefreshSkew || a.now == nil {
		t.Fatalf("invalid options were applied: %+v", a)
	}
	b := NewOAuth2(m.client(), WithClientID("Partner"), WithRefreshSkew(time.Minute))
	if b.clientID != "Partner" || b.skew != time.Minute {
		t.Fatalf("valid options were ignored: %+v", b)
	}
}

// ---------------------------------------------------------------- token

func TestTokenPredicatesAndRedaction(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tok := &Token{AccessToken: "access-secret-zzz", RefreshToken: "refresh-secret-zzz",
		Expiry: now.Add(time.Hour), RefreshExpiry: now.Add(24 * time.Hour)}

	if !tok.Valid(now) || tok.Valid(now.Add(2*time.Hour)) {
		t.Error("Valid")
	}
	if tok.NeedsRefresh(now, DefaultRefreshSkew) {
		t.Error("a fresh token does not need refreshing")
	}
	if !tok.NeedsRefresh(now.Add(56*time.Minute), DefaultRefreshSkew) {
		t.Error("a token inside the skew needs refreshing")
	}
	if !tok.RefreshUsable(now) || tok.RefreshUsable(now.Add(48*time.Hour)) {
		t.Error("RefreshUsable")
	}

	var nilTok *Token
	if nilTok.Valid(now) || !nilTok.NeedsRefresh(now, 0) || nilTok.RefreshUsable(now) {
		t.Error("a nil token must be treated as unusable")
	}
	if nilTok.String() != "Token(nil)" || nilTok.Clone() != nil {
		t.Error("nil token helpers")
	}

	// A token without an expiry never expires and never needs refreshing.
	noExpiry := &Token{AccessToken: "access-secret-zzz", RefreshToken: "refresh-secret-zzz"}
	if !noExpiry.Valid(now) || noExpiry.NeedsRefresh(now, DefaultRefreshSkew) || !noExpiry.RefreshUsable(now) {
		t.Error("a token without an expiry should stay usable")
	}

	// §9.4: printing a token must never reveal it.
	s := tok.String()
	mustNotContain(t, s, "access-secret-zzz", "token string")
	mustNotContain(t, s, "refresh-secret-zzz", "token string")
	mustContain(t, s, Redacted, "token string")

	clone := tok.Clone()
	clone.AccessToken = "changed"
	if tok.AccessToken != "access-secret-zzz" {
		t.Error("Clone must be a deep copy")
	}
}

func TestMemoryTokenStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryTokenStore()
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty store returned %v", err)
	}
	tok := &Token{AccessToken: "a"}
	if err := s.Save(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil || got.AccessToken != "a" {
		t.Fatalf("Load = %v, %v", got, err)
	}
	got.AccessToken = "mutated"
	again, _ := s.Load(ctx)
	if again.AccessToken != "a" {
		t.Fatal("the store handed out an aliased token")
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoToken) {
		t.Fatal("Delete did not clear the store")
	}
}

// ---------------------------------------------------------------- session

func TestSessionAuthLifecycle(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/v1/session/login.json":
			if body["passwd"] != "correct horse" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid username or password"}}`))
				return
			}
			if body["version"] != SessionLoginVersion {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"version is required"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"SessionID":"SID-abc","UserName":"derek","AccType":"1","FVersioning":1}`))
		case "/api/v1/session/exists.json":
			if body["session_id"] != "SID-abc" {
				_, _ = w.Write([]byte(`{"result":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":true}`))
		case "/api/v1/session/logout.json":
			_, _ = w.Write([]byte(`true`))
		default:
			_, _ = w.Write([]byte(`{"UserName":"derek"}`))
		}
	})

	c := m.client()
	auth := NewSessionAuth(c)
	c.SetAuthenticator(auth)
	ctx := context.Background()

	if _, err := auth.Credentials(ctx); ErrorKind(err) != KindUnauthorized {
		t.Fatalf("credentials before login: %v", err)
	}

	login, err := auth.Login(ctx, "derek", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if login.SessionID != "SID-abc" || auth.SessionID() != "SID-abc" {
		t.Fatalf("login = %+v", login)
	}
	if !auth.Info().FVersioning.Bool() {
		t.Error("the login response should be retained")
	}

	ok, err := auth.Exists(ctx)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}

	// A business call carries the real session id, not the OAuth marker.
	var info UserInfo
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/users/info.json"}, &info); err != nil {
		t.Fatal(err)
	}
	if got := m.lastCall().Query.Get("session_id"); got != "SID-abc" {
		t.Fatalf("session_id = %q", got)
	}
	if m.lastCall().Query.Has("access_token") {
		t.Fatal("session mode must not send an access token")
	}

	if err := auth.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if auth.SessionID() != "" {
		t.Fatal("Logout must forget the session")
	}
}

// §9.2: without the password there is nothing to refresh, so the user is told
// to log in again rather than being retried in a loop.
func TestSessionRefreshRequiresANewLogin(t *testing.T) {
	m := newMockUpstream(t)
	m.push(401, `{"error":{"code":401,"error":"invalid_token","error_description":"expired"}}`)
	c := m.client()
	auth := NewSessionAuth(c)
	c.SetAuthenticator(auth)

	m2 := newMockUpstream(t)
	m2.push(200, `{"SessionID":"SID-abc"}`)
	_ = m2

	err := auth.Refresh(context.Background())
	if ErrorKind(err) != KindUnauthorized {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, err.Error(), "log in again", "session refresh message")
}

func TestSessionLoginRejectsAResponseWithoutASessionID(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"UserName":"derek"}`)
	auth := NewSessionAuth(m.client())
	if _, err := auth.Login(context.Background(), "u", "p"); ErrorKind(err) != KindInvalidResponse {
		t.Fatalf("err = %v", err)
	}
}

// §2.6 #11: a captcha challenge is surfaced verbatim and never retried.
func TestSessionLoginSurfacesCaptcha(t *testing.T) {
	m := newMockUpstream(t)
	m.push(403, `{"error":{"code":403,"message":"Captcha required"}}`)
	auth := NewSessionAuth(m.client())
	_, err := auth.Login(context.Background(), "derek", "correct horse")
	if ErrorKind(err) != KindCaptchaRequired {
		t.Fatalf("err = %v", err)
	}
	if IsTemporary(err) {
		t.Fatal("a captcha challenge must not be retried")
	}
	if m.callCount() != 1 {
		t.Fatalf("login was attempted %d times", m.callCount())
	}
}

func TestSessionLoginOptions(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"SessionID":"SID"}`)
	auth := NewSessionAuth(m.client())
	if _, err := auth.Login(context.Background(), "derek", "pw",
		WithCaptchaResponse("solved"), WithPartnerID("partner-7")); err != nil {
		t.Fatal(err)
	}
	body := m.lastCall().Body
	if body["captcha_response"] != "solved" || body["partner_id"] != "partner-7" {
		t.Fatalf("body = %v", body)
	}
	if body["passwd"] != "pw" || body["version"] != SessionLoginVersion {
		t.Fatalf("body = %v", body)
	}
}

// §2.6 #1: captcharequired.json exists online but not in the PDF.
func TestCaptchaRequiredEndpoint(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"Required":"1","SiteKey":"6Lc-key"}`)
	c := m.client()
	got, err := CaptchaRequired(context.Background(), c, "derek")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Required.Bool() || got.SiteKey != "6Lc-key" {
		t.Fatalf("got %+v", got)
	}
	call := m.lastCall()
	if call.Query.Get("username") != "derek" {
		t.Fatalf("query = %v", call.Query)
	}
	if call.Query.Has("session_id") {
		t.Fatal("the captcha probe runs before any session exists")
	}

	m2 := newMockUpstream(t)
	m2.push(200, `{"Required":0}`)
	if _, err := CaptchaRequired(context.Background(), m2.client(), ""); err != nil {
		t.Fatal(err)
	}
	if m2.lastCall().Query.Has("username") {
		t.Fatal("an empty username must not be sent")
	}

	m3 := newMockUpstream(t)
	m3.push(500, `{"error":{"code":500}}`)
	if _, err := CaptchaRequired(context.Background(), m3.client(), "x"); err == nil {
		t.Fatal("expected an error")
	}
}

// ---------------------------------------------------------------- selection

func TestLoginPrefersOAuth2(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)
	c := m.client()

	auth, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "correct horse", NewMemoryTokenStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.(*OAuth2); !ok {
		t.Fatalf("authenticator = %T, want *OAuth2", auth)
	}
	grants, _, _ := srv.counts()
	if grants != 1 {
		t.Fatalf("grants = %d", grants)
	}
}

func TestLoginFallsBackToSessionWhenTheGrantEndpointIsMissing(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/api/v1/oauth2/grant.json":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Not found"}}`))
		case "/api/v1/session/login.json":
			_, _ = w.Write([]byte(`{"SessionID":"SID-fallback"}`))
		default:
			_, _ = w.Write([]byte(`true`))
		}
	})
	c := m.client()
	auth, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	sa, ok := auth.(*SessionAuth)
	if !ok {
		t.Fatalf("authenticator = %T, want *SessionAuth", auth)
	}
	if sa.SessionID() != "SID-fallback" {
		t.Fatalf("session = %q", sa.SessionID())
	}
}

// Bad credentials must not be retried in session mode: that would burn a second
// login attempt and move the account closer to a captcha lock (§2.6 #11).
func TestLoginDoesNotFallBackOnBadCredentials(t *testing.T) {
	m := newMockUpstream(t)
	srv := newOAuthServer()
	srv.install(m)
	c := m.client()

	_, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "wrong", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, _, calls := srv.counts(); calls != 0 {
		t.Fatal("no business call should have been made")
	}
	for _, call := range m.calls() {
		if strings.HasSuffix(call.Path, "/session/login.json") {
			t.Fatal("a rejected password must not be retried in session mode")
		}
	}
}

func TestLoginInSessionMode(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"SessionID":"SID-direct"}`)
	c := m.client()
	auth, err := Login(context.Background(), c, AuthModeSession, "derek", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.(*SessionAuth); !ok {
		t.Fatalf("authenticator = %T", auth)
	}
	if m.lastCall().Path != "/api/v1/session/login.json" {
		t.Fatalf("path = %q", m.lastCall().Path)
	}

	m2 := newMockUpstream(t)
	m2.push(401, `{"error":{"code":401,"message":"Invalid username or password"}}`)
	if _, err := Login(context.Background(), m2.client(), AuthModeSession, "derek", "pw", nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGrantUnsupportedDetection(t *testing.T) {
	yes := []error{
		&APIError{HTTPCode: http.StatusNotFound},
		&APIError{HTTPCode: http.StatusNotImplemented},
		&APIError{HTTPCode: http.StatusMethodNotAllowed},
		&APIError{Kind: KindInvalidResponse},
	}
	for _, e := range yes {
		if !grantUnsupported(e) {
			t.Errorf("%v should be treated as a missing grant endpoint", e)
		}
	}
	no := []error{
		&APIError{HTTPCode: http.StatusUnauthorized, Kind: KindUnauthorized},
		&APIError{Kind: KindCaptchaRequired, HTTPCode: 403},
		errors.New("plain"),
	}
	for _, e := range no {
		if grantUnsupported(e) {
			t.Errorf("%v must not trigger the session fallback", e)
		}
	}
}
