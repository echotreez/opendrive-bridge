package opendrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamSim is a mock OpenDrive that behaves the way the real one does for
// credentials: rolling refresh tokens, 401 invalid_token on a stale access
// token, invalid_grant on a retired refresh token, and a password that the test
// can change underneath the bridge (whitepaper §2.2).
type upstreamSim struct {
	mu sync.Mutex

	password string
	captcha  bool

	access   string
	refresh  string
	session  string
	issued   int
	sessions map[string]bool

	grants    int // password grants
	refreshes int // refresh grants
	logins    int // session logins
	business  int // calls to a business endpoint
}

func newUpstreamSim() *upstreamSim {
	return &upstreamSim{password: "correct horse", sessions: map[string]bool{}}
}

func (s *upstreamSim) install(m *mockUpstream) {
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/grant.json"):
			s.handleGrant(w, body)
		case strings.HasSuffix(r.URL.Path, "/session/login.json"):
			s.handleLogin(w, body)
		case strings.HasSuffix(r.URL.Path, "/session/exists.json"):
			ok := s.sessions[body["session_id"]]
			_, _ = fmt.Fprintf(w, `{"result":%t}`, ok)
		case strings.HasSuffix(r.URL.Path, "/session/logout.json"):
			delete(s.sessions, body["session_id"])
			_, _ = w.Write([]byte(`true`))
		default:
			s.handleBusiness(w, r)
		}
	})
}

func (s *upstreamSim) handleGrant(w http.ResponseWriter, body map[string]string) {
	switch body["grant_type"] {
	case "password":
		s.grants++
		if s.captcha {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Captcha required"}}`))
			return
		}
		if body["password"] != s.password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"Invalid username or password"}`))
			return
		}
		s.issue()
	case "refresh_token":
		s.refreshes++
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
		"expires_in":    86400,
	})
}

func (s *upstreamSim) handleLogin(w http.ResponseWriter, body map[string]string) {
	s.logins++
	if s.captcha {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Captcha required"}}`))
		return
	}
	if body["passwd"] != s.password {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid username or password"}}`))
		return
	}
	s.issued++
	s.session = fmt.Sprintf("SESSION-%02d-aaaaaaaa", s.issued)
	s.sessions[s.session] = true
	_, _ = fmt.Fprintf(w, `{"SessionID":%q,"UserName":"derek","UserID":"2125533","AccType":"1"}`, s.session)
}

func (s *upstreamSim) handleBusiness(w http.ResponseWriter, r *http.Request) {
	s.business++
	if tok := r.URL.Query().Get("access_token"); tok != "" {
		if tok != s.access {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"error":"invalid_token","error_description":"The access token provided has expired"}}`))
			return
		}
		if r.URL.Query().Get("session_id") != OAuthSessionID {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"session_id must be OAUTH"}}`))
			return
		}
	} else {
		sid := r.URL.Query().Get("session_id")
		if !s.sessions[sid] {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Session does not exist"}}`))
			return
		}
	}
	_, _ = w.Write([]byte(`{"UserName":"derek","StorageUsed":"1024"}`))
}

// issue rotates both tokens; the caller must hold s.mu.
func (s *upstreamSim) issue() {
	s.issued++
	s.access = fmt.Sprintf("access-%02d-xxxxxxxx", s.issued)
	s.refresh = fmt.Sprintf("refresh-%02d-xxxxxxxx", s.issued)
}

// expireAccessToken invalidates the access token without touching the refresh
// token, as upstream does after 24 hours.
func (s *upstreamSim) expireAccessToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = "rotated-out-of-band"
}

// retireRefreshToken makes the refresh grant fail, which forces the silent
// password re-login path.
func (s *upstreamSim) retireRefreshToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh = "retired"
}

// dropSession makes upstream forget every session, as it does on expiry.
func (s *upstreamSim) dropSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
}

// changePassword simulates the user changing the password elsewhere.
func (s *upstreamSim) changePassword(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = p
}

func (s *upstreamSim) requireCaptcha(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captcha = on
}

func (s *upstreamSim) counters() (grants, refreshes, logins, business int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants, s.refreshes, s.logins, s.business
}

// lockableStore is a CredentialStore that can be locked, standing in for a
// locked keyring or a Docker container without its secret mounted.
type lockableStore struct {
	mu       sync.Mutex
	cred     *StoredCredentials
	locked   bool
	saves    int
	failSave bool
}

func newLockableStore() *lockableStore { return &lockableStore{} }

func (s *lockableStore) Load(context.Context) (*StoredCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return nil, errors.New("keyring is locked")
	}
	if s.cred == nil {
		return nil, ErrNoCredentials
	}
	return s.cred.Clone(), nil
}

func (s *lockableStore) Save(_ context.Context, c *StoredCredentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked || s.failSave {
		return errors.New("keyring is locked")
	}
	s.saves++
	s.cred = c.Clone()
	return nil
}

func (s *lockableStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = nil
	return nil
}

func (s *lockableStore) lock(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locked = on
}

func (s *lockableStore) stored() *StoredCredentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred.Clone()
}

// oauthFixture wires a client, a simulated upstream, a store and a fake clock.
type oauthFixture struct {
	m     *mockUpstream
	sim   *upstreamSim
	store *lockableStore
	clock *fakeClock
	c     *Client
	auth  *OAuth2
}

func newOAuthFixture(t *testing.T, opts ...AuthOption) *oauthFixture {
	t.Helper()
	f := &oauthFixture{
		m:     newMockUpstream(t),
		sim:   newUpstreamSim(),
		store: newLockableStore(),
		clock: newFakeClock(),
	}
	f.sim.install(f.m)
	f.c = f.m.client()
	all := append([]AuthOption{WithCredentialStore(f.store), WithClock(f.clock.Now)}, opts...)
	f.auth = NewOAuth2(f.c, all...)
	f.c.SetAuthenticator(f.auth)
	return f
}

func (f *oauthFixture) login(t *testing.T) {
	t.Helper()
	if err := f.auth.Login(context.Background(), "derek", "correct horse"); err != nil {
		t.Fatalf("Login: %v", err)
	}
}

func (f *oauthFixture) call(ctx context.Context) error {
	var out UserInfo
	return f.c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointUsersInfo}, &out)
}

// ---------------------------------------------------------------- login

// §2.2 #1: one login stores everything needed to stay seamless forever.
func TestOAuth2LoginPersistsEveryCredential(t *testing.T) {
	f := newOAuthFixture(t)
	f.login(t)

	stored := f.store.stored()
	if stored == nil {
		t.Fatal("nothing was persisted")
	}
	if stored.Username != "derek" || stored.Password != "correct horse" {
		t.Fatalf("stored = %+v", stored)
	}
	if stored.Token == nil || stored.Token.AccessToken == "" || stored.Token.RefreshToken == "" {
		t.Fatal("the token pair was not persisted")
	}
	if stored.AuthMode != AuthModeOAuth2 {
		t.Fatalf("auth mode = %q", stored.AuthMode)
	}
	if stored.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not set")
	}

	if got := f.auth.AuthState(); got != StateAuthenticated {
		t.Fatalf("state = %q", got)
	}
	id := f.auth.Identity()
	if !id.Configured() || id.Username != "derek" || id.AuthMode != AuthModeOAuth2 || !id.Seamless {
		t.Fatalf("identity = %+v", id)
	}

	// §9.2: printing the record must not reveal anything.
	s := stored.String()
	mustNotContain(t, s, "correct horse", "StoredCredentials.String")
	mustNotContain(t, s, "derek", "StoredCredentials.String")
	mustContain(t, s, "password=yes", "StoredCredentials.String")
}

// §9.2: persist_password:false is the escape hatch, and it costs seamlessness.
func TestPersistPasswordFalseKeepsNoPassword(t *testing.T) {
	f := newOAuthFixture(t, WithPersistPassword(false))
	f.login(t)

	if stored := f.store.stored(); stored.Password != "" {
		t.Fatal("the password was stored despite persist_password:false")
	}
	if id := f.auth.Identity(); id.Seamless {
		t.Fatal("seamless must be false without a stored password")
	}

	// Without a password the refresh token is the only lifeline: once upstream
	// retires it the user has to log in again.
	f.sim.retireRefreshToken()
	f.sim.expireAccessToken()
	f.clock.Advance(DefaultAccessTokenTTL)
	err := f.call(context.Background())
	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v, want reauth_required", err)
	}
}

func TestOAuth2LoginRejectsBadCredentials(t *testing.T) {
	f := newOAuthFixture(t)
	err := f.auth.Login(context.Background(), "derek", "wrong")
	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("kind = %q, want reauth_required", ErrorKind(err))
	}
	mustNotContain(t, err.Error(), "wrong", "login error")
	if f.store.stored() != nil {
		t.Fatal("a failed login must not persist anything")
	}
}

// ---------------------------------------------------------------- lifecycle

// P1 exit criterion, the happy path: expiry, proactive refresh, reactive
// refresh with replay, and rolling tokens persisted every time.
func TestTokenLifecycleStaysSilent(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)
	first, _ := f.auth.Token()

	if err := f.call(ctx); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 0 {
		t.Fatalf("no refresh was due, saw %d", refreshes)
	}

	// Inside the skew the client renews before sending anything.
	f.clock.Advance(DefaultAccessTokenTTL - DefaultRefreshSkew + time.Second)
	if err := f.call(ctx); err != nil {
		t.Fatalf("proactive refresh: %v", err)
	}
	_, refreshes, _, _ := f.sim.counters()
	if refreshes != 1 {
		t.Fatalf("proactive refreshes = %d, want 1", refreshes)
	}
	second, _ := f.auth.Token()
	if second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatal("§2.2 B: both tokens must roll on every exchange")
	}
	if f.store.stored().Token.RefreshToken != second.RefreshToken {
		t.Fatal("§9.2: the rolled refresh token must be persisted")
	}

	// A token invalidated server-side: one 401, one refresh, one replay.
	before := f.m.callCount()
	f.sim.expireAccessToken()
	if err := f.call(ctx); err != nil {
		t.Fatalf("reactive refresh: %v", err)
	}
	if got := f.m.callCount() - before; got != 3 {
		t.Fatalf("the reactive path made %d calls, want 3", got)
	}
	if got := f.auth.AuthState(); got != StateAuthenticated {
		t.Fatalf("state = %q", got)
	}
}

// §2.2 #3: a dead refresh token is not the user's problem — the stored password
// buys a new one without anybody noticing.
func TestSilentReloginWhenTheRefreshTokenDies(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	f.sim.retireRefreshToken()
	f.sim.expireAccessToken()
	f.clock.Advance(DefaultAccessTokenTTL)

	if err := f.call(ctx); err != nil {
		t.Fatalf("the bridge failed to recover silently: %v", err)
	}
	grants, refreshes, _, _ := f.sim.counters()
	if refreshes < 1 {
		t.Fatal("the refresh grant should have been attempted first")
	}
	if grants != 2 { // the initial login plus the silent re-login
		t.Fatalf("password grants = %d, want 2", grants)
	}
	if got := f.auth.AuthState(); got != StateAuthenticated {
		t.Fatalf("state = %q", got)
	}
	// The new pair is persisted, so a restart stays seamless too.
	if f.store.stored().Token.AccessToken == "" {
		t.Fatal("the re-login token was not persisted")
	}
}

// The same recovery must work when the refresh token expired locally, without
// wasting a request on a grant that cannot succeed.
func TestSilentReloginWhenTheRefreshTokenExpiredLocally(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	f.clock.Advance(DefaultRefreshTokenTTL + time.Hour)
	f.sim.expireAccessToken()
	before := f.m.callCount()

	if err := f.call(ctx); err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	_, refreshes, _, _ := f.sim.counters()
	if refreshes != 0 {
		t.Fatal("an expired refresh token must not be sent upstream")
	}
	if got := f.m.callCount() - before; got != 2 { // grant + the business call
		t.Fatalf("calls = %d, want 2", got)
	}
}

// §2.2 #4a: the one case the user must be told about.
func TestPasswordChangedStopsEverything(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	f.sim.changePassword("something else")
	f.sim.retireRefreshToken()
	f.sim.expireAccessToken()
	f.clock.Advance(DefaultAccessTokenTTL)

	err := f.call(ctx)
	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v, want reauth_required", err)
	}
	if got := f.auth.AuthState(); got != StateReauthRequired {
		t.Fatalf("state = %q", got)
	}
	if IsTemporary(err) {
		t.Fatal("reauth_required must never be retried automatically")
	}

	// Every later call fails fast: no further upstream traffic at all, which is
	// what keeps the account away from a captcha lock.
	frozen := f.m.callCount()
	for i := 0; i < 5; i++ {
		if err := f.call(ctx); ErrorKind(err) != KindReauthRequired {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if f.m.callCount() != frozen {
		t.Fatalf("%d upstream calls were made after reauth_required", f.m.callCount()-frozen)
	}

	// The user supplies the new password: everything resumes.
	if err := f.auth.Login(ctx, "derek", "something else"); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if got := f.auth.AuthState(); got != StateAuthenticated {
		t.Fatalf("state after re-login = %q", got)
	}
	if err := f.call(ctx); err != nil {
		t.Fatalf("call after re-login: %v", err)
	}
}

// §2.2 #4b: a captcha challenge is equally terminal.
func TestCaptchaStopsEverything(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	f.sim.requireCaptcha(true)
	f.sim.retireRefreshToken()
	f.sim.expireAccessToken()
	f.clock.Advance(DefaultAccessTokenTTL)

	err := f.call(ctx)
	if ErrorKind(err) != KindCaptchaRequired {
		t.Fatalf("err = %v, want captcha_required", err)
	}
	if got := f.auth.AuthState(); got != StateCaptchaRequired {
		t.Fatalf("state = %q", got)
	}
	frozen := f.m.callCount()
	for i := 0; i < 3; i++ {
		_ = f.call(ctx)
	}
	if f.m.callCount() != frozen {
		t.Fatal("the bridge kept hammering upstream while a captcha was pending")
	}
}

// §4.5: a locked credential store makes zero upstream requests, and recovers on
// its own once the keyring is unlocked.
func TestKeystoreUnavailableMakesNoUpstreamRequest(t *testing.T) {
	ctx := context.Background()
	f := newOAuthFixture(t)
	f.login(t)

	// A fresh authenticator, as after a daemon restart, against a locked store.
	f.store.lock(true)
	auth := NewOAuth2(f.c, WithCredentialStore(f.store), WithClock(f.clock.Now))
	f.c.SetAuthenticator(auth)

	before := f.m.callCount()
	err := f.call(ctx)
	if ErrorKind(err) != KindKeystoreUnavailable {
		t.Fatalf("err = %v, want keystore_unavailable", err)
	}
	if !errors.Is(err, ErrKeystoreUnavailable) {
		t.Fatal("errors.Is(err, ErrKeystoreUnavailable) = false")
	}
	if auth.AuthState() != StateKeystoreUnavailable {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if f.m.callCount() != before {
		t.Fatalf("%d upstream calls were made with an unreadable store", f.m.callCount()-before)
	}
	if id := auth.Identity(); id.Configured() {
		t.Fatal("identity must be empty while the store is unreadable")
	}

	// Unlocking recovers without any user action.
	f.store.lock(false)
	if err := f.call(ctx); err != nil {
		t.Fatalf("after unlocking: %v", err)
	}
	if auth.AuthState() != StateAuthenticated {
		t.Fatalf("state = %q", auth.AuthState())
	}
}

// §2.2 #3: transient failures back off instead of looping.
func TestTransientRenewalFailureBacksOff(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	clock := newFakeClock()
	store := newLockableStore()

	// Upstream is down for everything.
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
	})
	c := m.client()
	_ = store.Save(ctx, &StoredCredentials{
		Username: "derek", Password: "correct horse", AuthMode: AuthModeOAuth2,
		Token: &Token{
			AccessToken: "stale", RefreshToken: "r",
			IssuedAt: clock.Now().Add(-2 * time.Hour),
			Expiry:   clock.Now().Add(-time.Hour), RefreshExpiry: clock.Now().Add(24 * time.Hour),
		},
	})
	auth := NewOAuth2(c, WithCredentialStore(store), WithClock(clock.Now))
	c.SetAuthenticator(auth)

	if _, err := auth.Credentials(ctx); err == nil {
		t.Fatal("expected a failure")
	}
	after := m.callCount()
	if after == 0 {
		t.Fatal("the first attempt should have reached upstream")
	}
	if auth.AuthState() != StateRefreshing {
		t.Fatalf("state = %q, want refreshing while backing off", auth.AuthState())
	}

	// Immediately afterwards, the gate is closed: no traffic at all.
	for i := 0; i < 3; i++ {
		if _, err := auth.Credentials(ctx); err == nil {
			t.Fatal("expected the cached failure")
		}
	}
	if m.callCount() != after {
		t.Fatalf("%d calls slipped through the backoff", m.callCount()-after)
	}

	// Once the backoff elapses it tries again.
	clock.Advance(maxRenewBackoff)
	if _, err := auth.Credentials(ctx); err == nil {
		t.Fatal("expected another failure")
	}
	if m.callCount() == after {
		t.Fatal("the backoff never reopened")
	}
}

// §9.2: concurrent 401s must rotate the refresh token exactly once.
func TestConcurrentRenewalRotatesOnce(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)
	f.sim.expireAccessToken()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.call(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 1 {
		t.Fatalf("refresh exchanges = %d, want exactly 1", refreshes)
	}
}

// §2.2 #2: the keep-alive timer rolls an idle refresh token before it dies.
func TestEnsureFreshRollsAnIdleToken(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	// Nothing is due yet.
	if err := f.auth.EnsureFresh(ctx); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0 when nothing is due", refreshes)
	}

	// After the renew interval it rolls, even with no traffic at all.
	f.clock.Advance(DefaultRenewInterval + time.Minute)
	before, _ := f.auth.Token()
	if err := f.auth.EnsureFresh(ctx); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	after, _ := f.auth.Token()
	if after.RefreshToken == before.RefreshToken {
		t.Fatal("the idle refresh token was not rolled")
	}
	if f.store.stored().Token.RefreshToken != after.RefreshToken {
		t.Fatal("the rolled token was not persisted")
	}
}

func TestEnsureFreshOnAnUnconfiguredBridge(t *testing.T) {
	m := newMockUpstream(t)
	auth := NewOAuth2(m.client(), WithCredentialStore(newLockableStore()))
	if err := auth.EnsureFresh(context.Background()); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v", err)
	}
	if auth.AuthState() != StateNotConfigured {
		t.Fatalf("state = %q", auth.AuthState())
	}
}

// §2.2 #1: a restart resumes from the store with no password and no network.
func TestResumeFromStore(t *testing.T) {
	ctx := context.Background()
	f := newOAuthFixture(t)
	f.login(t)

	calls := f.m.callCount()
	auth, err := Resume(ctx, f.c, f.store)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if f.m.callCount() != calls {
		t.Fatal("Resume must not touch the network")
	}
	if auth.AuthState() != StateAuthenticated {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if id := auth.Identity(); id.Username != "derek" || !id.Seamless {
		t.Fatalf("identity = %+v", id)
	}
	if err := f.call(ctx); err != nil {
		t.Fatalf("call after resume: %v", err)
	}
}

func TestResumeWithoutStoredCredentials(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	c := m.client()
	auth, err := Resume(ctx, c, newLockableStore())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if auth.AuthState() != StateNotConfigured {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if auth.Identity().Configured() {
		t.Fatal("identity must be empty")
	}
	if _, err := auth.Credentials(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v", err)
	}

	if _, err := Resume(ctx, c, nil); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("Resume(nil store) = %v", err)
	}
}

func TestResumeReportsALockedStore(t *testing.T) {
	store := newLockableStore()
	store.lock(true)
	m := newMockUpstream(t)
	_, err := Resume(context.Background(), m.client(), store)
	if ErrorKind(err) != KindKeystoreUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestResumePicksSessionModeFromTheStore(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	store := newLockableStore()
	_ = store.Save(ctx, &StoredCredentials{
		Username: "derek", Password: "correct horse",
		SessionID: "SESSION-01-aaaaaaaa", AuthMode: AuthModeSession,
	})

	auth, err := Resume(ctx, c, store)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, ok := auth.(*SessionAuth); !ok {
		t.Fatalf("authenticator = %T, want *SessionAuth", auth)
	}
	if auth.Identity().AuthMode != AuthModeSession {
		t.Fatalf("identity = %+v", auth.Identity())
	}
}

// §9.2: a store that cannot be written must not cost the user their session,
// because upstream has already rotated the credential.
func TestRotationSurvivesAStoreWriteFailure(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := context.Background()
	f.login(t)

	f.store.mu.Lock()
	f.store.failSave = true
	f.store.mu.Unlock()

	f.sim.expireAccessToken()
	if err := f.call(ctx); err != nil {
		t.Fatalf("a store write failure broke the call: %v", err)
	}
	if f.auth.AuthState() != StateAuthenticated {
		t.Fatalf("state = %q", f.auth.AuthState())
	}
}

func TestOAuth2Logout(t *testing.T) {
	ctx := context.Background()
	f := newOAuthFixture(t)
	f.login(t)
	if err := f.auth.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.auth.Token(); ok {
		t.Fatal("Logout must drop the token")
	}
	if f.store.stored() != nil {
		t.Fatal("Logout must clear the store")
	}
	if f.auth.AuthState() != StateNotConfigured {
		t.Fatalf("state = %q", f.auth.AuthState())
	}
	if _, err := f.auth.Credentials(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshStaleIgnoresAnAlreadyReplacedToken(t *testing.T) {
	ctx := context.Background()
	f := newOAuthFixture(t)
	f.login(t)

	if err := f.auth.RefreshStale(ctx, "some-older-token"); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 0 {
		t.Fatalf("a stale trigger caused %d refreshes, want 0", refreshes)
	}

	current, _ := f.auth.Token()
	if err := f.auth.RefreshStale(ctx, current.AccessToken); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}

func TestRefreshIsCallableDirectly(t *testing.T) {
	ctx := context.Background()
	f := newOAuthFixture(t)

	// Before any login it reports that the bridge is not configured.
	if err := f.auth.Refresh(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("Refresh on an empty bridge = %v", err)
	}

	f.login(t)
	if err := f.auth.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, refreshes, _, _ := f.sim.counters(); refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if f.auth.AuthState() != StateAuthenticated {
		t.Fatalf("state = %q", f.auth.AuthState())
	}
}

func TestSessionInfoIsRetained(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	auth := NewSessionAuth(c, WithCredentialStore(newLockableStore()))
	if info := auth.Info(); info.SessionID != "" {
		t.Fatal("there is nothing to report before a login")
	}
	if _, err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}
	info := auth.Info()
	if info.UserName != "derek" || info.AccType.Int() != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestAuthOptionsAreDefensive(t *testing.T) {
	m := newMockUpstream(t)
	a := NewOAuth2(m.client(), WithCredentialStore(nil), WithClientID(""),
		WithRefreshSkew(-1), WithRenewInterval(0), WithClock(nil))
	if a.cfg.store == nil || a.cfg.clientID != DefaultClientID ||
		a.cfg.skew != DefaultRefreshSkew || a.cfg.renewInterval != DefaultRenewInterval || a.cfg.now == nil {
		t.Fatalf("invalid options were applied: %+v", a.cfg)
	}
	b := NewOAuth2(m.client(), WithClientID("Partner"), WithRefreshSkew(time.Minute),
		WithRenewInterval(time.Hour))
	if b.cfg.clientID != "Partner" || b.cfg.skew != time.Minute || b.cfg.renewInterval != time.Hour {
		t.Fatalf("valid options were ignored: %+v", b.cfg)
	}
}

// ---------------------------------------------------------------- token

func TestTokenPredicatesAndRedaction(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tok := &Token{AccessToken: "access-secret-zzz", RefreshToken: "refresh-secret-zzz",
		IssuedAt: now, Expiry: now.Add(time.Hour), RefreshExpiry: now.Add(24 * time.Hour)}

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

	// §2.2 #2: half-life and renew interval both force a roll.
	if tok.StaleForRenewal(now, DefaultRenewInterval) {
		t.Error("a brand new token is not stale")
	}
	if !tok.StaleForRenewal(now.Add(31*time.Minute), DefaultRenewInterval) {
		t.Error("past the half-life the token is stale")
	}
	longLived := &Token{AccessToken: "a", IssuedAt: now}
	if !longLived.StaleForRenewal(now.Add(8*24*time.Hour), DefaultRenewInterval) {
		t.Error("past the renew interval the token is stale")
	}

	var nilTok *Token
	if nilTok.Valid(now) || !nilTok.NeedsRefresh(now, 0) || nilTok.RefreshUsable(now) ||
		!nilTok.StaleForRenewal(now, 0) {
		t.Error("a nil token must be treated as unusable")
	}
	if nilTok.String() != "Token(nil)" || nilTok.Clone() != nil {
		t.Error("nil token helpers")
	}

	s := tok.String()
	mustNotContain(t, s, "access-secret-zzz", "token string")
	mustNotContain(t, s, "refresh-secret-zzz", "token string")
	mustContain(t, s, Redacted, "token string")

	clone := tok.Clone()
	clone.AccessToken = "changed"
	if tok.AccessToken != "access-secret-zzz" {
		t.Error("Clone must be a deep copy")
	}

	var nilCred *StoredCredentials
	if nilCred.String() != "StoredCredentials(nil)" || nilCred.Clone() != nil {
		t.Error("nil credential helpers")
	}
	empty := (&StoredCredentials{}).String()
	mustContain(t, empty, "password=no", "empty credentials")
}

func TestMemoryTokenStoreIsEphemeral(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryTokenStore()
	if !s.Ephemeral() {
		t.Fatal("§9.2: the in-memory store must announce itself as ephemeral")
	}
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty store returned %v", err)
	}
	cred := &StoredCredentials{Username: "derek", Token: &Token{AccessToken: "a"}}
	if err := s.Save(ctx, cred); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil || got.Username != "derek" {
		t.Fatalf("Load = %v, %v", got, err)
	}
	got.Token.AccessToken = "mutated"
	again, _ := s.Load(ctx)
	if again.Token.AccessToken != "a" {
		t.Fatal("the store handed out an aliased token")
	}
	if err := s.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatal("Delete did not clear the store")
	}
}

// ---------------------------------------------------------------- session

// §2.2 #5: session mode is seamless too — an expired session is rebuilt from
// the stored password without involving the user.
func TestSessionModeSilentlyRebuildsTheSession(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	clock := newFakeClock()
	store := newLockableStore()
	c := m.client()
	auth := NewSessionAuth(c, WithCredentialStore(store), WithClock(clock.Now))
	c.SetAuthenticator(auth)

	login, err := auth.Login(ctx, "derek", "correct horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if login.SessionID == "" || auth.SessionID() != login.SessionID {
		t.Fatalf("login = %+v", login)
	}
	stored := store.stored()
	if stored.SessionID != login.SessionID || stored.Password != "correct horse" ||
		stored.AuthMode != AuthModeSession {
		t.Fatalf("stored = %+v", stored)
	}
	id := auth.Identity()
	if id.Username != "derek" || id.UserID != "2125533" || id.AccType != 1 ||
		id.AuthMode != AuthModeSession || !id.Seamless {
		t.Fatalf("identity = %+v", id)
	}

	var out UserInfo
	call := func() error {
		return c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointUsersInfo}, &out)
	}
	if err := call(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if m.lastCall().Query.Has("access_token") {
		t.Fatal("session mode must not send an access token")
	}

	// Upstream forgets the session: the bridge rebuilds it silently.
	sim.dropSession()
	if err := call(); err != nil {
		t.Fatalf("silent session rebuild failed: %v", err)
	}
	if _, _, logins, _ := sim.counters(); logins != 2 {
		t.Fatalf("logins = %d, want the original plus one silent rebuild", logins)
	}
	if auth.SessionID() == login.SessionID {
		t.Fatal("the session id was not replaced")
	}
	if store.stored().SessionID != auth.SessionID() {
		t.Fatal("the new session id was not persisted")
	}
	if auth.AuthState() != StateAuthenticated {
		t.Fatalf("state = %q", auth.AuthState())
	}
}

func TestSessionModePasswordChangeStopsEverything(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	store := newLockableStore()
	auth := NewSessionAuth(c, WithCredentialStore(store), WithClock(newFakeClock().Now))
	c.SetAuthenticator(auth)
	if _, err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}

	sim.changePassword("new one")
	sim.dropSession()

	var out UserInfo
	err := c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointUsersInfo}, &out)
	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v, want reauth_required", err)
	}
	if auth.AuthState() != StateReauthRequired {
		t.Fatalf("state = %q", auth.AuthState())
	}
	frozen := m.callCount()
	for i := 0; i < 3; i++ {
		_ = c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointUsersInfo}, &out)
	}
	if m.callCount() != frozen {
		t.Fatal("the bridge kept trying after the password was rejected")
	}
}

func TestSessionEnsureFreshValidatesAndRebuilds(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	auth := NewSessionAuth(c, WithCredentialStore(newLockableStore()), WithClock(newFakeClock().Now))
	c.SetAuthenticator(auth)
	if _, err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}

	// A live session is only verified, not rebuilt.
	if err := auth.EnsureFresh(ctx); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if _, _, logins, _ := sim.counters(); logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
	ok, err := auth.Exists(ctx)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}

	// A dead one is rebuilt on the spot.
	sim.dropSession()
	if err := auth.EnsureFresh(ctx); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if _, _, logins, _ := sim.counters(); logins != 2 {
		t.Fatalf("logins = %d, want 2", logins)
	}
}

func TestSessionAuthWithoutCredentials(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	auth := NewSessionAuth(m.client(), WithCredentialStore(newLockableStore()))
	if _, err := auth.Credentials(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v", err)
	}
	if auth.AuthState() != StateNotConfigured {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if auth.Identity().Configured() || auth.SessionID() != "" {
		t.Fatal("nothing should be configured")
	}
	if err := auth.Refresh(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("Refresh = %v", err)
	}
	if err := auth.EnsureFresh(ctx); ErrorKind(err) != KindReauthRequired {
		t.Fatalf("EnsureFresh = %v", err)
	}
	if ok, err := auth.Exists(ctx); ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
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

func TestSessionLogoutClearsEverything(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	store := newLockableStore()
	auth := NewSessionAuth(c, WithCredentialStore(store))
	c.SetAuthenticator(auth)
	if _, err := auth.Login(ctx, "derek", "correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := auth.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if auth.SessionID() != "" || store.stored() != nil {
		t.Fatal("Logout must clear both memory and the store")
	}
	if auth.AuthState() != StateNotConfigured {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if err := auth.Logout(ctx); err != nil {
		t.Fatalf("second logout: %v", err)
	}
}

func TestSessionKeystoreUnavailable(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	store := newLockableStore()
	store.lock(true)
	c := m.client()
	auth := NewSessionAuth(c, WithCredentialStore(store))
	c.SetAuthenticator(auth)

	if _, err := auth.Credentials(ctx); ErrorKind(err) != KindKeystoreUnavailable {
		t.Fatalf("err = %v", err)
	}
	if auth.AuthState() != StateKeystoreUnavailable {
		t.Fatalf("state = %q", auth.AuthState())
	}
	if m.callCount() != 0 {
		t.Fatal("a locked store must produce no upstream traffic")
	}
}

// §2.6 #1: captcharequired.json exists online but not in the PDF.
func TestCaptchaRequiredEndpoint(t *testing.T) {
	m := newMockUpstream(t)
	m.push(200, `{"Required":"1","SiteKey":"6Lc-key"}`)
	got, err := CaptchaRequired(context.Background(), m.client(), "derek")
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
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	store := newLockableStore()

	auth, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "correct horse", store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.(*OAuth2); !ok {
		t.Fatalf("authenticator = %T, want *OAuth2", auth)
	}
	if grants, _, _, _ := sim.counters(); grants != 1 {
		t.Fatalf("grants = %d", grants)
	}
	if store.stored() == nil {
		t.Fatal("Login must persist the credentials")
	}
}

func TestLoginFallsBackToSessionWhenTheGrantEndpointIsMissing(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/grant.json"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Not found"}}`))
		case strings.HasSuffix(r.URL.Path, "/session/login.json"):
			_, _ = w.Write([]byte(`{"SessionID":"SID-fallback"}`))
		default:
			_, _ = w.Write([]byte(`true`))
		}
	})
	c := m.client()
	store := newLockableStore()
	auth, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "pw", store)
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
	// §2.2 #5: the fallback is persisted too, so it is just as seamless.
	if stored := store.stored(); stored == nil || stored.AuthMode != AuthModeSession || stored.Password != "pw" {
		t.Fatalf("stored = %+v", store.stored())
	}
}

// Bad credentials must not be retried in session mode: that would burn a second
// login attempt and move the account closer to a captcha lock (§2.6 #11).
func TestLoginDoesNotFallBackOnBadCredentials(t *testing.T) {
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()

	_, err := Login(context.Background(), c, AuthModeOAuth2, "derek", "wrong", newLockableStore())
	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("err = %v", err)
	}
	if _, _, logins, _ := sim.counters(); logins != 0 {
		t.Fatal("a rejected password must not be retried in session mode")
	}
}

func TestLoginInSessionMode(t *testing.T) {
	m := newMockUpstream(t)
	sim := newUpstreamSim()
	sim.install(m)
	c := m.client()
	auth, err := Login(context.Background(), c, AuthModeSession, "derek", "correct horse", newLockableStore())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := auth.(*SessionAuth); !ok {
		t.Fatalf("authenticator = %T", auth)
	}

	m2 := newMockUpstream(t)
	sim2 := newUpstreamSim()
	sim2.install(m2)
	if _, err := Login(context.Background(), m2.client(), AuthModeSession, "derek", "wrong", nil); err == nil {
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
		&APIError{HTTPCode: http.StatusUnauthorized, Kind: KindReauthRequired},
		&APIError{Kind: KindCaptchaRequired, HTTPCode: 403},
		errors.New("plain"),
	}
	for _, e := range no {
		if grantUnsupported(e) {
			t.Errorf("%v must not trigger the session fallback", e)
		}
	}
}

// The renew gate is the safety valve; its arithmetic deserves its own test.
func TestRenewGateBackoffGrowsAndCaps(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	var g renewGate
	err := &APIError{Kind: KindNetwork}

	prev := time.Duration(0)
	for i := 0; i < 12; i++ {
		g.backoff(now, err)
		wait := g.nextAttempt.Sub(now)
		if wait > maxRenewBackoff {
			t.Fatalf("attempt %d waits %v, over the cap", i, wait)
		}
		if wait < prev {
			t.Fatalf("attempt %d waits %v, less than the previous %v", i, wait, prev)
		}
		prev = wait
		if g.state != StateRefreshing {
			t.Fatalf("state = %q", g.state)
		}
	}
	if g.blocked(now) == nil {
		t.Fatal("the gate should be closed right after a backoff")
	}
	if g.blocked(now.Add(maxRenewBackoff+time.Second)) != nil {
		t.Fatal("the gate should reopen once the wait elapses")
	}

	g.succeed()
	if g.state != StateAuthenticated || g.blocked(now) != nil || g.attempts != 0 {
		t.Fatalf("succeed left %+v", g)
	}

	g.terminal(StateReauthRequired, reauthError("nope"))
	if g.blocked(now.Add(365*24*time.Hour)) == nil {
		t.Fatal("a terminal state never reopens on its own")
	}
	g.reset()
	if g.blocked(now) != nil {
		t.Fatal("reset must reopen the gate")
	}
}
