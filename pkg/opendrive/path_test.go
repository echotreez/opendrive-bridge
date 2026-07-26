package opendrive

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeFolderPath(t *testing.T) {
	ok := map[string]string{
		"":                "/",
		"/":               "/",
		".":               "/",
		"Finance":         "/Finance",
		"/Finance":        "/Finance",
		"/Finance/":       "/Finance",
		"/Finance//2026":  "/Finance/2026",
		"Finance/2026/Q1": "/Finance/2026/Q1",
		"/Finance/./2026": "/Finance/2026",
		"/财务/报表":          "/财务/报表",
		"  /Finance  ":    "/Finance",
	}
	for in, want := range ok {
		got, err := NormalizeFolderPath(in)
		if err != nil {
			t.Errorf("NormalizeFolderPath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeFolderPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// §9.3: a path must never be able to climb out of the account namespace.
func TestNormalizeFolderPathRejectsTraversalAndBadNames(t *testing.T) {
	bad := []string{
		"/Finance/../../etc",
		"..",
		"../secrets",
		"/a/../../b",
		"/a\x00b",
		"/a:b",
		"/a?b",
		"/" + strings.Repeat("x", MaxNameLength+1),
	}
	for _, in := range bad {
		if got, err := NormalizeFolderPath(in); err == nil {
			t.Errorf("NormalizeFolderPath(%q) = %q, want an error", in, got)
		}
	}

	// Even a ".." that would resolve to a folder inside the account is
	// refused, rather than silently pointing somewhere else.
	if got, err := NormalizeFolderPath("/Finance/2026/../2025"); err == nil {
		t.Fatalf("got %q, want a refusal rather than a silent rewrite", got)
	}
}

func TestParentPath(t *testing.T) {
	cases := []struct{ in, parent, name string }{
		{"/", "/", ""},
		{"", "/", ""},
		{"/Finance", "/", "Finance"},
		{"/Finance/2026", "/Finance", "2026"},
		{"/a/b/c", "/a/b", "c"},
	}
	for _, c := range cases {
		parent, name := ParentPath(c.in)
		if parent != c.parent || name != c.name {
			t.Errorf("ParentPath(%q) = %q, %q; want %q, %q", c.in, parent, name, c.parent, c.name)
		}
	}
}

// fakeCache is a minimal PathCache that records what the SDK asks of it.
type fakeCache struct {
	mu          sync.Mutex
	entries     map[string]string
	observed    map[string]int64
	invalidated []string
	resets      int
	lookups     int
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: map[string]string{}, observed: map[string]int64{}}
}

func (c *fakeCache) Lookup(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	id, ok := c.entries[path]
	return id, ok
}

func (c *fakeCache) Store(path, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[path] = id
}

func (c *fakeCache) Observe(id string, dirUpdateTime int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed[id] = dirUpdateTime
}

func (c *fakeCache) InvalidatePath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidated = append(c.invalidated, "path:"+path)
	delete(c.entries, path)
}

func (c *fakeCache) InvalidateID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidated = append(c.invalidated, "id:"+id)
}

func (c *fakeCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resets++
	c.entries = map[string]string{}
}

func (c *fakeCache) snapshot() (entries map[string]string, invalidated []string, resets int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.entries))
	for k, v := range c.entries {
		out[k] = v
	}
	return out, append([]string(nil), c.invalidated...), c.resets
}

// §10.3: a resolved path is answered from the cache the second time, without a
// round trip.
func TestResolvePathUsesTheCache(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	cache := newFakeCache()
	c := m.client(
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithPathCache(cache),
	)
	folders := c.Folders()

	m.push(200, fixture(t, "idbypath.json"))
	id, err := folders.ResolvePath(ctx, "/Developing")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if id != "RXhhbXBsZUZvbGRlcklE" {
		t.Fatalf("id = %q", id)
	}
	if m.callCount() != 1 {
		t.Fatalf("calls = %d", m.callCount())
	}

	// Second time: no traffic, and normalisation means the spelling need not
	// match exactly.
	for _, variant := range []string{"/Developing", "Developing", "/Developing/"} {
		id, err = folders.ResolvePath(ctx, variant)
		if err != nil || id != "RXhhbXBsZUZvbGRlcklE" {
			t.Fatalf("ResolvePath(%q) = %q, %v", variant, id, err)
		}
	}
	if m.callCount() != 1 {
		t.Fatalf("the cache was bypassed: %d calls", m.callCount())
	}

	// The root never costs anything.
	if id, err := folders.ResolvePath(ctx, "/"); err != nil || id != RootFolderID {
		t.Fatalf("root = %q, %v", id, err)
	}
	if c.PathCache() != cache {
		t.Fatal("PathCache accessor")
	}
}

func TestResolvePathWithoutACacheStillWorks(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	m.push(200, fixture(t, "idbypath.json"))
	m.push(200, fixture(t, "idbypath.json"))

	for i := 0; i < 2; i++ {
		if _, err := c.Folders().ResolvePath(ctx, "/Developing"); err != nil {
			t.Fatalf("ResolvePath: %v", err)
		}
	}
	if m.callCount() != 2 {
		t.Fatalf("calls = %d; without a cache every lookup goes upstream", m.callCount())
	}
	if c.PathCache() != nil {
		t.Fatal("no cache should be attached")
	}
}

func TestResolvePathRejectsATraversal(t *testing.T) {
	m := newMockUpstream(t)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}))
	if _, err := c.Folders().ResolvePath(context.Background(), "/a/../../b"); ErrorKind(err) != KindInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	if m.callCount() != 0 {
		t.Fatal("a traversal must not reach upstream")
	}
}

// ListPath feeds DirUpdateTime and the children it saw back into the cache.
func TestListPathPrimesTheCache(t *testing.T) {
	ctx := context.Background()
	m := newMockUpstream(t)
	cache := newFakeCache()
	c := m.client(
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithPathCache(cache),
	)

	m.push(200, fixture(t, "list_root.json"))
	listing, err := c.Folders().ListPath(ctx, "/", ListOptions{})
	if err != nil {
		t.Fatalf("ListPath: %v", err)
	}
	if listing.Len() != 2 {
		t.Fatalf("listing = %d entries", listing.Len())
	}

	entries, _, _ := cache.snapshot()
	if got := entries["/Developing"]; got != "RXhhbXBsZUZvbGRlcklE" {
		t.Fatalf("the child folder was not cached: %v", entries)
	}
	cache.mu.Lock()
	observed := cache.observed[RootFolderID]
	cache.mu.Unlock()
	if observed != 1785053779 {
		t.Fatalf("DirUpdateTime observed = %d", observed)
	}

	// A listing deeper down builds child paths under it, not under the root.
	m2 := newMockUpstream(t)
	cache2 := newFakeCache()
	cache2.Store("/Finance", "FIN")
	c2 := m2.client(
		WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
		WithPathCache(cache2),
	)
	m2.push(200, fixture(t, "list_root.json"))
	if _, err := c2.Folders().ListPath(ctx, "/Finance", ListOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, _, _ = cache2.snapshot()
	if _, ok := entries["/Finance/Developing"]; !ok {
		t.Fatalf("child paths = %v", entries)
	}
}

// §10.3: a write invalidates what it touched, so the next read cannot serve a
// stale id.
func TestMutationsInvalidateTheCache(t *testing.T) {
	ctx := context.Background()

	newFixture := func(t *testing.T, response string) (*mockUpstream, *FolderService, *fakeCache) {
		t.Helper()
		m := newMockUpstream(t)
		cache := newFakeCache()
		c := m.client(
			WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "SID"}}),
			WithPathCache(cache),
		)
		m.push(200, response)
		return m, c.Folders(), cache
	}

	t.Run("rename", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"FolderID":"FID","Shared":"False"}`)
		if _, err := folders.Rename(ctx, "FID", "new"); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:FID")
	})

	t.Run("create invalidates the parent", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"FolderID":"NEW","Shared":"False"}`)
		if _, err := folders.Create(ctx, CreateFolderParams{Name: "n", ParentID: "PARENT"}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:PARENT")
	})

	t.Run("move touches both ends", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"FolderID":"FID","Shared":"False"}`)
		if _, err := folders.MoveCopy(ctx, MoveCopyParams{FolderID: "SRC", DstFolderID: "DST", Move: true}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:SRC")
		mustContainString(t, invalidated, "id:DST")
	})

	t.Run("trash", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"result":true}`)
		if err := folders.Trash(ctx, []string{"A", "B"}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:A")
		mustContainString(t, invalidated, "id:B")
	})

	t.Run("restore", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"result":true}`)
		if err := folders.Restore(ctx, []string{"A"}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:A")
	})

	t.Run("remove", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"result":true}`)
		if err := folders.Remove(ctx, []string{"A"}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:A")
	})

	t.Run("setaccess", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"result":true}`)
		if err := folders.SetAccess(ctx, "FID", FolderPublic, false); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:FID")
	})

	t.Run("settings", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"result":true}`)
		if err := folders.UpdateSettings(ctx, "FID", FolderSettings{Description: "d"}); err != nil {
			t.Fatal(err)
		}
		_, invalidated, _ := cache.snapshot()
		mustContainString(t, invalidated, "id:FID")
	})

	t.Run("emptying the trash resets everything", func(t *testing.T) {
		_, folders, cache := newFixture(t, `true`)
		if err := folders.EmptyTrash(ctx); err != nil {
			t.Fatal(err)
		}
		if _, _, resets := cache.snapshot(); resets != 1 {
			t.Fatalf("resets = %d", resets)
		}
	})

	t.Run("a failed write invalidates nothing", func(t *testing.T) {
		_, folders, cache := newFixture(t, `{"error":{"code":403,"message":"Permission denied"}}`)
		if err := folders.Trash(ctx, []string{"A"}); err == nil {
			t.Fatal("expected an error")
		}
		if _, invalidated, _ := cache.snapshot(); len(invalidated) != 0 {
			t.Fatalf("invalidated %v after a failed call", invalidated)
		}
	})
}

func mustContainString(t *testing.T, haystack []string, needle string) {
	t.Helper()
	for _, s := range haystack {
		if s == needle {
			return
		}
	}
	t.Fatalf("%v does not contain %q", haystack, needle)
}
