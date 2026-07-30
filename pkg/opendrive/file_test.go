package opendrive

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newFileFixture(t *testing.T) (*mockUpstream, *FileService) {
	t.Helper()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	return m, c.Files()
}

// fileFixture loads a response recorded from the sandbox
// (testdata/fixtures/file/).
func fileFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "file", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(raw)
}

// The recorded bodies are the reason the models look the way they do: a
// created file reports its id as FileId, its size as a quoted number and its
// EditOnline flag as an integer (§2.6 #5).
func TestFileModelsDecodeRecordedResponses(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, fileFixture(t, "info.json"))

	info, err := files.Info(context.Background(), "FID")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.FileID.String() != "RXhhbXBsZUZpbGVJRA" || info.Name != "Text.txt" {
		t.Fatalf("info = %+v", info)
	}
	if info.Size.Int() != 0 || info.FileHash != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("content fields = %+v", info)
	}
	if !info.EditOnline.Bool() || info.BWExceeded.Bool() || info.Encrypted.Bool() {
		t.Fatalf("flags = %+v", info)
	}
	if info.OwnerName != "Tester Tester" || info.AccessUser.String() != "60516" {
		t.Fatalf("ownership = %+v", info)
	}
	if info.DateUploaded.Unix() != 1785122523 || !info.DateAccessed.IsZero() || !info.DateTrashed.IsZero() {
		t.Fatalf("timestamps = %+v", info)
	}
	// §9.4: the temporary streaming link carries a temp_auth credential, so it
	// must not survive redaction.
	mustNotContain(t, RedactString(info.TempStreamingLink), "NOTAREALTOKEN", "temp streaming link")
}

// D30: the full-path endpoint answers in a field called DownloadLink and with
// backslash separators.
func TestFileFullPathNormalisesTheRecordedShape(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, fileFixture(t, "filefullpath.json"))
	got, err := files.FullPath(context.Background(), "FID")
	if err != nil {
		t.Fatalf("FullPath: %v", err)
	}
	if got != "Application/odb-f3-1785122522/Text.txt" {
		t.Fatalf("FullPath = %q", got)
	}

	// If upstream ever starts sending the documented field, it wins.
	m2, f2 := newFileFixture(t)
	m2.push(200, `{"FullPath":"A/b.txt"}`)
	if got, err := f2.FullPath(context.Background(), "FID"); err != nil || got != "A/b.txt" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// D32: upstream answers false for a correct password and for a wrong one, and
// never returns a TempKey.
func TestFileVerifyPasswordReportsWhatUpstreamActuallySends(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, fileFixture(t, "verifypassword.json"))
	got, err := files.VerifyPassword(context.Background(), "FID", "correct-password", "")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if got.OK() || got.TempKey != "" {
		t.Fatalf("verification = %+v", got)
	}
	// The call must not be retried: repeated attempts invite a captcha lock
	// (§2.6 #11).
	if m.callCount() != 1 {
		t.Fatalf("calls = %d", m.callCount())
	}
}

func TestFileInfoAndIDByPath(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, `{"FileID":"UPPER","Name":"report.txt","Shared":"False","Encrypted":"0","Size":"12"}`)
	m.push(200, `{"FileId":"LOWER"}`)
	m.push(200, `{"FileID":"UPPER"}`)
	info, err := files.Info(context.Background(), "FID", "SH")
	if err != nil {
		t.Fatal(err)
	}
	if info.FileID.String() != "UPPER" || info.Name != "report.txt" || info.Shared.Bool() || info.Encrypted.Bool() || info.Size.Int() != 12 {
		t.Fatalf("info = %+v", info)
	}
	if call := m.lastCall(); call.Path != "/api/v1/file/info.json/FID" || call.Query.Get("session_id") != "SID" || call.Query.Get("sharing_id") != "SH" {
		t.Fatalf("info request = %+v", call)
	}

	id, err := files.IDByPath(context.Background(), "/Reports/report.txt/")
	if err != nil || id != "LOWER" {
		t.Fatalf("IDByPath = %q, %v", id, err)
	}
	if body := m.lastCall().Body; body["path"] != "Reports/report.txt" || body["session_id"] != "SID" {
		t.Fatalf("idbypath body = %v", body)
	}

	// This is deliberately a standalone lookup, not Info: D27 says an Info hit
	// is not evidence that a remote object remains live.
	if id, err := files.IDByPath(context.Background(), "Reports/report.txt"); err != nil || id != "UPPER" {
		t.Fatalf("upper spelling = %q, %v", id, err)
	}

	m2, files2 := newFileFixture(t)
	if _, err := files2.IDByPath(context.Background(), "/"); ErrorKind(err) != KindInvalidRequest || m2.callCount() != 0 {
		t.Fatalf("root file lookup = %v, calls=%d", err, m2.callCount())
	}
	if _, err := files2.IDByPath(context.Background(), "/A/../secret"); ErrorKind(err) != KindInvalidRequest || m2.callCount() != 0 {
		t.Fatalf("traversal lookup = %v, calls=%d", err, m2.callCount())
	}
}

func TestFileReadEndpoints(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, `{"Path":"Reports/report.txt"}`)
	m.push(200, `{"FullPath":"Reports/report.txt"}`)
	m.push(200, `[{"FileId":"OLD","Version":"2","Size":"42"}]`)
	path, err := files.Path(context.Background(), "FID")
	if err != nil || path != "Reports/report.txt" {
		t.Fatalf("Path = %q, %v", path, err)
	}
	if m.lastCall().Path != "/api/v1/file/path.json/SID/FID" {
		t.Fatalf("path request = %q", m.lastCall().Path)
	}

	full, err := files.FullPath(context.Background(), "FID")
	if err != nil || full != "Reports/report.txt" {
		t.Fatalf("FullPath = %q, %v", full, err)
	}

	versions, err := files.Versions(context.Background(), "GROUP")
	if err != nil || len(versions) != 1 || versions[0].FileID.String() != "OLD" || versions[0].Size.Int() != 42 {
		t.Fatalf("Versions = %+v, %v", versions, err)
	}
	if m.lastCall().Path != "/api/v1/file/fileversions.json/SID/GROUP" {
		t.Fatalf("versions request = %q", m.lastCall().Path)
	}
}

func TestFileThumbnailUsesTheRawResponsePath(t *testing.T) {
	m, files := newFileFixture(t)
	m.push(200, "not-json-image-bytes")
	at := 1.25
	got, err := files.Thumbnail(context.Background(), "FID", ThumbnailOptions{SharingID: "SH", TimeOffset: &at, TempKey: "TEMP"})
	if err != nil || string(got) != "not-json-image-bytes" {
		t.Fatalf("Thumbnail = %q, %v", got, err)
	}
	call := m.lastCall()
	if call.Path != "/api/v1/file/thumb.json/FID" || call.Query.Get("session_id") != "SID" || call.Query.Get("time_offset") != "1.25" || call.Query.Get("temp_key") != "TEMP" {
		t.Fatalf("thumbnail request = %+v", call)
	}

	m2, files2 := newFileFixture(t)
	m2.push(200, `{"error":{"code":403,"message":"denied"}}`)
	if _, err := files2.Thumbnail(context.Background(), "FID", ThumbnailOptions{}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("a JSON error body must stay an API error, got %v", err)
	}
}

func TestFileCreateAndMutations(t *testing.T) {
	ctx := context.Background()
	m, files := newFileFixture(t)
	m.push(200, `{"FileId":"NEW","Name":"empty"}`)
	m.push(200, `true`)
	m.push(200, `{"FileId":"COPY"}`)
	m.push(200, `{"FileId":"FID","Name":"renamed.txt"}`)
	m.push(200, `true`)
	m.push(200, `true`)
	m.push(200, `true`)
	m.push(200, `true`)
	m.push(200, `true`)
	created, err := files.CreateEmpty(ctx, CreateEmptyFileParams{AccessFolderID: "AF", FolderID: "DIR", FileType: "text/plain", SharingID: "SH"})
	if err != nil || created.FileID.String() != "NEW" {
		t.Fatalf("CreateEmpty = %+v, %v", created, err)
	}
	if body := m.lastCall().Body; body["folder_id"] != "DIR" || body["access_folder_id"] != "AF" || body["file_type"] != "text/plain" || body["session_id"] != "SID" {
		t.Fatalf("create body = %v", body)
	}

	if err := files.SetAccess(ctx, "FID", FileHidden, "AF", "SH"); err != nil {
		t.Fatal(err)
	}
	if got := m.lastCall().Body["file_ispublic"]; got != float64(FileHidden) {
		t.Fatalf("visibility = %v", got)
	}

	_, err = files.MoveCopy(ctx, FileMoveCopyParams{SourceFileID: "SRC", DestinationFolder: "DST", Move: false, OverwriteIfExists: true, NewName: "copy.txt"})
	if err != nil {
		t.Fatal(err)
	}
	body := m.lastCall().Body
	if body["move"] != "false" || body["overwrite_if_exists"] != "true" {
		t.Fatalf("boolean encoding = %v", body)
	}

	if _, err := files.Rename(ctx, "FID", "renamed.txt", "AF", "SH"); err != nil {
		t.Fatal(err)
	}
	if body := m.lastCall().Body; body["new_file_name"] != "renamed.txt" || body["access_folder_id"] != "AF" {
		t.Fatalf("rename body = %v", body)
	}

	if err := files.Trash(ctx, []string{"A", "B"}, "SH"); err != nil {
		t.Fatal(err)
	}
	if m.lastCall().Body["file_id"] != "A,B" {
		t.Fatalf("trash body = %v", m.lastCall().Body)
	}

	if err := files.Restore(ctx, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	if err := files.Remove(ctx, []string{"A"}, "AF", "SH"); err != nil {
		t.Fatal(err)
	}
	if err := files.DeleteTrashed(ctx, "FID", "AF", "SH"); err != nil {
		t.Fatal(err)
	}
	if call := m.lastCall(); call.Method != http.MethodDelete || call.Path != "/api/v1/file.json/SID/FID" || call.Query.Get("access_folder_id") != "AF" {
		t.Fatalf("delete request = %+v", call)
	}
	if err := files.RemoveVersion(ctx, "VERSION"); err != nil {
		t.Fatal(err)
	}
	if call := m.lastCall(); call.Method != http.MethodDelete || call.Path != "/api/v1/file/removefileversion.json/SID/VERSION" {
		t.Fatalf("remove version request = %+v", call)
	}
}

func TestFileSettingsPasswordAndEmail(t *testing.T) {
	ctx := context.Background()
	m, files := newFileFixture(t)
	name, desc, password := "new.txt", "desc", "one-time"
	visibility := FilePublic
	yes := true
	tm := NewUnixTime(time.Unix(1700000000, 0))
	m.push(200, `true`)
	m.push(200, `{"TempKey":"TEMP","Valid":"1"}`)
	m.push(200, `true`)
	if err := files.UpdateSettings(ctx, "FID", FileSettings{Name: &name, Description: &desc, Password: &password, Visibility: &visibility, EditOnline: &yes, ModificationTime: &tm, SharingID: "SH"}); err != nil {
		t.Fatal(err)
	}
	call := m.lastCall()
	if call.Method != http.MethodPut || call.Body["file_password"] != "one-time" || call.Body["file_ispublic"] != float64(1) || call.Body["file_edit_online"] != float64(1) {
		t.Fatalf("settings call = %+v", call)
	}
	if _, ok := call.Body["file_price"]; ok {
		t.Fatalf("unset file_price leaked into body: %v", call.Body)
	}

	verified, err := files.VerifyPassword(ctx, "FID", "one-time", "captcha")
	if err != nil || verified.TempKey != "TEMP" || !verified.Valid.Bool() {
		t.Fatalf("VerifyPassword = %+v, %v", verified, err)
	}
	if body := m.lastCall().Body; body["password"] != "one-time" || body["captcha_response"] != "captcha" {
		t.Fatalf("verify body = %v", body)
	}

	if err := files.SendByEmail(ctx, FileEmailParams{FileIDs: []string{"A", "B"}, Recipients: []string{" a@example.com ", "", "b@example.com"}, Subject: "s", Body: "b", SendExpiring: true}); err != nil {
		t.Fatal(err)
	}
	if body := m.lastCall().Body; body["file_id"] != "A,B" || body["recipient_emails"] != "a@example.com,b@example.com" || body["send_expiring_enabled"] != true {
		t.Fatalf("email body = %v", body)
	}
}

func TestFileExpiringLinksAndLocalValidation(t *testing.T) {
	ctx := context.Background()
	m, files := newFileFixture(t)
	m.push(200, fileFixture(t, "expiringlink.json"))
	m.push(200, fileFixture(t, "fileexpiringlinks.json"))

	link, err := files.CreateExpiringLink(ctx, "FID", "2026-08-01", 3, false)
	if err != nil {
		t.Fatalf("CreateExpiringLink: %v", err)
	}
	// A file link carries DownloadLink and StreamingLink where a folder link
	// carries Link (docs/discrepancies.md D31).
	if link.Link != "" || link.DownloadLink == "" || link.StreamingLink == "" {
		t.Fatalf("link = %+v", link)
	}
	if link.URL() != link.DownloadLink {
		t.Fatalf("URL() = %q", link.URL())
	}
	if call := m.lastCall(); call.Path != "/api/v1/file/expiringlink.json/SID/2026-08-01/3/FID/false" {
		t.Fatalf("link path = %q", call.Path)
	}

	listed, err := files.ExpiringLinks(ctx, "FID")
	if err != nil {
		t.Fatalf("ExpiringLinks: %v", err)
	}
	if listed.CounterMax.Int() != 5 || listed.ExpiringDate != "2026-12-31" {
		t.Fatalf("ExpiringLinks = %+v", listed)
	}

	m2, f2 := newFileFixture(t)
	invalid := []error{
		func() error { _, err := f2.CreateEmpty(ctx, CreateEmptyFileParams{}); return err }(),
		func() error { _, err := f2.Rename(ctx, "FID", "bad/name", "", ""); return err }(),
		f2.Trash(ctx, nil),
		f2.Restore(ctx, []string{"bad,id"}),
		f2.RemoveVersion(ctx, ""),
		func() error { _, err := f2.VerifyPassword(ctx, "", "", ""); return err }(),
		f2.SendByEmail(ctx, FileEmailParams{FileIDs: []string{"A"}}),
	}
	for _, err := range invalid {
		if ErrorKind(err) != KindInvalidRequest && ErrorKind(err) != KindInvalidName {
			t.Errorf("invalid call = %v", err)
		}
	}
	if m2.callCount() != 0 {
		t.Fatalf("invalid inputs reached upstream %d times", m2.callCount())
	}
}
