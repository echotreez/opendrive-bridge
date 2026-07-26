package opendrive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OAuth2 implements Authenticator with upstream's simplified resource-owner
// password credentials flow (§2.2 B). It is the bridge default and it is built
// around one product requirement: after the initial setup the user is never
// asked for anything again unless the password itself changed (§2.2).
//
// The renewal ladder, all of it under one lock so only one renewal is ever in
// flight (§2.2 #3):
//
//  1. the refresh token is exchanged for a new pair;
//  2. if that fails — refresh expired or rejected — the stored password buys a
//     brand new pair, silently;
//  3. if upstream rejects the password, or demands a captcha, renewal stops
//     dead and waits for the user. Nothing is retried, because repeated
//     failures are what triggers a captcha lock (§2.6 #11);
//  4. any other failure (network, 5xx) is retried later behind an exponential
//     backoff, never in a loop.
//
// A credential store that cannot be read short-circuits everything: no upstream
// request is made at all (§4.5 keystore_unavailable).
type OAuth2 struct {
	c   *Client
	cfg authConfig

	mu     sync.Mutex
	cred   *StoredCredentials
	loaded bool
	gate   renewGate
}

// NewOAuth2 creates an OAuth2 authenticator bound to c. It does not attach
// itself: use Login, Resume, or c.SetAuthenticator.
func NewOAuth2(c *Client, opts ...AuthOption) *OAuth2 {
	cfg := defaultAuthConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return &OAuth2{c: c, cfg: cfg, gate: renewGate{state: StateNotConfigured}}
}

// grantResponse is the body of POST /oauth2/grant.json.
type grantResponse struct {
	AccessToken           string  `json:"access_token"`
	RefreshToken          string  `json:"refresh_token"`
	TokenType             string  `json:"token_type"`
	ExpiresIn             FlexInt `json:"expires_in"`
	RefreshTokenExpiresIn FlexInt `json:"refresh_token_expires_in"`
}

// Login exchanges a username and password for tokens and persists everything
// the bridge needs to stay seamless. It is the only method that takes a
// password (§2.2 #1).
func (a *OAuth2) Login(ctx context.Context, username, password string) error {
	tok, err := a.passwordGrant(ctx, username, password)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	cred := &StoredCredentials{
		Username: username,
		Token:    tok,
		AuthMode: AuthModeOAuth2,
	}
	if a.cfg.persistPassword {
		cred.Password = password
	}
	if a.cred != nil {
		cred.UserID, cred.AccType = a.cred.UserID, a.cred.AccType
	}
	a.loaded = true
	a.adopt(ctx, cred)
	a.gate.reset()
	return nil
}

// Credentials implements Authenticator, renewing proactively when the access
// token is within the refresh skew of expiry.
func (a *OAuth2) Credentials(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureLoaded(ctx); err != nil {
		return Credentials{}, err
	}
	if a.cred == nil || (a.cred.Token == nil && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return Credentials{}, notConfiguredError()
	}
	now := a.cfg.now()
	if a.cred.Token.NeedsRefresh(now, a.cfg.skew) {
		if err := a.renewLocked(ctx); err != nil {
			return Credentials{}, err
		}
	} else if a.gate.state != StateAuthenticated {
		a.gate.succeed()
	}
	return Credentials{SessionID: OAuthSessionID, AccessToken: a.cred.Token.AccessToken}, nil
}

// Refresh implements Authenticator.
func (a *OAuth2) Refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	return a.renewOrNotConfigured(ctx)
}

// RefreshStale implements StaleTokenRefresher: a renewal triggered by an access
// token that has already been replaced is a no-op.
func (a *OAuth2) RefreshStale(ctx context.Context, stale string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if stale != "" && a.cred != nil && a.cred.Token != nil && a.cred.Token.AccessToken != stale {
		return nil
	}
	return a.renewOrNotConfigured(ctx)
}

// EnsureFresh rolls the refresh token when it is due, so that a bridge nobody
// uses does not quietly lose its 30-day credential. The daemon calls it on a
// timer at least once per DefaultRenewInterval and on start-up (§2.2 #2).
//
// It is a no-op when nothing is due, and it never asks the user for anything.
func (a *OAuth2) EnsureFresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if a.cred == nil || (a.cred.Token == nil && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return notConfiguredError()
	}
	if !a.cred.Token.StaleForRenewal(a.cfg.now(), a.cfg.renewInterval) {
		return nil
	}
	return asError(a.renewLocked(ctx))
}

// Logout drops every credential, locally and in the store (§4.1).
func (a *OAuth2) Logout(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cred = nil
	a.loaded = true
	a.gate = renewGate{state: StateNotConfigured}
	if a.cfg.store == nil {
		return nil
	}
	if err := a.cfg.store.Delete(ctx); err != nil && !errors.Is(err, ErrNoCredentials) {
		return keystoreError(err)
	}
	return nil
}

// Identity implements Authenticator.
func (a *OAuth2) Identity() Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cred == nil {
		return Identity{AuthMode: AuthModeOAuth2}
	}
	return Identity{
		Username: a.cred.Username,
		UserID:   a.cred.UserID,
		AccType:  a.cred.AccType,
		AuthMode: AuthModeOAuth2,
		Seamless: a.cred.Password != "" && a.gate.state != StateKeystoreUnavailable,
	}
}

// AuthState implements Authenticator.
func (a *OAuth2) AuthState() AuthState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gate.state
}

// Token returns a copy of the current token, and whether there is one.
func (a *OAuth2) Token() (Token, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cred == nil || a.cred.Token == nil {
		return Token{}, false
	}
	return *a.cred.Token.Clone(), true
}

// ---------------------------------------------------------------- internals

// adoptLoaded seeds the authenticator from credentials the caller already read,
// used by Resume.
func (a *OAuth2) adoptLoaded(cred *StoredCredentials) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cred = cred.Clone()
	a.loaded = true
	if a.cred != nil && (a.cred.Token != nil || a.cred.Password != "") {
		a.gate.state = StateAuthenticated
	}
}

// ensureLoaded reads the store once. A store failure is terminal until it
// recovers: no upstream call may follow it (§4.5).
func (a *OAuth2) ensureLoaded(ctx context.Context) *APIError {
	if a.loaded || a.cfg.store == nil {
		a.loaded = true
		return nil
	}
	cred, err := a.cfg.store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrNoCredentials) {
			a.loaded = true
			a.gate.state = StateNotConfigured
			return nil
		}
		// Deliberately not marked loaded: the next call retries the store,
		// which is how the daemon recovers once the keyring is unlocked.
		return a.gate.terminal(StateKeystoreUnavailable, keystoreError(err))
	}
	a.loaded = true
	a.cred = cred
	if cred != nil && (cred.Token != nil || cred.Password != "") {
		a.gate.state = StateAuthenticated
	}
	return nil
}

func (a *OAuth2) renewOrNotConfigured(ctx context.Context) error {
	if a.cred == nil || (a.cred.Token == nil && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return notConfiguredError()
	}
	return asError(a.renewLocked(ctx))
}

// renewLocked walks the renewal ladder. The caller must hold a.mu, which is
// what makes renewal single-flight.
func (a *OAuth2) renewLocked(ctx context.Context) *APIError {
	now := a.cfg.now()
	if blocked := a.gate.blocked(now); blocked != nil {
		return blocked
	}
	a.gate.state = StateRefreshing

	// Step 1: exchange the refresh token.
	if a.cred.Token.RefreshUsable(now) {
		tok, err := a.refreshGrant(ctx, a.cred.Token.RefreshToken)
		if err == nil {
			next := a.cred.Clone()
			next.Token = tok
			a.adopt(ctx, next)
			a.gate.succeed()
			return nil
		}
		if fatal := a.classifyRenewFailure(err, now); fatal != nil {
			return fatal
		}
		a.c.Logger().Info("refresh token rejected, falling back to a silent password login",
			slog.String("error", RedactString(err.Error())))
	}

	// Step 2: silent re-login with the stored password.
	if a.cred.Password == "" {
		return a.gate.terminal(StateReauthRequired, reauthError(
			"the stored refresh token is no longer valid and no password is stored; log in again"))
	}
	tok, err := a.passwordGrant(ctx, a.cred.Username, a.cred.Password)
	if err == nil {
		next := a.cred.Clone()
		next.Token = tok
		a.adopt(ctx, next)
		a.gate.succeed()
		a.c.Logger().Info("silently re-authenticated with the stored password")
		return nil
	}
	if fatal := a.classifyRenewFailure(err, now); fatal != nil {
		return fatal
	}
	// A refresh grant that fails at this point has nowhere left to go.
	return a.gate.terminal(StateReauthRequired, reauthError(
		"upstream rejected the stored credentials; provide the current password"))
}

// classifyRenewFailure decides whether a renewal error ends the ladder. It
// returns nil when the caller should try the next rung.
func (a *OAuth2) classifyRenewFailure(err error, now time.Time) *APIError {
	var ae *APIError
	if !errors.As(err, &ae) {
		return a.gate.backoff(now, &APIError{Kind: KindNetwork, Err: err})
	}
	switch ae.Kind {
	case KindReauthRequired:
		return a.gate.terminal(StateReauthRequired, reauthError(
			"upstream rejected the stored credentials; the password has probably changed"))
	case KindCaptchaRequired:
		return a.gate.terminal(StateCaptchaRequired, ae)
	case KindRefreshTokenFailed, KindTokenExpired:
		return nil // try the next rung of the ladder
	default:
		if ae.Temporary() {
			return a.gate.backoff(now, ae)
		}
		return a.gate.backoff(now, ae)
	}
}

// adopt persists credentials before they become the in-memory ones, so a crash
// mid-rotation can never leave the store holding a refresh token upstream has
// already invalidated (§9.2). The caller must hold a.mu.
func (a *OAuth2) adopt(ctx context.Context, cred *StoredCredentials) {
	cred.UpdatedAt = a.cfg.now()
	if !a.cfg.persistPassword {
		cred.Password = ""
	}
	if a.cfg.store != nil {
		if err := a.cfg.store.Save(ctx, cred); err != nil {
			// Upstream has already rotated the credential: keeping the old one
			// would guarantee a logout, so the new one is adopted in memory and
			// the persistence failure is reported loudly instead.
			a.c.Logger().Error("cannot persist the rotated credentials; the session survives only in memory",
				slog.String("error", RedactString(err.Error())))
		}
	}
	a.cred = cred
	a.loaded = true
}

func (a *OAuth2) passwordGrant(ctx context.Context, username, password string) (*Token, error) {
	var out grantResponse
	err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointOAuth2Grant,
		SessionPlacement: SessionOmit,
		Body: map[string]string{
			"grant_type": "password",
			"client_id":  a.cfg.clientID,
			"username":   username,
			"password":   password,
		},
	}, &out)
	if err != nil {
		return nil, err
	}
	return a.tokenFrom(out, "")
}

func (a *OAuth2) refreshGrant(ctx context.Context, refresh string) (*Token, error) {
	var out grantResponse
	err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointOAuth2Grant,
		SessionPlacement: SessionOmit,
		Body: map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     a.cfg.clientID,
			"refresh_token": refresh,
		},
	}, &out)
	if err != nil {
		return nil, err
	}
	// Upstream rotates the refresh token on every exchange; if it withholds a
	// new one, the previous one stays valid (§2.2 B).
	return a.tokenFrom(out, refresh)
}

func (a *OAuth2) tokenFrom(g grantResponse, fallbackRefresh string) (*Token, error) {
	if g.AccessToken == "" {
		return nil, &APIError{Kind: KindInvalidResponse, UpstreamMsg: "grant response carried no access_token"}
	}
	now := a.cfg.now()
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
		IssuedAt:      now,
		Expiry:        now.Add(ttl),
		RefreshExpiry: now.Add(refreshTTL),
	}, nil
}
