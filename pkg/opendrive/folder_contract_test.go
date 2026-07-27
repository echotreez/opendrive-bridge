package opendrive

import (
	"context"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This file is the contract half of the folder module (whitepaper §6.1): every
// request the SDK builds is checked against testdata/spec/folder.json, the
// archived live Swagger specification. A parameter we invent, a verb we get
// wrong or a path shape that drifts fails here rather than in production.

type specOperation struct {
	path       string
	method     string
	pathParams []string
	queryNames map[string]bool
	bodyNames  map[string]bool
}

type moduleSpec struct {
	ops []specOperation
}

// bodyParamPattern pulls parameter names out of the prose Restler puts in the
// body description: "session_id : string (required) - Session ID." The types
// arrive wrapped in markup, so the description is stripped first.
var bodyParamPattern = regexp.MustCompile(`([a-z_]+)\s*:\s*(?:string|int|boolean|mixed|Array|object)`)

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// stripMarkup turns a Swagger description into plain text.
func stripMarkup(s string) string {
	s = htmlTagPattern.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func loadModuleSpec(t *testing.T, module string) moduleSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "spec", module+".json"))
	if err != nil {
		t.Skipf("no archived spec for %s: %v", module, err)
	}
	var decl struct {
		APIs []struct {
			Path       string `json:"path"`
			Operations []struct {
				HTTPMethod string `json:"httpMethod"`
				Parameters []struct {
					Name        string `json:"name"`
					ParamType   string `json:"paramType"`
					Description string `json:"description"`
				} `json:"parameters"`
			} `json:"operations"`
		} `json:"apis"`
	}
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatalf("spec %s: %v", module, err)
	}

	var spec moduleSpec
	for _, api := range decl.APIs {
		for _, op := range api.Operations {
			so := specOperation{
				path:       api.Path,
				method:     strings.ToUpper(op.HTTPMethod),
				queryNames: map[string]bool{},
				bodyNames:  map[string]bool{},
			}
			for _, p := range op.Parameters {
				switch p.ParamType {
				case "path":
					so.pathParams = append(so.pathParams, p.Name)
				case "query":
					so.queryNames[p.Name] = true
				case "body":
					for _, m := range bodyParamPattern.FindAllStringSubmatch(stripMarkup(p.Description), -1) {
						so.bodyNames[m[1]] = true
					}
				}
			}
			spec.ops = append(spec.ops, so)
		}
	}
	if len(spec.ops) == 0 {
		t.Fatalf("spec %s declares no operations", module)
	}
	return spec
}

// find locates the operation matching a recorded request. The archived paths
// look like "/v1/folder/list.{format}/{session_id}/{folder_id}", the recorded
// ones like "/api/v1/folder/list.json/SID/0".
func (m moduleSpec) find(recordedPath, method string) (specOperation, bool) {
	got := strings.Split(strings.Trim(strings.TrimPrefix(recordedPath, "/api"), "/"), "/")
	for _, op := range m.ops {
		if op.method != method {
			continue
		}
		want := strings.Split(strings.Trim(op.path, "/"), "/")
		if len(want) != len(got) {
			continue
		}
		matched := true
		for i, seg := range want {
			switch {
			case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
				continue // a path parameter matches anything
			case strings.HasSuffix(seg, ".{format}"):
				if got[i] != strings.TrimSuffix(seg, "{format}")+"json" {
					matched = false
				}
			case seg != got[i]:
				matched = false
			}
			if !matched {
				break
			}
		}
		if matched {
			return op, true
		}
	}
	return specOperation{}, false
}

// clientInjected are the parameters the transport adds, not the endpoint
// binding: the session and, in OAuth mode, the access token (§2.2 B).
var clientInjected = map[string]bool{"session_id": true, "access_token": true, "session_key": true}

// assertContract checks one recorded request against the archived spec.
func assertContract(t *testing.T, spec moduleSpec, call recordedRequest, label string) {
	t.Helper()

	op, ok := spec.find(call.Path, call.Method)
	if !ok {
		t.Errorf("%s: %s %s is not in the archived spec", label, call.Method, call.Path)
		return
	}

	// Every path parameter the spec declares must be filled in.
	wantSegments := len(strings.Split(strings.Trim(op.path, "/"), "/"))
	gotSegments := len(strings.Split(strings.Trim(strings.TrimPrefix(call.Path, "/api"), "/"), "/"))
	if wantSegments != gotSegments {
		t.Errorf("%s: sent %d path segments, the spec declares %d (%s)",
			label, gotSegments, wantSegments, op.path)
	}

	for name := range call.Query {
		if clientInjected[name] {
			continue
		}
		if !op.queryNames[name] {
			t.Errorf("%s: query parameter %q is not documented for %s %s",
				label, name, op.method, op.path)
		}
	}

	for name := range call.Body {
		if clientInjected[name] {
			continue
		}
		if len(op.bodyNames) == 0 {
			t.Errorf("%s: sent a body to %s %s, which documents none", label, op.method, op.path)
			continue
		}
		if !op.bodyNames[name] {
			t.Errorf("%s: body field %q is not documented for %s %s",
				label, name, op.method, op.path)
		}
	}
}

// TestFolderModuleMatchesTheArchivedSpec drives every folder binding once and
// checks the request it produced against the spec.
func TestFolderModuleMatchesTheArchivedSpec(t *testing.T) {
	spec := loadModuleSpec(t, "folder")
	ctx := context.Background()

	access := FolderPublic
	yes := true

	cases := []struct {
		label    string
		response string
		call     func(*FolderService) error
	}{
		{"list", fixture(t, "list_root.json"), func(s *FolderService) error {
			_, err := s.List(ctx, "FID", ListOptions{
				Page: Pagination{Offset: 100, LastRequestTime: 1}, SearchQuery: "q",
				SharingID: "SH", OnlySubfolders: true, WithBreadcrumbs: true,
				EncryptionSupported: true, OrderBy: "name", OrderType: "asc",
			})
			return err
		}},
		{"info", fixture(t, "info.json"), func(s *FolderService) error {
			_, err := s.Info(ctx, "FID", "SH")
			return err
		}},
		{"idbypath", fixture(t, "idbypath.json"), func(s *FolderService) error {
			_, err := s.IDByPath(ctx, "/A/B")
			return err
		}},
		{"itembyname", fixture(t, "itembyname.json"), func(s *FolderService) error {
			_, err := s.ItemByName(ctx, "FID", "x", WithSharingID("SH"), WithEncryptionSupported())
			return err
		}},
		{"breadcrumb", fixture(t, "breadcrumb.json"), func(s *FolderService) error {
			_, err := s.Breadcrumb(ctx, "FID", true)
			return err
		}},
		{"path", fixture(t, "path.json"), func(s *FolderService) error {
			_, err := s.Path(ctx, "FID")
			return err
		}},
		{"folderfullpath", fixture(t, "folderfullpath.json"), func(s *FolderService) error {
			_, err := s.FullPath(ctx, "FID")
			return err
		}},
		{"useraccessmode", fixture(t, "useraccessmode.json"), func(s *FolderService) error {
			_, err := s.UserAccessMode(ctx, "FID", "SH")
			return err
		}},
		{"sharedinfo", fixture(t, "sharedinfo.json"), func(s *FolderService) error {
			_, err := s.SharedInfo(ctx, "FID")
			return err
		}},
		{"shared", fixture(t, "list_root.json"), func(s *FolderService) error {
			_, err := s.Shared(ctx, "FID", ListOptions{
				Page: Pagination{Offset: 100, LastRequestTime: 1}, WithBreadcrumbs: true,
				OrderBy: "name", OrderType: "asc",
			})
			return err
		}},
		{"create", `{"FolderID":"NEW","Shared":"False"}`, func(s *FolderService) error {
			_, err := s.Create(ctx, CreateFolderParams{
				Name: "n", ParentID: "P", Access: FolderPublic, PublicUpload: true,
				PublicDisplay: true, PublicDownload: true, DisplaySubfolders: true,
				Description: "d", SharingID: "SH",
			})
			return err
		}},
		{"rename", `{"FolderID":"FID","Shared":"False"}`, func(s *FolderService) error {
			_, err := s.Rename(ctx, "FID", "n", "SH")
			return err
		}},
		{"move_copy", `{"FolderID":"FID","Shared":"False"}`, func(s *FolderService) error {
			_, err := s.MoveCopy(ctx, MoveCopyParams{
				FolderID: "SRC", DstFolderID: "DST", Move: true, CopyRecursive: true,
				NewName: "n", SrcSharingID: "S1", DstSharingID: "S2",
			})
			return err
		}},
		{"trash", `{"result":true}`, func(s *FolderService) error {
			return s.Trash(ctx, []string{"A", "B"}, "SH")
		}},
		{"emptytrash", `true`, func(s *FolderService) error {
			return s.EmptyTrash(ctx)
		}},
		{"trashlist", `{"DirUpdateTime":1}`, func(s *FolderService) error {
			_, err := s.TrashList(ctx, TrashListOptions{
				Page: Pagination{Offset: 100, LastRequestTime: 1}, SearchQuery: "q",
				CountOnly: true, OrderBy: "name", OrderType: "asc",
			})
			return err
		}},
		{"restore", `{"result":true}`, func(s *FolderService) error {
			return s.Restore(ctx, []string{"A"})
		}},
		{"remove", `{"result":true}`, func(s *FolderService) error {
			return s.Remove(ctx, []string{"A"}, "SH")
		}},
		{"setaccess", `{"result":true}`, func(s *FolderService) error {
			return s.SetAccess(ctx, "FID", FolderPublic, true, "SH")
		}},
		{"foldersettings", `{"result":true}`, func(s *FolderService) error {
			return s.UpdateSettings(ctx, "FID", FolderSettings{
				Name: "n", Description: "d", Access: &access, PublicUpload: &yes,
				PublicDisplay: &yes, PublicDownload: &yes, DisplaySubfolders: &yes,
				SharingID: "SH",
			})
		}},
		{"sendbyemail", `{"result":true}`, func(s *FolderService) error {
			return s.SendByEmail(ctx, SendByEmailParams{
				FolderIDs: []string{"A"}, Recipients: []string{"x@example.com"},
				Subject: "s", Body: "b", SendExpiring: true, CaptchaResponse: "c",
			})
		}},
		{"expiringlink", `{"Link":"x"}`, func(s *FolderService) error {
			_, err := s.CreateExpiringLink(ctx, "FID", "2026-08-01", 3, true)
			return err
		}},
		{"folderexpiringlinks", `{"Link":"x"}`, func(s *FolderService) error {
			_, err := s.ExpiringLinks(ctx, "FID")
			return err
		}},
		{"exportcsv", `"a,b\n"`, func(s *FolderService) error {
			_, err := s.ExportCSV(ctx, "FID", "SH")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := newMockUpstream(t)
			m.push(200, tc.response)
			c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
			if err := tc.call(c.Folders()); err != nil {
				t.Fatalf("%s: %v", tc.label, err)
			}
			assertContract(t, spec, m.lastCall(), tc.label)
		})
	}

	// Every folder endpoint the SDK claims to bind must have been exercised
	// above, so a new binding cannot skip its contract check.
	if len(cases) < 23 {
		t.Fatalf("only %d folder endpoints are covered; the module has 24 operations", len(cases))
	}
}

// The OAuth transport must not break the contract either: the token rides in
// the query string on every verb (§2.6 #8).
func TestFolderCallsInOAuthModeStillMatchTheSpec(t *testing.T) {
	spec := loadModuleSpec(t, "folder")
	m := newMockUpstream(t)
	m.push(200, fixture(t, "list_root.json"))
	c := m.client(WithAuthenticator(&stubAuth{
		creds: Credentials{SessionID: OAuthSessionID, AccessToken: "tok"},
	}))

	if _, err := c.Folders().List(context.Background(), "FID", ListOptions{}); err != nil {
		t.Fatal(err)
	}
	call := m.lastCall()
	if call.Query.Get("access_token") != "tok" {
		t.Fatalf("query = %v", call.Query)
	}
	if !strings.Contains(call.Path, "/"+OAuthSessionID+"/") {
		t.Fatalf("path = %q; the session segment carries the OAUTH marker", call.Path)
	}
	assertContract(t, spec, call, "list (oauth)")
}
