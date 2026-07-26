package opendrive

import (
	"net/url"
	"regexp"
	"strings"
)

// Redacted is the placeholder substituted for any credential.
const Redacted = "REDACTED"

// sensitiveParams are the query parameters and JSON fields that must never
// appear in a log line, an error message or a crash report (whitepaper §9.4).
var sensitiveParams = []string{
	"access_token", "refresh_token", "session_id", "session_key",
	"passwd", "password", "new_password", "old_password", "temp_key",
	"captcha_response", "client_secret", "api_key",
}

// RedactURL returns raw with every credential-bearing query parameter masked.
// The magic OAuth session value is preserved because it is not a secret and
// makes logs far easier to read (§2.2 B).
//
// A URL that cannot be parsed is replaced wholesale rather than risking a leak.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<redacted url>"
	}
	if u.User != nil {
		u.User = url.User(Redacted)
	}
	q := u.Query()
	changed := false
	for _, p := range sensitiveParams {
		vals, ok := q[p]
		if !ok {
			continue
		}
		for i, v := range vals {
			if v == "" || v == OAuthSessionID {
				continue
			}
			vals[i] = Redacted
			changed = true
		}
		q[p] = vals
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// tokenPattern matches "key=value" and "key":"value" forms for every sensitive
// parameter, so that both URLs and JSON fragments embedded in free text are
// covered.
var tokenPattern = regexp.MustCompile(
	`(?i)("?\b(?:` + strings.Join(sensitiveParams, "|") + `)\b"?\s*[:=]\s*"?)([^&"'\s,}]+)`)

// RedactString masks credentials anywhere inside an arbitrary string: log
// messages, upstream error text, wrapped transport errors (§9.4).
func RedactString(s string) string {
	if s == "" {
		return s
	}
	return tokenPattern.ReplaceAllStringFunc(s, func(m string) string {
		loc := tokenPattern.FindStringSubmatch(m)
		if len(loc) != 3 {
			return m
		}
		if loc[2] == OAuthSessionID {
			return m
		}
		return loc[1] + Redacted
	})
}
