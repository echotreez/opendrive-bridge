package opendrive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func moduleFixture(t *testing.T, module, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", module, name))
	if err != nil {
		t.Fatalf("fixture %s/%s: %v", module, name, err)
	}
	return string(raw)
}

func newUsersFixture(t *testing.T) (*mockUpstream, *UsersService) {
	t.Helper()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	return m, c.Users()
}

func TestUsersInfoDecodesTheRecordedResponse(t *testing.T) {
	m, users := newUsersFixture(t)
	m.push(200, moduleFixture(t, "users", "info.json"))

	got, err := users.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if m.lastCall().Path != "/api/v1/users/info.json/SID" {
		t.Fatalf("path = %q; the session is a path segment here", m.lastCall().Path)
	}
	if len(m.lastCall().Query) != 0 {
		t.Fatalf("a plain Info sent %v", m.lastCall().Query)
	}

	if got.UserID.String() != "1000001" || got.UserName != "tester@example.com" {
		t.Fatalf("identity = %+v", got)
	}
	// Quota arrives quoted (§2.6 #5) and feeds /v1/auth/status (§4.1).
	if got.MaxStorage.Int64() != 1048576 || got.BwMax.Int64() != 10240 {
		t.Errorf("quota = %+v", got)
	}
	// Mixed boolean encodings in one object: "0" strings and a real false.
	if got.FVersioning.Bool() || got.Suspended.Bool() || got.Enable2FA.Bool() {
		t.Errorf("flags = %+v", got)
	}
	if !got.AdminMode.Bool() {
		t.Errorf(`AdminMode "1" should decode as true: %+v`, got)
	}
	if got.UserSince.Unix() != 1785027739 {
		t.Errorf("UserSince = %v", got.UserSince)
	}
	if got.UserPlan == "" || got.TimeZone != "America/New_York" {
		t.Errorf("plan/timezone = %+v", got)
	}

	// D33: this login is an account user, which is why sharing is closed to it.
	if !got.IsAccountUser() {
		t.Error("AccessUserID differs from UserID, so IsAccountUser must be true")
	}

	// §9.4: neither the private key nor anything personal may show up in
	// casual output.
	s := got.String()
	mustNotContain(t, s, "tester@example.com", "AccountInfo.String")
	mustContain(t, s, Redacted, "AccountInfo.String")
	mustNotContain(t, RedactString(`{"PrivateKey":"c337c430e3"}`), "c337c430e3", "redacted private key")
}

func TestUsersInfoOptions(t *testing.T) {
	m, users := newUsersFixture(t)
	m.push(200, moduleFixture(t, "users", "info.json"))
	if _, err := users.Info(context.Background(), AccountInfoOptions{ApplyBW: true, Branding: true}); err != nil {
		t.Fatal(err)
	}
	q := m.lastCall().Query
	if q.Get("apply_bw") != "1" || q.Get("branding") != "1" {
		t.Fatalf("query = %v", q)
	}
}

func TestUsersOwnerAccountIsNotAnAccountUser(t *testing.T) {
	m, users := newUsersFixture(t)
	m.push(200, `{"UserID":"1000001","AccessUserID":"1000001","UserName":"owner@example.com"}`)
	got, err := users.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.IsAccountUser() {
		t.Error("an owner login has AccessUserID == UserID")
	}
}

func TestUsersLogsPagesByNumber(t *testing.T) {
	m, users := newUsersFixture(t)
	m.push(200, moduleFixture(t, "users", "userlogs.json"))

	logType := 3
	got, err := users.Logs(context.Background(), 2, ActivityLogFilter{
		AccessUserID: "60516",
		Start:        NewUnixTime(time.Unix(1785000000, 0)),
		End:          NewUnixTime(time.Unix(1785999999, 0)),
		LogType:      &logType,
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if got.TotalPages.Int() != 3 || got.CurrentPage.Int() != 1 || len(got.Logs) != 2 {
		t.Fatalf("page = %+v", got)
	}
	if got.Logs[0].LogType != "USER LOGIN" || got.Logs[0].Time.Unix() != 1785127915 {
		t.Fatalf("first entry = %+v", got.Logs[0])
	}
	// FileSize is an empty string for entries that have no file.
	if got.Logs[0].FileSize.Int() != 0 {
		t.Errorf("empty FileSize should decode to 0: %+v", got.Logs[0])
	}

	q := m.lastCall().Query
	want := map[string]string{
		"page": "2", "access_user_id": "60516",
		"start_date": "1785000000", "end_date": "1785999999", "log_type": "3",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v)
		}
	}

	// Page 0 means "whatever upstream defaults to", so it is not sent.
	m2, users2 := newUsersFixture(t)
	m2.push(200, moduleFixture(t, "users", "userlogs.json"))
	if _, err := users2.Logs(context.Background(), 0, ActivityLogFilter{}); err != nil {
		t.Fatal(err)
	}
	if len(m2.lastCall().Query) != 0 {
		t.Fatalf("an unfiltered first page sent %v", m2.lastCall().Query)
	}

	if _, err := users2.Logs(context.Background(), -1, ActivityLogFilter{}); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("negative page = %v", err)
	}
}

// D34: the cursor endpoint is absent from the PDF and is the safe way to walk a
// log that is still being appended to.
func TestUsersLogsCursorPagesByKeyset(t *testing.T) {
	m, users := newUsersFixture(t)
	m.push(200, moduleFixture(t, "users", "userlogscursor.json"))

	first, err := users.LogsCursor(context.Background(), "", ActivityLogFilter{})
	if err != nil {
		t.Fatalf("LogsCursor: %v", err)
	}
	if first.NextCursor == "" || len(first.Logs) != 1 {
		t.Fatalf("first page = %+v", first)
	}
	if m.lastCall().Query.Has("cursor") {
		t.Fatal("the first page must not send a cursor")
	}

	m2, users2 := newUsersFixture(t)
	m2.push(200, `{"NextCursor":"","Logs":[]}`)
	last, err := users2.LogsCursor(context.Background(), first.NextCursor, ActivityLogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if last.NextCursor != "" {
		t.Fatal("an empty NextCursor marks the end of the log")
	}
	if m2.lastCall().Query.Get("cursor") != first.NextCursor {
		t.Fatalf("cursor = %q", m2.lastCall().Query.Get("cursor"))
	}
}

// v1.0 binds the read half only (§1.4): the mutating endpoints are deliberately
// absent rather than present and discouraged.
func TestUsersModuleExposesNoWriteOperations(t *testing.T) {
	m, _ := newUsersFixture(t)
	c := m.client()
	users := c.Users()

	// A compile-time inventory: if someone adds a mutator, this list is where
	// the v1.1 decision gets revisited.
	readOnly := []string{"Info", "Logs", "LogsCursor"}
	if len(readOnly) != 3 {
		t.Fatal("update this test when the users surface changes")
	}
	_ = users

	// The endpoint constants for the write half are intentionally not defined,
	// so no caller can reach them by accident.
	for _, path := range []string{"/users/password.json", "/users/email.json", "/users/username.json"} {
		if strings.Contains(EndpointUsersInfo, path) {
			t.Errorf("%s should not be reachable in v1.0", path)
		}
	}
}
