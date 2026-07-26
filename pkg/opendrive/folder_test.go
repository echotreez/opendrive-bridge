package opendrive

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture loads a recorded upstream response (testdata/fixtures/README.md).
func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "folder", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(raw)
}

// folderFixture wires a client in session mode, which is where the path and
// body placements are most visible.
func newFolderFixture(t *testing.T) (*mockUpstream, *FolderService) {
	t.Helper()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	return m, c.Folders()
}

// ---------------------------------------------------------------- listing

func TestFolderListDecodesARecordedResponse(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "list_root.json"))

	got, err := folders.List(context.Background(), RootFolderID, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	call := m.lastCall()
	if call.Path != "/api/v1/folder/list.json/SID/0" {
		t.Fatalf("path = %q; session and folder id are both path segments", call.Path)
	}
	if len(call.Query) != 0 {
		t.Fatalf("a plain first-page listing sent %v", call.Query)
	}

	if got.DirUpdateTime.Unix() != 1785053779 {
		t.Errorf("DirUpdateTime = %v", got.DirUpdateTime)
	}
	if len(got.Folders) != 1 || len(got.Files) != 1 || got.Len() != 2 {
		t.Fatalf("decoded %d folders and %d files", len(got.Folders), len(got.Files))
	}

	f := got.Folders[0]
	if f.FolderID.String() != "RXhhbXBsZUZvbGRlcklE" || f.Name != "Developing" {
		t.Errorf("folder = %+v", f)
	}
	// The live quirks: Shared is the string "False", Encrypted the string "0"
	// inside an entry and the integer 0 at the top level (§2.6 #5).
	if f.Shared.Bool() || f.Encrypted.Bool() || got.Encrypted.Bool() {
		t.Errorf("loose booleans decoded wrong: entry=%v/%v listing=%v", f.Shared, f.Encrypted, got.Encrypted)
	}
	if f.Access.Int() != 2 || f.DateCreated.Unix() != 1785027933 {
		t.Errorf("folder metadata = %+v", f)
	}

	file := got.Files[0]
	if file.FileID.String() != "RXhhbXBsZUZpbGVJRA" {
		t.Errorf("upstream calls the field FileId here: %+v", file)
	}
	if file.Size.Int64() != 20480 || file.DateModified.Unix() != 1785027940 {
		t.Errorf("quoted numbers decoded wrong: %+v", file)
	}
	if file.FileHash != "9a0364b9e99bb480dd25e1f0284c8555" {
		t.Errorf("FileHash = %q", file.FileHash)
	}
}

func TestFolderListOptionsMapToTheDocumentedParameters(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "list_empty.json"))

	_, err := folders.List(context.Background(), "FID", ListOptions{
		Page:                Pagination{Offset: 100, LastRequestTime: 1785053779},
		SearchQuery:         "报表",
		SharingID:           "SH1",
		OnlySubfolders:      true,
		WithBreadcrumbs:     true,
		EncryptionSupported: true,
		OrderBy:             "name",
		OrderType:           "desc",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	q := m.lastCall().Query
	want := map[string]string{
		"offset": "100", "last_request_time": "1785053779", "search_query": "报表",
		"sharing_id": "SH1", "only_subfolders": "true", "with_breadcrumbs": "true",
		"encryption_supported": "1", "order_by": "name", "order_type": "desc",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("query %s = %q, want %q", k, got, v)
		}
	}
}

// §2.6 #14: paging past the first page without last_request_time silently
// loses entries, so the SDK refuses to send it.
func TestFolderListRejectsPagingWithoutLastRequestTime(t *testing.T) {
	m, folders := newFolderFixture(t)
	_, err := folders.List(context.Background(), "FID", ListOptions{Page: Pagination{Offset: 100}})
	if ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, err.Error(), "last_request_time", "paging error")
	if m.callCount() != 0 {
		t.Fatal("the bad request must not reach upstream")
	}
}

// The pager carries DirUpdateTime forward and stops on a short page.
func TestFolderPagerWalksEveryPage(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))

	full := strings.Builder{}
	full.WriteString(`{"DirUpdateTime":1785000001,"Folders":[`)
	for i := 0; i < MaxListPageSize; i++ {
		if i > 0 {
			full.WriteString(",")
		}
		full.WriteString(`{"FolderID":"F","Name":"n","Shared":"False"}`)
	}
	full.WriteString(`],"Files":[]}`)

	m.push(200, full.String())
	m.push(200, `{"DirUpdateTime":1785000002,"Folders":[{"FolderID":"G","Name":"tail","Shared":"False"}],"Files":[]}`)

	pager := c.Folders().Pages("FID", ListOptions{})
	var pages, entries int
	for {
		page, err := pager.Next(context.Background())
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if page == nil {
			break
		}
		pages++
		entries += page.Len()
		if pages > 5 {
			t.Fatal("the pager did not terminate")
		}
	}
	if pages != 2 || entries != MaxListPageSize+1 {
		t.Fatalf("walked %d pages and %d entries", pages, entries)
	}
	if !pager.Done() {
		t.Fatal("the pager should be done")
	}

	calls := m.calls()
	if calls[0].Query.Has("offset") {
		t.Fatalf("the first page must not send paging parameters: %v", calls[0].Query)
	}
	if got := calls[1].Query.Get("offset"); got != "100" {
		t.Fatalf("second page offset = %q", got)
	}
	if got := calls[1].Query.Get("last_request_time"); got != "1785000001" {
		t.Fatalf("second page last_request_time = %q, want the previous DirUpdateTime", got)
	}

	// Once done, the pager stays done.
	page, err := pager.Next(context.Background())
	if page != nil || err != nil {
		t.Fatalf("Next after exhaustion = %v, %v", page, err)
	}
}

func TestFolderListDefaultsToTheRoot(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "list_root.json"))
	if _, err := folders.List(context.Background(), "", ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(m.lastCall().Path, "/SID/0") {
		t.Fatalf("path = %q, want the root as the string 0", m.lastCall().Path)
	}
}

// ---------------------------------------------------------------- lookups

func TestFolderInfo(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "info.json"))

	got, err := folders.Info(context.Background(), "FID", "SH1")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if m.lastCall().Path != "/api/v1/folder/info.json/SID/FID" {
		t.Fatalf("path = %q", m.lastCall().Path)
	}
	if m.lastCall().Query.Get("sharing_id") != "SH1" {
		t.Fatalf("query = %v", m.lastCall().Query)
	}
	if got.Name != "Developing" || got.FolderID.String() != "RXhhbXBsZUZvbGRlcklE" {
		t.Fatalf("info = %+v", got)
	}
	// Real JSON booleans here, a "False" string for Shared, and OwnerSuspended
	// spelled correctly unlike the login response (§2.6 #3).
	if !got.PublicUpload.Bool() || got.Shared.Bool() || got.OwnerSuspended.Bool() {
		t.Fatalf("booleans = %+v", got)
	}
	if got.Owner.String() != "1000001" || got.ID.String() != "4000001" {
		t.Fatalf("ids = %+v", got)
	}
	// DateModified is literally 1 upstream; it must not become the zero time.
	if got.DateModified.Unix() != 1 {
		t.Errorf("DateModified = %v", got.DateModified)
	}
}

// docs/discrepancies.md D22: idbypath answers with FolderId, everything else
// with FolderID.
func TestFolderIDByPathAcceptsBothSpellings(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "idbypath.json"))

	got, err := folders.IDByPath(context.Background(), "/Developing/")
	if err != nil {
		t.Fatalf("IDByPath: %v", err)
	}
	if got != "RXhhbXBsZUZvbGRlcklE" {
		t.Fatalf("id = %q", got)
	}
	call := m.lastCall()
	if call.Body["path"] != "Developing" {
		t.Fatalf("body = %v; the path is normalised and sent without a leading slash", call.Body)
	}
	if call.Body["session_id"] != "SID" {
		t.Fatalf("the session belongs in the body here: %v", call.Body)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, `{"FolderID":"upper-case"}`)
	if got, err := f2.IDByPath(context.Background(), "/x"); err != nil || got != "upper-case" {
		t.Fatalf("got %q, %v", got, err)
	}

	m3, f3 := newFolderFixture(t)
	m3.push(200, `{"Something":"else"}`)
	if _, err := f3.IDByPath(context.Background(), "/x"); ErrorKind(err) != KindInvalidResponse {
		t.Fatalf("err = %v", err)
	}
}

func TestFolderIDByPathShortCircuitsTheRoot(t *testing.T) {
	m, folders := newFolderFixture(t)
	got, err := folders.IDByPath(context.Background(), "/")
	if err != nil || got != RootFolderID {
		t.Fatalf("got %q, %v", got, err)
	}
	if m.callCount() != 0 {
		t.Fatal("the root needs no upstream call")
	}
}

// docs/discrepancies.md D23: a miss is an empty body, not an error.
func TestFolderItemByName(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "itembyname.json"))

	got, err := folders.ItemByName(context.Background(), RootFolderID, "Developing", WithEncryptionSupported())
	if err != nil {
		t.Fatalf("ItemByName: %v", err)
	}
	if len(got.Folders) != 1 || got.Folders[0].Name != "Developing" {
		t.Fatalf("result = %+v", got)
	}
	q := m.lastCall().Query
	if q.Get("name") != "Developing" || q.Get("encryption_supported") != "1" {
		t.Fatalf("query = %v", q)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, fixture(t, "itembyname_missing.json"))
	_, err = f2.ItemByName(context.Background(), RootFolderID, "nope")
	if ErrorKind(err) != KindNotFound {
		t.Fatalf("a miss must map to not_found, got %v", err)
	}
	mustContain(t, err.Error(), "nope", "not found message")

	m3, f3 := newFolderFixture(t)
	if _, err := f3.ItemByName(context.Background(), "", ""); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if m3.callCount() != 0 {
		t.Fatal("an empty name must not reach upstream")
	}
}

// The breadcrumb response is a bare array, and the endpoint is spelled
// correctly upstream (docs/discrepancies.md D12).
func TestFolderBreadcrumb(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "breadcrumb.json"))

	got, err := folders.Breadcrumb(context.Background(), "FID", false)
	if err != nil {
		t.Fatalf("Breadcrumb: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Developing" {
		t.Fatalf("breadcrumb = %+v", got)
	}
	if !strings.Contains(m.lastCall().Path, "/folder/breadcrumb.json/") {
		t.Fatalf("path = %q", m.lastCall().Path)
	}
	if m.lastCall().Query.Has("with_subfolders") {
		t.Fatal("with_subfolders should be omitted when not asked for")
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, fixture(t, "breadcrumb_with_subfolders.json"))
	got, err = f2.Breadcrumb(context.Background(), "FID", true)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SubFolders == nil {
		t.Fatal("SubFolders should be present, even when empty")
	}
	if m2.lastCall().Query.Get("with_subfolders") != "true" {
		t.Fatalf("query = %v", m2.lastCall().Query)
	}
}

func TestFolderPathAndFullPath(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "path.json"))
	got, err := folders.Path(context.Background(), "FID")
	if err != nil || got != "Developing" {
		t.Fatalf("Path = %q, %v", got, err)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, fixture(t, "folderfullpath.json"))
	got, err = f2.FullPath(context.Background(), "FID")
	if err != nil || got != "Developing" {
		t.Fatalf("FullPath = %q, %v", got, err)
	}
}

// §2.6 #5 again: an integer inside a listing, a string here.
func TestFolderUserAccessMode(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "useraccessmode.json"))
	got, err := folders.UserAccessMode(context.Background(), "FID")
	if err != nil || got != 1 {
		t.Fatalf("UserAccessMode = %d, %v", got, err)
	}
}

// The shared endpoints need no session at all.
func TestFolderSharedEndpointsAreAnonymous(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, fixture(t, "sharedinfo.json"))
	info, err := folders.SharedInfo(context.Background(), "FID")
	if err != nil {
		t.Fatalf("SharedInfo: %v", err)
	}
	if info.ChildFolders.Int() != 0 || info.Name != "Developing" {
		t.Fatalf("info = %+v", info)
	}
	call := m.lastCall()
	if call.Query.Has("session_id") || strings.Contains(call.Path, "SID") {
		t.Fatalf("sharedinfo must carry no session: %s %v", call.Path, call.Query)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, fixture(t, "list_root.json"))
	if _, err := f2.Shared(context.Background(), "FID", ListOptions{
		Page: Pagination{Offset: 100, LastRequestTime: 5}, SearchQuery: "dropped",
		WithBreadcrumbs: true, OrderBy: "name",
	}); err != nil {
		t.Fatalf("Shared: %v", err)
	}
	q := m2.lastCall().Query
	if q.Get("offset") != "100" || q.Get("with_breadcrumbs") != "true" || q.Get("order_by") != "name" {
		t.Fatalf("query = %v", q)
	}
	if q.Has("search_query") || q.Has("sharing_id") {
		t.Fatalf("the shared listing has no search or sharing scope: %v", q)
	}
}

// ---------------------------------------------------------------- mutations

func TestFolderCreate(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"FolderID":"NEW","Name":"2026","DateCreated":1785027933,"Shared":"False","Encrypted":"0"}`)

	got, err := folders.Create(context.Background(), CreateFolderParams{
		Name: "2026", ParentID: "PARENT", Access: FolderHidden,
		PublicUpload: true, Description: "年度目录",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.FolderID.String() != "NEW" {
		t.Fatalf("created = %+v", got)
	}
	body := m.lastCall().Body
	if body["folder_name"] != "2026" || body["folder_sub_parent"] != "PARENT" {
		t.Fatalf("body = %v", body)
	}
	if body["folder_is_public"] != float64(FolderHidden) {
		t.Fatalf("folder_is_public = %v, want the tri-state integer", body["folder_is_public"])
	}
	if body["folder_public_upl"] != float64(1) || body["folder_public_dnl"] != float64(0) {
		t.Fatalf("public flags = %v", body)
	}
	if body["folder_description"] != "年度目录" {
		t.Fatalf("description = %v", body["folder_description"])
	}
	if body["session_id"] != "SID" {
		t.Fatal("the session belongs in the body for POST /folder.json")
	}
}

// §2.6 #10: an illegal name is rejected before it costs a round trip.
func TestFolderCreateValidatesTheNameLocally(t *testing.T) {
	m, folders := newFolderFixture(t)
	for _, name := range []string{"", "a/b", "a:b", strings.Repeat("x", MaxNameLength+1)} {
		_, err := folders.Create(context.Background(), CreateFolderParams{Name: name})
		if ErrorKind(err) != KindInvalidName {
			t.Errorf("Create(%q) = %v, want invalid_name", truncate(name, 12), err)
		}
	}
	if m.callCount() != 0 {
		t.Fatal("no invalid name should have reached upstream")
	}
}

func TestFolderRename(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"FolderID":"FID","Name":"renamed","Shared":"False"}`)
	if _, err := folders.Rename(context.Background(), "FID", "renamed", "SH1"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	body := m.lastCall().Body
	if body["folder_id"] != "FID" || body["folder_name"] != "renamed" || body["sharing_id"] != "SH1" {
		t.Fatalf("body = %v", body)
	}

	if _, err := folders.Rename(context.Background(), "FID", "bad/name"); ErrorKind(err) != KindInvalidName {
		t.Fatalf("err = %v", err)
	}
}

// docs/discrepancies.md D26: the archived spec calls move a real JSON boolean,
// but the live endpoint rejects the boolean false and accepts the strings, so a
// copy is only expressible as "false". Both flags go out as StringBool.
func TestFolderMoveCopySendsStringBooleans(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"FolderID":"FID","Name":"n","Shared":"False"}`)

	if _, err := folders.MoveCopy(context.Background(), MoveCopyParams{
		FolderID: "SRC", DstFolderID: "DST", Move: true, CopyRecursive: true, NewName: "copy",
	}); err != nil {
		t.Fatalf("MoveCopy: %v", err)
	}
	body := m.lastCall().Body
	if body["move"] != "true" || body["copy_recursive"] != "true" {
		t.Fatalf("move/copy_recursive = %v/%v, want the strings", body["move"], body["copy_recursive"])
	}

	// A copy must serialise as "false", the form the live endpoint accepts.
	m.push(200, `{"FolderID":"FID","Name":"n","Shared":"False"}`)
	if _, err := folders.MoveCopy(context.Background(), MoveCopyParams{
		FolderID: "SRC", DstFolderID: "DST", Move: false,
	}); err != nil {
		t.Fatalf("MoveCopy copy: %v", err)
	}
	if got := m.lastCall().Body["move"]; got != "false" {
		t.Fatalf("copy sent move=%v, want the string \"false\" (D26)", got)
	}
	if body["new_folder_name"] != "copy" {
		t.Fatalf("body = %v", body)
	}

	if _, err := folders.MoveCopy(context.Background(), MoveCopyParams{FolderID: "SRC"}); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if _, err := folders.MoveCopy(context.Background(), MoveCopyParams{
		FolderID: "SRC", DstFolderID: "DST", NewName: "bad*name",
	}); ErrorKind(err) != KindInvalidName {
		t.Fatalf("err = %v", err)
	}
}

// §2.6 #4: POST trashes, DELETE on the very same path empties the trash, and
// the session moves from the body to the path between the two.
func TestFolderTrashAndEmptyTrashShareAPathButNotAVerb(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"result":true}`)
	if err := folders.Trash(context.Background(), []string{"A", "B"}); err != nil {
		t.Fatalf("Trash: %v", err)
	}
	trash := m.lastCall()
	if trash.Method != http.MethodPost || trash.Body["folder_id"] != "A,B" {
		t.Fatalf("trash call = %s %v", trash.Method, trash.Body)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, `true`)
	if err := f2.EmptyTrash(context.Background()); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	empty := m2.lastCall()
	if empty.Method != http.MethodDelete {
		t.Fatalf("method = %s", empty.Method)
	}
	if empty.Path != "/api/v1/folder/trash.json/SID" {
		t.Fatalf("path = %q; DELETE takes the session as a path segment", empty.Path)
	}
	if empty.RawBody != "" {
		t.Fatalf("DELETE should send no body, got %q", empty.RawBody)
	}
}

func TestFolderIDListValidation(t *testing.T) {
	m, folders := newFolderFixture(t)
	ctx := context.Background()
	for _, ids := range [][]string{nil, {}, {""}, {"a", " "}, {"a,b"}} {
		if err := folders.Trash(ctx, ids); ErrorKind(err) != KindInvalidRequest {
			t.Errorf("Trash(%v) = %v", ids, err)
		}
	}
	if err := folders.Restore(ctx, nil); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("Restore = %v", err)
	}
	if err := folders.Remove(ctx, nil); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("Remove = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatal("no malformed id list should have reached upstream")
	}
}

func TestFolderRestoreAndRemove(t *testing.T) {
	ctx := context.Background()
	m, folders := newFolderFixture(t)
	m.push(200, `{"result":true}`)
	if err := folders.Restore(ctx, []string{"A"}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if m.lastCall().Body["folder_id"] != "A" {
		t.Fatalf("body = %v", m.lastCall().Body)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, `{"result":true}`)
	if err := f2.Remove(ctx, []string{"A", "B"}, "SH1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	body := m2.lastCall().Body
	if body["folder_id"] != "A,B" || body["sharing_id"] != "SH1" {
		t.Fatalf("body = %v", body)
	}
}

func TestFolderSetAccess(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"result":true}`)
	if err := folders.SetAccess(context.Background(), "FID", FolderPublic, true); err != nil {
		t.Fatalf("SetAccess: %v", err)
	}
	body := m.lastCall().Body
	if body["folder_is_public"] != float64(1) || body["with_child_files"] != true {
		t.Fatalf("body = %v", body)
	}
}

// D15: the settings endpoint is PUT, not POST, and only the fields the caller
// set are sent.
func TestFolderUpdateSettingsIsAPutAndOnlySendsWhatChanged(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(200, `{"result":true}`)

	access := FolderPublic
	yes := true
	if err := folders.UpdateSettings(context.Background(), "FID", FolderSettings{
		Description: "new", Access: &access, PublicDownload: &yes,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	call := m.lastCall()
	if call.Method != http.MethodPut {
		t.Fatalf("method = %s, want PUT (D15)", call.Method)
	}
	if call.Body["folder_description"] != "new" || call.Body["folder_access"] != float64(1) ||
		call.Body["folder_public_dnl"] != float64(1) {
		t.Fatalf("body = %v", call.Body)
	}
	for _, absent := range []string{"folder_name", "folder_public_upl", "folder_public_display", "folder_display_subfolders"} {
		if _, ok := call.Body[absent]; ok {
			t.Errorf("%s was sent even though the caller did not set it", absent)
		}
	}

	if err := folders.UpdateSettings(context.Background(), "", FolderSettings{}); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if err := folders.UpdateSettings(context.Background(), "FID", FolderSettings{Name: "bad|name"}); ErrorKind(err) != KindInvalidName {
		t.Fatalf("err = %v", err)
	}
}

func TestFolderSendByEmail(t *testing.T) {
	ctx := context.Background()
	m, folders := newFolderFixture(t)
	m.push(200, `{"result":true}`)

	err := folders.SendByEmail(ctx, SendByEmailParams{
		FolderIDs: []string{"A"}, Recipients: []string{"x@example.com"},
		Subject: "s", Body: "b", SendExpiring: true, CaptchaResponse: "solved",
	})
	if err != nil {
		t.Fatalf("SendByEmail: %v", err)
	}
	body := m.lastCall().Body
	// folder_id and recipient_emails are "mixed" upstream: arrays are accepted.
	if _, ok := body["folder_id"].([]any); !ok {
		t.Fatalf("folder_id = %#v, want an array", body["folder_id"])
	}
	if body["send_expiring_enabled"] != true || body["captcha_response"] != "solved" {
		t.Fatalf("body = %v", body)
	}

	if err := folders.SendByEmail(ctx, SendByEmailParams{Recipients: []string{"x@example.com"}}); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing folders: %v", err)
	}
	if err := folders.SendByEmail(ctx, SendByEmailParams{FolderIDs: []string{"A"}}); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing recipients: %v", err)
	}
}

// D18: every argument of an expiring link is a path segment, session first.
func TestFolderExpiringLinks(t *testing.T) {
	ctx := context.Background()
	m, folders := newFolderFixture(t)
	m.push(200, `{"Link":"https://od.lk/fl/x","ExpirationDate":1785999999,"Counter":10}`)

	got, err := folders.CreateExpiringLink(ctx, "FID", "2026-08-01", 10, true)
	if err != nil {
		t.Fatalf("CreateExpiringLink: %v", err)
	}
	if got.Link == "" || got.Counter.Int() != 10 || got.ExpiresAt.Unix() != 1785999999 {
		t.Fatalf("link = %+v", got)
	}
	if p := m.lastCall().Path; p != "/api/v1/folder/expiringlink.json/SID/2026-08-01/10/FID/true" {
		t.Fatalf("path = %q", p)
	}

	if _, err := folders.CreateExpiringLink(ctx, "", "2026-08-01", 1, true); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, `[{"Link":"https://od.lk/fl/x","Counter":"3"}]`)
	links, err := f2.ExpiringLinks(ctx, "FID")
	if err != nil || len(links) != 1 || links[0].Counter.Int() != 3 {
		t.Fatalf("links = %+v, %v", links, err)
	}
}

func TestFolderTrashList(t *testing.T) {
	ctx := context.Background()
	m, folders := newFolderFixture(t)
	m.push(200, `{"DirUpdateTime":1785053815,"Count":"7"}`)

	got, err := folders.TrashList(ctx, TrashListOptions{CountOnly: true})
	if err != nil {
		t.Fatalf("TrashList: %v", err)
	}
	if got.Count.Int() != 7 {
		t.Fatalf("count = %d", got.Count.Int())
	}
	if m.lastCall().Path != "/api/v1/folder/trashlist.json/SID" {
		t.Fatalf("path = %q", m.lastCall().Path)
	}
	if m.lastCall().Query.Get("count_only") != "1" {
		t.Fatalf("query = %v", m.lastCall().Query)
	}

	m2, f2 := newFolderFixture(t)
	m2.push(200, `{"DirUpdateTime":1,"Folders":[],"Files":[]}`)
	if _, err := f2.TrashList(ctx, TrashListOptions{
		Page: Pagination{Offset: 100, LastRequestTime: 42}, SearchQuery: "q",
		OrderBy: "name", OrderType: "asc",
	}); err != nil {
		t.Fatal(err)
	}
	q := m2.lastCall().Query
	if q.Get("offset") != "100" || q.Get("last_request_time") != "42" ||
		q.Get("search_query") != "q" || q.Get("order_by") != "name" || q.Get("order_type") != "asc" {
		t.Fatalf("query = %v", q)
	}

	m3, f3 := newFolderFixture(t)
	if _, err := f3.TrashList(ctx, TrashListOptions{Page: Pagination{Offset: 1}}); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if m3.callCount() != 0 {
		t.Fatal("a bad page must not reach upstream")
	}
}

func TestFolderExportCSV(t *testing.T) {
	ctx := context.Background()
	m, folders := newFolderFixture(t)
	m.push(200, `"name,size\nreport.xlsx,20480\n"`)
	got, err := folders.ExportCSV(ctx, "FID")
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if !strings.Contains(got, "report.xlsx") {
		t.Fatalf("csv = %q", got)
	}

	// The restricted-user answer upstream gives is a plain upstream error, not
	// a credential problem (v1.1 §4.5).
	m2, f2 := newFolderFixture(t)
	m2.push(403, `{"error":{"code":403,"message":"Export failed. Permission denied for restricted user"}}`)
	if _, err := f2.ExportCSV(ctx, "FID"); ErrorKind(err) != KindUpstreamError {
		t.Fatalf("err = %v", err)
	}
}

// A missing folder is a clean not_found, whatever wording upstream picks.
func TestFolderErrorsAreMapped(t *testing.T) {
	m, folders := newFolderFixture(t)
	m.push(404, `{"error":{"code":404,"message":"Directory does not exist"}}`)
	_, err := folders.List(context.Background(), "999999999", ListOptions{})
	if ErrorKind(err) != KindNotFound {
		t.Fatalf("err = %v", err)
	}
}
