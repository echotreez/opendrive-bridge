package opendrive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// RootFolderID is the identifier of the account root. Upstream always expects
// the string "0", never the number 0 (whitepaper §2.6 #9).
const RootFolderID = "0"

// MaxNameLength is the upstream limit for file and folder names (§2.6 #10).
const MaxNameLength = 255

// IllegalNameChars are the characters upstream rejects in file and folder
// names (§2.6 #10). Validating locally lets the Bridge return a friendlier
// error than upstream does.
const IllegalNameChars = `\/:*?"<>|`

// MaxListPageSize is the hard cap upstream applies to folder/list.json when the
// offset parameter is used (§2.3, §2.6 #14).
const MaxListPageSize = 100

// ---------------------------------------------------------------- FlexBool

// FlexBool decodes an upstream boolean that may arrive as a JSON boolean, as
// the integers 0/1, or as one of the strings "0", "1", "true", "false",
// "True", "False", "yes", "no" or "" (§2.6 #5). It marshals as a JSON boolean.
type FlexBool bool

// Bool returns the value as a plain bool.
func (b FlexBool) Bool() bool { return bool(b) }

// UnmarshalJSON implements json.Unmarshaler.
func (b *FlexBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*b = false
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		v, err := parseLooseBool(s)
		if err != nil {
			return err
		}
		*b = FlexBool(v)
		return nil
	}
	v, err := parseLooseBool(string(data))
	if err != nil {
		return err
	}
	*b = FlexBool(v)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (b FlexBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

func parseLooseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off", "null":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	}
	// Any other number is truthy when non-zero, e.g. "2".
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f != 0, nil
	}
	return false, fmt.Errorf("opendrive: cannot decode %q as a boolean", s)
}

// ---------------------------------------------------------------- StringBool

// StringBool is a request-side boolean that marshals to the strings "true" and
// "false". Several upstream endpoints (file/move_copy.json's move and
// overwrite_if_exists among them) reject real JSON booleans (§2.6 #13).
type StringBool bool

// MarshalJSON implements json.Marshaler.
func (b StringBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte(`"true"`), nil
	}
	return []byte(`"false"`), nil
}

// UnmarshalJSON accepts the same loose forms as FlexBool.
func (b *StringBool) UnmarshalJSON(data []byte) error {
	var f FlexBool
	if err := f.UnmarshalJSON(data); err != nil {
		return err
	}
	*b = StringBool(f)
	return nil
}

// String returns "true" or "false", suitable for a query parameter.
func (b StringBool) String() string { return strconv.FormatBool(bool(b)) }

// ---------------------------------------------------------------- FlexInt

// FlexInt decodes an upstream integer that may arrive as a JSON number, as a
// quoted number, as a float, or as "" / null (§2.6 #5, #6).
type FlexInt int64

// Int returns the value as an int.
func (i FlexInt) Int() int { return int(i) }

// Int64 returns the value as an int64.
func (i FlexInt) Int64() int64 { return int64(i) }

// String renders the value in base 10. Upstream returns the same identifier as
// a number in one response and as a string in another (docs/discrepancies.md
// D19), so callers that need a stable id use this.
func (i FlexInt) String() string { return strconv.FormatInt(int64(i), 10) }

// UnmarshalJSON implements json.Unmarshaler.
func (i *FlexInt) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*i = 0
		return nil
	}
	s := string(data)
	if data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
		if s == "" {
			*i = 0
			return nil
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*i = FlexInt(n)
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("opendrive: cannot decode %q as an integer", s)
	}
	*i = FlexInt(int64(f))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (i FlexInt) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(i), 10)), nil
}

// ---------------------------------------------------------------- FlexString

// FlexString decodes a value that upstream sometimes quotes and sometimes
// does not, most notably identifiers (§2.6 #5).
type FlexString string

// String returns the value.
func (s FlexString) String() string { return string(s) }

// UnmarshalJSON implements json.Unmarshaler.
func (s *FlexString) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*s = ""
		return nil
	}
	if data[0] == '"' {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		*s = FlexString(str)
		return nil
	}
	if data[0] == '{' || data[0] == '[' {
		return fmt.Errorf("opendrive: cannot decode %s as a string", truncate(string(data), 40))
	}
	*s = FlexString(strings.Trim(string(data), `"`))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (s FlexString) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

// ---------------------------------------------------------------- UnixTime

// UnixTime decodes an upstream timestamp. Most are Unix seconds as integers,
// some are the same value quoted, and absent values arrive as 0, "" or null
// (§2.6 #6). The zero value maps to the zero time.Time, never to 1970.
type UnixTime struct{ time.Time }

// NewUnixTime builds a UnixTime from a time.Time.
func NewUnixTime(t time.Time) UnixTime { return UnixTime{t} }

// UnmarshalJSON implements json.Unmarshaler.
func (t *UnixTime) UnmarshalJSON(data []byte) error {
	var n FlexInt
	if err := n.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("opendrive: cannot decode %s as a timestamp", truncate(string(data), 40))
	}
	if n == 0 {
		t.Time = time.Time{}
		return nil
	}
	t.Time = time.Unix(int64(n), 0).UTC()
	return nil
}

// MarshalJSON implements json.Marshaler, emitting Unix seconds (0 when unset)
// because that is what upstream expects on the request side, e.g. file_time on
// close_file_upload (§2.4).
func (t UnixTime) MarshalJSON() ([]byte, error) {
	if t.Time.IsZero() {
		return []byte("0"), nil
	}
	return []byte(strconv.FormatInt(t.Unix(), 10)), nil
}

// IsZero reports whether the timestamp is unset.
func (t UnixTime) IsZero() bool { return t.Time.IsZero() }

// ---------------------------------------------------------------- results

// BoolResult decodes the "success" bodies upstream is inconsistent about: a
// bare true, {"result":true}, {"result":"1"} or an empty object (§2.6 #7).
type BoolResult bool

// OK reports whether the call succeeded.
func (r BoolResult) OK() bool { return bool(r) }

// UnmarshalJSON implements json.Unmarshaler.
func (r *BoolResult) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*r = false
		return nil
	}
	if trimmed[0] == '{' {
		var obj struct {
			Result *FlexBool `json:"result"`
			Status *FlexBool `json:"status"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return err
		}
		switch {
		case obj.Result != nil:
			*r = BoolResult(*obj.Result)
		case obj.Status != nil:
			*r = BoolResult(*obj.Status)
		default:
			// An empty object is upstream's way of saying "done".
			*r = true
		}
		return nil
	}
	var f FlexBool
	if err := f.UnmarshalJSON(trimmed); err != nil {
		return err
	}
	*r = BoolResult(f)
	return nil
}

// ---------------------------------------------------------------- models

// SessionLogin is the response of POST /session/login.json (PDF §11.1).
// Field names follow the wire exactly, including upstream's spelling of
// OwnerSuspendet (§2.6 #3).
type SessionLogin struct {
	SessionID          string   `json:"SessionID"`
	UserName           string   `json:"UserName"`
	UserFirstName      string   `json:"UserFirstName"`
	UserLastName       string   `json:"UserLastName"`
	AccType            FlexInt  `json:"AccType"`
	UserLang           string   `json:"UserLang"`
	UserID             FlexInt  `json:"UserID"`
	IsAccountUser      FlexBool `json:"IsAccountUser"`
	DriveName          string   `json:"DriveName"`
	UserLevel          string   `json:"UserLevel"`
	UserPlan           string   `json:"UserPlan"`
	FVersioning        FlexBool `json:"FVersioning"`
	UserDomain         string   `json:"UserDomain"`
	PartnerUsersDomain string   `json:"PartnerUsersDomain"`
	UploadSpeedLimit   FlexInt  `json:"upload_speed_limit"`
	DownloadSpeedLimit FlexInt  `json:"download_speed_limit"`
	MaxFileSize        FlexInt  `json:"max_file_size"`
	OwnerSuspendet     FlexBool `json:"OwnerSuspendet"` // upstream spelling, §2.6 #3
	Enable2FA          FlexBool `json:"Enable2FA"`
}

// SessionInfo is the response of GET /session/info.json.
type SessionInfo struct {
	SessionID  string   `json:"SessionID"`
	UserID     FlexInt  `json:"UserID"`
	UserName   string   `json:"UserName"`
	AccType    FlexInt  `json:"AccType"`
	IsActive   FlexBool `json:"IsActive"`
	ExpiryTime UnixTime `json:"ExpiryTime"`
}

// UserInfo is the response of GET /users/info.json (PDF §13).
type UserInfo struct {
	UserID          FlexString `json:"UserID"`
	UserName        string     `json:"UserName"`
	FirstName       string     `json:"FirstName"`
	LastName        string     `json:"LastName"`
	AccType         FlexInt    `json:"AccType"`
	Email           string     `json:"Email"`
	StorageUsed     FlexInt    `json:"StorageUsed"`
	MaxStorage      FlexInt    `json:"MaxStorage"`
	BWUsed          FlexInt    `json:"BWUsed"`
	MaxBW           FlexInt    `json:"MaxBW"`
	AccountCreation UnixTime   `json:"AccountCreation"`
	Suspended       FlexBool   `json:"Suspended"`
	Enable2FA       FlexBool   `json:"Enable2FA"`
}

// CaptchaStatus is the response of GET /session/captcharequired.json, an
// endpoint that exists online but not in the PDF (§2.2, §2.6 #1).
type CaptchaStatus struct {
	Required FlexBool `json:"Required"`
	SiteKey  string   `json:"SiteKey"`
}

// ---------------------------------------------------------------- pagination

// Pagination carries the folder/list.json paging protocol: upstream returns at
// most MaxListPageSize entries per call when offset is set, and the caller must
// echo the DirUpdateTime of the previous response as last_request_time or the
// listing silently loses entries (§2.3, §2.6 #14).
type Pagination struct {
	Offset int
	// Limit is clamped to MaxListPageSize; 0 means MaxListPageSize.
	Limit int
	// LastRequestTime must be 0 on the first page and the previous response's
	// DirUpdateTime on every subsequent page.
	LastRequestTime int64
	// firstPage records whether this is the initial request, so that a zero
	// LastRequestTime on a later page can be reported as a mistake.
	firstPage bool
}

// FirstPage returns the pagination state for the initial request.
func FirstPage(limit int) Pagination {
	return Pagination{Limit: clampLimit(limit), firstPage: true}
}

// Next advances the pagination state using the DirUpdateTime of the response
// just received.
func (p Pagination) Next(dirUpdateTime int64) Pagination {
	return Pagination{
		Offset:          p.Offset + clampLimit(p.Limit),
		Limit:           clampLimit(p.Limit),
		LastRequestTime: dirUpdateTime,
	}
}

// Validate reports a caller mistake before it reaches the network.
func (p Pagination) Validate() error {
	if p.Offset < 0 {
		return invalidRequest("pagination offset must not be negative")
	}
	if p.Limit < 0 {
		return invalidRequest("pagination limit must not be negative")
	}
	if p.Offset > 0 && p.LastRequestTime == 0 && !p.firstPage {
		return invalidRequest("pagination past the first page requires last_request_time (the previous response's DirUpdateTime); see whitepaper §2.6 #14")
	}
	return nil
}

// EffectiveLimit returns the page size upstream will actually honour.
func (p Pagination) EffectiveLimit() int { return clampLimit(p.Limit) }

func clampLimit(n int) int {
	if n <= 0 || n > MaxListPageSize {
		return MaxListPageSize
	}
	return n
}

// ---------------------------------------------------------------- names

// ValidateName checks a file or folder name against the upstream rules before
// a request is made: non-empty, at most MaxNameLength bytes, valid UTF-8, no
// path separators or reserved characters, and no "." / ".." (§2.6 #10, §9.3).
func ValidateName(name string) error {
	switch {
	case name == "":
		return &APIError{Kind: KindInvalidName, UpstreamMsg: "name must not be empty"}
	case name == "." || name == "..":
		return &APIError{Kind: KindInvalidName, UpstreamMsg: fmt.Sprintf("%q is not a usable name", name)}
	case len(name) > MaxNameLength:
		return &APIError{Kind: KindInvalidName, UpstreamMsg: fmt.Sprintf("name is %d bytes, the limit is %d", len(name), MaxNameLength)}
	case !utf8.ValidString(name):
		return &APIError{Kind: KindInvalidName, UpstreamMsg: "name is not valid UTF-8"}
	case strings.ContainsAny(name, IllegalNameChars):
		return &APIError{Kind: KindInvalidName, UpstreamMsg: fmt.Sprintf("name must not contain any of %s", IllegalNameChars)}
	case strings.TrimSpace(name) == "":
		return &APIError{Kind: KindInvalidName, UpstreamMsg: "name must not be only whitespace"}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return &APIError{Kind: KindInvalidName, UpstreamMsg: "name must not contain control characters"}
		}
	}
	return nil
}
