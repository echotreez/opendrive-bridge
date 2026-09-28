package server

import (
	"context"
	"errors"
	"github.com/echotreez/opendrive-bridge/internal/datacache"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// ListResponse is the /v1/ls body: upstream's separate Folders and Files
// arrays merged into one, because a caller listing a directory wants what is in
// it, not upstream's internal division of it.
type ListResponse struct {
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	// DirUpdateTime is what upstream's paging needs echoed back to fetch the
	// next page (D7): there is no page token, and asking for a later page
	// without it silently re-reads the first one.
	DirUpdateTime int64 `json:"dir_update_time"`
	// NextOffset is null when the listing is complete.
	NextOffset *int `json:"next_offset"`
	// Partial is true when the listing could not include what OpenDrive holds:
	// OpenDrive was unreachable and this is only what the bridge is holding.
	// Detail says so in words. A listing that silently left things out would
	// tell a caller they had been lost.
	Partial bool   `json:"partial,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	p, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	folderID, err := s.folderIDFor(r.Context(), p)
	if err != nil {
		// A folder that exists only because the bridge is holding writes for it
		// — made by a write accepted during an outage, not yet created upstream —
		// lists from the cache alone.
		if pending := s.pendingListing(p); pending != nil && (isNotFound(err) || opendrive.IsTemporary(err)) {
			if opendrive.IsTemporary(err) {
				pending.Partial = true
				pending.Detail = "OpenDrive cannot be reached, so this lists only what the bridge is " +
					"holding for this folder. Anything already on OpenDrive is left out until it answers again."
			}
			writeJSON(w, r, http.StatusOK, pending)
			return
		}
		WriteError(w, r, notFoundIfMissing(err, p))
		return
	}

	opts := opendrive.ListOptions{SearchQuery: r.URL.Query().Get("search")}
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, convErr := parseInt(v)
		if convErr != nil || offset < 0 {
			WriteError(w, r, BadRequest("'offset' must be a number of entries to skip, such as 100."))
			return
		}
		last, lastErr := parseInt64(r.URL.Query().Get("dir_update_time"))
		if lastErr != nil || last == 0 {
			// D7: upstream pages by offset *plus* the previous page's
			// DirUpdateTime, and quietly re-serves page one without it.
			WriteError(w, r, BadRequest("Paging past the first page needs 'dir_update_time' "+
				"from the previous response as well as 'offset'."))
			return
		}
		opts.Page = opendrive.Pagination{Offset: offset, LastRequestTime: last}
	}

	listing, err := s.client.Folders().List(r.Context(), folderID, opts)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	out := ListResponse{Path: p, DirUpdateTime: listing.DirUpdateTime.Unix(), Entries: []Entry{}}
	for i := range listing.Folders {
		f := &listing.Folders[i]
		out.Entries = append(out.Entries, folderEntry(f, joinPath(p, f.Name)))
	}
	for i := range listing.Files {
		f := &listing.Files[i]
		out.Entries = append(out.Entries, fileEntry(f, joinPath(p, f.Name)))
	}
	if len(out.Entries) >= opendrive.MaxListPageSize {
		next := opts.Page.Offset + len(out.Entries)
		out.NextOffset = &next
	}
	// Merged into the first page only: pending entries are few, and adding them
	// to every page would list them once per page.
	if opts.Page.Offset == 0 {
		out.Entries = s.mergePending(p, out.Entries)
	}
	writeJSON(w, r, http.StatusOK, out)
}

// mergePending adds what the bridge holds for folder p and OpenDrive does not
// have yet. An entry upstream already lists is marked pending (a newer version
// is on its way) rather than listed twice.
func (s *Server) mergePending(p string, entries []Entry) []Entry {
	if s.datacache == nil {
		return entries
	}
	files, folders := datacache.ChildrenOf(s.datacache.Unsent(), p)
	if len(files) == 0 && len(folders) == 0 {
		return entries
	}
	index := map[string]int{}
	for i, e := range entries {
		index[e.Name] = i
	}
	for _, name := range folders {
		if _, ok := index[name]; !ok {
			entries = append(entries, Entry{Name: name, Path: joinPath(p, name), Kind: kindFolder, Pending: true})
		}
	}
	for _, o := range files {
		_, name := splitPath(o.RemotePath)
		modified := o.StoredAt.UTC().Format(time.RFC3339)
		pe := Entry{Name: name, Path: o.RemotePath, Kind: kindFile, Size: o.Size, Modified: &modified, Pending: true}
		if i, ok := index[name]; ok {
			entries[i] = pe
			continue
		}
		entries = append(entries, pe)
	}
	return entries
}

// pendingListing is the listing of a folder that exists only in the cache, or
// nil when the cache holds nothing under it.
func (s *Server) pendingListing(p string) *ListResponse {
	if s.datacache == nil {
		return nil
	}
	entries := s.mergePending(p, []Entry{})
	if len(entries) == 0 {
		return nil
	}
	return &ListResponse{Path: p, Entries: entries}
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	p, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, t.Entry)
}

// mkdirRequest is the /v1/mkdir body.
type mkdirRequest struct {
	Path string `json:"path"`
	// Access is private, public or hidden. Empty means private, which is the
	// safe default: a folder that is public by accident is a data leak.
	Access string `json:"access,omitempty"`
	// Parents creates missing intermediate folders, like mkdir -p.
	Parents bool `json:"parents,omitempty"`
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req mkdirRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := normalisePath(req.Path)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if p == "/" {
		WriteError(w, r, BadRequest("The top-level folder already exists; give the path of the "+
			"folder to create, such as /Documents/2026."))
		return
	}
	access, err := parseAccess(req.Access)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	created, err := s.mkdir(r.Context(), p, access, req.Parents)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, created)
}

func (s *Server) mkdir(ctx context.Context, p string, access opendrive.FolderAccess, parents bool) (Entry, error) {
	parentPath, name := splitPath(p)

	parentID, err := s.folderIDFor(ctx, parentPath)
	if err != nil {
		if !parents || !isNotFound(err) {
			return Entry{}, notFoundIfMissing(err, parentPath)
		}
		// mkdir -p: make the parent first, then carry on.
		if _, mkErr := s.mkdir(ctx, parentPath, access, true); mkErr != nil {
			return Entry{}, mkErr
		}
		if parentID, err = s.folderIDFor(ctx, parentPath); err != nil {
			return Entry{}, err
		}
	}

	created, err := s.client.Folders().Create(ctx, opendrive.CreateFolderParams{
		Name: name, ParentID: parentID, Access: access,
	})
	if err != nil {
		return Entry{}, err
	}
	s.invalidateSubtree(p, parentPath)
	return Entry{Name: created.Name, Path: p, Kind: kindFolder}, nil
}

// moveRequest is the body of /v1/mv and /v1/cp.
type moveRequest struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	// Overwrite replaces anything already at the destination.
	Overwrite bool `json:"overwrite,omitempty"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) { s.moveOrCopy(w, r, true) }
func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) { s.moveOrCopy(w, r, false) }

// moveOrCopy backs both endpoints. Upstream's own move and copy differ only in
// a flag, and its two modules disagree about the type of that flag — folders
// refuse the documented boolean outright (D26) — which the SDK settles.
func (s *Server) moveOrCopy(w http.ResponseWriter, r *http.Request, move bool) {
	var req moveRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	src, err := normalisePath(req.Src)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	dst, err := normalisePath(req.Dst)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	source, err := s.resolve(r.Context(), src)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// The destination may be an existing folder to drop the item into, or a new
	// path to give it. Working out which needs the parent listing, never
	// info.json (D27).
	dstFolder, dstName, err := s.destination(r.Context(), dst, source.Name)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	if source.Kind == kindFolder {
		_, err = s.client.Folders().MoveCopy(r.Context(), opendrive.MoveCopyParams{
			FolderID: source.ID, DstFolderID: dstFolder.ID, Move: move, NewName: dstName,
		})
	} else {
		_, err = s.client.Files().MoveCopy(r.Context(), opendrive.FileMoveCopyParams{
			SourceFileID: source.ID, DestinationFolder: dstFolder.ID,
			Move: move, OverwriteIfExists: req.Overwrite, NewName: dstName,
		})
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.invalidateSubtree(src, dst, dstFolder.Path)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"src": src, "dst": joinPath(dstFolder.Path, dstName), "moved": move,
	})
}

// destination works out the folder an item is going into and the name it will
// have there.
func (s *Server) destination(ctx context.Context, dst, sourceName string) (*target, string, error) {
	if t, err := s.resolve(ctx, dst); err == nil && t.Kind == kindFolder {
		// The destination names an existing folder: keep the source's name.
		return t, sourceName, nil
	} else if err != nil && !isNotFound(err) {
		return nil, "", err
	}

	parentPath, name := splitPath(dst)
	parent, err := s.requireFolder(ctx, parentPath)
	if err != nil {
		return nil, "", notFoundIfMissing(err, parentPath)
	}
	if err := opendrive.ValidateName(name); err != nil {
		return nil, "", err
	}
	return parent, name, nil
}

// renameRequest is the /v1/rename body.
type renameRequest struct {
	Path    string `json:"path"`
	NewName string `json:"new_name"`
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := normalisePath(req.Path)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// Checked locally first so the caller gets a clear answer rather than
	// upstream's (§2.6 #10).
	if err := opendrive.ValidateName(req.NewName); err != nil {
		WriteError(w, r, err)
		return
	}

	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if t.Kind == kindFolder {
		_, err = s.client.Folders().Rename(r.Context(), t.ID, req.NewName)
	} else {
		_, err = s.client.Files().Rename(r.Context(), t.ID, req.NewName, "", "")
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	parentPath, _ := splitPath(p)
	s.invalidateSubtree(p, parentPath)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"path": joinPath(parentPath, req.NewName), "renamed_from": p,
	})
}

// removeRequest is the /v1/rm body.
type removeRequest struct {
	Path string `json:"path"`
	// Permanent deletes outright instead of moving to the trash. The default is
	// the trash, because upstream's permanent delete has no undo and does not
	// require the item to be trashed first (D29).
	Permanent bool `json:"permanent,omitempty"`
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req removeRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := normalisePath(req.Path)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if p == "/" {
		WriteError(w, r, BadRequest("The top-level folder cannot be removed."))
		return
	}

	// The bridge's own copy goes first, whatever its state. An unsent object
	// left behind would be uploaded by the flusher after this delete, putting
	// back what the user removed; a clean one would keep being served. Remove
	// waits out an upload in progress for the same reason.
	held := 0
	if s.datacache != nil {
		n, err := s.datacache.RemoveTree(r.Context(), p)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		held = n
	}

	t, err := s.resolve(r.Context(), p)
	if err != nil {
		if held > 0 && isNotFound(err) {
			// It existed only on the bridge, waiting to be sent. Dropping it
			// was the whole delete.
			parentPath, _ := splitPath(p)
			s.invalidateSubtree(p, parentPath)
			writeJSON(w, r, http.StatusOK, map[string]any{
				"path": p, "permanent": true,
				"detail": "Removed from the bridge before it reached OpenDrive, so there was nothing to put in the trash.",
			})
			return
		}
		WriteError(w, r, err)
		return
	}

	if t.Kind == kindFolder {
		if err := s.client.Folders().Trash(r.Context(), []string{t.ID}); err != nil {
			WriteError(w, r, err)
			return
		}
		if req.Permanent {
			if err := s.client.Folders().Remove(r.Context(), []string{t.ID}); err != nil {
				WriteError(w, r, err)
				return
			}
		}
	} else {
		if err := s.client.Files().Trash(r.Context(), []string{t.ID}); err != nil {
			WriteError(w, r, err)
			return
		}
		if req.Permanent {
			if err := s.client.Files().Remove(r.Context(), []string{t.ID}, "", ""); err != nil {
				WriteError(w, r, err)
				return
			}
		}
	}
	parentPath, _ := splitPath(p)
	s.invalidateSubtree(p, parentPath)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"path": p, "permanent": req.Permanent,
		"detail": removalDetail(req.Permanent),
	})
}

func removalDetail(permanent bool) string {
	if permanent {
		return "Deleted for good. This cannot be undone."
	}
	return "Moved to the trash. Restore it from there if you change your mind."
}

func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	listing, err := s.client.Folders().TrashList(r.Context(), opendrive.TrashListOptions{})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	entries := []Entry{}
	for i := range listing.Folders {
		f := &listing.Folders[i]
		entries = append(entries, folderEntry(f, "/"+f.Name))
	}
	for i := range listing.Files {
		f := &listing.Files[i]
		entries = append(entries, fileEntry(f, "/"+f.Name))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Folders().EmptyTrash(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	if s.cache != nil {
		s.cache.Reset()
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"detail": "The trash is empty. Everything in it has been deleted for good.",
	})
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	p, err := normalisePath(r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := s.resolve(r.Context(), p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if t.Kind != kindFile {
		WriteError(w, r, BadRequest(p+" is a folder, and only files have versions."))
		return
	}

	versions, err := s.client.Files().Versions(r.Context(), t.ID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(versions))
	for i := range versions {
		v := &versions[i]
		out = append(out, map[string]any{
			"version":  v.Version.String(),
			"name":     v.Name,
			"size":     v.Size.Int64(),
			"modified": timePtr(v.DateModified),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"path": p, "versions": out})
}

func parseAccess(s string) (opendrive.FolderAccess, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "private":
		return opendrive.FolderPrivate, nil
	case "public":
		return opendrive.FolderPublic, nil
	case "hidden":
		return opendrive.FolderHidden, nil
	default:
		return 0, BadRequest("'access' must be private, public or hidden.")
	}
}

// isNotFound reports whether an error means "there is nothing there", whether it
// came from the SDK or from this package.
func isNotFound(err error) bool {
	if errors.Is(err, opendrive.ErrNotFound) {
		return true
	}
	var re *RequestError
	return errors.As(err, &re) && re.HTTP == http.StatusNotFound
}

func parseInt(s string) (int, error) { return strconv.Atoi(s) }

func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}
