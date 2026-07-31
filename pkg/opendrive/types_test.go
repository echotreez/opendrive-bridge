package opendrive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// §2.6 #5: booleans arrive as 0/1 integers and occasionally as "True"/"False"
// strings, so the decoder has to be loose.
func TestFlexBoolDecodesEveryUpstreamForm(t *testing.T) {
	cases := map[string]bool{
		`true`:    true,
		`false`:   false,
		`1`:       true,
		`0`:       false,
		`"1"`:     true,
		`"0"`:     false,
		`"true"`:  true,
		`"false"`: false,
		`"True"`:  true,
		`"False"`: false,
		`"yes"`:   true,
		`"no"`:    false,
		`""`:      false,
		`null`:    false,
		`2`:       true,
		`"2"`:     true,
	}
	for in, want := range cases {
		var b FlexBool
		if err := json.Unmarshal([]byte(in), &b); err != nil {
			t.Errorf("FlexBool(%s): %v", in, err)
			continue
		}
		if b.Bool() != want {
			t.Errorf("FlexBool(%s) = %v, want %v", in, b.Bool(), want)
		}
	}
}

func TestFlexBoolRejectsNonsense(t *testing.T) {
	var b FlexBool
	if err := json.Unmarshal([]byte(`"maybe"`), &b); err == nil {
		t.Fatal("expected an error for a non-boolean string")
	}
	if err := json.Unmarshal([]byte(`{"a":1}`), &b); err == nil {
		t.Fatal("expected an error for an object")
	}
}

func TestFlexBoolMarshalsAsJSONBoolean(t *testing.T) {
	raw, err := json.Marshal(struct {
		A FlexBool `json:"a"`
		B FlexBool `json:"b"`
	}{A: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":true,"b":false}` {
		t.Fatalf("marshalled %s", raw)
	}
}

// §2.6 #13: some request parameters must be the strings "true"/"false".
func TestStringBoolMarshalsAsQuotedString(t *testing.T) {
	raw, err := json.Marshal(map[string]StringBool{"move": true, "overwrite_if_exists": false})
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	mustContain(t, got, `"move":"true"`, "StringBool")
	mustContain(t, got, `"overwrite_if_exists":"false"`, "StringBool")

	var b StringBool
	if err := json.Unmarshal([]byte(`"true"`), &b); err != nil || !bool(b) {
		t.Fatalf("unmarshal: %v %v", b, err)
	}
	if b.String() != "true" {
		t.Fatalf("String() = %q", b.String())
	}
	if err := json.Unmarshal([]byte(`"nope"`), &b); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFlexIntDecodesEveryUpstreamForm(t *testing.T) {
	cases := map[string]int64{
		`42`:                42,
		`"42"`:              42,
		`" 42 "`:            42,
		`-7`:                -7,
		`42.0`:              42,
		`"42.9"`:            42,
		`""`:                0,
		`null`:              0,
		`0`:                 0,
		`92233720368547758`: 92233720368547758,
	}
	for in, want := range cases {
		var n FlexInt
		if err := json.Unmarshal([]byte(in), &n); err != nil {
			t.Errorf("FlexInt(%s): %v", in, err)
			continue
		}
		if n.Int64() != want {
			t.Errorf("FlexInt(%s) = %d, want %d", in, n.Int64(), want)
		}
	}
	var n FlexInt
	if err := json.Unmarshal([]byte(`"abc"`), &n); err == nil {
		t.Error("expected an error for a non-numeric string")
	}
	if err := json.Unmarshal([]byte(`{}`), &n); err == nil {
		t.Error("expected an error for an object")
	}
	raw, _ := json.Marshal(FlexInt(5))
	if string(raw) != "5" {
		t.Errorf("marshal = %s", raw)
	}
	if FlexInt(5).Int() != 5 {
		t.Error("Int()")
	}
}

func TestFlexString(t *testing.T) {
	cases := map[string]string{
		`"abc"`: "abc",
		`123`:   "123",
		`null`:  "",
		`""`:    "",
		`1.5`:   "1.5",
	}
	for in, want := range cases {
		var s FlexString
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Errorf("FlexString(%s): %v", in, err)
			continue
		}
		if s.String() != want {
			t.Errorf("FlexString(%s) = %q, want %q", in, s, want)
		}
	}
	var s FlexString
	if err := json.Unmarshal([]byte(`{"a":1}`), &s); err == nil {
		t.Error("expected an error for an object")
	}
	raw, _ := json.Marshal(FlexString("x"))
	if string(raw) != `"x"` {
		t.Errorf("marshal = %s", raw)
	}
}

// §2.6 #6: timestamps are Unix seconds, sometimes quoted, and absent values
// must not decode to 1970.
func TestUnixTime(t *testing.T) {
	var ts UnixTime
	if err := json.Unmarshal([]byte(`1753444800`), &ts); err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(time.Unix(1753444800, 0).UTC()) {
		t.Fatalf("integer timestamp = %v", ts.Time)
	}
	if err := json.Unmarshal([]byte(`"1753444800"`), &ts); err != nil {
		t.Fatal(err)
	}
	if ts.Unix() != 1753444800 {
		t.Fatalf("string timestamp = %v", ts.Time)
	}
	for _, empty := range []string{`0`, `""`, `null`} {
		if err := json.Unmarshal([]byte(empty), &ts); err != nil {
			t.Fatalf("%s: %v", empty, err)
		}
		if !ts.IsZero() {
			t.Fatalf("%s decoded to %v, want the zero time", empty, ts.Time)
		}
	}
	if err := json.Unmarshal([]byte(`"not a time"`), &ts); err == nil {
		t.Fatal("expected an error")
	}

	raw, err := json.Marshal(NewUnixTime(time.Unix(1753444800, 0)))
	if err != nil || string(raw) != "1753444800" {
		t.Fatalf("marshal = %s (%v)", raw, err)
	}
	raw, _ = json.Marshal(UnixTime{})
	if string(raw) != "0" {
		t.Fatalf("zero marshal = %s", raw)
	}
}

// §2.6 #7: success bodies are not uniform.
func TestBoolResultDecodesEverySuccessShape(t *testing.T) {
	cases := map[string]bool{
		`true`:              true,
		`false`:             false,
		`{"result":true}`:   true,
		`{"result":"1"}`:    true,
		`{"result":0}`:      false,
		`{"status":"True"}`: true,
		`{}`:                true,
		`"1"`:               true,
		`null`:              false,
	}
	for in, want := range cases {
		var r BoolResult
		if err := json.Unmarshal([]byte(in), &r); err != nil {
			t.Errorf("BoolResult(%s): %v", in, err)
			continue
		}
		if r.OK() != want {
			t.Errorf("BoolResult(%s) = %v, want %v", in, r.OK(), want)
		}
	}
	var r BoolResult
	if err := json.Unmarshal([]byte(`{"result":"maybe"}`), &r); err == nil {
		t.Error("expected an error")
	}
	if err := json.Unmarshal([]byte(`"maybe"`), &r); err == nil {
		t.Error("expected an error")
	}
}

// §2.6 #3: upstream's own spelling is the contract, typos included.
func TestSessionLoginKeepsUpstreamFieldSpelling(t *testing.T) {
	const body = `{"SessionID":"abc","UserName":"derek","AccType":"1","FVersioning":"1",
	               "OwnerSuspendet":"0","max_file_size":"107374182400","Enable2FA":0}`
	var s SessionLogin
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.SessionID != "abc" || s.UserName != "derek" {
		t.Fatalf("decoded %+v", s)
	}
	if s.AccType.Int() != 1 || !s.FVersioning.Bool() {
		t.Fatalf("flexible fields decoded wrong: %+v", s)
	}
	if s.OwnerSuspendet.Bool() {
		t.Error("OwnerSuspendet should be false")
	}
	if s.MaxFileSize.Int64() != 107374182400 {
		t.Errorf("MaxFileSize = %d", s.MaxFileSize.Int64())
	}
}

func TestUserInfoDecodesMixedTypes(t *testing.T) {
	const body = `{"UserID":12345,"UserName":"derek","StorageUsed":"1024","MaxStorage":2048,
	               "AccountCreation":1600000000,"Suspended":"False"}`
	var u UserInfo
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatal(err)
	}
	if u.UserID.String() != "12345" {
		t.Errorf("UserID = %q, want the numeric id as a string", u.UserID)
	}
	if u.StorageUsed.Int64() != 1024 || u.MaxStorage.Int64() != 2048 {
		t.Errorf("quota decoded wrong: %+v", u)
	}
	if u.Suspended.Bool() || u.AccountCreation.IsZero() {
		t.Errorf("decoded %+v", u)
	}
}

// §2.6 #9: the account root is the string "0".
func TestRootFolderIDIsAString(t *testing.T) {
	if RootFolderID != "0" {
		t.Fatalf("RootFolderID = %q", RootFolderID)
	}
	raw, _ := json.Marshal(map[string]string{"folder_id": RootFolderID})
	if string(raw) != `{"folder_id":"0"}` {
		t.Fatalf("root folder marshals as %s, want a quoted zero", raw)
	}
}

// §2.6 #10: reject illegal names locally, with a friendlier message than
// upstream's.
func TestValidateName(t *testing.T) {
	valid := []string{"report.xlsx", "2026 财务", "a", strings.Repeat("x", MaxNameLength), "dot.in.name"}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", truncate(n, 20), err)
		}
	}
	invalid := []string{
		"", ".", "..", "  ",
		`a/b`, `a\b`, `a:b`, `a*b`, `a?b`, `a"b`, `a<b`, `a>b`, `a|b`,
		strings.Repeat("x", MaxNameLength+1),
		"bell\x07", "nul\x00", "del\x7f",
		string([]byte{0xff, 0xfe}),
	}
	for _, n := range invalid {
		err := ValidateName(n)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", truncate(n, 20))
			continue
		}
		if ErrorKind(err) != KindInvalidName {
			t.Errorf("ValidateName(%q) kind = %q, want %q", truncate(n, 20), ErrorKind(err), KindInvalidName)
		}
	}
}

// §2.6 #14: folder/list.json caps a page at 100 entries and needs
// last_request_time on every page after the first.
func TestPaginationProtocol(t *testing.T) {
	first := FirstPage(0)
	if first.EffectiveLimit() != MaxListPageSize {
		t.Fatalf("default page size = %d, want %d", first.EffectiveLimit(), MaxListPageSize)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("first page: %v", err)
	}
	if FirstPage(5000).EffectiveLimit() != MaxListPageSize {
		t.Fatal("page size must be clamped to the upstream cap")
	}
	if FirstPage(20).EffectiveLimit() != 20 {
		t.Fatal("a smaller page size must be honoured")
	}

	second := first.Next(1753444800)
	if second.Offset != MaxListPageSize || second.LastRequestTime != 1753444800 {
		t.Fatalf("second page = %+v", second)
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("second page: %v", err)
	}

	bad := Pagination{Offset: 100}
	if err := bad.Validate(); err == nil {
		t.Fatal("a later page without last_request_time must be rejected")
	} else {
		mustContain(t, err.Error(), "last_request_time", "pagination error")
	}
	if err := (Pagination{Offset: -1}).Validate(); err == nil {
		t.Fatal("negative offset must be rejected")
	}
	if err := (Pagination{Limit: -1}).Validate(); err == nil {
		t.Fatal("negative limit must be rejected")
	}
}

// A name upstream would trim into "." or ".." is not a name. Found by fuzzing
// the Bridge's joinPath, which turned an accepted ". " into the path "/." —
// the parent, not a child (D46).
func TestValidateNameRejectsPaddedDotNames(t *testing.T) {
	for _, name := range []string{". ", " .", " . ", ".. ", " ..", "\t..\t", ".\n"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) allowed a name upstream would store as a dot segment", name)
		}
	}
	// Ordinary names that merely contain a dot are unaffected.
	for _, name := range []string{".hidden", "a.b", "..hidden", "report.pdf", "v1.2..3"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}
