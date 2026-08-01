package server

import (
	"context"
	"net/http"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// StatusResponse is the /v1/auth/status schema of whitepaper §4.1.
type StatusResponse struct {
	// Account is null until credentials are configured, and answering that
	// costs no network call — which is the whole point of the endpoint.
	Account  *AccountBlock `json:"account"`
	AuthMode string        `json:"auth_mode"`
	State    string        `json:"state"`
	Seamless bool          `json:"seamless"`
	// TokenExpiresAt is null in session mode and before the first sign-in.
	TokenExpiresAt *time.Time    `json:"token_expires_at"`
	Keystore       KeystoreBlock `json:"keystore"`
	// Quota is fetched lazily and is null when it cannot be had. It never
	// blocks the rest of the answer (§4.1).
	Quota *QuotaBlock `json:"quota"`
}

// AccountBlock identifies the configured login.
type AccountBlock struct {
	Username string `json:"username"`
	UserID   string `json:"user_id"`
	AccType  int    `json:"acc_type"`
}

// KeystoreBlock reports where credentials live and whether they can be read
// right now.
type KeystoreBlock struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
}

// QuotaBlock is the account's usage.
type QuotaBlock struct {
	StorageUsed int64 `json:"storage_used"`
	StorageMax  int64 `json:"storage_max"`
	BWUsed      int64 `json:"bw_used"`
	BWMax       int64 `json:"bw_max"`
}

// loginRequest is the /v1/auth/login body.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin signs in and hands the credentials to the CredentialStore, after
// which the bridge runs silently until the password changes (§2.2, §4.1).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		WriteError(w, r, BadRequest("Send your OpenDrive username and password as "+
			`{"username": "...", "password": "..."}.`))
		return
	}

	// The credential store is checked first. Signing in against a store that
	// cannot hold the result would look like success and then forget everything
	// on restart, and retrying blindly is how an account meets a captcha lock
	// (§4.5, keystore_unavailable makes no upstream request).
	if err := s.keystoreAvailable(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}

	if err := s.auth.Login(r.Context(), req.Username, req.Password); err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeStatus(w, r, http.StatusOK)
}

// handleLogout revokes upstream and clears every stored credential, the
// password included (§4.1).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "signed out",
		"detail": "The bridge has forgotten your password and will not sign in again until you do.",
	})
}

// handleStatus answers from local state.
//
// It must work when nothing is configured, when the keyring is locked, and when
// upstream is unreachable — those are exactly the moments somebody runs it. So
// the only part that touches the network is the quota, and that is allowed to
// fail without taking the rest of the answer with it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.writeStatus(w, r, http.StatusOK)
}

func (s *Server) writeStatus(w http.ResponseWriter, r *http.Request, code int) {
	identity := s.auth.Identity()
	state := s.auth.AuthState()

	ks := KeystoreBlock{Backend: string(keystore.BackendEphemeral)}
	if s.store != nil {
		ks.Backend = string(s.store.Backend())
		ks.Available = s.store.Available(r.Context()) == nil
	}
	// A store that cannot be read outranks whatever the authenticator last
	// managed to do: nothing can be renewed until it comes back, and no upstream
	// request may be attempted meanwhile (§4.5).
	if !ks.Available && s.store != nil {
		state = opendrive.StateKeystoreUnavailable
	}

	out := StatusResponse{
		AuthMode: string(identity.AuthMode),
		State:    string(state),
		Seamless: identity.Seamless,
		Keystore: ks,
	}
	if identity.Username != "" {
		out.Account = &AccountBlock{
			Username: identity.Username,
			UserID:   identity.UserID,
			AccType:  identity.AccType,
		}
	} else if state == opendrive.StateAuthenticated {
		// Authenticated with no identity would be a contradiction; report the
		// configuration state honestly instead.
		out.State = string(opendrive.StateNotConfigured)
	}

	if tr, ok := s.auth.(TokenReporter); ok {
		if tok, have := tr.Token(); have && !tok.Expiry.IsZero() {
			expiry := tok.Expiry.UTC()
			out.TokenExpiresAt = &expiry
		}
	}

	// One account read serves both the quota and the parts of the identity the
	// OAuth2 grant never carries. Doing it once, only when the state allows any
	// upstream request at all, is what keeps status cheap and safe.
	if info := s.accountInfo(r.Context(), state); info != nil {
		out.Quota = quotaFrom(info)
		if out.Account != nil {
			if out.Account.UserID == "" {
				out.Account.UserID = info.UserID.String()
			}
			if out.Account.AccType == 0 {
				out.Account.AccType = info.AccType.Int()
			}
		}
	}
	writeJSON(w, r, code, out)
}

// bytesPerMB converts the two account limits that arrive in megabytes.
//
// Upstream reports usage in bytes and the limits in megabytes, in the same
// object: an account measured at 188726232 used against a "maximum" of 1048576
// (docs/discrepancies.md D45). Passing both through unchanged would tell a user
// they are 180 times over a one-megabyte quota. Normalising to bytes here is the
// Bridge doing its job — one unit per field, and the field means what it says.
const bytesPerMB = 1 << 20

func quotaFrom(info *opendrive.AccountInfo) *QuotaBlock {
	return &QuotaBlock{
		StorageUsed: info.StorageUsed.Int64(),
		StorageMax:  info.MaxStorage.Int64() * bytesPerMB,
		BWUsed:      info.BwUsed.Int64(),
		BWMax:       info.BwMax.Int64() * bytesPerMB,
	}
}

// accountInfo reads the account, and gives up quietly.
//
// Two conditions stop it before it starts: nothing configured, and a state in
// which no upstream request may be made at all. The second is not an
// optimisation — asking upstream while the keystore is locked or a captcha is
// pending is precisely what walks an account into a lockout (§2.2, §4.5).
func (s *Server) accountInfo(ctx context.Context, state opendrive.AuthState) *opendrive.AccountInfo {
	if s.client == nil {
		return nil
	}
	switch state {
	case opendrive.StateAuthenticated, opendrive.StateRefreshing:
	default:
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	info, err := s.client.Users().Info(ctx)
	if err != nil {
		if log := loggerFrom(ctx); log != nil {
			log.Debug("account details unavailable; status answers without them",
				"error", opendrive.RedactString(err.Error()))
		}
		return nil
	}
	return info
}

// keystoreAvailable turns an unreadable credential store into the one error
// that must never be confused with a wrong password.
func (s *Server) keystoreAvailable(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	if err := s.store.Available(ctx); err != nil {
		return &opendrive.APIError{
			Kind:        opendrive.KindKeystoreUnavailable,
			UpstreamMsg: "the credential store cannot be read, so no sign-in was attempted",
			Err:         err,
		}
	}
	return nil
}
