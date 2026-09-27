package opendrive

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func newSharingFixture(t *testing.T) (*mockUpstream, *SharingService) {
	t.Helper()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	return m, c.Sharing()
}

// The success shapes below are constructed from the spec, not recorded: the
// test account cannot reach the sharing module at all (D33). They pin down the
// request the SDK builds, which is what the contract test also checks; the
// response decoding is provisional until an owner account can record it.
func TestSharingShare(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, `{"SharingID":"U2hhcmluZ0lE","FolderID":"FID","UserName":"colleague@example.com","ShareMode":"1"}`)

	got, err := sharing.Share(context.Background(), "FID", "colleague@example.com", ShareFullAccess)
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if got.SharingID.String() != "U2hhcmluZ0lE" {
		t.Fatalf("share = %+v", got)
	}
	body := m.lastCall().Body
	if body["folder_id"] != "FID" || body["username"] != "colleague@example.com" {
		t.Fatalf("body = %v", body)
	}
	// The spec types sharemode as a string, so it goes on the wire as one.
	if body["sharemode"] != "1" {
		t.Fatalf("sharemode = %#v, want the string \"1\"", body["sharemode"])
	}
	if body["session_id"] != "SID" {
		t.Fatal("the session belongs in the body for POST /sharing.json")
	}
}

func TestSharingShareValidatesLocally(t *testing.T) {
	m, sharing := newSharingFixture(t)
	ctx := context.Background()
	if _, err := sharing.Share(ctx, "", "user@example.com", ShareViewOnly); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing folder = %v", err)
	}
	if _, err := sharing.Share(ctx, "FID", "", ShareViewOnly); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing username = %v", err)
	}
	if err := sharing.SetMode(ctx, "", ShareViewOnly); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing sharing id = %v", err)
	}
	if err := sharing.Revoke(ctx, ""); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing sharing id = %v", err)
	}
	if _, err := sharing.ListSharedFolders(ctx, ""); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing sharing id = %v", err)
	}
	if _, err := sharing.ListFolderUsers(ctx, ""); ErrorKind(err) != KindInvalidRequest {
		t.Errorf("missing folder id = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatalf("invalid input reached upstream %d times", m.callCount())
	}
}

// §2.6 #4: POST shares and DELETE revokes on the same resource, and the
// arguments move from the body into the path between the two.
func TestSharingShareAndRevokeShareAPathButNotAVerb(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, `{"result":true}`)
	if err := sharing.Revoke(context.Background(), "SHID"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	call := m.lastCall()
	if call.Method != http.MethodDelete {
		t.Fatalf("method = %s", call.Method)
	}
	if call.Path != "/api/v1/sharing.json/SID/SHID" {
		t.Fatalf("path = %q", call.Path)
	}
	if call.RawBody != "" {
		t.Fatalf("DELETE sent a body: %q", call.RawBody)
	}
}

// D15's sibling: setmode is a PUT even though the share was created with POST.
func TestSharingSetModeIsAPut(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, `{"result":true}`)
	if err := sharing.SetMode(context.Background(), "SHID", ShareViewOnly); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	call := m.lastCall()
	if call.Method != http.MethodPut {
		t.Fatalf("method = %s, want PUT", call.Method)
	}
	if call.Body["sharing_id"] != "SHID" || call.Body["sharemode"] != "0" {
		t.Fatalf("body = %v", call.Body)
	}
}

func TestSharingListings(t *testing.T) {
	ctx := context.Background()

	m, sharing := newSharingFixture(t)
	m.push(200, moduleFixture(t, "sharing", "listsharedusers.json"))
	users, err := sharing.ListSharedUsers(ctx)
	if err != nil {
		t.Fatalf("ListSharedUsers: %v", err)
	}
	if len(users) != 1 || users[0].UserName != "colleague@example.com" {
		t.Fatalf("users = %+v", users)
	}
	if users[0].ShareMode.Int() != 1 || users[0].DateShared.Unix() != 1785027933 {
		t.Fatalf("share metadata = %+v", users[0])
	}
	if m.lastCall().Path != "/api/v1/sharing/listsharedusers.json/SID" {
		t.Fatalf("path = %q", m.lastCall().Path)
	}

	m2, sharing2 := newSharingFixture(t)
	m2.push(200, `[]`)
	if _, err := sharing2.ListFolderUsers(ctx, "FID"); err != nil {
		t.Fatalf("ListFolderUsers: %v", err)
	}
	if m2.lastCall().Path != "/api/v1/sharing/listusers.json/SID/FID" {
		t.Fatalf("path = %q", m2.lastCall().Path)
	}

	m3, sharing3 := newSharingFixture(t)
	m3.push(200, `[{"FolderID":"FID","SharingID":"SHID","Name":"Shared reports"}]`)
	folders, err := sharing3.ListSharedFolders(ctx, "SHID")
	if err != nil || len(folders) != 1 || folders[0].Name != "Shared reports" {
		t.Fatalf("ListSharedFolders = %+v, %v", folders, err)
	}
	if m3.lastCall().Path != "/api/v1/sharing/listsharedfolders.json/SID/SHID" {
		t.Fatalf("path = %q", m3.lastCall().Path)
	}
}

func TestSharingCheckAccountUsersAccess(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, `{"result":true}`)
	got, err := sharing.CheckAccountUsersAccess(context.Background())
	if err != nil {
		t.Fatalf("CheckAccountUsersAccess: %v", err)
	}
	if !got.OK() {
		t.Fatalf("access = %+v", got)
	}
	// The spec declares no parameters, so the session rides in the query.
	if m.lastCall().Query.Get("session_id") != "SID" {
		t.Fatalf("query = %v", m.lastCall().Query)
	}
}

// D33: an account-user login is refused on every sharing operation, and the
// refusal is a plain upstream error rather than a credential problem — mapping
// it to reauth_required would send the silent re-login state machine chasing a
// password that is not the problem (v1.1 §4.5).
func TestSharingAccountUserRefusalIsAnUpstreamError(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(403, moduleFixture(t, "sharing", "account_user_denied.json"))

	_, err := sharing.ListSharedUsers(context.Background())
	if ErrorKind(err) != KindUpstreamError {
		t.Fatalf("err = %v, want upstream_error", err)
	}
	mustContain(t, err.Error(), "Account users cannot list shared users", "refusal message")
	if IsTemporary(err) {
		t.Fatal("a permission refusal must not be retried")
	}
}

// A share changes what the folder looks like, so the path cache entry for it
// has to go (§10.3).
func TestSharingInvalidatesTheFolderCache(t *testing.T) {
	m := newMockUpstream(t)
	cache := newFakeCache()
	c := m.client(
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithPathCache(cache),
	)
	m.push(200, `{"SharingID":"SHID","FolderID":"FID"}`)
	if _, err := c.Sharing().Share(context.Background(), "FID", "user@example.com", ShareViewOnly); err != nil {
		t.Fatal(err)
	}
	_, invalidated, _ := cache.snapshot()
	mustContainString(t, invalidated, "id:FID")
}

func TestShareModeRendersAsADecimalString(t *testing.T) {
	if ShareViewOnly.String() != "0" || ShareFullAccess.String() != "1" {
		t.Fatalf("modes render as %q and %q", ShareViewOnly, ShareFullAccess)
	}
}

// D48: with nothing shared, listsharedusers answers a folder-listing-shaped
// object rather than []. Recorded from an owner login on 2026-09-27.
func TestSharingListSharedUsersEmptyObjectIsAnEmptyList(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, moduleFixture(t, "sharing", "listsharedusers_empty.json"))
	users, err := sharing.ListSharedUsers(context.Background())
	if err != nil {
		t.Fatalf("the recorded empty answer was refused: %v", err)
	}
	if users == nil || len(users) != 0 {
		t.Fatalf("users = %#v, want an empty, non-nil list", users)
	}
}

// The sibling endpoint says the same thing with a real empty array.
func TestSharingListFolderUsersEmptyArray(t *testing.T) {
	m, sharing := newSharingFixture(t)
	m.push(200, moduleFixture(t, "sharing", "listusers_empty.json"))
	users, err := sharing.ListFolderUsers(context.Background(), "FID")
	if err != nil || len(users) != 0 {
		t.Fatalf("users = %v, err = %v", users, err)
	}
}

// Any other object is not guessed at: the non-empty shape has never been seen,
// so an object carrying anything beyond D48's two keys is an invalid response
// that names what it did carry.
func TestSharingListSharedUsersUnknownObjectIsNotGuessed(t *testing.T) {
	for _, body := range []string{
		`{"DirUpdateTime":1,"ResponseType":1,"Users":[{"UserID":"1"}]}`,
		`{"Something":"else"}`,
	} {
		m, sharing := newSharingFixture(t)
		m.push(200, body)
		_, err := sharing.ListSharedUsers(context.Background())
		if ErrorKind(err) != KindInvalidResponse {
			t.Errorf("%s: kind = %q, want invalid_response", body, ErrorKind(err))
			continue
		}
		if !strings.Contains(err.Error(), "D48") {
			t.Errorf("%s: the error does not point at D48: %v", body, err)
		}
	}
	m, sharing := newSharingFixture(t)
	m.push(200, `null`)
	if users, err := sharing.ListSharedUsers(context.Background()); err != nil || len(users) != 0 {
		t.Errorf("null: users = %v, err = %v", users, err)
	}
	m2, sharing2 := newSharingFixture(t)
	m2.push(200, `not json`)
	if _, err := sharing2.ListSharedUsers(context.Background()); ErrorKind(err) != KindInvalidResponse {
		t.Errorf("garbage: kind = %q", ErrorKind(err))
	}
}
