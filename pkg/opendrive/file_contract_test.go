package opendrive

import (
	"context"
	"testing"
)

// TestFileModuleMatchesTheArchivedSpec is the contract half of the file
// binding (whitepaper §6.1). It drives every public file endpoint once and
// checks its method, route, path segments, query and JSON body against the
// authenticated Swagger archive in testdata/spec/file.json.
func TestFileModuleMatchesTheArchivedSpec(t *testing.T) {
	spec := loadModuleSpec(t, "file")
	ctx := context.Background()
	visibility := FilePublic
	name, description, price := "new.txt", "description", "0"
	yes := true

	cases := []struct {
		label    string
		response string
		call     func(*FileService) error
	}{
		{"info", `{"FileId":"FID"}`, func(s *FileService) error {
			_, err := s.Info(ctx, "FID", "SH")
			return err
		}},
		{"idbypath", `{"FileId":"FID"}`, func(s *FileService) error {
			_, err := s.IDByPath(ctx, "/A/a.txt")
			return err
		}},
		{"path", `{"Path":"A/a.txt"}`, func(s *FileService) error {
			_, err := s.Path(ctx, "FID")
			return err
		}},
		{"filefullpath", `{"FullPath":"A/a.txt"}`, func(s *FileService) error {
			_, err := s.FullPath(ctx, "FID")
			return err
		}},
		{"fileversions", `[]`, func(s *FileService) error {
			_, err := s.Versions(ctx, "123")
			return err
		}},
		{"thumb", "thumbnail-bytes", func(s *FileService) error {
			at := 2.5
			_, err := s.Thumbnail(ctx, "FID", ThumbnailOptions{SharingID: "SH", TimeOffset: &at, TempKey: "TEMP"})
			return err
		}},
		{"create", `{"FileId":"NEW"}`, func(s *FileService) error {
			_, err := s.CreateEmpty(ctx, CreateEmptyFileParams{AccessFolderID: "AF", FolderID: "DIR", FileType: "text/plain", SharingID: "SH"})
			return err
		}},
		{"access", `true`, func(s *FileService) error {
			return s.SetAccess(ctx, "FID", FilePublic, "AF", "SH")
		}},
		{"move_copy", `{"FileId":"NEW"}`, func(s *FileService) error {
			_, err := s.MoveCopy(ctx, FileMoveCopyParams{SourceFileID: "SRC", DestinationFolder: "DST", Move: false, OverwriteIfExists: true, SourceAccessID: "SA", DestinationAccess: "DA", SourceSharingID: "S1", DestinationShare: "S2", NewName: "copy.txt"})
			return err
		}},
		{"rename", `{"FileId":"FID"}`, func(s *FileService) error {
			_, err := s.Rename(ctx, "FID", "new.txt", "AF", "SH")
			return err
		}},
		{"trash", `true`, func(s *FileService) error {
			return s.Trash(ctx, []string{"A", "B"}, "SH")
		}},
		{"restore", `true`, func(s *FileService) error {
			return s.Restore(ctx, []string{"A", "B"})
		}},
		{"remove", `true`, func(s *FileService) error {
			return s.Remove(ctx, []string{"A", "B"}, "AF", "SH")
		}},
		{"delete", `true`, func(s *FileService) error {
			return s.DeleteTrashed(ctx, "FID", "AF", "SH")
		}},
		{"removeversion", `true`, func(s *FileService) error {
			return s.RemoveVersion(ctx, "FID")
		}},
		{"filesettings", `true`, func(s *FileService) error {
			return s.UpdateSettings(ctx, "FID", FileSettings{Price: &price, Name: &name, Description: &description, Visibility: &visibility, EditOnline: &yes, SharingID: "SH"})
		}},
		{"verifypassword", `{"TempKey":"TEMP","Valid":true}`, func(s *FileService) error {
			_, err := s.VerifyPassword(ctx, "FID", "password", "captcha")
			return err
		}},
		{"sendbyemail", `true`, func(s *FileService) error {
			return s.SendByEmail(ctx, FileEmailParams{FileIDs: []string{"A", "B"}, Recipients: []string{"a@example.com", "b@example.com"}, Subject: "s", Body: "b", SendExpiring: true, CaptchaResponse: "captcha"})
		}},
		{"expiringlink", `{"Link":"https://od.lk/f/x","Counter":2}`, func(s *FileService) error {
			_, err := s.CreateExpiringLink(ctx, "FID", "2026-08-01", 2, true)
			return err
		}},
		{"fileexpiringlinks", `{}`, func(s *FileService) error {
			_, err := s.ExpiringLinks(ctx, "FID")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := newMockUpstream(t)
			m.push(200, tc.response)
			c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
			if err := tc.call(c.Files()); err != nil {
				t.Fatalf("%s: %v", tc.label, err)
			}
			assertContract(t, spec, m.lastCall(), tc.label)
		})
	}

	if len(cases) != 20 {
		t.Fatalf("covered %d operations, want all 20 file operations", len(cases))
	}
}

// The OAuth transport adds session_id=OAUTH to a query-only endpoint and an
// access token to every verb. Its injected parameters are intentionally not
// part of the endpoint contract itself (§2.2 B, §2.6 #8).
func TestFileCallsInOAuthModeStillMatchTheSpec(t *testing.T) {
	spec := loadModuleSpec(t, "file")
	m := newMockUpstream(t)
	m.push(200, `{"FileId":"FID"}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: OAuthSessionID, AccessToken: "tok"}}))

	if _, err := c.Files().Info(context.Background(), "FID"); err != nil {
		t.Fatal(err)
	}
	call := m.lastCall()
	if call.Query.Get("session_id") != OAuthSessionID || call.Query.Get("access_token") != "tok" {
		t.Fatalf("query = %v", call.Query)
	}
	assertContract(t, spec, call, "info (oauth)")
}
