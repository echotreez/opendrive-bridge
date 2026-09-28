// Package odfake is an in-memory OpenDrive that keeps what it is given.
//
// tools/mockupstream answers every call with the same canned file, which is
// enough to prove a binary starts and a request goes round. It cannot carry an
// S3 client end to end: restic writes a repository and reads it back, and a
// store that forgets the bytes, or invents a hash, fails that on the first
// check (§3.6, S1). This one stores folders and files for real.
//
// It reproduces the upstream behaviour the SDK was built around, because a fake
// politer than the real thing tests nothing: idbypath takes paths without a
// leading slash (D22), numbers arrive as strings (D19), listings split folders
// from files and page at 100 (§2.6 #14), the chunk endpoint reports each chunk's
// size rather than a running total (D35), chunks must arrive in order (D37), a
// created but unclosed file is listed with size 0 and no hash (D52), and the
// hash close_file_upload reports is computed from the bytes that arrived, not
// echoed from the request.
//
// It is for tests. It checks no password, and every session id is accepted.
package odfake

import (
	"crypto/md5" //nolint:gosec // OpenDrive's file hash is MD5
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RootID is the account root's folder id, as upstream spells it.
const RootID = "0"

type folder struct {
	id, name, parent  string
	created, modified time.Time
	trashed           bool
}

type file struct {
	id, name, folder string
	data             []byte
	hash             string
	modified         time.Time
	trashed          time.Time
	closed           bool
	// staging is the content being uploaded, between open and close.
	staging []byte
}

// Server is the fake. Its Handler serves the API under any prefix ending in
// the endpoint paths, so a client configured with ".../api/v1" works.
type Server struct {
	mu      sync.Mutex
	next    int
	folders map[string]*folder
	files   map[string]*file
	down    bool
	now     func() time.Time
	calls   map[string]int
}

// New returns an empty account.
func New() *Server {
	s := &Server{
		folders: map[string]*folder{},
		files:   map[string]*file{},
		now:     time.Now,
		calls:   map[string]int{},
	}
	s.folders[RootID] = &folder{id: RootID, created: s.now(), modified: s.now()}
	return s
}

// SetDown makes every request fail with a 503, as an outage would.
func (s *Server) SetDown(v bool) {
	s.mu.Lock()
	s.down = v
	s.mu.Unlock()
}

// Calls reports how many requests reached an endpoint whose path contains the
// given fragment.
func (s *Server) Calls(fragment string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.calls {
		if strings.Contains(k, fragment) {
			n += v
		}
	}
	return n
}

// ---------------------------------------------------------------- inspection

// File returns the content stored at a path and whether a closed, untrashed
// file is there.
func (s *Server) File(p string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fileByPath(p)
	if f == nil || !f.closed {
		return nil, false
	}
	return append([]byte(nil), f.data...), true
}

// Trashed reports whether a file at the path is in the trash.
func (s *Server) Trashed(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	parentPath, name := splitPath(p)
	fid := s.folderByPath(parentPath)
	if fid == "" {
		return false
	}
	for _, f := range s.files {
		if f.folder == fid && f.name == name && !f.trashed.IsZero() {
			return true
		}
	}
	return false
}

// PutFile stores a file directly, creating folders, for seeding a test.
func (s *Server) PutFile(p string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parentPath, name := splitPath(p)
	fid := s.mkdirAll(parentPath)
	f := s.fileIn(fid, name)
	if f == nil {
		f = &file{id: s.newID("FILE"), name: name, folder: fid}
		s.files[f.id] = f
	}
	f.data = append([]byte(nil), data...)
	f.hash = md5hex(f.data)
	f.closed = true
	f.modified = s.now()
}

// Mkdir creates a folder path, for seeding a test.
func (s *Server) Mkdir(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirAll(strings.Trim(p, "/"))
}

// ---------------------------------------------------------------- internals

func (s *Server) newID(prefix string) string {
	s.next++
	return fmt.Sprintf("%s%d", prefix, s.next)
}

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // upstream's hash
	return hex.EncodeToString(sum[:])
}

func splitPath(p string) (string, string) {
	p = strings.Trim(p, "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

func (s *Server) child(parent, name string) *folder {
	for _, f := range s.folders {
		if f.parent == parent && f.name == name && !f.trashed && f.id != RootID {
			return f
		}
	}
	return nil
}

// folderByPath resolves a path with no leading slash; "" is the root.
func (s *Server) folderByPath(p string) string {
	p = strings.Trim(p, "/")
	id := RootID
	if p == "" {
		return id
	}
	for _, seg := range strings.Split(p, "/") {
		c := s.child(id, seg)
		if c == nil {
			return ""
		}
		id = c.id
	}
	return id
}

func (s *Server) mkdirAll(p string) string {
	id := RootID
	if p == "" {
		return id
	}
	for _, seg := range strings.Split(p, "/") {
		c := s.child(id, seg)
		if c == nil {
			c = &folder{id: s.newID("FOLDER"), name: seg, parent: id, created: s.now(), modified: s.now()}
			s.folders[c.id] = c
		}
		id = c.id
	}
	return id
}

func (s *Server) fileIn(folderID, name string) *file {
	for _, f := range s.files {
		if f.folder == folderID && f.name == name && f.trashed.IsZero() {
			return f
		}
	}
	return nil
}

func (s *Server) fileByPath(p string) *file {
	parentPath, name := splitPath(p)
	fid := s.folderByPath(parentPath)
	if fid == "" {
		return nil
	}
	return s.fileIn(fid, name)
}

func (s *Server) trashFolder(id string) {
	f := s.folders[id]
	if f == nil || id == RootID {
		return
	}
	f.trashed = true
	for _, sub := range s.folders {
		if sub.parent == id && !sub.trashed {
			s.trashFolder(sub.id)
		}
	}
	for _, fl := range s.files {
		if fl.folder == id && fl.trashed.IsZero() {
			fl.trashed = s.now()
		}
	}
}

// ---------------------------------------------------------------- HTTP

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func num(v int64) string { return strconv.FormatInt(v, 10) }

func (s *Server) fileJSON(f *file) map[string]any {
	size := int64(len(f.data))
	hash := f.hash
	if !f.closed {
		size, hash = 0, "" // D52
	}
	out := map[string]any{
		"FileId": f.id, "Name": f.name, "Size": num(size), "FileHash": hash,
		"DateModified": f.modified.Unix(), "Access": "0", "DateTrashed": 0,
	}
	if !f.trashed.IsZero() {
		out["DateTrashed"] = f.trashed.Unix()
	}
	return out
}

func lastSegment(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	return segs[len(segs)-1]
}

// Handler serves the fake API.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.mu.Lock()
	s.calls[path]++
	down := s.down
	s.mu.Unlock()
	if down {
		apiError(w, http.StatusServiceUnavailable, "Service Unavailable")
		return
	}

	// JSON bodies, except the chunk upload, which is multipart.
	body := map[string]any{}
	if !strings.Contains(path, "upload_file_chunk") && r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	str := func(k string) string {
		switch v := body[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case nil:
			return ""
		default:
			return fmt.Sprint(v)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.Contains(path, "oauth2/grant"):
		writeJSON(w, 200, map[string]any{"access_token": "fake-access", "refresh_token": "fake-refresh",
			"expires_in": 86400, "token_type": "Bearer"})
	case strings.Contains(path, "session/login"):
		writeJSON(w, 200, map[string]any{"SessionID": "fake-session", "UserID": "1", "AccType": "1",
			"UserName": "fake@example.com", "FVersioning": "0"})
	case strings.Contains(path, "session/logout"):
		writeJSON(w, 200, map[string]any{"result": true})
	case strings.Contains(path, "users/info"):
		writeJSON(w, 200, map[string]any{"UserID": 1, "AccType": "1", "UserName": "fake@example.com",
			"StorageUsed": "0", "MaxStorage": "1024", "BwUsed": "0", "BwMax": "10240", "AccessUserID": 1})

	case strings.Contains(path, "folder/idbypath"):
		id := s.folderByPath(str("path"))
		if id == "" {
			apiError(w, 404, "Folder not found")
			return
		}
		writeJSON(w, 200, map[string]any{"FolderId": id})

	case strings.Contains(path, "file/idbypath"):
		f := s.fileByPath(str("path"))
		if f == nil {
			apiError(w, 404, "File not found")
			return
		}
		writeJSON(w, 200, map[string]any{"FileId": f.id})

	case strings.Contains(path, "folder/list"):
		s.list(w, r, lastSegment(path))

	case strings.HasSuffix(path, "/folder.json") && r.Method == http.MethodPost:
		parent := str("folder_sub_parent")
		if parent == "" {
			parent = RootID
		}
		p := s.folders[parent]
		if p == nil || p.trashed {
			apiError(w, 404, "Parent folder not found")
			return
		}
		name := strings.TrimSpace(str("folder_name")) // D46
		if name == "" {
			apiError(w, 400, "Folder name is required")
			return
		}
		if s.child(parent, name) != nil {
			apiError(w, 409, "Folder already exists")
			return
		}
		f := &folder{id: s.newID("FOLDER"), name: name, parent: parent, created: s.now(), modified: s.now()}
		s.folders[f.id] = f
		writeJSON(w, 200, map[string]any{"FolderID": f.id, "Name": f.name, "DateCreated": f.created.Unix()})

	case strings.Contains(path, "folder/trash.json") && r.Method == http.MethodPost:
		for _, id := range strings.Split(str("folder_id"), ",") {
			s.trashFolder(strings.TrimSpace(id))
		}
		writeJSON(w, 200, map[string]any{"result": true})

	case strings.Contains(path, "folder/remove.json"):
		for _, id := range strings.Split(str("folder_id"), ",") {
			if id = strings.TrimSpace(id); id != RootID {
				delete(s.folders, id)
			}
		}
		writeJSON(w, 200, map[string]any{"result": true})

	case strings.Contains(path, "file/trash.json"):
		for _, id := range strings.Split(str("file_id"), ",") {
			if f := s.files[strings.TrimSpace(id)]; f != nil {
				f.trashed = s.now()
			}
		}
		writeJSON(w, 200, map[string]any{"result": true})

	case strings.Contains(path, "file/remove.json"):
		for _, id := range strings.Split(str("file_id"), ",") {
			delete(s.files, strings.TrimSpace(id))
		}
		writeJSON(w, 200, map[string]any{"result": true})

	case strings.Contains(path, "file/info.json"):
		f := s.files[lastSegment(path)]
		if f == nil {
			apiError(w, 404, "File not found")
			return
		}
		writeJSON(w, 200, s.fileJSON(f))

	case strings.Contains(path, "upload/create_file"):
		fid := str("folder_id")
		if fid == "" {
			fid = RootID
		}
		if p := s.folders[fid]; p == nil || p.trashed {
			apiError(w, 404, "Folder not found")
			return
		}
		name := strings.TrimSpace(str("file_name"))
		f := s.fileIn(fid, name)
		if f != nil && str("open_if_exists") != "1" {
			apiError(w, 409, "File already exists")
			return
		}
		if f == nil {
			f = &file{id: s.newID("FILE"), name: name, folder: fid, modified: s.now()}
			s.files[f.id] = f
		}
		f.staging = nil
		writeJSON(w, 200, map[string]any{"FileId": f.id, "Name": f.name, "TempLocation": "tmp/" + f.id,
			"RequireCompression": false, "RequireHash": false, "RequireHashOnly": false,
			"DirUpdateTime": s.now().Unix()})

	case strings.Contains(path, "upload/open_file_upload"):
		f := s.files[str("file_id")]
		if f == nil {
			apiError(w, 404, "File not found")
			return
		}
		f.staging = []byte{}
		writeJSON(w, 200, map[string]any{"TempLocation": "tmp/" + f.id, "RequireCompression": false,
			"RequireHash": false, "RequireHashOnly": false, "SpeedLimit": "0"})

	case strings.Contains(path, "upload_file_chunk"):
		s.chunk(w, r, lastSegment(path))

	case strings.Contains(path, "upload/close_file_upload"):
		f := s.files[str("file_id")]
		if f == nil {
			apiError(w, 404, "File not found")
			return
		}
		want, _ := strconv.ParseInt(str("file_size"), 10, 64)
		if int64(len(f.staging)) != want {
			apiError(w, 400, fmt.Sprintf("Invalid upload file size. Total uploaded=%d", len(f.staging)))
			return
		}
		f.data, f.staging = f.staging, nil
		f.hash = md5hex(f.data)
		f.closed = true
		f.modified = s.now()
		if t := str("file_time"); t != "" {
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				f.modified = time.Unix(n, 0)
			}
		}
		writeJSON(w, 200, s.fileJSON(f))

	case strings.Contains(path, "upload/checkfileexists"), strings.Contains(path, "upload/has_ddref"):
		writeJSON(w, 200, map[string]any{"exists": false})

	case strings.Contains(path, "download/file.json"):
		s.download(w, r, lastSegment(path))

	default:
		apiError(w, 501, "odfake does not implement "+path)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, id string) {
	f := s.folders[id]
	if f == nil || f.trashed {
		apiError(w, 404, "Folder not found")
		return
	}
	type item struct {
		name string
		v    map[string]any
		dir  bool
	}
	var items []item
	onlyFolders := r.URL.Query().Get("only_subfolders") == "true"
	for _, sub := range s.folders {
		if sub.parent != id || sub.trashed || sub.id == RootID {
			continue
		}
		children := 0
		for _, c := range s.folders {
			if c.parent == sub.id && !c.trashed {
				children++
			}
		}
		items = append(items, item{name: sub.name, dir: true, v: map[string]any{
			"FolderID": sub.id, "Name": sub.name, "DateCreated": sub.created.Unix(),
			"DateModified": sub.modified.Unix(), "ChildFolders": num(int64(children)),
			"Size": "0", "Access": "0",
		}})
	}
	if !onlyFolders {
		for _, fl := range s.files {
			if fl.folder == id && fl.trashed.IsZero() {
				items = append(items, item{name: fl.name, v: s.fileJSON(fl)})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].dir != items[j].dir {
			return items[i].dir
		}
		return items[i].name < items[j].name
	})
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + 100
	if end > len(items) {
		end = len(items)
	}
	folders, files := []any{}, []any{}
	for _, it := range items[offset:end] {
		if it.dir {
			folders = append(folders, it.v)
		} else {
			files = append(files, it.v)
		}
	}
	writeJSON(w, 200, map[string]any{
		"DirUpdateTime": f.modified.Unix(), "Name": f.name,
		"TotalFolders": num(int64(len(folders))), "TotalFiles": num(int64(len(files))),
		"Folders": folders, "Files": files,
	})
}

func (s *Server) chunk(w http.ResponseWriter, r *http.Request, id string) {
	f := s.files[id]
	if f == nil {
		apiError(w, 404, "File not found")
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.ParseInt(q.Get("chunk_offset"), 10, 64)
	size, _ := strconv.ParseInt(q.Get("chunk_size"), 10, 64)
	if offset != int64(len(f.staging)) {
		apiError(w, 400, fmt.Sprintf("Incorrect chunk offset: uploaded=%d, chunk_offset=%d", len(f.staging), offset))
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		apiError(w, 400, "Expected a multipart body")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	var data []byte
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "file_data" {
			data, _ = io.ReadAll(part)
		}
	}
	if int64(len(data)) != size {
		apiError(w, 400, fmt.Sprintf("Chunk size mismatch: declared=%d, received=%d", size, len(data)))
		return
	}
	f.staging = append(f.staging, data...)
	writeJSON(w, 200, map[string]any{"TotalWritten": num(size)}) // D35: this chunk, not the total
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("test") == "1" {
		writeJSON(w, 200, map[string]any{"result": true, "dl_stream_status": true, "BWExceeded": false})
		return
	}
	f := s.files[id]
	if f == nil || !f.closed || !f.trashed.IsZero() {
		apiError(w, 404, "File not found")
		return
	}
	data := f.data
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, f.name))
	w.Header().Set("Content-Type", "application/octet-stream")
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") && strings.HasSuffix(rng, "-") {
		start, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"), 10, 64)
		if err == nil && start > 0 && start < int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-int(start)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start:])
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
