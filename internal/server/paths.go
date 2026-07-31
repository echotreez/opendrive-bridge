package server

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// The Bridge is path-addressed: callers say /Docs/2026/report.pdf and never see
// an upstream id. Everything in this file is about turning one into the other
// truthfully.
//
// The rule that shapes it: **existence is decided by the parent listing or by
// idbypath, never by info.json.** folder/info.json keeps describing a folder
// that was permanently deleted (D27), so a /v1/stat built on it would tell a
// user a deleted folder is still there. The same rule governs the pre-flight
// before an upload.

// entryKind distinguishes the two things a path can name.
type entryKind string

const (
	kindFolder entryKind = "folder"
	kindFile   entryKind = "file"
)

// Entry is one item in a listing or the answer to /v1/stat. It is the Bridge's
// own shape, not upstream's: names a caller can use, sizes in bytes, times as
// RFC 3339, and no upstream identifiers at all.
type Entry struct {
	Name string    `json:"name"`
	Path string    `json:"path"`
	Kind entryKind `json:"kind"`
	Size int64     `json:"size"`
	// Modified is null when upstream did not report one.
	Modified *string `json:"modified"`
	// Public reports whether the item is reachable without signing in.
	Public bool `json:"public"`
	// Children counts sub-items, for folders only.
	Children int64 `json:"children,omitempty"`
	// Link is the public share link when there is one.
	Link string `json:"link,omitempty"`
}

// target is a resolved path: what it is, and the ids the SDK needs.
type target struct {
	Path     string
	Kind     entryKind
	ID       string // folder id or file id
	ParentID string
	Name     string
	Entry    Entry
}

// normalisePath turns a caller's path into the canonical form used everywhere:
// a leading slash, no trailing slash, no "." or ".." segments.
//
// ".." is rejected rather than resolved. Resolving it locally would let a path
// that looks confined to a subtree escape it, and the bridge has no business
// deciding that a caller meant to leave the folder they named.
func normalisePath(p string) (string, error) {
	if p == "" {
		return "", BadRequest("A 'path' is required, for example /Documents/report.pdf.")
	}
	if !strings.HasPrefix(p, "/") {
		return "", BadRequest("Paths must start with a slash, for example /Documents/report.pdf.")
	}

	// Each segment is trimmed before anything else looks at it, matching the
	// SDK and matching upstream: measured on the live API, a folder created as
	// "report " is stored as "report", while interior spaces survive (D46). A
	// caller who pastes a path with a stray space is asking for a folder that
	// upstream cannot have, so trimming here is the difference between finding
	// their folder and being told it does not exist.
	//
	// It happens before the traversal check because " .. " is not ".." to a
	// string comparison but is to upstream, which trims the name first.
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = strings.TrimSpace(seg)
	}
	p = strings.Join(segments, "/")

	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", BadRequest("Paths cannot contain '..'. Give the full path from the top, " +
				"for example /Documents/report.pdf.")
		}
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		cleaned = "/"
	}
	return cleaned, nil
}

// splitPath returns the parent directory and the final element.
func splitPath(p string) (parent, name string) {
	if p == "/" {
		return "/", ""
	}
	parent = path.Dir(p)
	name = path.Base(p)
	return parent, name
}

// folderIDFor resolves a folder path to an upstream id through idbypath, which
// the path cache sits in front of.
func (s *Server) folderIDFor(ctx context.Context, p string) (string, error) {
	if p == "/" {
		return opendrive.RootFolderID, nil
	}
	id, err := s.client.Folders().IDByPath(ctx, p)
	if err != nil {
		return "", err
	}
	return id, nil
}

// resolve finds what a path names, without ever asking info.json.
//
// One idbypath for the parent and one listing of it: the listing is the only
// answer that is true for both "does this exist" and "what is it", and it
// carries the metadata /v1/stat needs, so nothing further is required.
func (s *Server) resolve(ctx context.Context, p string) (*target, error) {
	if p == "/" {
		listing, err := s.client.Folders().List(ctx, opendrive.RootFolderID, opendrive.ListOptions{})
		if err != nil {
			return nil, err
		}
		return &target{
			Path: "/", Kind: kindFolder, ID: opendrive.RootFolderID, Name: "/",
			Entry: Entry{Name: "/", Path: "/", Kind: kindFolder,
				Children: int64(len(listing.Folders) + len(listing.Files))},
		}, nil
	}

	parentPath, name := splitPath(p)
	parentID, err := s.folderIDFor(ctx, parentPath)
	if err != nil {
		return nil, notFoundIfMissing(err, p)
	}

	listing, err := s.client.Folders().List(ctx, parentID, opendrive.ListOptions{})
	if err != nil {
		return nil, err
	}

	for i := range listing.Folders {
		f := &listing.Folders[i]
		if f.Name != name {
			continue
		}
		return &target{
			Path: p, Kind: kindFolder, ID: f.FolderID.String(), ParentID: parentID, Name: name,
			Entry: folderEntry(f, p),
		}, nil
	}
	for i := range listing.Files {
		f := &listing.Files[i]
		if f.Name != name {
			continue
		}
		return &target{
			Path: p, Kind: kindFile, ID: f.FileID.String(), ParentID: parentID, Name: name,
			Entry: fileEntry(f, p),
		}, nil
	}
	return nil, pathNotFound(p)
}

// exists reports whether a path names anything, without info.json (D27) and
// without treating a transient failure as absence.
func (s *Server) exists(ctx context.Context, p string) (bool, error) {
	_, err := s.resolve(ctx, p)
	if err == nil {
		return true, nil
	}
	// isNotFound, not errors.Is against the SDK sentinel: resolve reports a
	// missing path with this package's own error so the message can name the
	// path. Checking only the SDK's sentinel would treat "not there" as a real
	// failure — which made every upload to a new name fail with "there is
	// nothing at" the file it was about to create.
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// requireFolder resolves a path that must name a folder.
func (s *Server) requireFolder(ctx context.Context, p string) (*target, error) {
	t, err := s.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if t.Kind != kindFolder {
		return nil, &RequestError{Code: string(opendrive.KindInvalidRequest), HTTP: http.StatusBadRequest,
			Message: p + " is a file, and this needs a folder."}
	}
	return t, nil
}

// pathNotFound is the one error a caller sees most, so it names the path back.
func pathNotFound(p string) error {
	return &RequestError{
		Code: string(opendrive.KindNotFound), HTTP: http.StatusNotFound,
		Message: "There is nothing at " + p + ".",
	}
}

// notFoundIfMissing rewrites the SDK's not_found so the message names the path
// the caller asked about rather than the part of it that failed to resolve.
func notFoundIfMissing(err error, p string) error {
	if errors.Is(err, opendrive.ErrNotFound) {
		return pathNotFound(p)
	}
	return err
}

// invalidateSubtree drops everything the path cache holds at or below a path.
//
// Every write calls it. Upstream's own read-after-write is not guaranteed
// (D44), so a stale cache entry would outlive the change that invalidated it
// and the next read would answer from a world that no longer exists.
func (s *Server) invalidateSubtree(paths ...string) {
	if s.cache == nil {
		return
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		s.cache.InvalidatePath(p)
		if parent, _ := splitPath(p); parent != p {
			s.cache.InvalidatePath(parent)
		}
	}
}

func folderEntry(f *opendrive.FolderEntry, p string) Entry {
	return Entry{
		Name: f.Name, Path: p, Kind: kindFolder,
		Size: f.Size.Int64(), Modified: timePtr(f.DateModified),
		Public:   f.Access.Int() == int(opendrive.FolderPublic),
		Children: f.ChildFolders.Int64(), Link: f.Link,
	}
}

func fileEntry(f *opendrive.FileEntry, p string) Entry {
	return Entry{
		Name: f.Name, Path: p, Kind: kindFile,
		Size: f.Size.Int64(), Modified: timePtr(f.DateModified),
		Public: f.Access.Int() == int(opendrive.FilePublic), Link: f.Link,
	}
}

func timePtr(t opendrive.UnixTime) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z")
	return &s
}

// joinPath builds the path of a child, and the result is always in the form
// normalisePath produces — every caller here hands it straight back to a caller
// of the API or uses it as a path-cache key, and a key that does not match the
// one a lookup would compute is a cache that never hits.
//
// The name is trimmed for the reason in D46: upstream removes surrounding
// whitespace from names, so a listing entry called " report" describes a folder
// upstream knows as "report", and building "/ report" from it would produce a
// path that resolves to nothing. A fuzzer found this by joining "/" and " 0".
func joinPath(dir, name string) string {
	name = strings.TrimSpace(name)
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
