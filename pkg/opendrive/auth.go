package opendrive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Authentication defaults (whitepaper §2.2).
const (
	// DefaultClientID is the client_id upstream expects for the simplified
	// resource-owner password credentials flow.
	DefaultClientID = "OpenDrive"
	// DefaultAccessTokenTTL is the documented access token lifetime, used when
	// the grant response omits expires_in.
	DefaultAccessTokenTTL = 86400 * time.Second
	// DefaultRefreshTokenTTL is the documented refresh token lifetime.
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour
	// DefaultRefreshSkew is how long before expiry the access token is
	// refreshed proactively.
	DefaultRefreshSkew = 5 * time.Minute
	// DefaultRenewInterval is the upper bound on how long a refresh token may
	// sit unused. The daemon's keep-alive timer calls EnsureFresh at least this
	// often so that an idle bridge never loses its 30-day refresh token
	// (§2.2 #2).
	DefaultRenewInterval = 7 * 24 * time.Hour
	// SessionLoginVersion is the "version" field the login endpoint expects.
	SessionLoginVersion = "10"

	// maxRenewBackoff caps the wait between failed renewal attempts. Renewal is
	// never retried in a tight loop: an account that keeps failing to
	// authenticate is one captcha away from being locked out (§2.2 #3).
	maxRenewBackoff  = 15 * time.Minute
	baseRenewBackoff = 5 * time.Second
)

// ErrNoCredentials is returned by a CredentialStore that holds nothing yet.
var ErrNoCredentials = errors.New("opendrive: no stored credentials")

// AuthState is the authentication state machine of whitepaper §4.1/§4.5. The
// daemon reports it verbatim through /v1/auth/status.
type AuthState string

// Authentication states.
const (
	// StateNotConfigured means no credentials have ever been stored.
	StateNotConfigured AuthState = "not_configured"
	// StateAuthenticated means usable credentials are in hand.
	StateAuthenticated AuthState = "authenticated"
	// StateRefreshing means a renewal is in progress or waiting out a backoff
	// after a transient failure.
	StateRefreshing AuthState = "refreshing"
	// StateReauthRequired means upstream rejected the stored credentials.
	// Automatic attempts have stopped and the user must supply a password.
	StateReauthRequired AuthState = "reauth_required"
	// StateCaptchaRequired means upstream demands a captcha the bridge cannot
	// answer.
	StateCaptchaRequired AuthState = "captcha_required"
	// StateKeystoreUnavailable means the credential store cannot be read. No
	// upstream request is made while in this state.
	StateKeystoreUnavailable AuthState = "keystore_unavailable"
)

// Identity is the account the bridge is configured for. It is answered from
// local state only: /v1/auth/status must not make a network call (§4.1).
type Identity struct {
	// Username is the configured login. Empty when nothing is configured.
	Username string
	// UserID and AccType are filled in when a login response provided them.
	UserID  string
	AccType int
	// AuthMode is the scheme in use.
	AuthMode AuthMode
	// Seamless reports whether the bridge can recover on its own indefinitely,
	// which requires a stored password and a working credential store
	// (§2.2 #5, §4.1).
	Seamless bool
}

// Configured reports whether any account is set up.
func (i Identity) Configured() bool { return i.Username != "" }

// Credentials are what an Authenticator hands to the client for a single call.
type Credentials struct {
	// SessionID is a real session id in session mode, or OAuthSessionID when
	// AccessToken is set.
	SessionID string
	// AccessToken is the OAuth2 access token; upstream requires it in the URL
	// query string rather than in a header (§2.2 B).
	AccessToken string
}

// Authenticator supplies credentials, renews them silently, and reports what
// state it is in. Both implementations (OAuth2 and SessionAuth) satisfy the
// whole interface, so the daemon never branches on the auth mode (§2.2 #6).
type Authenticator interface {
	// Credentials returns the credentials to use for the next call, renewing
	// them first if they are expired or about to expire.
	Credentials(ctx context.Context) (Credentials, error)
	// Refresh renews credentials after upstream rejected them. It is called at
	// most once per request (§2.2 #3).
	Refresh(ctx context.Context) error
	// Identity reports the configured account without touching the network.
	Identity() Identity
	// AuthState reports the current state machine position.
	AuthState() AuthState
}

// SessionIDProvider is implemented by authenticators that can supply a real
// upstream session id.
//
// It exists because one endpoint refuses OAuth2 outright: the chunk upload is
// served by a different front end that authenticates on the session id in the
// URL path and never looks at the access token, answering a bare HTML 401
// instead (docs/discrepancies.md D38). A request marked NeedsSessionID is given
// a real session in place of the OAUTH marker.
//
// In OAuth2 mode the session is obtained with the stored password and cached,
// so the user is still never involved (§2.2 #1).
type SessionIDProvider interface {
	SessionID(ctx context.Context) (string, error)
}

// StaleTokenRefresher is an optional Authenticator extension. When the client
// replays a request after a 401 it reports which access token failed, so that
// an authenticator can ignore refresh requests triggered by a token another
// goroutine has already replaced. Without it, concurrent 401s would each burn
// one rolling refresh token and the losers would be logged out (§9.2).
type StaleTokenRefresher interface {
	RefreshStale(ctx context.Context, staleAccessToken string) error
}

// ---------------------------------------------------------------- credentials

// Token is an OAuth2 credential pair. Its String method is deliberately
// redacted so that a stray log statement cannot leak it (§9.4).
type Token struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	IssuedAt      time.Time `json:"issued_at"`
	Expiry        time.Time `json:"expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry"`
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

// StaleForRenewal reports whether the token should be rolled even though it is
// still valid, either because it has passed its half-life or because it has not
// been rotated for interval (§2.2 #2).
func (t *Token) StaleForRenewal(now time.Time, interval time.Duration) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	if !t.IssuedAt.IsZero() && interval > 0 && now.Sub(t.IssuedAt) >= interval {
		return true
	}
	if !t.IssuedAt.IsZero() && !t.Expiry.IsZero() {
		half := t.IssuedAt.Add(t.Expiry.Sub(t.IssuedAt) / 2)
		return !now.Before(half)
	}
	return false
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

// StoredCredentials is everything the bridge persists to stay seamless: the
// four credential kinds of whitepaper §9.2.
//
// Password is stored deliberately, as a documented deviation from upstream's
// OAuth2 terms: a refresh token lasts 30 days and only rolls when used, so
// without the password a bridge that sits idle longer than that would demand a
// login, which the product requirement forbids (§2.2 #1, §9.2). Users who
// cannot accept that set persist_password:false and give up seamlessness.
type StoredCredentials struct {
	Username  string    `json:"username"`
	Password  string    `json:"password,omitempty"`
	Token     *Token    `json:"token,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	AuthMode  AuthMode  `json:"auth_mode"`
	UserID    string    `json:"user_id,omitempty"`
	AccType   int       `json:"acc_type,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// String implements fmt.Stringer without revealing anything secret (§9.2: the
// debug output prints field presence, never values).
func (c *StoredCredentials) String() string {
	if c == nil {
		return "StoredCredentials(nil)"
	}
	has := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return "StoredCredentials(user=" + Redacted +
		", password=" + has(c.Password != "") +
		", token=" + has(c.Token != nil) +
		", session=" + has(c.SessionID != "") +
		", mode=" + string(c.AuthMode) + ")"
}

// Clone returns a deep copy.
func (c *StoredCredentials) Clone() *StoredCredentials {
	if c == nil {
		return nil
	}
	out := *c
	out.Token = c.Token.Clone()
	return &out
}

// CredentialStore persists the credentials that keep the bridge seamless.
// Implementations live in internal/keystore (OS keyring, encrypted file
// fallback); the SDK only needs this contract (§9.2).
type CredentialStore interface {
	// Load returns the stored credentials, or ErrNoCredentials when there are
	// none. Any other error means the store itself is unavailable and puts the
	// authenticator into StateKeystoreUnavailable.
	Load(ctx context.Context) (*StoredCredentials, error)
	// Save persists the credentials, replacing any previous set. It must be
	// atomic: a crash may never leave the store without a usable refresh token
	// (§9.2).
	Save(ctx context.Context, c *StoredCredentials) error
	// Delete removes everything.
	Delete(ctx context.Context) error
}

// EphemeralStore is implemented by stores that do not survive a restart. The
// daemon refuses to start on one unless the operator asked for it explicitly,
// so that a bridge can never look configured and then forget everything on
// reboot (§9.2).
type EphemeralStore interface {
	Ephemeral() bool
}

// MemoryTokenStore is an in-process CredentialStore.
//
// It is for tests and for explicitly ephemeral runs only: it reports itself as
// ephemeral so that internal/keystore can refuse to hand it to a daemon that
// did not ask for it (§9.2).
type MemoryTokenStore struct {
	mu   sync.Mutex
	cred *StoredCredentials
}

// NewMemoryTokenStore returns an empty in-memory store.
func NewMemoryTokenStore() *MemoryTokenStore { return &MemoryTokenStore{} }

// Load implements CredentialStore.
func (s *MemoryTokenStore) Load(context.Context) (*StoredCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred == nil {
		return nil, ErrNoCredentials
	}
	return s.cred.Clone(), nil
}

// Save implements CredentialStore.
func (s *MemoryTokenStore) Save(_ context.Context, c *StoredCredentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = c.Clone()
	return nil
}

// Delete implements CredentialStore.
func (s *MemoryTokenStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = nil
	return nil
}

// Ephemeral implements EphemeralStore.
func (s *MemoryTokenStore) Ephemeral() bool { return true }

// ---------------------------------------------------------------- renew gate

// renewGate is the shared part of the silent re-login state machine: it holds
// the current state, the last error, and the backoff that keeps a failing
// renewal from turning into a retry loop (§2.2 #3).
type renewGate struct {
	state       AuthState
	lastErr     *APIError
	attempts    int
	nextAttempt time.Time
}

// terminal parks the gate in a state only the user can leave.
func (g *renewGate) terminal(state AuthState, err *APIError) *APIError {
	g.state = state
	g.lastErr = err
	g.attempts = 0
	g.nextAttempt = time.Time{}
	return err
}

// backoff records a transient failure and schedules the next allowed attempt.
func (g *renewGate) backoff(now time.Time, err *APIError) *APIError {
	g.state = StateRefreshing
	g.lastErr = err
	g.attempts++
	wait := baseRenewBackoff << min(g.attempts-1, 8)
	if wait > maxRenewBackoff || wait <= 0 {
		wait = maxRenewBackoff
	}
	g.nextAttempt = now.Add(wait)
	return err
}

// succeed clears the gate after a successful renewal.
func (g *renewGate) succeed() {
	g.state = StateAuthenticated
	g.lastErr = nil
	g.attempts = 0
	g.nextAttempt = time.Time{}
}

// blocked reports the error to return without touching the network, if any.
func (g *renewGate) blocked(now time.Time) *APIError {
	switch g.state {
	case StateReauthRequired, StateCaptchaRequired, StateKeystoreUnavailable:
		return g.lastErr
	}
	if !g.nextAttempt.IsZero() && now.Before(g.nextAttempt) {
		return g.lastErr
	}
	return nil
}

// reset returns the gate to a pristine state, used after an explicit login.
func (g *renewGate) reset() {
	g.state = StateAuthenticated
	g.lastErr = nil
	g.attempts = 0
	g.nextAttempt = time.Time{}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// asError converts a possibly-nil *APIError into an error interface. Returning
// a typed nil pointer directly would produce a non-nil error interface, which
// is the classic Go trap this helper exists to avoid.
func asError(e *APIError) error {
	if e == nil {
		return nil
	}
	return e
}

// keystoreError wraps a credential store failure. §4.5: while in this state the
// SDK issues no upstream request at all.
func keystoreError(err error) *APIError {
	return &APIError{
		Kind:        KindKeystoreUnavailable,
		UpstreamMsg: "the credential store is unavailable; unlock the keyring or check the configured key",
		Err:         err,
	}
}

// reauthError signals that only the user can move things forward.
func reauthError(msg string) *APIError {
	return &APIError{Kind: KindReauthRequired, UpstreamMsg: msg}
}

// notConfiguredError is the reauth error used before any login has happened.
func notConfiguredError() *APIError {
	return reauthError("the bridge is not configured yet; log in once to store the account")
}

// ---------------------------------------------------------------- selection

// AuthMode selects the authentication scheme (config auth_mode, §3.4).
type AuthMode string

// Authentication modes.
const (
	AuthModeOAuth2  AuthMode = "oauth2"
	AuthModeSession AuthMode = "session"
)

// Login authenticates c, persists the credentials and attaches the resulting
// Authenticator. It is the one entry point that takes a password: from here on
// the bridge renews itself silently (§2.2).
//
// With AuthModeOAuth2 (the default) it uses the OAuth2 grant and falls back to
// session mode only when upstream lacks the grant endpoint — a 404 or 501 —
// never when the credentials themselves were rejected, because retrying a bad
// password only moves the account closer to a captcha lock (§2.6 #11).
func Login(ctx context.Context, c *Client, mode AuthMode, username, password string, store CredentialStore, opts ...AuthOption) (Authenticator, error) {
	if mode == AuthModeSession {
		sa := NewSessionAuth(c, append([]AuthOption{WithCredentialStore(store)}, opts...)...)
		if _, err := sa.Login(ctx, username, password); err != nil {
			return nil, err
		}
		c.SetAuthenticator(sa)
		return sa, nil
	}

	oa := NewOAuth2(c, append([]AuthOption{WithCredentialStore(store)}, opts...)...)
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
	sa := NewSessionAuth(c, append([]AuthOption{WithCredentialStore(store)}, opts...)...)
	if _, lerr := sa.Login(ctx, username, password); lerr != nil {
		return nil, lerr
	}
	c.SetAuthenticator(sa)
	return sa, nil
}

// Resume rebuilds an Authenticator from the credential store without asking for
// a password, which is what the daemon does on every start (§2.2 #1). It makes
// no network call: the first business request triggers whatever renewal is due.
//
// A store that holds nothing yields an authenticator in StateNotConfigured
// rather than an error, so /v1/auth/status can report it.
func Resume(ctx context.Context, c *Client, store CredentialStore, opts ...AuthOption) (Authenticator, error) {
	if store == nil {
		return nil, invalidRequest("a credential store is required to resume a session")
	}
	cred, err := store.Load(ctx)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoCredentials):
		cred = nil
	default:
		return nil, keystoreError(err)
	}

	all := append([]AuthOption{WithCredentialStore(store)}, opts...)
	if cred != nil && cred.AuthMode == AuthModeSession {
		sa := NewSessionAuth(c, all...)
		sa.adoptLoaded(cred)
		c.SetAuthenticator(sa)
		return sa, nil
	}
	oa := NewOAuth2(c, all...)
	if cred != nil {
		oa.adoptLoaded(cred)
	}
	c.SetAuthenticator(oa)
	return oa, nil
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

// ---------------------------------------------------------------- options

// AuthOption configures an authenticator. The same options apply to both
// implementations so that callers need not branch on the mode.
type AuthOption func(*authConfig)

type authConfig struct {
	store           CredentialStore
	clientID        string
	skew            time.Duration
	renewInterval   time.Duration
	persistPassword bool
	now             func() time.Time
}

func defaultAuthConfig() authConfig {
	return authConfig{
		store:           NewMemoryTokenStore(),
		clientID:        DefaultClientID,
		skew:            DefaultRefreshSkew,
		renewInterval:   DefaultRenewInterval,
		persistPassword: true,
		now:             time.Now,
	}
}

// WithCredentialStore persists credentials through the given store.
func WithCredentialStore(s CredentialStore) AuthOption {
	return func(c *authConfig) {
		if s != nil {
			c.store = s
		}
	}
}

// WithClientID overrides the OAuth2 client_id (the partner id, see
// docs/discrepancies.md D6).
func WithClientID(id string) AuthOption {
	return func(c *authConfig) {
		if id != "" {
			c.clientID = id
		}
	}
}

// WithRefreshSkew sets how early the access token is refreshed.
func WithRefreshSkew(d time.Duration) AuthOption {
	return func(c *authConfig) {
		if d >= 0 {
			c.skew = d
		}
	}
}

// WithRenewInterval sets the maximum age a refresh token may reach before
// EnsureFresh rolls it (§2.2 #2).
func WithRenewInterval(d time.Duration) AuthOption {
	return func(c *authConfig) {
		if d > 0 {
			c.renewInterval = d
		}
	}
}

// WithPersistPassword controls whether the password is kept for silent
// re-login. Turning it off honours upstream's OAuth2 terms literally at the
// cost of seamlessness after a long idle period (config persist_password,
// §9.2).
func WithPersistPassword(on bool) AuthOption {
	return func(c *authConfig) { c.persistPassword = on }
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) AuthOption {
	return func(c *authConfig) {
		if now != nil {
			c.now = now
		}
	}
}
