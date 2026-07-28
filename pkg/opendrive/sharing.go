package opendrive

import (
	"context"
	"net/http"
	"strconv"
)

// SharingService binds the upstream sharing module (PDF §9, live spec
// testdata/spec/sharing.json): sharing a folder with another OpenDrive user,
// listing those shares and revoking them.
//
// This is user-to-user sharing, which is a different thing from the public
// links of folder/file expiringlink.json. It is also the one module the test
// account cannot exercise at all: an *account user* login is refused on every
// operation, read included (docs/discrepancies.md D33).
type SharingService struct {
	c *Client
}

// Sharing returns the sharing module bound to this client.
func (c *Client) Sharing() *SharingService { return &SharingService{c: c} }

// ShareMode is the permission a share grants (spec: "0 - View only,
// 1 - Full access mode").
type ShareMode int

// Share modes.
const (
	ShareViewOnly   ShareMode = 0
	ShareFullAccess ShareMode = 1
)

// String renders the mode the way upstream expects it in a request body: a
// decimal string, not an integer (the spec types sharemode as a string).
func (m ShareMode) String() string { return strconv.Itoa(int(m)) }

// SharedFolder is one entry of sharing/listsharedfolders.json.
type SharedFolder struct {
	FolderID      FlexString `json:"FolderID"`
	SharingID     FlexString `json:"SharingID"`
	Name          string     `json:"Name"`
	Owner         FlexString `json:"Owner"`
	OwnerName     string     `json:"OwnerName"`
	ShareMode     FlexInt    `json:"ShareMode"`
	DateShared    UnixTime   `json:"DateShared"`
	DirUpdateTime UnixTime   `json:"DirUpdateTime"`
	Access        FlexInt    `json:"Access"`
	Size          FlexInt    `json:"Size"`
	Link          string     `json:"Link"`
}

// SharedUser is one entry of sharing/listsharedusers.json and
// sharing/listusers.json.
type SharedUser struct {
	UserID     FlexString `json:"UserID"`
	SharingID  FlexString `json:"SharingID"`
	UserName   string     `json:"UserName"`
	FirstName  string     `json:"FirstName"`
	LastName   string     `json:"LastName"`
	Email      string     `json:"Email"`
	ShareMode  FlexInt    `json:"ShareMode"`
	DateShared UnixTime   `json:"DateShared"`
	FolderID   FlexString `json:"FolderID"`
	FolderName string     `json:"FolderName"`
}

// CreatedShare is the response of POST /sharing.json.
type CreatedShare struct {
	SharingID FlexString `json:"SharingID"`
	FolderID  FlexString `json:"FolderID"`
	UserName  string     `json:"UserName"`
	ShareMode FlexInt    `json:"ShareMode"`
}

// AccountUsersAccess is the response of sharing/checkaccountusersaccess.json.
type AccountUsersAccess struct {
	Access  FlexBool `json:"Access"`
	Allowed FlexBool `json:"Allowed"`
	Result  FlexBool `json:"result"`
}

// OK reports whether account users may be granted access, accepting whichever
// field upstream fills in.
func (a AccountUsersAccess) OK() bool {
	return a.Access.Bool() || a.Allowed.Bool() || a.Result.Bool()
}

// Share grants another OpenDrive user access to a folder.
//
// POST /sharing.json, session in the body. The username may be a login name or
// an email address, and sharemode is sent as a decimal string because the spec
// types it as one (PDF §9.4).
func (s *SharingService) Share(ctx context.Context, folderID, username string, mode ShareMode) (*CreatedShare, error) {
	if folderID == "" {
		return nil, invalidRequest("share needs a folder id")
	}
	if username == "" {
		return nil, invalidRequest("share needs a username or email address")
	}
	var out CreatedShare
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodPost,
		Path:             EndpointSharing,
		SessionPlacement: SessionInBody,
		Body: map[string]string{
			"folder_id": folderID,
			"username":  username,
			"sharemode": mode.String(),
		},
	}, &out); err != nil {
		return nil, err
	}
	s.invalidate(folderID)
	return &out, nil
}

// SetMode changes the permission an existing share grants.
//
// PUT /sharing/setmode.json — a PUT, unlike the POST that created the share
// (§2.6 #4).
func (s *SharingService) SetMode(ctx context.Context, sharingID string, mode ShareMode) error {
	if sharingID == "" {
		return invalidRequest("set share mode needs a sharing id")
	}
	var out BoolResult
	return s.c.Do(ctx, Request{
		Method:           http.MethodPut,
		Path:             EndpointSharingSetMode,
		SessionPlacement: SessionInBody,
		Body: map[string]string{
			"sharing_id": sharingID,
			"sharemode":  mode.String(),
		},
	}, &out)
}

// Revoke withdraws a share.
//
// DELETE /sharing.json/{session_id}/{sharing_id} — the same resource as Share,
// with the arguments moved from the body into the path (§2.6 #4).
func (s *SharingService) Revoke(ctx context.Context, sharingID string) error {
	if sharingID == "" {
		return invalidRequest("revoking a share needs a sharing id")
	}
	var out BoolResult
	return s.c.Do(ctx, Request{
		Method:           http.MethodDelete,
		Path:             EndpointSharing,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{sharingID},
	}, &out)
}

// ListSharedFolders lists the folders reachable through a share.
//
// GET /sharing/listsharedfolders.json/{session_id}/{sharing_id}.
func (s *SharingService) ListSharedFolders(ctx context.Context, sharingID string) ([]SharedFolder, error) {
	if sharingID == "" {
		return nil, invalidRequest("listing shared folders needs a sharing id")
	}
	var out []SharedFolder
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointSharingListFolders,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{sharingID},
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSharedUsers lists the users who have shared something with this account.
//
// GET /sharing/listsharedusers.json/{session_id}.
func (s *SharingService) ListSharedUsers(ctx context.Context) ([]SharedUser, error) {
	var out []SharedUser
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointSharingListUsers,
		SessionPlacement: SessionInPath,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFolderUsers lists the users a particular folder is shared with.
//
// GET /sharing/listusers.json/{session_id}/{folder_id}. Note the near-collision
// with ListSharedUsers: upstream's two "list users" endpoints answer different
// questions, which is why the SDK names them apart.
func (s *SharingService) ListFolderUsers(ctx context.Context, folderID string) ([]SharedUser, error) {
	if folderID == "" {
		return nil, invalidRequest("listing a folder's users needs a folder id")
	}
	var out []SharedUser
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointSharingListFolderUsers,
		SessionPlacement: SessionInPath,
		PathSegments:     []string{folderID},
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckAccountUsersAccess reports whether account users may be given access to
// shared content.
//
// GET /sharing/checkaccountusersaccess.json, session in the query string: the
// spec declares no parameters at all for this operation, and the session is
// what the transport adds.
func (s *SharingService) CheckAccountUsersAccess(ctx context.Context) (*AccountUsersAccess, error) {
	var out AccountUsersAccess
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointSharingCheckAccess,
		SessionPlacement: SessionInQuery,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// invalidate drops cached knowledge about a folder whose sharing changed.
func (s *SharingService) invalidate(folderID string) {
	if cache := s.c.pathCache; cache != nil && folderID != "" {
		cache.InvalidateID(folderID)
	}
}
