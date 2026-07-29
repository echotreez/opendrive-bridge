package opendrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// FileService binds the upstream file module (PDF §4, live spec
// testdata/spec/file.json). The file API is intentionally not inferred from
// the folder API: its resource root, verbs, session placement and boolean
// encodings all differ (docs/discrepancies.md D14, D15, D24, D26).
type FileService struct {
	c *Client
}

// Files returns the file module bound to this client.
func (c *Client) Files() *FileService { return &FileService{c: c} }

// FileInfo is the metadata returned by file/info.json and most file mutation
// endpoints. FileEntry is the narrower shape emitted inside folder/list.json.
//
// A successful Info call is metadata only, not proof that the file remains
// live: existence decisions must use IDByPath or a parent FolderService.List
// result (docs/discrepancies.md D27).
type FileInfo struct {
	FileID            FlexString `json:"-"`
	FileGroupID       FlexString `json:"FileGroupID"`
	Name              string     `json:"Name"`
	Extension         string     `json:"Extension"`
	Size              FlexInt    `json:"Size"`
	DateCreated       UnixTime   `json:"DateCreated"`
	DateModified      UnixTime   `json:"DateModified"`
	DateTrashed       UnixTime   `json:"DateTrashed"`
	DirUpdateTime     UnixTime   `json:"DirUpdateTime"`
	Access            FlexInt    `json:"Access"`
	Shared            FlexBool   `json:"Shared"`
	Encrypted         FlexBool   `json:"Encrypted"`
	FileHash          string     `json:"FileHash"`
	Description       string     `json:"Description"`
	Link              string     `json:"Link"`
	DownloadLink      string     `json:"DownloadLink"`
	StreamingLink     string     `json:"StreamingLink"`
	TempStreamingLink string     `json:"TempStreamingLink"`
	MimeType          string     `json:"MimeType"`
	Version           FlexString `json:"Version"`
	Views             FlexInt    `json:"Views"`
	Downloads         FlexInt    `json:"Downloads"`
	Owner             FlexString `json:"Owner"`
	OwnerLevel        FlexInt    `json:"OwnerLevel"`
	OwnerSuspended    FlexBool   `json:"OwnerSuspended"`
	PublicDownload    FlexBool   `json:"PublicDownload"`
	PublicContent     FlexBool   `json:"PublicContent"`
	PublicUpload      FlexBool   `json:"PublicUpload"`
	EditOnline        FlexBool   `json:"EditOnline"`
	BWExceeded        FlexBool   `json:"BWExceeded"`

	// The rest were recorded from the sandbox and are absent from the PDF
	// (testdata/fixtures/file/info.json).
	GroupID        FlexString `json:"GroupID"`
	OwnerName      string     `json:"OwnerName"`
	AccessUser     FlexString `json:"AccessUser"`
	AccessDisabled FlexBool   `json:"AccessDisabled"`
	IsArchive      FlexBool   `json:"IsArchive"`
	Password       string     `json:"Password"`
	DestURL        string     `json:"DestURL"`
	EditLink       string     `json:"EditLink"`
	ThumbLink      string     `json:"ThumbLink"`
	DateUploaded   UnixTime   `json:"DateUploaded"`
	DateAccessed   UnixTime   `json:"DateAccessed"`
}

// UnmarshalJSON accepts both FileId and FileID. Upstream uses FileId in
// listings; the more detailed endpoints have changed spelling over time, so
// accepting both prevents a wire spelling change from becoming data loss.
func (f *FileInfo) UnmarshalJSON(data []byte) error {
	type alias FileInfo
	var wire struct {
		FileIDUpper FlexString `json:"FileID"`
		FileIDLower FlexString `json:"FileId"`
		*alias
	}
	wire.alias = (*alias)(f)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.FileIDUpper != "" {
		f.FileID = wire.FileIDUpper
	} else {
		f.FileID = wire.FileIDLower
	}
	return nil
}

// FileVersion is one historical version returned by file/fileversions.json.
type FileVersion struct {
	FileID       FlexString `json:"FileId"`
	FileGroupID  FlexString `json:"FileGroupID"`
	Version      FlexString `json:"Version"`
	Name         string     `json:"Name"`
	Size         FlexInt    `json:"Size"`
	DateCreated  UnixTime   `json:"DateCreated"`
	DateModified UnixTime   `json:"DateModified"`
	FileHash     string     `json:"FileHash"`
}

// FilePasswordVerification is the result of file/verifypassword.json.
//
// Recorded behaviour, which does not match the PDF: upstream answers
// {"result":false} for a correct password and for a wrong one alike, and never
// returns the TempKey the download and thumbnail endpoints accept. The fields
// below are therefore reported as observed, and OK cannot be relied on to mean
// "the password was right" (docs/discrepancies.md D32).
type FilePasswordVerification struct {
	// Result is upstream's verdict, observed to be false in every case.
	Result FlexBool `json:"result"`
	// TempKey is the temporary key documented for password-protected
	// downloads. It has never been observed in a response.
	TempKey string `json:"TempKey"`
	// Valid is the PDF's field name for the verdict, kept in case upstream
	// starts sending it.
	Valid FlexBool `json:"Valid"`
}

// OK reports whether upstream accepted the password. See the type comment: the
// sandbox answers false even for a correct password, so a false result is not
// proof that the password is wrong.
func (v FilePasswordVerification) OK() bool { return v.Result.Bool() || v.Valid.Bool() }

// FileVisibility is the file_ispublic tri-state used by the access and
// settings endpoints: 0 private, 1 public, 2 hidden.
type FileVisibility int

// File visibility values.
const (
	FilePrivate FileVisibility = 0
	FilePublic  FileVisibility = 1
	FileHidden  FileVisibility = 2
)

// ---------------------------------------------------------------- reads

// Info returns a file's metadata.
//
// GET /file/info.json/{file_id}; session_id is a query parameter, not a path
// segment (PDF §4.2, live file.json). Do not use this as an existence check
// (D27); use IDByPath or the parent folder listing instead.
func (s *FileService) Info(ctx context.Context, fileID string, sharingID ...string) (*FileInfo, error) {
	if fileID == "" {
		return nil, invalidRequest("file info needs a file id")
	}
	var out FileInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFileInfo,
		SessionPlacement: SessionInQuery,
		PathSegments:     []string{fileID},
		Query:            optionalQuery("sharing_id", sharingID),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// fileIDByPathResponse accepts the two spellings observed across OpenDrive
// response shapes. It intentionally does not call Info afterwards: an id
// lookup itself is the liveness check (D27).
type fileIDByPathResponse struct {
	FileIDLower FlexString `json:"FileId"`
	FileIDUpper FlexString `json:"FileID"`
}

func (r fileIDByPathResponse) id() string {
	if r.FileIDLower != "" {
		return r.FileIDLower.String()
	}
	return r.FileIDUpper.String()
}

// IDByPath resolves a file path to an upstream file id.
//
// POST /file/idbypath.json; session_id and path are JSON body fields. The
// path is normalised with the same strict namespace rules as folder paths.
func (s *FileService) IDByPath(ctx context.Context, p string) (string, error) {
	clean, err := NormalizeFolderPath(p)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return "", invalidRequest("the account root is a folder, not a file")
	}
	var out fileIDByPathResponse
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFileIDByPath,
		SessionPlacement: SessionInBody,
		Body:             map[string]string{"path": strings.TrimPrefix(clean, "/")},
	}, &out); err != nil {
		return "", err
	}
	if id := out.id(); id != "" {
		return id, nil
	}
	return "", &APIError{Kind: KindInvalidResponse, Op: "POST " + EndpointFileIDByPath,
		UpstreamMsg: "the response carried no file id"}
}

// Path returns a file's path relative to the account root, without a leading
// slash.
//
// GET /file/path.json/{session_id}/{file_id}.
func (s *FileService) Path(ctx context.Context, fileID string) (string, error) {
	if fileID == "" {
		return "", invalidRequest("file path needs a file id")
	}
	var out struct {
		Path string `json:"Path"`
	}
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFilePath,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{fileID},
	}, &out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// FullPath returns a file's full path, using forward slashes.
//
// GET /file/filefullpath.json/{session_id}/{file_id}. Upstream returns the
// path in a field called DownloadLink and separates the segments with
// backslashes — neither the field name nor the separator matches the folder
// module's equivalent (docs/discrepancies.md D30). Both are normalised here;
// FullPath is still accepted in case upstream ever corrects the field.
func (s *FileService) FullPath(ctx context.Context, fileID string) (string, error) {
	if fileID == "" {
		return "", invalidRequest("file full path needs a file id")
	}
	var out struct {
		FullPath     string `json:"FullPath"`
		DownloadLink string `json:"DownloadLink"`
	}
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFileFullPath,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{fileID},
	}, &out); err != nil {
		return "", err
	}
	path := out.FullPath
	if path == "" {
		path = out.DownloadLink
	}
	return strings.ReplaceAll(path, `\`, "/"), nil
}

// Versions returns every retained version in a file group.
//
// GET /file/fileversions.json/{session_id}/{file_group_id}. The group id is a
// numeric parameter in the spec but is carried as an opaque string by callers
// because upstream returns identifiers in both quoted and numeric forms.
//
// A file only has a group id once versioning has produced one: a freshly
// created file reports GroupID 0, and passing that yields 400 "Invalid argument
// File Group ID". Accounts with FVersioning disabled never get one.
func (s *FileService) Versions(ctx context.Context, fileGroupID string) ([]FileVersion, error) {
	if fileGroupID == "" {
		return nil, invalidRequest("file versions needs a file group id")
	}
	var out []FileVersion
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFileVersions,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{fileGroupID},
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ThumbnailOptions carries the optional query arguments accepted by
// file/thumb.json.
type ThumbnailOptions struct {
	SharingID  string
	TimeOffset *float64
	TempKey    string
}

// Thumbnail downloads the image bytes for a file's thumbnail.
//
// GET /file/thumb.json/{file_id}; session_id is a query parameter. Unlike the
// rest of this module the successful body is image data, so the raw-response
// transport path is used while preserving normal auth and error handling.
func (s *FileService) Thumbnail(ctx context.Context, fileID string, opts ThumbnailOptions) ([]byte, error) {
	if fileID == "" {
		return nil, invalidRequest("file thumbnail needs a file id")
	}
	q := url.Values{}
	if opts.SharingID != "" {
		q.Set("sharing_id", opts.SharingID)
	}
	if opts.TimeOffset != nil {
		q.Set("time_offset", strconv.FormatFloat(*opts.TimeOffset, 'f', -1, 64))
	}
	if opts.TempKey != "" {
		q.Set("temp_key", opts.TempKey)
	}
	var out []byte
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointFileThumb,
		SessionPlacement: SessionInQuery,
		PathSegments:     []string{fileID},
		Query:            q,
		Accept:           "image/*",
		RawResponse:      true,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------- mutations

// CreateEmptyFileParams describes POST /file.json. This creates an upstream
// empty file; P3's upload/create_file.json creates a content-bearing file and
// is a separate protocol.
type CreateEmptyFileParams struct {
	AccessFolderID string
	FolderID       string
	FileType       string
	SharingID      string
}

// CreateEmpty creates an empty file.
//
// POST /file.json, session_id in the JSON body.
//
// access_folder_id is required and an empty string does not satisfy it:
// upstream answers 400 "`access_folder_id` is required". When the caller has no
// access folder to scope the call to, the value is the root marker "0", which
// is what this method sends by default. Passing the target folder's own id
// instead is refused with 403 for an account user (docs/discrepancies.md D28).
//
// FileType is the extension, not a MIME type: "txt" yields a file named
// "Text.txt", and a second call in the same folder yields "Text (1).txt".
func (s *FileService) CreateEmpty(ctx context.Context, p CreateEmptyFileParams) (*FileInfo, error) {
	if p.FolderID == "" {
		return nil, invalidRequest("create empty file needs a folder id")
	}
	if strings.TrimSpace(p.FileType) == "" {
		return nil, invalidRequest("create empty file needs a file type")
	}
	accessFolder := p.AccessFolderID
	if accessFolder == "" {
		accessFolder = RootFolderID
	}
	body := map[string]string{
		"access_folder_id": accessFolder,
		"folder_id":        p.FolderID,
		"file_type":        p.FileType,
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}
	var out FileInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFile,
		SessionPlacement: SessionInBody,
		Body:             body,
		// Write rights are per folder, so the success witness is too
		// (docs/error-taxonomy.md T2).
		Scope: p.FolderID,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAccess changes a file's private/public/hidden state.
//
// POST /file/access.json, not PUT (docs/discrepancies.md D15). The visibility
// itself is an integer; it is deliberately not represented as a bool.
func (s *FileService) SetAccess(ctx context.Context, fileID string, visibility FileVisibility, accessFolderID, sharingID string) error {
	if fileID == "" {
		return invalidRequest("file access needs a file id")
	}
	body := map[string]any{"file_id": fileID, "file_ispublic": int(visibility)}
	if accessFolderID != "" {
		body["access_folder_id"] = accessFolderID
	}
	if sharingID != "" {
		body["sharing_id"] = sharingID
	}
	var out BoolResult
	return s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFileAccess,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out)
}

// FileMoveCopyParams describes a file move or copy.
type FileMoveCopyParams struct {
	SourceFileID      string
	DestinationFolder string
	Move              bool
	OverwriteIfExists bool
	SourceAccessID    string
	DestinationAccess string
	SourceSharingID   string
	DestinationShare  string
	NewName           string
}

// MoveCopy moves or copies a file.
//
// POST /file/move_copy.json. Both Move and OverwriteIfExists are sent as the
// strings "true" or "false". This is verified by live integration tests, not
// inferred from Go's bool type (whitepaper §2.6 #13; D24, D26).
func (s *FileService) MoveCopy(ctx context.Context, p FileMoveCopyParams) (*FileInfo, error) {
	if p.SourceFileID == "" || p.DestinationFolder == "" {
		return nil, invalidRequest("file move_copy needs a source file id and destination folder id")
	}
	if p.NewName != "" {
		if err := ValidateName(p.NewName); err != nil {
			return nil, err
		}
	}
	body := map[string]any{
		"src_file_id":         p.SourceFileID,
		"dst_folder_id":       p.DestinationFolder,
		"move":                StringBool(p.Move),
		"overwrite_if_exists": StringBool(p.OverwriteIfExists),
	}
	if p.SourceAccessID != "" {
		body["src_access_folder_id"] = p.SourceAccessID
	}
	if p.DestinationAccess != "" {
		body["dst_access_folder_id"] = p.DestinationAccess
	}
	if p.SourceSharingID != "" {
		body["src_sharing_id"] = p.SourceSharingID
	}
	if p.DestinationShare != "" {
		body["dst_sharing_id"] = p.DestinationShare
	}
	if p.NewName != "" {
		body["new_file_name"] = p.NewName
	}
	var out FileInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFileMoveCopy,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rename changes a file's name.
//
// POST /file/rename.json, session_id in the JSON body.
func (s *FileService) Rename(ctx context.Context, fileID, newName, accessFolderID, sharingID string) (*FileInfo, error) {
	if fileID == "" {
		return nil, invalidRequest("file rename needs a file id")
	}
	if err := ValidateName(newName); err != nil {
		return nil, err
	}
	body := map[string]string{"file_id": fileID, "new_file_name": newName}
	if accessFolderID != "" {
		body["access_folder_id"] = accessFolderID
	}
	if sharingID != "" {
		body["sharing_id"] = sharingID
	}
	var out FileInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointFileRename,
		SessionPlacement: SessionInBody,
		Body:             body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Trash moves files to the trash.
//
// POST /file/trash.json, file_id is a comma separated list in the JSON body.
func (s *FileService) Trash(ctx context.Context, fileIDs []string, sharingID ...string) error {
	ids, err := joinIDs(fileIDs)
	if err != nil {
		return err
	}
	body := map[string]string{"file_id": ids}
	if len(sharingID) > 0 && sharingID[0] != "" {
		body["sharing_id"] = sharingID[0]
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodPost, Path: EndpointFileTrash,
		SessionPlacement: SessionInBody, Body: body}, &out)
}

// Restore restores files from the trash.
//
// POST /file/restore.json, file_id is a comma separated list in the JSON body.
func (s *FileService) Restore(ctx context.Context, fileIDs []string) error {
	ids, err := joinIDs(fileIDs)
	if err != nil {
		return err
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodPost, Path: EndpointFileRestore,
		SessionPlacement: SessionInBody, Body: map[string]string{"file_id": ids}}, &out)
}

// Remove permanently deletes already-trashed files.
//
// POST /file/remove.json, file_id is a comma separated list in the JSON body.
func (s *FileService) Remove(ctx context.Context, fileIDs []string, accessFolderID, sharingID string) error {
	ids, err := joinIDs(fileIDs)
	if err != nil {
		return err
	}
	body := map[string]string{"file_id": ids}
	if accessFolderID != "" {
		body["access_folder_id"] = accessFolderID
	}
	if sharingID != "" {
		body["sharing_id"] = sharingID
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodPost, Path: EndpointFileRemove,
		SessionPlacement: SessionInBody, Body: body}, &out)
}

// DeleteTrashed permanently deletes a file through the second, multi-verb file
// resource.
//
// DELETE /file.json/{session_id}/{file_id}; optional access_folder_id and
// sharing_id are query parameters (PDF §4.8, live file.json). This is distinct
// from POST /file/remove.json even though both permanently delete a file.
//
// The name follows the PDF, but the sandbox accepts a file that was never
// trashed and deletes it outright, so do not rely on the trash as a safety net
// (docs/discrepancies.md D29).
func (s *FileService) DeleteTrashed(ctx context.Context, fileID, accessFolderID, sharingID string) error {
	if fileID == "" {
		return invalidRequest("delete trashed file needs a file id")
	}
	q := url.Values{}
	if accessFolderID != "" {
		q.Set("access_folder_id", accessFolderID)
	}
	if sharingID != "" {
		q.Set("sharing_id", sharingID)
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodDelete, Path: EndpointFile,
		SessionPlacement: SessionInPath, PathSegments: []string{fileID}, Query: q}, &out)
}

// RemoveVersion permanently deletes one historic file version.
//
// DELETE /file/removefileversion.json/{session_id}/{file_id}.
func (s *FileService) RemoveVersion(ctx context.Context, fileID string) error {
	if fileID == "" {
		return invalidRequest("remove file version needs a file id")
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodDelete, Path: EndpointFileRemoveVersion,
		SessionPlacement: SessionInPath, PathSegments: []string{fileID}}, &out)
}

// FileSettings contains the mutable settings of a file. Nil pointers mean
// "leave the upstream value alone", avoiding accidental public exposure.
type FileSettings struct {
	Price            *string
	Name             *string
	Description      *string
	Password         *string
	DestinationURL   *string
	Visibility       *FileVisibility
	EditOnline       *bool
	ModificationTime *UnixTime
	SharingID        string
}

// UpdateSettings applies selected settings to a file.
//
// PUT /file/filesettings.json, not POST (docs/discrepancies.md D15). Boolean
// edit-online is represented by its documented 0/1 integer, not a JSON bool.
func (s *FileService) UpdateSettings(ctx context.Context, fileID string, p FileSettings) error {
	if fileID == "" {
		return invalidRequest("file settings needs a file id")
	}
	if p.Name != nil {
		if err := ValidateName(*p.Name); err != nil {
			return err
		}
	}
	body := map[string]any{"file_id": fileID}
	if p.Price != nil {
		body["file_price"] = *p.Price
	}
	if p.Name != nil {
		body["file_name"] = *p.Name
	}
	if p.Description != nil {
		body["file_description"] = *p.Description
	}
	if p.Password != nil {
		body["file_password"] = *p.Password
	}
	if p.DestinationURL != nil {
		body["file_dest_url"] = *p.DestinationURL
	}
	if p.Visibility != nil {
		body["file_ispublic"] = int(*p.Visibility)
	}
	if p.EditOnline != nil {
		body["file_edit_online"] = boolToInt(*p.EditOnline)
	}
	if p.ModificationTime != nil {
		body["file_modification_time"] = p.ModificationTime.Unix()
	}
	if p.SharingID != "" {
		body["sharing_id"] = p.SharingID
	}
	var out BoolResult
	return s.c.Do(ctx, Request{Method: http.MethodPut, Path: EndpointFileSettings,
		SessionPlacement: SessionInBody, Body: body}, &out)
}

// VerifyPassword verifies a password for a protected file and returns the
// temporary key used by thumbnail/download requests when upstream supplies it.
//
// POST /file/verifypassword.json, session_id in the JSON body. This method
// never retries bad passwords, because upstream can require captcha after
// repeated failures (§2.6 #11).
func (s *FileService) VerifyPassword(ctx context.Context, fileID, password, captchaResponse string) (*FilePasswordVerification, error) {
	if fileID == "" || password == "" {
		return nil, invalidRequest("verify file password needs a file id and password")
	}
	body := map[string]string{"file_id": fileID, "password": password}
	if captchaResponse != "" {
		body["captcha_response"] = captchaResponse
	}
	var out FilePasswordVerification
	if err := s.c.Do(ctx, Request{Method: http.MethodPost, Path: EndpointFileVerifyPassword,
		SessionPlacement: SessionInBody, Body: body, Retryable: Retryable(false)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileEmailParams describes an outbound file-share email.
type FileEmailParams struct {
	FileIDs         []string
	Recipients      []string
	Subject         string
	Body            string
	SendExpiring    bool
	CaptchaResponse string
}

// SendByEmail sends links to one or more files.
//
// POST /file/sendbyemail.json. Upstream documents comma-separated strings;
// arrays are used here only after being joined, so the request is stable across
// both the PHP samples and the live spec.
func (s *FileService) SendByEmail(ctx context.Context, p FileEmailParams) error {
	ids, err := joinIDs(p.FileIDs)
	if err != nil {
		return err
	}
	recipients := strings.Join(nonEmpty(p.Recipients), ",")
	if recipients == "" {
		return invalidRequest("send file by email needs at least one recipient")
	}
	body := map[string]any{
		"file_id":               ids,
		"recipient_emails":      recipients,
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
	return s.c.Do(ctx, Request{Method: http.MethodPost, Path: EndpointFileSendByEmail,
		SessionPlacement: SessionInBody, Body: body, Retryable: Retryable(false)}, &out)
}

// CreateExpiringLink makes or updates a file's time- and use-limited link.
//
// GET /file/expiringlink.json/{session_id}/{date}/{counter}/{file_id}/{enable}.
// The request mutates state despite using GET, so it is explicitly non-retryable.
func (s *FileService) CreateExpiringLink(ctx context.Context, fileID, date string, counter int, enable bool) (*ExpiringLink, error) {
	if fileID == "" || date == "" || counter < 0 {
		return nil, invalidRequest("file expiring link needs a file id, date and non-negative counter")
	}
	var out ExpiringLink
	if err := s.c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointFileExpiringLink,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{date, strconv.Itoa(counter), fileID, strconv.FormatBool(enable)},
		Retryable:        Retryable(false)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExpiringLinks returns the expiring link configured on a file.
//
// GET /file/fileexpiringlinks.json/{session_id}/{file_id}. Despite the plural
// name upstream answers with a single object carrying DownloadLink and
// StreamingLink rather than the folder module's Link
// (docs/discrepancies.md D31).
func (s *FileService) ExpiringLinks(ctx context.Context, fileID string) (*ExpiringLink, error) {
	if fileID == "" {
		return nil, invalidRequest("file expiring links needs a file id")
	}
	var out ExpiringLink
	if err := s.c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointFileExpiringLinks,
		SessionPlacement: SessionInPath, PathSegments: []string{fileID}}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
