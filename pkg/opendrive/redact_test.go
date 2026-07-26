package opendrive

import (
	"strings"
	"testing"
)

// §9.4: the access token rides in the query string, so every URL that can be
// logged has to be scrubbed first.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		absent  []string
		present []string
	}{
		{
			name:    "oauth call",
			in:      "https://dev.opendrive.com/api/v1/file/info.json?session_id=OAUTH&access_token=abc123&file_id=7",
			absent:  []string{"abc123"},
			present: []string{"session_id=OAUTH", "access_token=REDACTED", "file_id=7"},
		},
		{
			name:    "session call",
			in:      "https://dev.opendrive.com/api/v1/session/info.json?session_id=SID-987",
			absent:  []string{"SID-987"},
			present: []string{"session_id=REDACTED"},
		},
		{
			name:    "download all uses session_key",
			in:      "https://dev.opendrive.com/api/v1/download/all.json?session_key=SID-987",
			absent:  []string{"SID-987"},
			present: []string{"session_key=REDACTED"},
		},
		{
			name:    "password reset style parameters",
			in:      "https://x/api/v1/users/password.json?passwd=hunter2&new_password=hunter3&temp_key=tk1",
			absent:  []string{"hunter2", "hunter3", "tk1"},
			present: []string{"REDACTED"},
		},
		{
			name:    "credentials in userinfo",
			in:      "https://user:secret@dev.opendrive.com/api/v1/x.json",
			absent:  []string{"secret"},
			present: []string{"REDACTED@"},
		},
		{
			name:    "nothing to redact",
			in:      "https://dev.opendrive.com/api/v1/session/captcharequired.json?username=derek",
			present: []string{"username=derek"},
		},
		{
			name:    "empty parameter value is left alone",
			in:      "https://x/api/v1/x.json?session_id=",
			present: []string{"session_id="},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			for _, a := range tc.absent {
				mustNotContain(t, got, a, "redacted URL")
			}
			for _, p := range tc.present {
				mustContain(t, got, p, "redacted URL")
			}
		})
	}

	if got := RedactURL("://nonsense"); got != "<redacted url>" {
		t.Fatalf("an unparseable URL must be dropped entirely, got %q", got)
	}
}

func TestRedactString(t *testing.T) {
	tests := []struct {
		in     string
		absent string
	}{
		{`GET https://x/api/v1/x.json?access_token=abc123 failed`, "abc123"},
		{`{"refresh_token":"rt-secret","expires_in":86400}`, "rt-secret"},
		{`login body {"username":"derek","passwd":"hunter2"}`, "hunter2"},
		{`session_id=SID-1 and session_key=SID-2`, "SID-1"},
		{`Password: "swordfish", captcha_response: "xyz"`, "swordfish"},
	}
	for _, tc := range tests {
		got := RedactString(tc.in)
		mustNotContain(t, got, tc.absent, "redacted string")
		mustContain(t, got, Redacted, "redacted string")
	}

	// The OAuth marker is not a secret and stays readable.
	if got := RedactString(`session_id=OAUTH&access_token=abc`); !strings.Contains(got, "session_id=OAUTH") {
		t.Fatalf("the OAUTH marker should survive: %q", got)
	}
	if RedactString("") != "" {
		t.Fatal("empty input must stay empty")
	}
	if got := RedactString("nothing sensitive here"); got != "nothing sensitive here" {
		t.Fatalf("harmless text was mangled: %q", got)
	}
}

// A token must not survive anywhere in an error chain that a user might see.
func TestRedactStringCoversWrappedTransportErrors(t *testing.T) {
	raw := `Get "https://dev.opendrive.com/api/v1/users/info.json?access_token=tok-987&session_id=OAUTH": dial tcp: i/o timeout`
	got := RedactString(raw)
	mustNotContain(t, got, "tok-987", "wrapped transport error")
	mustContain(t, got, "i/o timeout", "wrapped transport error")
}
