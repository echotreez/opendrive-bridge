package opendrive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Authentication defaults (whitepaper §2.2 B).
const (
	// DefaultClientID is the client_id upstream expects for the simplified
	// resource-owner password credentials flow.
	DefaultClientID = "OpenDrive"
	// DefaultAccessTokenTTL is the documented access token lifetime, used when
	// the grant response omits expires_in.
	DefaultAccessTokenTTL = 86400 * time.Second
	// DefaultRefreshTokenTTL is the documented refresh token lifetime.
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour
	// DefaultRefreshSkew is how long before expiry the client refreshes
	// proactively.
	DefaultRefreshSkew = 5 * time.Minute
	// SessionLoginVersion is the "version" field the login endpoint expects.
	SessionLoginVersion = "10"
)

// ErrNoToken is returned by a TokenStore that holds nothing yet.
var ErrNoToken = errors.New("opendrive: no stored token")

// Token is an OAuth2 credential pair. Its String method is deliberately
// redacted so that a stray log statement cannot leak it (§9.4).
type Token struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	Expiry        time.Time `json:"expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry"`
	Account       string    `json:"account,omitempty"`
}

// Valid reports whether the access token can still be used at now.
func (t *Token) Valid(now time.Time) bool {
	return t != nil && t.AccessToken != "" && (t.Expiry.IsZero() || now.Before(t.Expiry))
}

// NeedsRefresh reports whether the access token expires within skew.
func (t *Token) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if t.Expiry.IsZero() {
		return false
	}
	return !now.Before(t.Expiry.Add(-skew))
}

// RefreshUsable reports whether the refresh token can still be exchanged.
func (t *Token) RefreshUsable(now time.Time) bool {
	return t != nil && t.RefreshToken != "" && (t.RefreshExpiry.IsZero() || now.Before(t.RefreshExpiry))
}

// String implements fmt.Stringer without revealing the secrets.
func (t *Token) String() string {
	if t == nil {
		return "Token(nil)"
	}
	return "Token(access=" + Redacted + ", refresh=" + Redacted + ", expiry=" + t.Expiry.Format(time.RFC3339) + ")"
}

// Clone returns a copy of the token.
func (t *Token) Clone() *Token {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// TokenStore persists OAuth2 tokens. Implementations live in internal/keystore
// (OS keyring, encrypted file fallback); the SDK only needs this contract
// (§9.2).
type TokenStore interface {
	// Load returns the stored token, or ErrNoToken when there is none.
	Load(ctx context.Context) (*Token, error)
	// Save persists the token, replacing any previous one. It must be atomic:
	// a crash may never leave the store without a usable refresh token.
	Save(ctx context.Context, t *Token) error
	// Delete removes the stored token.
	Delete(ctx context.Context) error
}

// MemoryTokenStore is an in-process TokenStore, used by tests and by the
// --direct CLI mode where nothing should touch disk.
type MemoryTokenStore struct {
	mu  sync.Mutex
	tok *Token
}

// NewMemoryTokenStore returns an empty store.
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{} }

// Load implements TokenStore.
func (s *MemoryTokenStore) Load(context.Context) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok == nil {
		return nil, ErrNoToken
	}
	return s.tok.Clone(), nil
}

// Save implements TokenStore.
func (s *MemoryTokenStore) Save(_ context.Context, t *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tok = t.Clone()
	return nil
}

// Delete implements TokenStore.
func (s *MemoryTokenStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tok = nil
	return nil
}

// StaleTokenRefresher is an optional Authenticator extension. When the client
// replays a request after a 401 it reports which access token failed, so that
// an authenticator can ignore refresh requests triggered by a token another
// goroutine has already replaced. Without it, concurrent 401s would each burn
// one rolling refresh token and the losers would be logged out (§2.2 B, §9.2).
type StaleTokenRefresher interface {
	RefreshStale(ctx context.Context, staleAccessToken string) error
}

// ---------------------------------------------------------------- OAuth2

// OAuth2 implements Authenticator using upstream's simplified resource-owner
// password credentials flow (§2.2 B). It is the Bridge default: the password is
// exchanged for tokens once and then dropped, and refresh tokens are rotated
// and persisted atomically.
type OAuth2 struct {
	c        *Client
	store    TokenStore
	clientID string
	skew     time.Duration
	now      func() time.Time

	mu     sync.Mutex
	tok    *Token
	loaded bool
}

// OAuth2Option configures an OAuth2 authenticator.
type OAuth2Option func(*OAuth2)

// WithTokenStore persists tokens through the given store.
func WithTokenStore(s TokenStore) OAuth2Option {
	return func(a *OAuth2) {
		if s != nil {
			a.store = s
		}
	}
}

// WithClientID overrides the OAuth2 client_id.
func WithClientID(id string) OAuth2Option {
	return func(a *OAuth2) {
		if id != "" {
			a.clientID = id
		}
	}
}

// WithRefreshSkew sets how early the access token is refreshed.
func WithRefreshSkew(d time.Duration) OAuth2Option {
	return func(a *OAuth2) {
		if d >= 0 {
			a.skew = d
		}
	}
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) OAuth2Option {
	return func(a *OAuth2) {
		if now != nil {
			a.now = now
		}
	}
}

// NewOAuth2 creates an OAuth2 authenticator bound to c. It does not attach
// itself: call c.SetAuthenticator(a) or opendrive.New(WithAuthenticator(a)).
func NewOAuth2(c *Client, opts ...OAuth2Option) *OAuth2 {
	a := &OAuth2{
		c:        c,
		store:    NewMemoryTokenStore(),
		clientID: DefaultClientID,
		skew:     DefaultRefreshSkew,
		now:      time.Now,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// grantResponse is the body of POST /oauth2/grant.json.
type grantResponse struct {
	AccessToken           string  `json:"access_token"`
	RefreshToken          string  `json:"refresh_token"`
	TokenType             string  `json:"token_type"`
	ExpiresIn             FlexInt `json:"expires_in"`
	RefreshTokenExpiresIn FlexInt `json:"refresh_token_expires_in"`
}

// Login exchanges a username and password for tokens. The password is used for
// this single call and never stored, logged or persisted (§2.2 B, §9.2).
func (a *OAuth2) Login(ctx context.Context, username, password string) error {
	var out grantResponse
	err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             "/oauth2/grant.json",
		SessionPlacement: SessionOmit,
		Body: map[string]string{
			"grant_type": "password",
			"client_id":  a.clientID,
			"username":   username,
			"password":   password,
		},
	}, &out)
	if err != nil {
		return err
	}
	tok, err := a.tokenFrom(out, "")
	if err != nil {
		return err
	}
	tok.Account = username

	a.mu.Lock()
	defer a.mu.Unlock()
	return a.adopt(ctx, tok)
}

// Credentials implements Authenticator, refreshing proactively when the access
// token is within the refresh skew of expiry (§2.2 Bridge strategy).
func (a *OAuth2) Credentials(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureLoaded(ctx); err != nil {
		return Credentials{}, err
	}
	if a.tok == nil {
		return Credentials{}, &APIError{Kind: KindUnauthorized, UpstreamMsg: "not logged in"}
	}
	if a.tok.NeedsRefresh(a.now(), a.skew) {
		if err := a.refreshLocked(ctx); err != nil {
			return Credentials{}, err
		}
	}
	return Credentials{SessionID: OAuthSessionID, AccessToken: a.tok.AccessToken}, nil
}

// Refresh implements Authenticator.
func (a *OAuth2) Refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	return a.refreshLocked(ctx)
}

// RefreshStale implements StaleTokenRefresher: a refresh triggered by an access
// token that has already been replaced is a no-op.
func (a *OAuth2) RefreshStale(ctx context.Context, stale string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if stale != "" && a.tok != nil && a.tok.AccessToken != stale {
		return nil
	}
	return a.refreshLocked(ctx)
}

// Logout drops the tokens locally. Upstream has no token revocation endpoint in
// the documented surface, so this is a local operation.
func (a *OAuth2) Logout(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = nil
	a.loaded = true
	if a.store == nil {
		return nil
	}
	if err := a.store.Delete(ctx); err != nil && !errors.Is(err, ErrNoToken) {
		return err
	}
	return nil
}

// Token returns a copy of the current token, and whether there is one. The
// daemon uses it for /v1/auth/status (§4.1).
func (a *OAuth2) Token() (Token, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		return Token{}, false
	}
	return *a.tok.Clone(), true
}

func (a *OAuth2) ensureLoaded(ctx context.Context) error {
	if a.loaded || a.store == nil {
		a.loaded = true
		return nil
	}
	a.loaded = true
	tok, err := a.store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrNoToken) {
			return nil
		}
		return &APIError{Kind: KindUnauthorized, UpstreamMsg: "cannot read the stored token", Err: err}
	}
	a.tok = tok
	return nil
}

// refreshLocked exchanges the refresh token. The caller must hold a.mu.
func (a *OAuth2) refreshLocked(ctx context.Context) error {
	now := a.now()
	if a.tok == nil || a.tok.RefreshToken == "" {
		return &APIError{Kind: KindUnauthorized, UpstreamMsg: "not logged in: no refresh token available"}
	}
	if !a.tok.RefreshUsable(now) {
		return &APIError{Kind: KindRefreshTokenFailed, UpstreamMsg: "the refresh token has expired, log in again"}
	}

	var out grantResponse
	err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             "/oauth2/grant.json",
		SessionPlacement: SessionOmit,
		Body: map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     a.clientID,
			"refresh_token": a.tok.RefreshToken,
		},
	}, &out)
	if err != nil {
		return err
	}
	// Upstream rotates the refresh token on every exchange; if it withholds a
	// new one, the previous one stays valid (§2.2 B).
	tok, err := a.tokenFrom(out, a.tok.RefreshToken)
	if err != nil {
		return err
	}
	tok.Account = a.tok.Account
	return a.adopt(ctx, tok)
}

// adopt persists the new token before it becomes the in-memory one, so a crash
// mid-refresh can never leave the store holding a refresh token upstream has
// already invalidated (§9.2). The caller must hold a.mu.
func (a *OAuth2) adopt(ctx context.Context, tok *Token) error {
	if a.store != nil {
		if err := a.store.Save(ctx, tok); err != nil {
			// Upstream has already rotated the credential: keeping the old
			// token would guarantee a logout, so the new one is adopted in
			// memory and the persistence failure is reported loudly instead.
			a.c.Logger().Error("cannot persist the refreshed token; the session survives only in memory",
				slog.String("error", RedactString(err.Error())))
		}
	}
	a.tok = tok
	a.loaded = true
	return nil
}

func (a *OAuth2) tokenFrom(g grantResponse, fallbackRefresh string) (*Token, error) {
	if g.AccessToken == "" {
		return nil, &APIError{Kind: KindInvalidResponse, UpstreamMsg: "grant response carried no access_token"}
	}
	now := a.now()
	ttl := time.Duration(g.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = DefaultAccessTokenTTL
	}
	refresh := g.RefreshToken
	if refresh == "" {
		refresh = fallbackRefresh
	}
	refreshTTL := time.Duration(g.RefreshTokenExpiresIn) * time.Second
	if refreshTTL <= 0 {
		refreshTTL = DefaultRefreshTokenTTL
	}
	return &Token{
		AccessToken:   g.AccessToken,
		RefreshToken:  refresh,
		Expiry:        now.Add(ttl),
		RefreshExpiry: now.Add(refreshTTL),
	}, nil
}

// ---------------------------------------------------------------- session

// SessionAuth implements Authenticator with the legacy session mode (§2.2 A).
// It is the fallback for deployments or endpoints that do not accept OAuth2.
// The password is not retained, so an expired session requires an explicit
// Login rather than a silent refresh (§9.2).
type SessionAuth struct {
	c *Client

	mu      sync.Mutex
	session string
	info    SessionLogin
}

// NewSessionAuth creates a session-mode authenticator bound to c.
func NewSessionAuth(c *Client) *SessionAuth { return &SessionAuth{c: c} }

// LoginOption customises a session login.
type LoginOption func(map[string]string)

// WithCaptchaResponse supplies the solved captcha upstream asked for (§2.6 #11).
func WithCaptchaResponse(resp string) LoginOption {
	return func(body map[string]string) { body["captcha_response"] = resp }
}

// WithPartnerID sets the partner_id login field.
func WithPartnerID(id string) LoginOption {
	return func(body map[string]string) { body["partner_id"] = id }
}

// Login performs POST /session/login.json. A captcha challenge surfaces as an
// APIError of Kind KindCaptchaRequired, which callers must not retry: the user
// has to solve it (§2.6 #11).
func (a *SessionAuth) Login(ctx context.Context, username, password string, opts ...LoginOption) (*SessionLogin, error) {
	body := map[string]string{
		"username":         username,
		"passwd":           password,
		"version":          SessionLoginVersion,
		"partner_id":       "",
		"captcha_response": "",
	}
	for _, o := range opts {
		o(body)
	}
	var out SessionLogin
	if err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             "/session/login.json",
		SessionPlacement: SessionOmit,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" {
		return nil, &APIError{Kind: KindInvalidResponse, UpstreamMsg: "login response carried no SessionID"}
	}
	a.mu.Lock()
	a.session, a.info = out.SessionID, out
	a.mu.Unlock()
	return &out, nil
}

// Credentials implements Authenticator.
func (a *SessionAuth) Credentials(context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == "" {
		return Credentials{}, &APIError{Kind: KindUnauthorized, UpstreamMsg: "not logged in"}
	}
	return Credentials{SessionID: a.session}, nil
}

// Refresh implements Authenticator. Session mode has nothing to refresh: the
// password is not kept, so the caller must log in again (§9.2).
func (a *SessionAuth) Refresh(context.Context) error {
	a.mu.Lock()
	a.session = ""
	a.mu.Unlock()
	return &APIError{Kind: KindUnauthorized, UpstreamMsg: "the session has expired, log in again"}
}

// SessionID returns the current session id, empty when not logged in.
func (a *SessionAuth) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

// Info returns the account information the login response carried.
func (a *SessionAuth) Info() SessionLogin {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.info
}

// Exists checks the session with POST /session/exists.json.
func (a *SessionAuth) Exists(ctx context.Context) (bool, error) {
	var out BoolResult
	if err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             "/session/exists.json",
		SessionPlacement: SessionInBody,
		Body:             map[string]string{},
	}, &out); err != nil {
		return false, err
	}
	return out.OK(), nil
}

// Logout performs POST /session/logout.json and forgets the session.
func (a *SessionAuth) Logout(ctx context.Context) error {
	var out BoolResult
	err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             "/session/logout.json",
		SessionPlacement: SessionInBody,
		Body:             map[string]string{},
	}, &out)
	a.mu.Lock()
	a.session, a.info = "", SessionLogin{}
	a.mu.Unlock()
	return err
}

// CaptchaRequired reports whether upstream currently demands a captcha for this
// client or account. The endpoint exists online but not in the PDF (§2.6 #1).
func CaptchaRequired(ctx context.Context, c *Client, username string) (*CaptchaStatus, error) {
	q := map[string][]string{}
	if username != "" {
		q["username"] = []string{username}
	}
	var out CaptchaStatus
	if err := c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             "/session/captcharequired.json",
		Query:            q,
		SessionPlacement: SessionOmit,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------- selection

// AuthMode selects the authentication scheme (config auth_mode, §3.4).
type AuthMode string

// Authentication modes.
const (
	AuthModeOAuth2  AuthMode = "oauth2"
	AuthModeSession AuthMode = "session"
)

// Login authenticates c and attaches the resulting Authenticator to it.
//
// With AuthModeOAuth2 (the Bridge default) it uses the OAuth2 grant and falls
// back to session mode when upstream rejects the grant endpoint itself — a 404
// or 501 — rather than the credentials. Bad credentials, a captcha challenge or
// any other definite answer are returned as-is, because retrying them in
// session mode would only burn another login attempt (§2.2, §2.6 #11).
func Login(ctx context.Context, c *Client, mode AuthMode, username, password string, store TokenStore) (Authenticator, error) {
	if mode == AuthModeSession {
		sa := NewSessionAuth(c)
		if _, err := sa.Login(ctx, username, password); err != nil {
			return nil, err
		}
		c.SetAuthenticator(sa)
		return sa, nil
	}

	oa := NewOAuth2(c, WithTokenStore(store))
	err := oa.Login(ctx, username, password)
	if err == nil {
		c.SetAuthenticator(oa)
		return oa, nil
	}
	if !grantUnsupported(err) {
		return nil, err
	}
	c.Logger().Warn("oauth2 grant is unavailable upstream, falling back to session mode",
		slog.String("error", RedactString(err.Error())))
	sa := NewSessionAuth(c)
	if _, lerr := sa.Login(ctx, username, password); lerr != nil {
		return nil, lerr
	}
	c.SetAuthenticator(sa)
	return sa, nil
}

// grantUnsupported reports whether the error means "this deployment has no
// OAuth2 grant endpoint" rather than "these credentials are wrong".
func grantUnsupported(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.HTTPCode {
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return true
	}
	return ae.Kind == KindInvalidResponse
}
