package opendrive

import (
	"context"
	"testing"
)

const missingFolder = `{"error":{"code":404,"message":"Directory does not exist"}}`

// EnsurePath makes each missing level, parent first, and remembers what it made.
func TestEnsurePathCreatesWhatIsMissing(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	cache := newFakeCache()
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}), WithPathCache(cache))

	m.push(404, missingFolder)                       // /backup/data: missing
	m.push(404, missingFolder)                       // /backup: missing
	m.push(200, `{"FolderID":"FB","Name":"backup"}`) // create /backup under root
	m.push(200, `{"FolderID":"FD","Name":"data"}`)   // create /backup/data under FB

	id, err := c.Folders().EnsurePath(ctx, "/backup/data")
	if err != nil {
		t.Fatalf("EnsurePath: %v", err)
	}
	if id != "FD" {
		t.Fatalf("id = %q, want FD", id)
	}
	calls := m.calls()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	if calls[2].Body["folder_name"] != "backup" || calls[2].Body["folder_sub_parent"] != RootFolderID {
		t.Errorf("first create = %v, want backup under the root", calls[2].Body)
	}
	if calls[3].Body["folder_name"] != "data" || calls[3].Body["folder_sub_parent"] != "FB" {
		t.Errorf("second create = %v, want data under FB", calls[3].Body)
	}
	// Both are remembered: the next write under them costs nothing.
	for p, want := range map[string]string{"/backup": "FB", "/backup/data": "FD"} {
		if got, ok := cache.Lookup(p); !ok || got != want {
			t.Errorf("cache[%s] = %q, %v; want %s", p, got, ok, want)
		}
	}
}

// A path that exists is one lookup and no create.
func TestEnsurePathLeavesAnExistingFolderAlone(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	m.push(200, fixture(t, "idbypath.json"))
	if _, err := c.Folders().EnsurePath(context.Background(), "/Developing"); err != nil {
		t.Fatal(err)
	}
	if m.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", m.callCount())
	}
}

// Another flusher made the folder between our lookup and our create: the create
// fails, one more lookup finds it, and that is the answer.
func TestEnsurePathSurvivesLosingTheRaceToCreate(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithRetryPolicy(RetryPolicy{Max: 0}))
	m.push(404, missingFolder)                                                     // lookup: missing
	m.push(409, `{"error":{"code":409,"message":"Folder with same name exists"}}`) // create: lost the race
	m.push(200, `{"FolderId":"FWON"}`)                                             // lookup again: there
	id, err := c.Folders().EnsurePath(context.Background(), "/raced")
	if err != nil {
		t.Fatalf("EnsurePath: %v", err)
	}
	if id != "FWON" {
		t.Fatalf("id = %q, want the folder the other creator made", id)
	}
}

// Anything but "not found" is not a reason to create: an outage must not turn
// into a folder made in the wrong place.
func TestEnsurePathDoesNotCreateOnOtherErrors(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithRetryPolicy(RetryPolicy{Max: 0}))
	m.push(503, `{"error":{"code":503,"message":"busy"}}`)
	if _, err := c.Folders().EnsurePath(context.Background(), "/x"); err == nil {
		t.Fatal("EnsurePath succeeded through a 503")
	}
	if m.callCount() != 1 {
		t.Fatalf("calls = %d; a create was attempted after a non-404 failure", m.callCount())
	}
}
