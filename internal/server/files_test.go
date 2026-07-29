package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/StormRealm/opendrive-bridge/internal/cache"
	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// fakeUpstream is enough OpenDrive to drive the path-addressed endpoints: a
// two-level tree, idbypath, listings and the mutations.
type fakeUpstream struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// folders maps a path to an id; files maps a path to size.
	folders  map[string]string
	files    map[string]int64
	calls    []string
	infoHits int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{
		t:       t,
		folders: map[string]string{"/": "0", "/Docs": "FD1", "/Docs/2026": "FD2"},
		files:   map[string]int64{"/Docs/report.pdf": 1024},
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) pathForID(id string) string {
	for p, fid := range u.folders {
		if fid == id {
			return p
		}
	}
	return ""
}

func (u *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	path := r.URL.Path

	u.mu.Lock()
	u.calls = append(u.calls, r.Method+" "+path)
	if strings.Contains(path, "info.json") {
		u.infoHits++
	}
	u.mu.Unlock()

	switch {
	case strings.Contains(path, "folder/idbypath"):
		want, _ := body["path"].(string)
		// The SDK sends paths the way upstream wants them: no leading slash
		// (see the D22 probe). The fake has to speak the same dialect.
		if !strings.HasPrefix(want, "/") {
			want = "/" + want
		}
		if want == "/" || want == "" {
			want = "/"
		}
		u.mu.Lock()
		id, ok := u.folders[want]
		u.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Folder not found"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"FolderId":%q}`, id)

	case strings.Contains(path, "folder/list.json"):
		segs := strings.Split(strings.Trim(path, "/"), "/")
		id := segs[len(segs)-1]
		u.mu.Lock()
		parent := u.pathForID(id)
		var folders, files []string
		for p := range u.folders {
			if p != "/" && parentOf(p) == parent {
				folders = append(folders, p)
			}
		}
		for p := range u.files {
			if parentOf(p) == parent {
				files = append(files, p)
			}
		}
		u.mu.Unlock()

		var fb, fl []string
		for _, p := range folders {
			fb = append(fb, fmt.Sprintf(`{"FolderID":%q,"Name":%q,"DateModified":1785000000}`,
				u.folders[p], baseOf(p)))
		}
		for _, p := range files {
			fl = append(fl, fmt.Sprintf(`{"FileId":%q,"Name":%q,"Size":%d,"DateModified":1785000000}`,
				"FILE-"+baseOf(p), baseOf(p), u.files[p]))
		}
		_, _ = fmt.Fprintf(w, `{"DirUpdateTime":1785000001,"Folders":[%s],"Files":[%s]}`,
			strings.Join(fb, ","), strings.Join(fl, ","))

	case strings.HasSuffix(path, "/folder.json") && r.Method == http.MethodPost:
		name, _ := body["folder_name"].(string)
		parentID, _ := body["folder_sub_parent"].(string)
		u.mu.Lock()
		parent := u.pathForID(parentID)
		newPath := joinPath(parent, name)
		id := "NEW-" + name
		u.folders[newPath] = id
		u.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"FolderID":%q,"Name":%q}`, id, name)

	case strings.Contains(path, "folder/trash.json"), strings.Contains(path, "file/trash.json"),
		strings.Contains(path, "folder/remove.json"), strings.Contains(path, "file/remove.json"):
		_, _ = io.WriteString(w, `{"result":true}`)

	case strings.Contains(path, "folder/rename.json"):
		_, _ = io.WriteString(w, `{"FolderID":"FD1","Name":"renamed"}`)
	case strings.Contains(path, "file/rename.json"):
		_, _ = io.WriteString(w, `{"FileId":"F1","Name":"renamed"}`)

	case strings.Contains(path, "move_copy"):
		_, _ = io.WriteString(w, `{"FolderID":"FD9","FileId":"F9","Name":"moved"}`)

	case strings.Contains(path, "fileversions"):
		_, _ = io.WriteString(w, `[{"FileId":"F1","Version":"2","Name":"report.pdf","Size":"1024"}]`)

	case strings.Contains(path, "users/info.json"):
		_, _ = io.WriteString(w, `{"UserID":"1","AccType":"1"}`)

	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

func parentOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func baseOf(p string) string { return p[strings.LastIndex(p, "/")+1:] }

func (u *fakeUpstream) server(t *testing.T) *Server {
	t.Helper()
	pc := cache.NewPathCache()
	c, err := opendrive.New(
		opendrive.WithBaseURL(u.srv.URL+"/api/v1"),
		opendrive.WithHTTPClient(u.srv.Client()),
		opendrive.WithAuthenticator(stubAuth{}),
		opendrive.WithAccessProbe(nil),
		opendrive.WithPathCache(pc),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Addr: "127.0.0.1:0"},
		&fakeAuth{state: opendrive.StateAuthenticated,
			identity: opendrive.Identity{Username: "d@example.com", AuthMode: opendrive.AuthModeOAuth2}},
		WithClient(c), WithPathCache(pc))
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// ---------------------------------------------------------------- reads

func TestListMergesFoldersAndFiles(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/ls?path=/Docs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	entries := body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want the folder and the file: %v", len(entries), entries)
	}
	// Paths come back absolute, so a caller can feed one straight back in.
	for _, e := range entries {
		m := e.(map[string]any)
		if !strings.HasPrefix(m["path"].(string), "/Docs/") {
			t.Errorf("entry path is not absolute: %v", m["path"])
		}
	}
	if body["dir_update_time"] == nil {
		t.Error("dir_update_time is missing; a client cannot page without it (D7)")
	}
}

// D7: paging past the first page needs the previous DirUpdateTime, and upstream
// silently re-serves page one without it. Better to refuse than to loop.
func TestListRefusesAnOffsetWithoutTheDirUpdateTime(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/ls?path=/Docs&offset=100", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	assertUserReadable(t, msg)
	if !strings.Contains(msg, "dir_update_time") {
		t.Errorf("the message does not name what is missing: %q", msg)
	}
}

// D27: stat is answered from the parent listing. info.json still describes
// folders that have been deleted, so a stat built on it would lie.
func TestStatNeverAsksInfoJSON(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/stat?path=/Docs/report.pdf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if body["kind"] != "file" || body["name"] != "report.pdf" {
		t.Errorf("stat = %v", body)
	}
	if body["size"].(float64) != 1024 {
		t.Errorf("size = %v", body["size"])
	}

	u.mu.Lock()
	hits := u.infoHits
	u.mu.Unlock()
	if hits != 0 {
		t.Errorf("info.json was called %d times; D27 forbids it for existence", hits)
	}
}

func TestStatOnAMissingPath(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/stat?path=/Docs/nope.pdf", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "/Docs/nope.pdf") {
		t.Errorf("the message does not name the path asked for: %q", msg)
	}
	assertUserReadable(t, msg)
}

func TestPathsAreValidated(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	for _, p := range []string{"", "Docs", "/Docs/../../etc"} {
		rec, body := do(t, srv, http.MethodGet, "/v1/stat?path="+p, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", p, rec.Code)
			continue
		}
		assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
	}
}

// ---------------------------------------------------------------- writes

func TestMkdirCreatesAndInvalidates(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	// Warm the cache so the invalidation has something to drop.
	do(t, srv, http.MethodGet, "/v1/ls?path=/Docs", "")

	rec, body := do(t, srv, http.MethodPost, "/v1/mkdir", `{"path":"/Docs/new"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if body["path"] != "/Docs/new" || body["kind"] != "folder" {
		t.Errorf("body = %v", body)
	}

	// The new folder must be visible immediately, which it can only be if the
	// cached listing was dropped.
	_, after := do(t, srv, http.MethodGet, "/v1/ls?path=/Docs", "")
	var found bool
	for _, e := range after["entries"].([]any) {
		if e.(map[string]any)["name"] == "new" {
			found = true
		}
	}
	if !found {
		t.Error("the new folder is missing from the listing; the cache was not invalidated")
	}
}

func TestMkdirParentsCreatesTheChain(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, _ := do(t, srv, http.MethodPost, "/v1/mkdir", `{"path":"/Docs/a/b/c","parents":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, want := range []string{"/Docs/a", "/Docs/a/b", "/Docs/a/b/c"} {
		if _, ok := u.folders[want]; !ok {
			t.Errorf("%s was not created", want)
		}
	}
}

func TestMkdirWithoutParentsFailsClearly(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/mkdir", `{"path":"/Docs/x/y"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
}

func TestRenameValidatesTheNameLocally(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/rename",
		`{"path":"/Docs/report.pdf","new_name":"bad/name.pdf"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
	// The rejection happens locally, so upstream is never troubled.
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.calls {
		if strings.Contains(c, "rename") {
			t.Error("an invalid name was sent upstream")
		}
	}
}

func TestRemoveDefaultsToTheTrashAndSaysSo(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	_, body := do(t, srv, http.MethodPost, "/v1/rm", `{"path":"/Docs/report.pdf"}`)
	if body["permanent"] != false {
		t.Errorf("permanent = %v, want false by default", body["permanent"])
	}
	detail := body["detail"].(string)
	if !strings.Contains(strings.ToLower(detail), "trash") {
		t.Errorf("the caller is not told where it went: %q", detail)
	}

	_, perm := do(t, srv, http.MethodPost, "/v1/rm", `{"path":"/Docs/report.pdf","permanent":true}`)
	if !strings.Contains(strings.ToLower(perm["detail"].(string)), "cannot be undone") {
		t.Errorf("a permanent delete does not warn: %q", perm["detail"])
	}
}

func TestRemoveRefusesTheRoot(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	rec, _ := do(t, srv, http.MethodPost, "/v1/rm", `{"path":"/"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for removing the root", rec.Code)
	}
}

func TestMoveIntoAnExistingFolderKeepsTheName(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/mv",
		`{"src":"/Docs/report.pdf","dst":"/Docs/2026"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if body["dst"] != "/Docs/2026/report.pdf" {
		t.Errorf("dst = %v, want the name preserved inside the folder", body["dst"])
	}
	if body["moved"] != true {
		t.Errorf("moved = %v", body["moved"])
	}
}

func TestCopyReportsItselfAsACopy(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	_, body := do(t, srv, http.MethodPost, "/v1/cp",
		`{"src":"/Docs/report.pdf","dst":"/Docs/2026/copy.pdf"}`)
	if body["moved"] != false {
		t.Errorf("moved = %v, want false for a copy", body["moved"])
	}
	if body["dst"] != "/Docs/2026/copy.pdf" {
		t.Errorf("dst = %v", body["dst"])
	}
}

func TestVersionsRefusesAFolder(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/versions?path=/Docs/2026", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
}

// ---------------------------------------------------------------- transfers

// D42: the archive endpoint takes a folder and nothing else. A files parameter
// would answer 200 with an empty archive, so it is not on the Bridge at all.
func TestArchiveTakesNoFilesParameter(t *testing.T) {
	data, err := readSpec()
	if err != nil {
		t.Fatal(err)
	}
	block := specSection(data, "/v1/download/archive:")
	if block == "" {
		t.Fatal("the archive operation is missing from the spec")
	}
	// The prose explains *why* files are not offered, so the check has to look
	// at the declared parameters rather than at the description.
	for _, declared := range []string{"name: files", "files:", "file_ids"} {
		if strings.Contains(block, declared) {
			t.Errorf("the archive operation declares %q; D42 says a file list yields an empty archive",
				declared)
		}
	}
	if !strings.Contains(block, "parameters/Path") {
		t.Error("the archive operation does not take a path")
	}
}

func TestJobEndpointsNeedAnEngine(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t) // built without a job engine

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/jobs", ""},
		{http.MethodPost, "/v1/upload", `{"local_path":"/tmp/x","remote_path":"/Docs/x"}`},
		{http.MethodPost, "/v1/download", `{"remote_path":"/Docs/report.pdf","local_path":"/tmp/x"}`},
	} {
		rec, body := do(t, srv, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", tc.method, tc.path, rec.Code)
			continue
		}
		assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
	}
}

func TestUploadRefusesAMissingLocalFile(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)
	srv.engine = nil // the local-file check runs before the engine is needed

	rec, _ := do(t, srv, http.MethodPost, "/v1/upload",
		`{"local_path":"/definitely/not/here.bin","remote_path":"/Docs/x.bin"}`)
	// Without an engine this reports 503 first; with one it would be a 400.
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rec.Code)
	}
}

// Uploading to a name that does not exist yet is the ordinary case, and it must
// not be refused as "there is nothing there". This regressed once because
// exists() checked only the SDK's not-found sentinel while resolve reports a
// missing path with this package's own error.
func TestExistsTreatsAMissingPathAsAbsentNotAsAFailure(t *testing.T) {
	u := newFakeUpstream(t)
	srv := u.server(t)

	present, err := srv.exists(context.Background(), "/Docs/report.pdf")
	if err != nil || !present {
		t.Errorf("existing file: present=%v err=%v", present, err)
	}

	absent, err := srv.exists(context.Background(), "/Docs/not-here-yet.bin")
	if err != nil {
		t.Fatalf("a missing path was reported as a failure: %v", err)
	}
	if absent {
		t.Error("a missing path was reported as present")
	}
}
