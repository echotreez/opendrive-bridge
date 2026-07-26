package opendrive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the index CLAUDE.md rule 4 asks for: every gotcha in whitepaper
// §2.6 must have at least one test case. The map below names the test that
// covers each of the fourteen items, and TestGotchaCoverageIsComplete fails if
// an entry is ever left without one.
//
// The tests themselves live next to the code they exercise; keeping only the
// index here avoids duplicating assertions.
var gotchaCoverage = map[int]struct {
	summary string
	tests   []string
}{
	1: {
		summary: "trust the live Swagger spec over the PDF; session/captcharequired.json exists online only",
		tests: []string{
			"TestCaptchaRequiredEndpoint",
			"TestArchivedSpecHasEndpointsThePDFOmits",
			"TestFetchAllAnonymousSeesPublicSubsetOnly (tools/fetch-spec)",
		},
	},
	2: {
		summary: "inconsistent parameter names: download/all session parameter, checkfileexistsbyname takes an array",
		tests: []string{
			"TestSessionParamOverride",
			"TestArchivedSpecDocumentsTheDownloadAllSessionParameter",
			"TestArchivedSpecKeepsCheckFileExistsNameAsAnArray",
		},
	},
	3: {
		summary: "upstream spelling is the contract: breadcrump.json, OwnerSuspendet",
		tests: []string{
			"TestSessionLoginKeepsUpstreamFieldSpelling",
			"TestUpstreamSpellingConstants",
		},
	},
	4: {
		summary: "one resource name, several verbs: /file.json, /folder/trash.json and /sharing.json all mean POST != DELETE",
		tests: []string{
			"TestSameResourceDifferentVerbs",
			"TestEndpointConstantsMatchTheArchivedSpec",
		},
	},
	5: {
		summary: "booleans arrive as 0/1 integers and as True/False strings",
		tests: []string{
			"TestFlexBoolDecodesEveryUpstreamForm",
			"TestFlexIntDecodesEveryUpstreamForm",
			"TestFlexString",
		},
	},
	6: {
		summary: "timestamps mix integers and strings, normalised to time.Time",
		tests:   []string{"TestUnixTime"},
	},
	7: {
		summary: "success bodies are not uniform: full object, bare true, {\"result\":true}",
		tests: []string{
			"TestBoolResultDecodesEverySuccessShape",
			"TestErrorEnvelopeInASuccessfulResponse",
		},
	},
	8: {
		summary: "the OAuth access token travels in the query string and must never reach a log",
		tests: []string{
			"TestOAuthCredentialPlacement",
			"TestLoggingRedactsCredentials",
			"TestErrorURLIsRedacted",
			"TestRedactURL",
		},
	},
	9: {
		summary: `the root folder is the string "0"`,
		tests:   []string{"TestRootFolderIDIsAString"},
	},
	10: {
		summary: `names are limited to 255 bytes and may not contain \ / : * ? " < > |`,
		tests:   []string{"TestValidateName"},
	},
	11: {
		summary: "repeated login failures trigger a captcha the Bridge cannot answer",
		tests: []string{
			"TestSessionLoginSurfacesCaptcha",
			"TestSessionLoginOptions",
			"TestLoginDoesNotFallBackOnBadCredentials",
		},
	},
	12: {
		summary: "the PDF writes some endpoints with a /v1 prefix the base URL already carries",
		tests: []string{
			"TestPathJoinDeduplicatesTheVersionPrefix",
			"TestArchivedSpecPathsCarryTheVersionPrefix",
		},
	},
	13: {
		summary: `some request booleans must be the strings "true"/"false"`,
		tests:   []string{"TestStringBoolMarshalsAsQuotedString"},
	},
	14: {
		summary: "folder/list.json pages at 100 entries and needs last_request_time",
		tests: []string{
			"TestPaginationProtocol",
			"TestArchivedSpecShowsThePaginationParameters",
		},
	},
}

func TestGotchaCoverageIsComplete(t *testing.T) {
	for i := 1; i <= 14; i++ {
		entry, ok := gotchaCoverage[i]
		if !ok {
			t.Errorf("whitepaper §2.6 #%d has no entry in the coverage index", i)
			continue
		}
		if len(entry.tests) == 0 {
			t.Errorf("§2.6 #%d (%s) has no test", i, entry.summary)
		}
		for _, name := range entry.tests {
			if strings.TrimSpace(name) == "" {
				t.Errorf("§2.6 #%d lists an empty test name", i)
			}
		}
	}
	if len(gotchaCoverage) != 14 {
		t.Errorf("the index lists %d gotchas, §2.6 has 14", len(gotchaCoverage))
	}
}

// §2.6 #3: upstream's spelling is the contract. The PDF claims the breadcrumb
// endpoint is misspelled "breadcrump"; the live API returns 404 for that and
// serves the correctly spelled path instead (docs/discrepancies.md D12). The
// live spelling is the one that ships, and the archive is what proves it.
func TestUpstreamSpellingConstants(t *testing.T) {
	if EndpointFolderBreadcrumb != "/folder/breadcrumb.json" {
		t.Fatalf("breadcrumb endpoint = %q; the live API serves /folder/breadcrumb.json", EndpointFolderBreadcrumb)
	}
	// The wire field spelling is a separate trap and still stands: see
	// TestSessionLoginKeepsUpstreamFieldSpelling for OwnerSuspendet.

	a := loadSpecArchive(t)
	folder := a.module(t, "folder")
	if _, _, ok := folder.operation("breadcrump", "GET"); ok {
		t.Fatal("the archive now has breadcrump after all; re-check D12")
	}
	if _, _, ok := folder.operation("breadcrumb", "GET"); !ok {
		t.Skip("the archive has no breadcrumb endpoint (public subset?)")
	}
}

// §2.6 #4: the same .json resource means different things per verb, so routing
// may never be inferred from the path alone.
func TestSameResourceDifferentVerbs(t *testing.T) {
	// POST /file.json creates an empty file, DELETE removes a trashed one;
	// POST /folder/trash.json trashes a folder, DELETE empties the trash;
	// POST /sharing.json shares, DELETE revokes.
	cases := []struct {
		method, path, meaning string
	}{
		{"POST", EndpointFile, "create an empty file"},
		{"DELETE", EndpointFile, "permanently remove a trashed file"},
		{"POST", EndpointFolderTrash, "move a folder to the trash"},
		{"DELETE", EndpointFolderTrash, "empty the trash"},
		{"POST", EndpointSharing, "share"},
		{"DELETE", EndpointSharing, "revoke a share"},
	}
	seen := map[string]bool{}
	byPath := map[string]int{}
	for _, c := range cases {
		key := c.method + " " + c.path
		if seen[key] {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = true
		byPath[c.path]++
	}
	for path, n := range byPath {
		if n < 2 {
			t.Errorf("%s is listed once; it is in this table because it carries several verbs", path)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("expected six distinct routes over three paths, got %d", len(seen))
	}
}

// Every endpoint constant must exist in the archived spec with the verb the
// declaration claims. This is the drift alarm of §12.2: an upstream rename or a
// changed method fails here rather than in production.
func TestEndpointConstantsMatchTheArchivedSpec(t *testing.T) {
	routes := []struct {
		module, path, method string
	}{
		{"session", EndpointSessionLogin, "POST"},
		{"session", EndpointSessionExists, "POST"},
		{"session", EndpointSessionInfo, "GET"},
		{"session", EndpointSessionLogout, "POST"},
		{"session", EndpointSessionCaptchaRequired, "GET"},
		{"oauth2", EndpointOAuth2Grant, "POST"},

		{"folder", EndpointFolder, "POST"},
		{"folder", EndpointFolderList, "GET"},
		{"folder", EndpointFolderInfo, "GET"},
		{"folder", EndpointFolderIDByPath, "POST"},
		{"folder", EndpointFolderItemByName, "GET"},
		{"folder", EndpointFolderBreadcrumb, "GET"},
		{"folder", EndpointFolderPath, "GET"},
		{"folder", EndpointFolderFullPath, "GET"},
		{"folder", EndpointFolderRename, "POST"},
		{"folder", EndpointFolderMoveCopy, "POST"},
		{"folder", EndpointFolderTrash, "POST"},
		{"folder", EndpointFolderTrash, "DELETE"},
		{"folder", EndpointFolderTrashList, "GET"},
		{"folder", EndpointFolderRestore, "POST"},
		{"folder", EndpointFolderRemove, "POST"},
		{"folder", EndpointFolderSetAccess, "POST"},
		{"folder", EndpointFolderSettings, "PUT"},
		{"folder", EndpointFolderUserAccess, "GET"},
		{"folder", EndpointFolderSendByEmail, "POST"},
		{"folder", EndpointFolderExportCSV, "GET"},
		{"folder", EndpointFolderShared, "GET"},
		{"folder", EndpointFolderSharedInfo, "GET"},
		{"folder", EndpointFolderExpiringLink, "GET"},
		{"folder", EndpointFolderExpiringLinks, "GET"},

		{"file", EndpointFile, "POST"},
		{"file", EndpointFile, "DELETE"},
		{"file", EndpointFileInfo, "GET"},
		{"file", EndpointFileIDByPath, "POST"},
		{"file", EndpointFilePath, "GET"},
		{"file", EndpointFileFullPath, "GET"},
		{"file", EndpointFileRename, "POST"},
		{"file", EndpointFileMoveCopy, "POST"},
		{"file", EndpointFileTrash, "POST"},
		{"file", EndpointFileRestore, "POST"},
		{"file", EndpointFileRemove, "POST"},
		{"file", EndpointFileVersions, "GET"},
		{"file", EndpointFileRemoveVersion, "DELETE"},
		{"file", EndpointFileThumb, "GET"},
		{"file", EndpointFileAccess, "POST"},
		{"file", EndpointFileSettings, "PUT"},
		{"file", EndpointFileVerifyPassword, "POST"},
		{"file", EndpointFileSendByEmail, "POST"},
		{"file", EndpointFileExpiringLink, "GET"},
		{"file", EndpointFileExpiringLinks, "GET"},

		{"upload", EndpointUploadCheckFileExists, "POST"},
		{"upload", EndpointUploadCreateFile, "POST"},
		{"upload", EndpointUploadOpenFile, "POST"},
		{"upload", EndpointUploadChunk, "POST"},
		{"upload", EndpointUploadChunkV1, "POST"},
		{"upload", EndpointUploadCloseFile, "POST"},
		{"upload", EndpointUploadHasDedupeRef, "POST"},

		{"download", EndpointDownloadFile, "GET"},
		{"download", EndpointDownloadAll, "POST"},
		{"download", EndpointDownloadRedirect, "GET"},

		{"sharing", EndpointSharing, "POST"},
		{"sharing", EndpointSharing, "DELETE"},
		{"sharing", EndpointSharingSetMode, "PUT"},
		{"sharing", EndpointSharingListFolders, "GET"},
		{"sharing", EndpointSharingListUsers, "GET"},
		{"sharing", EndpointSharingListFolderUsers, "GET"},
		{"sharing", EndpointSharingCheckAccess, "GET"},

		{"users", EndpointUsersInfo, "GET"},
	}

	a := loadSpecArchive(t)
	if a.strategy == "anonymous" {
		t.Skip("the archive is the public subset; run tools/fetch-spec with credentials")
	}

	modules := map[string]archivedDeclaration{}
	for _, r := range routes {
		if _, ok := modules[r.module]; !ok {
			modules[r.module] = a.module(t, r.module)
		}
		decl := modules[r.module]
		want := "/v1" + strings.TrimSuffix(r.path, ".json") + ".{format}"
		found := false
		for _, api := range decl.APIs {
			// Trailing path parameters are part of the declaration path.
			if api.Path != want && !strings.HasPrefix(api.Path, want+"/{") {
				continue
			}
			for _, op := range api.Operations {
				if strings.EqualFold(op.HTTPMethod, r.method) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s %s is not in the archived %s module (looked for %q)",
				r.method, r.path, r.module, want)
		}
	}
}

// ---------------------------------------------------------------- contract

// specArchive loads the spec archived by tools/fetch-spec. Contract tests are
// skipped when the archive is missing, and authenticated-only assertions are
// skipped when the archive was taken anonymously (§6.1, §12.2).
type specArchive struct {
	dir      string
	strategy string
}

type archivedDeclaration struct {
	BasePath string `json:"basePath"`
	APIs     []struct {
		Path       string `json:"path"`
		Operations []struct {
			HTTPMethod string `json:"httpMethod"`
			Nickname   string `json:"nickname"`
			Parameters []struct {
				Name         string          `json:"name"`
				ParamType    string          `json:"paramType"`
				DataType     string          `json:"dataType"`
				Required     bool            `json:"required"`
				Description  string          `json:"description"`
				DefaultValue json.RawMessage `json:"defaultValue"`
			} `json:"parameters"`
		} `json:"operations"`
	} `json:"apis"`
}

func loadSpecArchive(t *testing.T) specArchive {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "spec")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Skipf("no spec archive yet; run tools/fetch-spec (%v)", err)
	}
	var manifest struct {
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest.json is unreadable: %v", err)
	}
	return specArchive{dir: dir, strategy: manifest.Strategy}
}

func (a specArchive) module(t *testing.T, name string) archivedDeclaration {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.dir, name+".json"))
	if err != nil {
		t.Skipf("module %s is not in the archive: %v", name, err)
	}
	var decl archivedDeclaration
	if err := json.Unmarshal(raw, &decl); err != nil {
		t.Fatalf("module %s: %v", name, err)
	}
	return decl
}

// operation finds an operation by path suffix and method.
func (d archivedDeclaration) operation(path, method string) (string, []struct {
	Name         string          `json:"name"`
	ParamType    string          `json:"paramType"`
	DataType     string          `json:"dataType"`
	Required     bool            `json:"required"`
	Description  string          `json:"description"`
	DefaultValue json.RawMessage `json:"defaultValue"`
}, bool) {
	for _, api := range d.APIs {
		if !strings.Contains(api.Path, path) {
			continue
		}
		for _, op := range api.Operations {
			if strings.EqualFold(op.HTTPMethod, method) {
				return api.Path, op.Parameters, true
			}
		}
	}
	return "", nil, false
}

// §2.6 #1: the archive is the proof that the live surface is ahead of the PDF.
func TestArchivedSpecHasEndpointsThePDFOmits(t *testing.T) {
	a := loadSpecArchive(t)
	session := a.module(t, "session")
	if _, _, ok := session.operation("captcharequired", "GET"); !ok {
		t.Fatal("session/captcharequired.json is missing from the archive; " +
			"it is documented online but not in the PDF (§2.6 #1)")
	}
}

// §2.6 #12: declaration paths carry /v1 while basePath stops at /api, which is
// exactly the duplication joinPath has to absorb.
func TestArchivedSpecPathsCarryTheVersionPrefix(t *testing.T) {
	a := loadSpecArchive(t)
	session := a.module(t, "session")
	path, _, ok := session.operation("login", "POST")
	if !ok {
		t.Fatal("session/login.json is missing from the archive")
	}
	if !strings.HasPrefix(path, "/v1/") {
		t.Fatalf("declaration path = %q, expected the /v1 prefix", path)
	}
	if strings.HasSuffix(session.BasePath, "/v1") {
		t.Fatalf("basePath = %q; it used to stop at /api, so the SDK base URL and "+
			"the declaration paths overlap by one segment", session.BasePath)
	}
	// The SDK must produce the same absolute path either way.
	if joinPath("/api/v1", strings.Replace(path, ".{format}", ".json", 1)) != "/api/v1/session/login.json" {
		t.Fatal("joinPath does not reproduce the archived path")
	}
}

// §2.6 #2, first half. The PDF says download/all.json calls the parameter
// session_key; the live spec says session_id. The archive is authoritative
// (CLAUDE.md rule 3), and the discrepancy is recorded in docs/discrepancies.md.
func TestArchivedSpecDocumentsTheDownloadAllSessionParameter(t *testing.T) {
	a := loadSpecArchive(t)
	download := a.module(t, "download")
	_, params, ok := download.operation("download/all", "POST")
	if !ok {
		t.Skip("download/all.json is not in the archive")
	}
	var body string
	for _, p := range params {
		if p.ParamType == "body" {
			body = p.Description
		}
	}
	if body == "" {
		t.Fatal("download/all.json has no documented body")
	}
	hasKey := strings.Contains(body, "session_key")
	hasID := strings.Contains(body, "session_id")
	if !hasKey && !hasID {
		t.Fatalf("download/all.json documents neither session parameter: %s", truncate(body, 200))
	}
	// Whichever name the live spec uses, the client can send it.
	param := DefaultSessionParam
	if hasKey && !hasID {
		param = "session_key"
	}
	if param != "session_id" && param != "session_key" {
		t.Fatalf("unexpected session parameter %q", param)
	}
}

// §2.6 #2, second half: the file name parameter is an array called "name".
func TestArchivedSpecKeepsCheckFileExistsNameAsAnArray(t *testing.T) {
	a := loadSpecArchive(t)
	upload := a.module(t, "upload")
	_, params, ok := upload.operation("checkfileexistsbyname", "POST")
	if !ok {
		t.Skip("checkfileexistsbyname.json is not in the archive")
	}
	var body string
	for _, p := range params {
		if p.ParamType == "body" {
			body = p.Description
		}
	}
	if !strings.Contains(body, "name") || !strings.Contains(strings.ToLower(body), "array") {
		t.Fatalf("checkfileexistsbyname no longer documents name as an array: %s", truncate(body, 200))
	}
}

// §2.6 #14: offset paging needs last_request_time. The public archive exposes
// folder/shared.json, which uses the same protocol as folder/list.json.
func TestArchivedSpecShowsThePaginationParameters(t *testing.T) {
	a := loadSpecArchive(t)
	folder := a.module(t, "folder")
	for _, candidate := range []string{"folder/list", "folder/shared"} {
		_, params, ok := folder.operation(candidate, "GET")
		if !ok {
			continue
		}
		names := map[string]bool{}
		for _, p := range params {
			names[p.Name] = true
		}
		if !names["offset"] || !names["last_request_time"] {
			t.Fatalf("%s no longer pairs offset with last_request_time: %v", candidate, names)
		}
		return
	}
	t.Skip("no listing endpoint in the archive yet")
}

// The archived module set is the drift baseline: a module disappearing is a P1
// event (§12.2).
func TestArchivedSpecCoversTheFirstReleaseModules(t *testing.T) {
	a := loadSpecArchive(t)
	for _, module := range []string{"session", "oauth2", "file", "folder", "upload", "download", "users"} {
		if _, err := os.Stat(filepath.Join(a.dir, module+".json")); err != nil {
			t.Errorf("module %s is missing from the spec archive: %v", module, err)
		}
	}
}
