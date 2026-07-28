package opendrive

import (
	"context"
	"testing"
	"time"
)

// TestSharingModuleMatchesTheArchivedSpec drives every sharing binding once and
// checks the request against testdata/spec/sharing.json (whitepaper §6.1).
//
// This module has no recorded responses to test against (D33), which makes the
// contract check the only automated guard on it — worth stating plainly.
func TestSharingModuleMatchesTheArchivedSpec(t *testing.T) {
	spec := loadModuleSpec(t, "sharing")
	ctx := context.Background()

	cases := []struct {
		label    string
		response string
		call     func(*SharingService) error
	}{
		{"share", `{"SharingID":"SHID"}`, func(s *SharingService) error {
			_, err := s.Share(ctx, "FID", "user@example.com", ShareFullAccess)
			return err
		}},
		{"setmode", `{"result":true}`, func(s *SharingService) error {
			return s.SetMode(ctx, "SHID", ShareViewOnly)
		}},
		{"revoke", `{"result":true}`, func(s *SharingService) error {
			return s.Revoke(ctx, "SHID")
		}},
		{"listsharedfolders", `[]`, func(s *SharingService) error {
			_, err := s.ListSharedFolders(ctx, "SHID")
			return err
		}},
		{"listsharedusers", `[]`, func(s *SharingService) error {
			_, err := s.ListSharedUsers(ctx)
			return err
		}},
		{"listusers", `[]`, func(s *SharingService) error {
			_, err := s.ListFolderUsers(ctx, "FID")
			return err
		}},
		{"checkaccountusersaccess", `{"result":true}`, func(s *SharingService) error {
			_, err := s.CheckAccountUsersAccess(ctx)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := newMockUpstream(t)
			m.push(200, tc.response)
			c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
			if err := tc.call(c.Sharing()); err != nil {
				t.Fatalf("%s: %v", tc.label, err)
			}
			assertContract(t, spec, m.lastCall(), tc.label)
		})
	}

	if len(cases) != 7 {
		t.Fatalf("covered %d operations, want all 7 sharing operations", len(cases))
	}
}

// TestUsersModuleMatchesTheArchivedSpec covers the read half that v1.0 binds.
// The module has fourteen operations; the eleven mutating ones are v1.1 (§1.4).
func TestUsersModuleMatchesTheArchivedSpec(t *testing.T) {
	spec := loadModuleSpec(t, "users")
	ctx := context.Background()
	logType := 1

	cases := []struct {
		label    string
		response string
		call     func(*UsersService) error
	}{
		{"info", moduleFixture(t, "users", "info.json"), func(s *UsersService) error {
			_, err := s.Info(ctx, AccountInfoOptions{ApplyBW: true, Branding: true})
			return err
		}},
		{"userlogs", moduleFixture(t, "users", "userlogs.json"), func(s *UsersService) error {
			_, err := s.Logs(ctx, 2, ActivityLogFilter{
				AccessUserID: "60516",
				Start:        NewUnixTime(time.Unix(1785000000, 0)),
				End:          NewUnixTime(time.Unix(1785999999, 0)),
				LogType:      &logType,
			})
			return err
		}},
		{"userlogscursor", moduleFixture(t, "users", "userlogscursor.json"), func(s *UsersService) error {
			_, err := s.LogsCursor(ctx, "CURSOR", ActivityLogFilter{AccessUserID: "60516"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := newMockUpstream(t)
			m.push(200, tc.response)
			c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
			if err := tc.call(c.Users()); err != nil {
				t.Fatalf("%s: %v", tc.label, err)
			}
			assertContract(t, spec, m.lastCall(), tc.label)
		})
	}
}

// The endpoint constants for both modules must still match the archive.
func TestSharingAndUsersEndpointsMatchTheArchive(t *testing.T) {
	routes := []struct{ module, path, method string }{
		{"sharing", EndpointSharing, "POST"},
		{"sharing", EndpointSharing, "DELETE"},
		{"sharing", EndpointSharingSetMode, "PUT"},
		{"sharing", EndpointSharingListFolders, "GET"},
		{"sharing", EndpointSharingListUsers, "GET"},
		{"sharing", EndpointSharingListFolderUsers, "GET"},
		{"sharing", EndpointSharingCheckAccess, "GET"},
		{"users", EndpointUsersInfo, "GET"},
		{"users", EndpointUsersLogs, "GET"},
		{"users", EndpointUsersLogsCursor, "GET"},
	}

	specs := map[string]moduleSpec{}
	for _, r := range routes {
		if _, ok := specs[r.module]; !ok {
			specs[r.module] = loadModuleSpec(t, r.module)
		}
		want := "/v1" + trimJSONSuffix(r.path) + ".{format}"
		found := false
		for _, op := range specs[r.module].ops {
			if op.method != r.method {
				continue
			}
			if op.path == want || len(op.path) > len(want) && op.path[:len(want)+1] == want+"/" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %s is not in the archived %s module (looked for %q)",
				r.method, r.path, r.module, want)
		}
	}
}

func trimJSONSuffix(p string) string {
	const suffix = ".json"
	if len(p) > len(suffix) && p[len(p)-len(suffix):] == suffix {
		return p[:len(p)-len(suffix)]
	}
	return p
}
