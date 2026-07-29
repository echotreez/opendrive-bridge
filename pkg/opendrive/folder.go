package opendrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FolderService binds the upstream folder module (PDF §5, live spec
// testdata/spec/folder.json). Every method notes where its parameters come
// from, because the PDF and the wire disagree in several places
// (docs/discrepancies.md D12, D15).
//
// Identifiers are opaque strings upstream — a real one looks like
// "MzNfNDg3Njg1N19WM3pvSg" — so they are never treated as numbers, and the
// account root is the string "0" (§2.6 #9).
type FolderService struct {
	c *Client
}

// Folders returns the folder module bound to this client.
func (c *Client) Folders() *FolderService { return &FolderService{c: c} }

// ---------------------------------------------------------------- models

// FolderEntry is a folder as it appears inside a listing. The mixed types are
// upstream's: Shared arrives as the string "False", Encrypted as "0", while the
// enclosing listing reports Encrypted as the integer 0 (§2.6 #5).
type FolderEntry struct {
	FolderID      FlexString `json:"FolderID"`
	Name          string     `json:"Name"`
	DateCreated   UnixTime   `json:"DateCreated"`
	DirUpdateTime UnixTime   `json:"DirUpdateTime"`
	Access        FlexInt    `json:"Access"`
	DateModified  UnixTime   `json:"DateModified"`
	Shared        FlexBool   `json:"Shared"`
	ChildFolders  FlexInt    `json:"ChildFolders"`
	Size          FlexInt    `json:"Size"`
	Link          string     `json:"Link"`
	Encrypted     FlexBool   `json:"Encrypted"`
	Description   string     `json:"Description"`
}

// FileEntry is a file as it appears inside a folder listing. The file module of
// phase P2 reuses it.
type FileEntry struct {
	FileID            FlexString `json:"FileId"` // upstream spells it FileId here
	Name              string     `json:"Name"`
	GroupID           FlexInt    `json:"GroupID"`
	Extension         string     `json:"Extension"`
	Size              FlexInt    `json:"Size"`
	Views             FlexInt    `json:"Views"`
	Version           FlexString `json:"Version"`
	Downloads         FlexInt    `json:"Downloads"`
	DateModified      UnixTime   `json:"DateModified"`
	Access            FlexInt    `json:"Access"`
	Link              string     `json:"Link"`
	DownloadLink      string     `json:"DownloadLink"`
	StreamingLink     string     `json:"StreamingLink"`
	TempStreamingLink string     `json:"TempStreamingLink"`
	DirUpdateTime     UnixTime   `json:"DirUpdateTime"`
	Encrypted         FlexBool   `json:"Encrypted"`
	EditOnline        FlexBool   `json:"EditOnline"`
	FileHash          string     `json:"FileHash"`
	Description       string     `json:"Description"`
	ThumbLink         string     `json:"ThumbLink"`
	MimeType          string     `json:"MimeType"`
	// BWExceeded marks a file whose download bandwidth allowance is spent;
	// downloading it must fail fast rather than retry (§2.5).
	BWExceeded FlexBool `json:"BWExceeded"`
}

// Breadcrumb is one step of a folder path.
type Breadcrumb struct {
	FolderID   FlexString   `json:"FolderID"`
	Name       string       `json:"Name"`
	SubFolders []Breadcrumb `json:"SubFolders"`
}

// FolderListing is the response of folder/list.json.
//
// DirUpdateTime is the paging and cache key at once: it must be echoed as
// last_request_time on every page after the first (§2.6 #14), and it is what
// invalidates the path cache (§10.3).
type FolderListing struct {
	DirUpdateTime    UnixTime      `json:"DirUpdateTime"`
	Name             string        `json:"Name"`
	Encrypted        FlexBool      `json:"Encrypted"`
	TotalFiles       FlexInt       `json:"TotalFiles"`
	TotalFolders     FlexInt       `json:"TotalFolders"`
	Size             FlexInt       `json:"Size"`
	UserAccessMode   FlexInt       `json:"UserAccessMode"`
	ParentFolderID   FlexString    `json:"ParentFolderID"`
	DirectFolderLink string        `json:"DirectFolderLink"`
	ResponseType     FlexInt       `json:"ResponseType"`
	Breadcrumbs      []Breadcrumb  `json:"Breadcrumbs"`
	Folders          []FolderEntry `json:"Folders"`
	Files            []FileEntry   `json:"Files"`
}

// Len returns how many entries the page carried, folders and files together.
func (l *FolderListing) Len() int {
	if l == nil {
		return 0
	}
	return len(l.Folders) + len(l.Files)
}

// FolderInfo is the response of folder/info.json and folder/sharedinfo.json.
// Note OwnerSuspended: here upstream spells it correctly, while the session
// login response says OwnerSuspendet (§2.6 #3).
type FolderInfo struct {
	FolderID          FlexString `json:"FolderID"`
	Name              string     `json:"Name"`
	DateCreated       UnixTime   `json:"DateCreated"`
	DateTrashed       UnixTime   `json:"DateTrashed"`
	DirUpdateTime     UnixTime   `json:"DirUpdateTime"`
	Access            FlexInt    `json:"Access"`
	PublicUpload      FlexBool   `json:"PublicUpload"`
	PublicContent     FlexBool   `json:"PublicContent"`
	PublicDownload    FlexBool   `json:"PublicDownload"`
	DateModified      UnixTime   `json:"DateModified"`
	Owner             FlexString `json:"Owner"`
	DisplaySubfolders FlexBool   `json:"DisplaySubfolders"`
	Shared            FlexBool   `json:"Shared"`
	ChildFolders      FlexInt    `json:"ChildFolders"`
	Size              FlexInt    `json:"Size"`
	OwnerLevel        FlexInt    `json:"OwnerLevel"`
	OwnerSuspended    FlexBool   `json:"OwnerSuspended"`
	ID                FlexString `json:"ID"`
	Description       string     `json:"Description"`
	Permission        FlexInt    `json:"Permission"`
	Link              string     `json:"Link"`
	Lang              string     `json:"Lang"`
	CompanyName       string     `json:"CompanyName"`
	Encrypted         FlexBool   `json:"Encrypted"`
}

// ItemByNameResult is the response of folder/itembyname.json. Upstream reports
// "nothing matched" by omitting the arrays entirely rather than by returning an
// error, so the SDK turns that into ErrNotFound (docs/discrepancies.md D23).
type ItemByNameResult struct {
	DirUpdateTime UnixTime      `json:"DirUpdateTime"`
	Folders       []FolderEntry `json:"Folders"`
	Files         []FileEntry   `json:"Files"`
}

// CreatedFolder is the response of POST /folder.json.
type CreatedFolder struct {
	FolderID      FlexString `json:"FolderID"`
	Name          string     `json:"Name"`
	DateCreated   UnixTime   `json:"DateCreated"`
	DirUpdateTime UnixTime   `json:"DirUpdateTime"`
	Access        FlexInt    `json:"Access"`
	DateModified  UnixTime   `json:"DateModified"`
	Shared        FlexBool   `json:"Shared"`
	ChildFolders  FlexInt    `json:"ChildFolders"`
	Link          string     `json:"Link"`
	Encrypted     FlexBool   `json:"Encrypted"`
}

// TrashListing is the response of folder/trashlist.json.
type TrashListing struct {
	DirUpdateTime UnixTime      `json:"DirUpdateTime"`
	Folders       []FolderEntry `json:"Folders"`
	Files         []FileEntry   `json:"Files"`
	// Count is populated when the call asked for count_only.
	Count FlexInt `json:"Count"`
}

// ExpiringLink is the response of the expiring-link endpoints of both the
// folder and the file module.
//
// The shape was recorded from the sandbox, not taken from the spec, which
// declares the response class as void (docs/discrepancies.md D31). A folder
// answers with Link; a file answers with DownloadLink and StreamingLink and no
// Link at all. Use URL to get whichever one is present.
//
// ExpiringDate is a calendar date string such as "2026-12-31", not a Unix
// timestamp, and Counter/CounterMax arrive quoted.
type ExpiringLink struct {
	Link          string   `json:"Link"`
	DownloadLink  string   `json:"DownloadLink"`
	StreamingLink string   `json:"StreamingLink"`
	ExpiringDate  string   `json:"ExpiringDate"`
	Counter       FlexInt  `json:"Counter"`
	CounterMax    FlexInt  `json:"CounterMax"`
	CounterEnable FlexBool `json:"CounterEnable"`
}

// URL returns the shareable link, whichever field upstream used.
func (l ExpiringLink) URL() string {
	if l.Link != "" {
		return l.Link
	}
	return l.DownloadLink
}

// FolderAccess mirrors upstream's folder_is_public tri-state (§2.3).
type FolderAccess int

// Folder visibility values.
const (
	FolderPrivate FolderAccess = 0
	FolderPublic  FolderAccess = 1
	FolderHidden  FolderAccess = 2
)

// ---------------------------------------------------------------- listing

// ListOptions carries the optional parameters of folder/list.json. The live
// spec documents three the PDF does not: EncryptionSupported, OrderBy and
// OrderType (docs/discrepancies.md D17).
type ListOptions struct {
	// Page controls offset paging. The zero value asks for the first page.
	Page Pagination
	// SearchQuery filters entries by name.
	SearchQuery string
	// SharingID scopes the call to a share.
	SharingID string
	// OnlySubfolders drops the Files array from the response.
	OnlySubfolders bool
	// WithBreadcrumbs asks upstream to include the breadcrumb trail.
	WithBreadcrumbs bool
	// EncryptionSupported tells upstream this client understands encrypted
	// folders and files.
	EncryptionSupported bool
	// OrderBy and OrderType sort the listing server side.
	OrderBy   string
	OrderType string
}

func (o ListOptions) query() (url.Values, error) {
	if err := o.Page.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{}
	// Paging is only meaningful once an offset is in play; sending offset=0
	// with last_request_time=0 is what upstream expects for the first page
	// (§2.6 #14).
	if o.Page.Offset > 0 || o.Page.LastRequestTime > 0 {
		q.Set("offset", strconv.Itoa(o.Page.Offset))
		q.Set("last_request_time", strconv.FormatInt(o.Page.LastRequestTime, 10))
	}
	if o.SearchQuery != "" {
		q.Set("search_query", o.SearchQuery)
	}
	if o.SharingID != "" {
		q.Set("sharing_id", o.SharingID)
	}
	if o.OnlySubfolders {
		q.Set("only_subfolders", "true")
	}
	if o.WithBreadcrumbs {
		q.Set("with_breadcrumbs", "true")
	}
	if o.EncryptionSupported {
		q.Set("encryption_supported", "1")
	}
	if o.OrderBy != "" {
		q.Set("order_by", o.OrderBy)
	}
	if o.OrderType != "" {
		q.Set("order_type", o.OrderType)
	}
	return q, nil
}

// List returns one page of a folder's contents.
//
// GET /folder/list.json/{session_id}/{folder_id} (PDF §5.2). Session and folder
// id are path segments; everything else is a query parameter.
//
// Upstream caps a page at MaxListPageSize entries and has no limit parameter,
// so paging is offset plus last_request_time. Use Pages for the whole folder.
func (s *FolderService) List(ctx context.Context, folderID string, opts ListOptions) (*FolderListing, error) {
	if folderID == "" {
		folderID = RootFolderID
	}
	q, err := opts.query()
	if err != nil {
		return nil, err
	}
	var out FolderListing
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderList,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FolderPager walks every page of a folder, carrying the last_request_time
// protocol so that a large directory is not silently truncated (§2.6 #14).
//
//	p := c.Folders().Pages(folderID, opendrive.ListOptions{})
//	for {
//	    page, err := p.Next(ctx)
//	    if err != nil { return err }
//	    if page == nil { break }
//	    ...
//	}
type FolderPager struct {
	s        *FolderService
	folderID string
	opts     ListOptions
	done     bool
	started  bool
}

// Pages returns a pager over the folder's contents.
func (s *FolderService) Pages(folderID string, opts ListOptions) *FolderPager {
	if folderID == "" {
		folderID = RootFolderID
	}
	opts.Page = FirstPage(opts.Page.Limit)
	return &FolderPager{s: s, folderID: folderID, opts: opts}
}

// Next returns the next page, or nil when the folder is exhausted.
func (p *FolderPager) Next(ctx context.Context) (*FolderListing, error) {
	if p.done {
		return nil, nil
	}
	page, err := p.s.List(ctx, p.folderID, p.opts)
	if err != nil {
		return nil, err
	}
	p.started = true

	// A short page is the last one: upstream gives no total to compare against
	// for a filtered listing, so the page size is the only signal.
	if page.Len() < p.opts.Page.EffectiveLimit() {
		p.done = true
	} else {
		p.opts.Page = p.opts.Page.Next(page.DirUpdateTime.Unix())
	}
	return page, nil
}

// Done reports whether the pager has walked the whole folder.
func (p *FolderPager) Done() bool { return p.done }

// ---------------------------------------------------------------- lookups

// Info returns a folder's metadata.
//
// GET /folder/info.json/{session_id}/{folder_id} (PDF §5.3).
func (s *FolderService) Info(ctx context.Context, folderID string, sharingID ...string) (*FolderInfo, error) {
	if folderID == "" {
		folderID = RootFolderID
	}
	var out FolderInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderInfo,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
		Query:            optionalQuery("sharing_id", sharingID),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// idByPathResponse is the reply of folder/idbypath.json. Upstream spells the
// field FolderId here and FolderID everywhere else (docs/discrepancies.md D22),
// so both spellings are accepted.
type idByPathResponse struct {
	FolderIDLower FlexString `json:"FolderId"`
	FolderIDUpper FlexString `json:"FolderID"`
}

func (r idByPathResponse) id() string {
	if r.FolderIDLower != "" {
		return r.FolderIDLower.String()
	}
	return r.FolderIDUpper.String()
}

// IDByPath resolves a folder path to its identifier.
//
// POST /folder/idbypath.json (PDF §5.5), session in the body. A leading slash
// is optional upstream; the SDK normalises the path first so that "/A/B",
// "A/B" and "/A/B/" all hit the same cache entry (§10.3).
func (s *FolderService) IDByPath(ctx context.Context, path string) (string, error) {
	clean, err := NormalizeFolderPath(path)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return RootFolderID, nil
	}
	var out idByPathResponse
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderIDByPath,
		SessionPlacement: SessionInBody,
		Body:             map[string]string{"path": strings.TrimPrefix(clean, "/")},
	}, &out); err != nil {
		return "", err
	}
	id := out.id()
	if id == "" {
		return "", &APIError{Kind: KindInvalidResponse, Op: "POST " + EndpointFolderIDByPath,
			UpstreamMsg: "the response carried no folder id"}
	}
	return id, nil
}

// ItemByName finds a direct child of a folder by name.
//
// GET /folder/itembyname.json/{session_id}/{folder_id} (PDF §5.6). Upstream
// answers a miss with a body that simply omits the arrays, so this returns an
// error matching ErrNotFound instead (docs/discrepancies.md D23).
func (s *FolderService) ItemByName(ctx context.Context, parentID, name string, opts ...ItemByNameOption) (*ItemByNameResult, error) {
	if parentID == "" {
		parentID = RootFolderID
	}
	if name == "" {
		return nil, invalidRequest("itembyname needs a name")
	}
	q := url.Values{"name": []string{name}}
	for _, o := range opts {
		o(q)
	}
	var out ItemByNameResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderItemByName,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{parentID},
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Folders) == 0 && len(out.Files) == 0 {
		return nil, &APIError{Kind: KindNotFound, Op: "GET " + EndpointFolderItemByName,
			UpstreamMsg: "no item named " + strconv.Quote(name) + " in this folder"}
	}
	return &out, nil
}

// ItemByNameOption customises an itembyname lookup.
type ItemByNameOption func(url.Values)

// WithSharingID scopes a lookup to a share.
func WithSharingID(id string) ItemByNameOption {
	return func(q url.Values) {
		if id != "" {
			q.Set("sharing_id", id)
		}
	}
}

// WithEncryptionSupported announces support for encrypted items.
func WithEncryptionSupported() ItemByNameOption {
	return func(q url.Values) { q.Set("encryption_supported", "1") }
}

// Breadcrumb returns the trail from the root down to a folder.
//
// GET /folder/breadcrumb.json/{session_id}/{folder_id}. The live API spells
// this correctly; the PDF's "breadcrump" is a 404 (docs/discrepancies.md D12).
// The response is a bare JSON array, not an object.
func (s *FolderService) Breadcrumb(ctx context.Context, folderID string, withSubfolders bool) ([]Breadcrumb, error) {
	if folderID == "" {
		folderID = RootFolderID
	}
	q := url.Values{}
	if withSubfolders {
		q.Set("with_subfolders", "true")
	}
	var out []Breadcrumb
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderBreadcrumb,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Path returns a folder's path relative to the account root, without a leading
// slash.
//
// GET /folder/path.json/{session_id}/{folder_id}.
func (s *FolderService) Path(ctx context.Context, folderID string) (string, error) {
	var out struct {
		Path string `json:"Path"`
	}
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderPath,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
	}, &out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// FullPath returns a folder's full path.
//
// GET /folder/folderfullpath.json/{session_id}/{folder_id}. The field is called
// FullPath here and Path in folder/path.json.
func (s *FolderService) FullPath(ctx context.Context, folderID string) (string, error) {
	var out struct {
		FullPath string `json:"FullPath"`
	}
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderFullPath,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
	}, &out); err != nil {
		return "", err
	}
	return out.FullPath, nil
}

// UserAccessMode returns the caller's access level for a folder. Upstream
// answers with a string here and an integer inside a listing (§2.6 #5).
//
// GET /folder/useraccessmode.json/{session_id}/{folder_id}.
func (s *FolderService) UserAccessMode(ctx context.Context, folderID string, sharingID ...string) (int, error) {
	if folderID == "" {
		folderID = RootFolderID
	}
	var out struct {
		UserAccessMode FlexInt `json:"UserAccessMode"`
	}
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderUserAccess,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
		Query:            optionalQuery("sharing_id", sharingID),
	}, &out); err != nil {
		return 0, err
	}
	return out.UserAccessMode.Int(), nil
}

// SharedInfo returns the public metadata of a shared folder. The endpoint needs
// no session at all.
//
// GET /folder/sharedinfo.json/{folder_id}.
func (s *FolderService) SharedInfo(ctx context.Context, folderID string) (*FolderInfo, error) {
	var out FolderInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderSharedInfo,
		SessionPlacement: SessionOmit,
		PathSegments:     []string{folderID},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Shared lists the contents of a shared folder without a session.
//
// GET /folder/shared.json/{folder_id}. It takes the same paging parameters as
// folder/list.json (docs/discrepancies.md D7).
func (s *FolderService) Shared(ctx context.Context, folderID string, opts ListOptions) (*FolderListing, error) {
	q, err := opts.query()
	if err != nil {
		return nil, err
	}
	// The shared listing has no search or sharing scope of its own.
	q.Del("search_query")
	q.Del("sharing_id")
	q.Del("only_subfolders")
	q.Del("encryption_supported")
	var out FolderListing
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderShared,
		SessionPlacement: SessionOmit,
		PathSegments:     []string{folderID},
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------- mutations

// CreateFolderParams describes a new folder (POST /folder.json, PDF §5.1).
type CreateFolderParams struct {
	// Name is validated locally first, so the caller gets a clearer error than
	// upstream's (§2.6 #10).
	Name string
	// ParentID is the containing folder; empty means the account root.
	ParentID string
	// Access is the visibility tri-state.
	Access FolderAccess
	// PublicUpload, PublicDisplay and PublicDownload configure a public folder.
	PublicUpload      bool
	PublicDisplay     bool
	PublicDownload    bool
	DisplaySubfolders bool
	Description       string
	SharingID         string
}

// Create makes a folder.
func (s *FolderService) Create(ctx context.Context, p CreateFolderParams) (*CreatedFolder, error) {
	if err := ValidateName(p.Name); err != nil {
		return nil, err
	}
	parent := p.ParentID
	if parent == "" {
		parent = RootFolderID
	}
	body := map[string]any{
		"folder_name":               p.Name,
		"folder_sub_parent":         parent,
		"folder_is_public":          int(p.Access),
		"folder_public_upl":         boolToInt(p.PublicUpload),
		"folder_public_display":     boolToInt(p.PublicDisplay),
		"folder_public_dnl":         boolToInt(p.PublicDownload),
		"folder_display_subfolders": boolToInt(p.DisplaySubfolders),
	}
	if p.Description != "" {
		body["folder_description"] = p.Description
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}
	var out CreatedFolder
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolder,
		SessionPlacement: SessionInBody,
		Body:             body,
		// Write rights are per folder, so the success witness has to be too
		// (docs/error-taxonomy.md T2).
		Scope: parent,
	}, &out); err != nil {
		return nil, err
	}
	// The parent gained a child, so anything cached below it is out of date.
	s.invalidate(parent)
	return &out, nil
}

// Rename changes a folder's name.
//
// POST /folder/rename.json (PDF §5.7).
func (s *FolderService) Rename(ctx context.Context, folderID, newName string, sharingID ...string) (*FolderEntry, error) {
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	body := map[string]string{"folder_id": folderID, "folder_name": newName}
	if len(sharingID) > 0 && sharingID[0] != "" {
		body["sharing_id"] = sharingID[0]
	}
	var out FolderEntry
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderRename,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	// The old path no longer points anywhere, and the parent listing changed.
	s.invalidate(folderID)
	return &out, nil
}

// MoveCopyParams describes a folder move or copy (POST /folder/move_copy.json).
//
// The archived spec documents move and copy_recursive as real JSON booleans,
// but the live endpoint rejects the boolean false with "Invalid value specified
// for `move`. Expecting boolean value" while accepting the strings "true" and
// "false" — so a copy is impossible in the documented encoding. Both flags are
// therefore sent as StringBool, matching file/move_copy.json after all
// (§2.6 #13, docs/discrepancies.md D24, D26).
type MoveCopyParams struct {
	FolderID    string
	DstFolderID string
	// Move selects move (true) or copy (false).
	Move bool
	// CopyRecursive copies subfolders too; without it only the folder and its
	// files are copied.
	CopyRecursive bool
	NewName       string
	SrcSharingID  string
	DstSharingID  string
}

// MoveCopy moves or copies a folder.
func (s *FolderService) MoveCopy(ctx context.Context, p MoveCopyParams) (*FolderEntry, error) {
	if p.FolderID == "" || p.DstFolderID == "" {
		return nil, invalidRequest("move_copy needs both a source and a destination folder id")
	}
	if p.NewName != "" {
		if err := ValidateName(p.NewName); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"folder_id":      p.FolderID,
		"dst_folder_id":  p.DstFolderID,
		"move":           StringBool(p.Move),
		"copy_recursive": StringBool(p.CopyRecursive),
	}
	if p.NewName != "" {
		body["new_folder_name"] = p.NewName
	}
	if p.SrcSharingID != "" {
		body["src_sharing_id"] = p.SrcSharingID
	}
	if p.DstSharingID != "" {
		body["dst_sharing_id"] = p.DstSharingID
	}
	var out FolderEntry
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderMoveCopy,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	s.invalidate(p.FolderID, p.DstFolderID)
	return &out, nil
}

// Trash moves folders to the trash. Upstream takes a comma separated list.
//
// POST /folder/trash.json (PDF §5.9). The same resource with DELETE empties the
// trash, which is why the verb is never inferred from the path (§2.6 #4).
func (s *FolderService) Trash(ctx context.Context, folderIDs []string, sharingID ...string) error {
	ids, err := joinIDs(folderIDs)
	if err != nil {
		return err
	}
	body := map[string]string{"folder_id": ids}
	if len(sharingID) > 0 && sharingID[0] != "" {
		body["sharing_id"] = sharingID[0]
	}
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderTrash,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return err
	}
	s.invalidate(folderIDs...)
	return nil
}

// EmptyTrash discards everything in the trash.
//
// DELETE /folder/trash.json/{session_id} — the session is a path segment for
// this verb and a body field for POST on the very same resource.
func (s *FolderService) EmptyTrash(ctx context.Context) error {
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodDelete,
		Path:             EndpointFolderTrash,
		SessionPlacement: SessionInPath,
	}, &out); err != nil {
		return err
	}
	if cache := s.c.pathCache; cache != nil {
		cache.Reset()
	}
	return nil
}

// TrashListOptions carries the optional parameters of folder/trashlist.json.
type TrashListOptions struct {
	Page        Pagination
	SearchQuery string
	// CountOnly asks upstream for the item count instead of the items.
	CountOnly bool
	OrderBy   string
	OrderType string
}

// TrashList lists the trash.
//
// GET /folder/trashlist.json/{session_id} (PDF §5.10). It pages like
// folder/list.json and additionally understands count_only (D17).
func (s *FolderService) TrashList(ctx context.Context, opts TrashListOptions) (*TrashListing, error) {
	if err := opts.Page.Validate(); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Page.Offset > 0 || opts.Page.LastRequestTime > 0 {
		q.Set("offset", strconv.Itoa(opts.Page.Offset))
		q.Set("last_request_time", strconv.FormatInt(opts.Page.LastRequestTime, 10))
	}
	if opts.SearchQuery != "" {
		q.Set("search_query", opts.SearchQuery)
	}
	if opts.CountOnly {
		q.Set("count_only", "1")
	}
	if opts.OrderBy != "" {
		q.Set("order_by", opts.OrderBy)
	}
	if opts.OrderType != "" {
		q.Set("order_type", opts.OrderType)
	}
	var out TrashListing
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderTrashList,
		SessionPlacement: SessionInPath,
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Restore brings folders back from the trash.
//
// POST /folder/restore.json (PDF §5.11).
func (s *FolderService) Restore(ctx context.Context, folderIDs []string) error {
	ids, err := joinIDs(folderIDs)
	if err != nil {
		return err
	}
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderRestore,
		SessionPlacement: SessionInBody,
		Body:             map[string]string{"folder_id": ids},
	}, &out); err != nil {
		return err
	}
	s.invalidate(folderIDs...)
	return nil
}

// Remove deletes folders permanently.
//
// POST /folder/remove.json (PDF §5.12). This is not the trash: the content is
// gone.
func (s *FolderService) Remove(ctx context.Context, folderIDs []string, sharingID ...string) error {
	ids, err := joinIDs(folderIDs)
	if err != nil {
		return err
	}
	body := map[string]string{"folder_id": ids}
	if len(sharingID) > 0 && sharingID[0] != "" {
		body["sharing_id"] = sharingID[0]
	}
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderRemove,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return err
	}
	s.invalidate(folderIDs...)
	return nil
}

// SetAccess changes a folder's visibility.
//
// POST /folder/setaccess.json (PDF §5.13).
func (s *FolderService) SetAccess(ctx context.Context, folderID string, access FolderAccess, withChildFiles bool, sharingID ...string) error {
	body := map[string]any{
		"folder_id":        folderID,
		"folder_is_public": int(access),
		"with_child_files": withChildFiles,
	}
	if len(sharingID) > 0 && sharingID[0] != "" {
		body["sharing_id"] = sharingID[0]
	}
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderSetAccess,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return err
	}
	s.invalidate(folderID)
	return nil
}

// FolderSettings carries the fields of PUT /folder/foldersettings.json. Only
// the non-empty ones are sent, so a caller can change one setting without
// restating the others.
//
// The verb is PUT, not the POST the whitepaper lists (docs/discrepancies.md
// D15).
type FolderSettings struct {
	Name              string
	Description       string
	Access            *FolderAccess
	PublicUpload      *bool
	PublicDisplay     *bool
	PublicDownload    *bool
	DisplaySubfolders *bool
	SharingID         string
}

// UpdateSettings changes a folder's settings.
func (s *FolderService) UpdateSettings(ctx context.Context, folderID string, settings FolderSettings) error {
	if folderID == "" {
		return invalidRequest("foldersettings needs a folder id")
	}
	body := map[string]any{"folder_id": folderID}
	if settings.Name != "" {
		if err := ValidateName(settings.Name); err != nil {
			return err
		}
		body["folder_name"] = settings.Name
	}
	if settings.Description != "" {
		body["folder_description"] = settings.Description
	}
	if settings.Access != nil {
		body["folder_access"] = int(*settings.Access)
	}
	if settings.PublicUpload != nil {
		body["folder_public_upl"] = boolToInt(*settings.PublicUpload)
	}
	if settings.PublicDisplay != nil {
		body["folder_public_display"] = boolToInt(*settings.PublicDisplay)
	}
	if settings.PublicDownload != nil {
		body["folder_public_dnl"] = boolToInt(*settings.PublicDownload)
	}
	if settings.DisplaySubfolders != nil {
		body["folder_display_subfolders"] = boolToInt(*settings.DisplaySubfolders)
	}
	if settings.SharingID != "" {
		body["sharing_id"] = settings.SharingID
	}
	var out BoolResult
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPut,
		Path:             EndpointFolderSettings,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return err
	}
	s.invalidate(folderID)
	return nil
}

// SendByEmailParams describes a folder share by email (POST
// /folder/sendbyemail.json, PDF §5.15).
type SendByEmailParams struct {
	FolderIDs       []string
	Recipients      []string
	Subject         string
	Body            string
	SendExpiring    bool
	CaptchaResponse string
}

// SendByEmail shares folders by email.
func (s *FolderService) SendByEmail(ctx context.Context, p SendByEmailParams) error {
	if len(p.FolderIDs) == 0 {
		return invalidRequest("sendbyemail needs at least one folder id")
	}
	if len(p.Recipients) == 0 {
		return invalidRequest("sendbyemail needs at least one recipient")
	}
	body := map[string]any{
		"folder_id":             p.FolderIDs,
		"recipient_emails":      p.Recipients,
		"send_expiring_enabled": p.SendExpiring,
	}
	if p.Subject != "" {
		body["message_subject"] = p.Subject
	}
	if p.Body != "" {
		body["message_body"] = p.Body
	}
	if p.CaptchaResponse != "" {
		body["captcha_response"] = p.CaptchaResponse
	}
	var out BoolResult
	return s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFolderSendByEmail,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out)
}

// CreateExpiringLink creates a time and use limited share link.
//
// GET /folder/expiringlink.json/{session_id}/{date}/{counter}/{folder_id}/{enable}
// — every argument is a path segment, the session first (§2.6 #12,
// docs/discrepancies.md D18).
func (s *FolderService) CreateExpiringLink(ctx context.Context, folderID, date string, counter int, enable bool) (*ExpiringLink, error) {
	if folderID == "" || date == "" {
		return nil, invalidRequest("an expiring link needs a folder id and a date")
	}
	var out ExpiringLink
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderExpiringLink,
		SessionPlacement: SessionInPath,
		PathSegments: []string{
			date, strconv.Itoa(counter), folderID, strconv.FormatBool(enable),
		},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExpiringLinks returns the expiring link configured on a folder.
//
// GET /folder/folderexpiringlinks.json/{session_id}/{folder_id}. Despite the
// plural name upstream answers with a single object, not an array
// (docs/discrepancies.md D31). A folder with no expiring link answers with an
// empty object, which decodes to a zero ExpiringLink.
func (s *FolderService) ExpiringLinks(ctx context.Context, folderID string) (*ExpiringLink, error) {
	if folderID == "" {
		return nil, invalidRequest("folder expiring links needs a folder id")
	}
	var out ExpiringLink
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderExpiringLinks,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExportCSV returns a CSV listing of a folder.
//
// GET /folder/exportcsv.json/{session_id}/{folder_id}. The response is not
// JSON, so it is returned verbatim.
func (s *FolderService) ExportCSV(ctx context.Context, folderID string, sharingID ...string) (string, error) {
	if folderID == "" {
		folderID = RootFolderID
	}
	var out json.RawMessage
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFolderExportCSV,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
		Query:            optionalQuery("sharing_id", sharingID),
		Accept:           "text/csv, application/json",
	}, &out); err != nil {
		return "", err
	}
	// A CSV body decodes as a JSON string when upstream quotes it, and as raw
	// bytes otherwise.
	var quoted string
	if err := json.Unmarshal(out, &quoted); err == nil {
		return quoted, nil
	}
	return string(out), nil
}

// ---------------------------------------------------------------- helpers

func optionalQuery(key string, values []string) url.Values {
	if len(values) == 0 || values[0] == "" {
		return nil
	}
	return url.Values{key: []string{values[0]}}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// joinIDs renders the comma separated id list several folder endpoints expect.
func joinIDs(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", invalidRequest("at least one folder id is required")
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return "", invalidRequest("a folder id must not be empty")
		}
		if strings.Contains(id, ",") {
			return "", invalidRequest("a folder id must not contain a comma: %q", id)
		}
	}
	return strings.Join(ids, ","), nil
}
