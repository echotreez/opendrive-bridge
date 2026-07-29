package opendrive

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
)

// SessionAuth implements Authenticator with the legacy session mode (§2.2 A).
//
// Since v1.1 it is not a degraded mode: the password is persisted just like in
// OAuth2 mode, so an expired session is rebuilt silently and the user is never
// asked for anything (§2.2 #5). Only the renewal ladder is shorter — there is
// no refresh token, so a dead session goes straight to a silent login.
type SessionAuth struct {
	c   *Client
	cfg authConfig

	mu     sync.Mutex
	cred   *StoredCredentials
	info   SessionLogin
	loaded bool
	gate   renewGate
}

// NewSessionAuth creates a session-mode authenticator bound to c.
func NewSessionAuth(c *Client, opts ...AuthOption) *SessionAuth {
	cfg := defaultAuthConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return &SessionAuth{c: c, cfg: cfg, gate: renewGate{state: StateNotConfigured}}
}

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

// Login performs POST /session/login.json and persists the credentials. A
// captcha challenge surfaces as KindCaptchaRequired, which callers must not
// retry: only the user can solve it (§2.6 #11).
func (a *SessionAuth) Login(ctx context.Context, username, password string, opts ...LoginOption) (*SessionLogin, error) {
	out, err := a.login(ctx, username, password, opts...)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	cred := &StoredCredentials{
		Username:  username,
		SessionID: out.SessionID,
		AuthMode:  AuthModeSession,
		UserID:    out.UserID.String(),
		AccType:   out.AccType.Int(),
	}
	if a.cfg.persistPassword {
		cred.Password = password
	}
	a.info = *out
	a.loaded = true
	a.adopt(ctx, cred)
	a.gate.reset()
	return out, nil
}

// Credentials implements Authenticator.
func (a *SessionAuth) Credentials(ctx context.Context) (Credentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensureLoaded(ctx); err != nil {
		return Credentials{}, err
	}
	if a.cred == nil || (a.cred.SessionID == "" && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return Credentials{}, notConfiguredError()
	}
	if a.cred.SessionID == "" {
		if err := a.renewLocked(ctx); err != nil {
			return Credentials{}, err
		}
	} else if a.gate.state != StateAuthenticated {
		a.gate.succeed()
	}
	return Credentials{SessionID: a.cred.SessionID}, nil
}

// Refresh implements Authenticator: upstream forgot the session, so a new one
// is built from the stored password without involving the user (§2.2 #5).
func (a *SessionAuth) Refresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if a.cred == nil || (a.cred.SessionID == "" && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return notConfiguredError()
	}
	// The current session is known bad; drop it before rebuilding so a
	// concurrent caller cannot hand it out again.
	a.cred.SessionID = ""
	return asError(a.renewLocked(ctx))
}

// EnsureFresh verifies the stored session and rebuilds it when upstream has
// forgotten it. The daemon's keep-alive timer calls it the same way it calls
// the OAuth2 one (§2.2 #2).
func (a *SessionAuth) EnsureFresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoaded(ctx); err != nil {
		return err
	}
	if a.cred == nil || (a.cred.SessionID == "" && a.cred.Password == "") {
		a.gate.state = StateNotConfigured
		return notConfiguredError()
	}
	if a.cred.SessionID != "" {
		ok, err := a.exists(ctx, a.cred.SessionID)
		if err == nil && ok {
			a.gate.succeed()
			return nil
		}
		a.cred.SessionID = ""
	}
	return asError(a.renewLocked(ctx))
}

// Logout ends the session upstream and clears every stored credential.
func (a *SessionAuth) Logout(ctx context.Context) error {
	a.mu.Lock()
	session := ""
	if a.cred != nil {
		session = a.cred.SessionID
	}
	a.cred = nil
	a.info = SessionLogin{}
	a.loaded = true
	a.gate = renewGate{state: StateNotConfigured}
	store := a.cfg.store
	a.mu.Unlock()

	var reqErr error
	if session != "" {
		var out BoolResult
		reqErr = a.c.Do(ctx, Request{
			Method:           http.MethodPost,
			Path:             EndpointSessionLogout,
			SessionPlacement: SessionOmit,
			Body:             map[string]string{"session_id": session},
		}, &out)
	}
	if store != nil {
		if err := store.Delete(ctx); err != nil && !errors.Is(err, ErrNoCredentials) {
			return keystoreError(err)
		}
	}
	return reqErr
}

// Identity implements Authenticator.
func (a *SessionAuth) Identity() Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cred == nil {
		return Identity{AuthMode: AuthModeSession}
	}
	return Identity{
		Username: a.cred.Username,
		UserID:   a.cred.UserID,
		AccType:  a.cred.AccType,
		AuthMode: AuthModeSession,
		Seamless: a.cred.Password != "" && a.gate.state != StateKeystoreUnavailable,
	}
}

// AuthState implements Authenticator.
func (a *SessionAuth) AuthState() AuthState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gate.state
}

// CurrentSessionID returns the session id held right now, empty when there is
// none. It makes no network call.
func (a *SessionAuth) CurrentSessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cred == nil {
		return ""
	}
	return a.cred.SessionID
}

// SessionID implements SessionIDProvider, rebuilding the session first when
// there is none. In session mode this is simply the session already in use
// (docs/discrepancies.md D38).
func (a *SessionAuth) SessionID(ctx context.Context) (string, error) {
	creds, err := a.Credentials(ctx)
	if err != nil {
		return "", err
	}
	return creds.SessionID, nil
}

// Info returns the account information the last login response carried.
func (a *SessionAuth) Info() SessionLogin {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.info
}

// Exists checks the stored session with POST /session/exists.json.
func (a *SessionAuth) Exists(ctx context.Context) (bool, error) {
	a.mu.Lock()
	session := ""
	if a.cred != nil {
		session = a.cred.SessionID
	}
	a.mu.Unlock()
	if session == "" {
		return false, nil
	}
	return a.exists(ctx, session)
}

// ---------------------------------------------------------------- internals

func (a *SessionAuth) adoptLoaded(cred *StoredCredentials) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cred = cred.Clone()
	a.loaded = true
	if a.cred != nil && (a.cred.SessionID != "" || a.cred.Password != "") {
		a.gate.state = StateAuthenticated
	}
}

func (a *SessionAuth) ensureLoaded(ctx context.Context) *APIError {
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
		return a.gate.terminal(StateKeystoreUnavailable, keystoreError(err))
	}
	a.loaded = true
	a.cred = cred
	if cred != nil && (cred.SessionID != "" || cred.Password != "") {
		a.gate.state = StateAuthenticated
	}
	return nil
}

// renewLocked rebuilds the session from the stored password. The caller must
// hold a.mu, which keeps renewal single-flight.
func (a *SessionAuth) renewLocked(ctx context.Context) *APIError {
	now := a.cfg.now()
	if blocked := a.gate.blocked(now); blocked != nil {
		return blocked
	}
	a.gate.state = StateRefreshing

	if a.cred.Password == "" {
		return a.gate.terminal(StateReauthRequired, reauthError(
			"the session has expired and no password is stored; log in again"))
	}

	out, err := a.login(ctx, a.cred.Username, a.cred.Password)
	if err == nil {
		next := a.cred.Clone()
		next.SessionID = out.SessionID
		next.UserID, next.AccType = out.UserID.String(), out.AccType.Int()
		a.info = *out
		a.adopt(ctx, next)
		a.gate.succeed()
		a.c.Logger().Info("silently rebuilt the upstream session with the stored password")
		return nil
	}

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
	default:
		return a.gate.backoff(now, ae)
	}
}

func (a *SessionAuth) adopt(ctx context.Context, cred *StoredCredentials) {
	cred.UpdatedAt = a.cfg.now()
	if !a.cfg.persistPassword {
		cred.Password = ""
	}
	if a.cfg.store != nil {
		if err := a.cfg.store.Save(ctx, cred); err != nil {
			a.c.Logger().Error("cannot persist the session credentials; they survive only in memory",
				slog.String("error", RedactString(err.Error())))
		}
	}
	a.cred = cred
	a.loaded = true
}

// login performs the raw login call without touching any state.
func (a *SessionAuth) login(ctx context.Context, username, password string, opts ...LoginOption) (*SessionLogin, error) {
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
		Path:             EndpointSessionLogin,
		SessionPlacement: SessionOmit,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	if out.SessionID == "" {
		return nil, &APIError{Kind: KindInvalidResponse, UpstreamMsg: "login response carried no SessionID"}
	}
	return &out, nil
}

func (a *SessionAuth) exists(ctx context.Context, session string) (bool, error) {
	var out BoolResult
	if err := a.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointSessionExists,
		SessionPlacement: SessionOmit,
		Body:             map[string]string{"session_id": session},
	}, &out); err != nil {
		return false, err
	}
	return out.OK(), nil
}

// CaptchaRequired reports whether upstream currently demands a captcha for this
// client or account. The endpoint exists online but not in the PDF
// (docs/discrepancies.md D2).
func CaptchaRequired(ctx context.Context, c *Client, username string) (*CaptchaStatus, error) {
	q := map[string][]string{}
	if username != "" {
		q["username"] = []string{username}
	}
	var out CaptchaStatus
	if err := c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointSessionCaptchaRequired,
		Query:            q,
		SessionPlacement: SessionOmit,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
