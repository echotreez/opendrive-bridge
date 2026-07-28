package opendrive

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// UsersService binds the read half of the upstream users module (PDF §13, live
// spec testdata/spec/users.json).
//
// v1.0 is deliberately read-only (§1.4). The mutating endpoints — changing the
// email, username or password, updating the profile, scheduling the account for
// deletion — are v1.1 material and are left unbound rather than bound and
// discouraged, so that no caller can reach them by accident.
type UsersService struct {
	c *Client
}

// Users returns the users module bound to this client.
func (c *Client) Users() *UsersService { return &UsersService{c: c} }

// AccountInfo is the response of users/info.json. It is the widest object
// upstream returns and the source of the quota figures the Bridge reports
// through /v1/auth/status (§4.1).
//
// Two cautions. AccessUserID differs from UserID when the login is an *account
// user* rather than the account owner, which is what closes the sharing module
// (docs/discrepancies.md D33). And the response carries credential-shaped and
// personal fields — PrivateKey, the postal address — that must never reach a
// log; the redaction list covers PrivateKey and this type's String method keeps
// the rest out of casual output (§9.4).
type AccountInfo struct {
	UserID       FlexString `json:"UserID"`
	AccessUserID FlexString `json:"AccessUserID"`
	UserName     string     `json:"UserName"`
	FirstName    string     `json:"UserFirstName"`
	LastName     string     `json:"UserLastName"`
	Email        string     `json:"Email"`
	AccType      FlexInt    `json:"AccType"`
	Level        FlexInt    `json:"Level"`
	UserPlan     string     `json:"UserPlan"`
	UserLang     string     `json:"UserLang"`
	TimeZone     string     `json:"TimeZone"`
	CompanyName  string     `json:"CompanyName"`
	UserSince    UnixTime   `json:"UserSince"`
	DueDate      string     `json:"DueDate"`

	// Quota, all quoted numbers on the wire (§2.6 #5).
	MaxStorage  FlexInt `json:"MaxStorage"`
	StorageUsed FlexInt `json:"StorageUsed"`
	BwMax       FlexInt `json:"BwMax"`
	BwUsed      FlexInt `json:"BwUsed"`
	MaxFileSize FlexInt `json:"MaxFileSize"`

	// Capabilities the transfer pipeline of P3 has to respect.
	FVersioning        FlexBool `json:"FVersioning"`
	FVersions          FlexInt  `json:"FVersions"`
	UploadSpeedLimit   FlexInt  `json:"UploadSpeedLimit"`
	DownloadSpeedLimit FlexInt  `json:"DownloadSpeedLimit"`
	MaxAccountUsers    FlexInt  `json:"MaxAccountUsers"`

	// Account state.
	Trial                FlexBool `json:"Trial"`
	Suspended            FlexBool `json:"Suspended"`
	Enable2FA            FlexBool `json:"Enable2FA"`
	Verified             FlexBool `json:"Verified"`
	SignupVerified       FlexBool `json:"SignupVerified"`
	AdminMode            FlexBool `json:"AdminMode"`
	IsPartner            FlexBool `json:"IsPartner"`
	Partner              string   `json:"Partner"`
	RootFolderPermission FlexInt  `json:"RootFolderPermission"`
	CanChangePwd         FlexBool `json:"CanChangePwd"`

	// PrivateKey is credential-shaped and is redacted from logs (§9.4, D20).
	PrivateKey string `json:"PrivateKey"`
}

// IsAccountUser reports whether the login is an account user rather than the
// account owner. Account users are refused by the whole sharing module and
// cannot write to the account root (D25, D33).
func (a AccountInfo) IsAccountUser() bool {
	return a.AccessUserID != "" && a.AccessUserID != a.UserID
}

// String keeps the personal and credential fields out of casual output. Use
// the struct fields directly when a value is genuinely needed (§9.4).
func (a AccountInfo) String() string {
	return "AccountInfo(user=" + Redacted + ", plan=" + a.UserPlan +
		", storage=" + strconv.FormatInt(a.StorageUsed.Int64(), 10) + "/" +
		strconv.FormatInt(a.MaxStorage.Int64(), 10) + ")"
}

// AccountInfoOptions carries the two optional query parameters of
// users/info.json, neither of which the PDF mentions.
type AccountInfoOptions struct {
	// ApplyBW asks upstream to fold bandwidth accounting into the response.
	ApplyBW bool
	// Branding asks for the partner branding fields.
	Branding bool
}

// Info returns the account information.
//
// GET /users/info.json/{session_id}, session as a path segment.
func (s *UsersService) Info(ctx context.Context, opts ...AccountInfoOptions) (*AccountInfo, error) {
	q := url.Values{}
	if len(opts) > 0 {
		if opts[0].ApplyBW {
			q.Set("apply_bw", "1")
		}
		if opts[0].Branding {
			q.Set("branding", "1")
		}
	}
	var out AccountInfo
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointUsersInfo,
		SessionPlacement: SessionInPath,
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ActivityLog is one entry of the account activity log.
type ActivityLog struct {
	Time     UnixTime `json:"Time"`
	User     string   `json:"User"`
	LogType  string   `json:"LogType"`
	IP       string   `json:"IP"`
	FileName string   `json:"FileName"`
	FileSize FlexInt  `json:"FileSize"`
	Details  string   `json:"Details"`
	App      string   `json:"App"`
}

// ActivityLogPage is one page of users/userlogs.json, which pages by number.
type ActivityLogPage struct {
	TotalPages  FlexInt       `json:"TotalPages"`
	CurrentPage FlexInt       `json:"CurrentPage"`
	Logs        []ActivityLog `json:"Logs"`
}

// ActivityLogCursor is one page of users/userlogscursor.json, which pages by
// keyset. NextCursor is empty on the last page.
type ActivityLogCursor struct {
	NextCursor string        `json:"NextCursor"`
	Logs       []ActivityLog `json:"Logs"`
}

// ActivityLogFilter narrows an activity log query. The zero value asks for
// everything the account can see.
type ActivityLogFilter struct {
	// AccessUserID scopes the query to one account user.
	AccessUserID string
	// Start and End bound the window; zero values are omitted.
	Start UnixTime
	End   UnixTime
	// LogType filters by upstream's numeric log category. Nil means all.
	LogType *int
}

func (f ActivityLogFilter) apply(q url.Values) {
	if f.AccessUserID != "" {
		q.Set("access_user_id", f.AccessUserID)
	}
	if !f.Start.IsZero() {
		q.Set("start_date", strconv.FormatInt(f.Start.Unix(), 10))
	}
	if !f.End.IsZero() {
		q.Set("end_date", strconv.FormatInt(f.End.Unix(), 10))
	}
	if f.LogType != nil {
		q.Set("log_type", strconv.Itoa(*f.LogType))
	}
}

// Logs returns one numbered page of the account activity log.
//
// GET /users/userlogs.json/{session_id}. Pages start at 1; the response
// reports TotalPages, so a caller can walk them. Prefer LogsCursor for
// anything longer than a glance: numbered paging over a log that is still
// being written can repeat or skip entries.
func (s *UsersService) Logs(ctx context.Context, page int, filter ActivityLogFilter) (*ActivityLogPage, error) {
	if page < 0 {
		return nil, invalidRequest("a log page number must not be negative")
	}
	q := url.Values{}
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	filter.apply(q)

	var out ActivityLogPage
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointUsersLogs,
		SessionPlacement: SessionInPath,
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LogsCursor returns one keyset page of the account activity log, newest
// first. Pass the previous response's NextCursor to continue; an empty cursor
// starts at the beginning and an empty NextCursor means there is no more.
//
// GET /users/userlogscursor.json/{session_id}. The endpoint is absent from the
// PDF (docs/discrepancies.md D34) and is the safe way to walk a log that is
// still being appended to.
func (s *UsersService) LogsCursor(ctx context.Context, cursor string, filter ActivityLogFilter) (*ActivityLogCursor, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	filter.apply(q)

	var out ActivityLogCursor
	if err := s.c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointUsersLogsCursor,
		SessionPlacement: SessionInPath,
		Query:            q,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
