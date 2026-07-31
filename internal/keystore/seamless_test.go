package keystore

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// TestSeamlessAcrossRestarts is the product requirement end to end (§2.2): the
// user logs in once, the daemon restarts, and everything keeps working without
// anybody typing a password — including after upstream has retired both tokens,
// which is the state a bridge comes back to after a long holiday.
func TestSeamlessAcrossRestarts(t *testing.T) {
	ctx := context.Background()

	var (
		mu       sync.Mutex
		access   = "access-1"
		refresh  = "refresh-1"
		issued   = 1
		grants   int
		password = "correct horse"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		defer mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/grant.json"):
			switch body["grant_type"] {
			case "password":
				grants++
				if body["password"] != password {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"Invalid username or password"}`))
					return
				}
			case "refresh_token":
				if body["refresh_token"] != refresh {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Refresh token is invalid"}`))
					return
				}
			}
			issued++
			access = "access-" + string(rune('0'+issued))
			refresh = "refresh-" + string(rune('0'+issued))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": access, "refresh_token": refresh, "expires_in": 86400,
			})
		default:
			if r.URL.Query().Get("access_token") != access {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":401,"error":"invalid_token"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"UserName":"derek"}`))
		}
	}))
	defer srv.Close()

	newClient := func() *opendrive.Client {
		c, err := opendrive.New(
			opendrive.WithBaseURL(srv.URL+"/api/v1"),
			opendrive.WithHTTPClient(srv.Client()),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}
	call := func(c *opendrive.Client) error {
		var out opendrive.UserInfo
		return c.Do(ctx, opendrive.Request{Method: http.MethodGet, Path: opendrive.EndpointUsersInfo}, &out)
	}

	// The KDF is slowed to 600000 iterations in production and this test seals
	// the file a dozen times; the format is not what it is checking.
	cheapKDF(t)

	// The credential store is a real encrypted file, as it would be in Docker.
	t.Setenv(DefaultKeyEnv, "a configured passphrase")
	path := filepath.Join(t.TempDir(), "credentials.enc")
	store, err := Open(Config{Backend: BackendFile, Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// --- first run: the one and only time a password is involved.
	first := newClient()
	if _, err := opendrive.Login(ctx, first, opendrive.AuthModeOAuth2, "derek", "correct horse", store); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := call(first); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// --- restart: a brand new process reopens the same file and resumes.
	reopened, err := Open(Config{Backend: BackendFile, Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	second := newClient()
	auth, err := opendrive.Resume(ctx, second, reopened)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if auth.AuthState() != opendrive.StateAuthenticated {
		t.Fatalf("state after restart = %q", auth.AuthState())
	}
	if id := auth.Identity(); id.Username != "derek" || !id.Seamless {
		t.Fatalf("identity after restart = %+v", id)
	}
	if err := call(second); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if grants != 1 {
		t.Fatalf("password grants = %d; a restart must not need the password", grants)
	}

	// --- long holiday: upstream retired both tokens. The stored password gets
	// the bridge back on its feet, still silently.
	mu.Lock()
	access, refresh = "rotated", "rotated"
	mu.Unlock()

	third := newClient()
	reopenedAgain, err := Open(Config{Backend: BackendFile, Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := opendrive.Resume(ctx, third, reopenedAgain); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := call(third); err != nil {
		t.Fatalf("after a long idle period: %v", err)
	}
	if grants != 2 {
		t.Fatalf("password grants = %d, want one silent re-login", grants)
	}

	// --- the user changes the password elsewhere: now, and only now, the
	// bridge asks for help.
	mu.Lock()
	password = "a new password"
	access, refresh = "rotated-again", "rotated-again"
	mu.Unlock()

	fourth := newClient()
	reopenedOnce, err := Open(Config{Backend: BackendFile, Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	auth, err = opendrive.Resume(ctx, fourth, reopenedOnce)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	err = call(fourth)
	if opendrive.ErrorKind(err) != opendrive.KindReauthRequired {
		t.Fatalf("err = %v, want reauth_required", err)
	}
	if auth.AuthState() != opendrive.StateReauthRequired {
		t.Fatalf("state = %q", auth.AuthState())
	}

	// The user supplies the new password once and seamlessness resumes.
	if _, err := opendrive.Login(ctx, fourth, opendrive.AuthModeOAuth2, "derek", "a new password", reopenedOnce); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if err := call(fourth); err != nil {
		t.Fatalf("after re-login: %v", err)
	}

	stored, err := reopenedOnce.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.Password != "a new password" {
		t.Fatal("the new password was not persisted")
	}
}
