package opendrive

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Kind is the stable, machine readable classification of an error. The values
// mirror the Bridge API error codes defined in whitepaper §4.5 so that the REST
// layer can pass them through unchanged.
type Kind string

// Error kinds. The first ten are the §4.5 enumeration; the remaining ones cover
// failures that never reach the Bridge API surface as-is.
const (
	KindUnauthorized       Kind = "unauthorized"
	KindTokenExpired       Kind = "token_expired"
	KindCaptchaRequired    Kind = "captcha_required"
	KindNotFound           Kind = "not_found"
	KindConflict           Kind = "conflict"
	KindQuotaExceeded      Kind = "quota_exceeded"
	KindBandwidthExceeded  Kind = "bandwidth_exceeded"
	KindInvalidName        Kind = "invalid_name"
	KindUpstreamError      Kind = "upstream_error"
	KindRateLimited        Kind = "rate_limited"
	KindNetwork            Kind = "network"
	KindInvalidResponse    Kind = "invalid_response"
	KindInvalidRequest     Kind = "invalid_request"
	KindRefreshTokenFailed Kind = "refresh_token_failed"
)

// Sentinel errors for errors.Is. Every APIError reports itself as the sentinel
// matching its Kind, so callers can write errors.Is(err, ErrTokenExpired).
var (
	ErrUnauthorized       = &APIError{Kind: KindUnauthorized}
	ErrTokenExpired       = &APIError{Kind: KindTokenExpired}
	ErrCaptchaRequired    = &APIError{Kind: KindCaptchaRequired}
	ErrNotFound           = &APIError{Kind: KindNotFound}
	ErrConflict           = &APIError{Kind: KindConflict}
	ErrQuotaExceeded      = &APIError{Kind: KindQuotaExceeded}
	ErrBandwidthExceeded  = &APIError{Kind: KindBandwidthExceeded}
	ErrInvalidName        = &APIError{Kind: KindInvalidName}
	ErrUpstream           = &APIError{Kind: KindUpstreamError}
	ErrRateLimited        = &APIError{Kind: KindRateLimited}
	ErrNetwork            = &APIError{Kind: KindNetwork}
	ErrInvalidResponse    = &APIError{Kind: KindInvalidResponse}
	ErrInvalidRequest     = &APIError{Kind: KindInvalidRequest}
	ErrRefreshTokenFailed = &APIError{Kind: KindRefreshTokenFailed}
)

// APIError is the single error type the SDK returns for upstream failures. It
// normalises the three error shapes upstream produces (whitepaper §11):
//
//	{"error":{"code":404,"message":"File not exists"}}                 // REST
//	{"error":{"code":401,"error":"invalid_token","error_description":…} // OAuth on a REST call
//	{"error":"invalid_grant","error_description":"…"}                  // OAuth grant
//
// as well as plain-text and boolean error bodies.
//
// Error messages are safe to surface to end users: URLs are redacted and no
// credential ever appears in them (§9.4).
type APIError struct {
	Kind Kind

	// HTTPCode is the HTTP status of the upstream response, 0 for transport
	// level failures.
	HTTPCode int
	// UpstreamCode is the code carried inside the upstream error object, which
	// is not always equal to the HTTP status.
	UpstreamCode int
	// UpstreamMsg is the upstream message or error_description.
	UpstreamMsg string
	// OAuthError is the OAuth2 error identifier (invalid_token,
	// invalid_grant, ...) when the body used the OAuth shape.
	OAuthError string
	// Op describes the call, e.g. "POST /session/login.json".
	Op string
	// URL is the redacted request URL.
	URL string
	// retryAfter is populated from the Retry-After header on 429/503.
	retryAfter time.Duration

	// Err is an optional wrapped cause (transport or decode error).
	Err error
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Kind))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.HTTPCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d", e.HTTPCode)
		if e.UpstreamCode != 0 && e.UpstreamCode != e.HTTPCode {
			fmt.Fprintf(&b, ", upstream %d", e.UpstreamCode)
		}
		b.WriteString(")")
	}
	if e.OAuthError != "" {
		b.WriteString(": ")
		b.WriteString(e.OAuthError)
	}
	if e.UpstreamMsg != "" {
		b.WriteString(": ")
		b.WriteString(e.UpstreamMsg)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the underlying transport or decoding failure.
func (e *APIError) Unwrap() error { return e.Err }

// Is reports whether the error matches one of the package sentinels. Matching
// is by Kind only, so an APIError carrying full upstream detail still satisfies
// errors.Is(err, ErrNotFound).
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	if !ok {
		return false
	}
	return t.Kind == e.Kind
}

// Temporary reports whether retrying the same call has any chance of a
// different outcome. Captcha, quota, bandwidth and name errors never do
// (whitepaper §11).
func (e *APIError) Temporary() bool {
	switch e.Kind {
	case KindRateLimited, KindNetwork:
		return true
	case KindUpstreamError:
		return e.HTTPCode == 0 || e.HTTPCode >= 500
	default:
		return false
	}
}

// RetryAfter returns the delay requested by the server, or 0 when it gave none.
func (e *APIError) RetryAfter() time.Duration { return e.retryAfter }

// IsTemporary reports whether err is an APIError that may be retried.
func IsTemporary(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Temporary()
	}
	return false
}

// RetryAfter returns the server requested delay carried by err, if any.
func RetryAfter(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.RetryAfter()
	}
	return 0
}

// ErrorKind returns the Kind of err, or "" when err is not an APIError.
func ErrorKind(err error) Kind {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return ""
}

// ---------------------------------------------------------------- parsing

// wireError models every error envelope upstream is known to emit. The nested
// object and the bare-string OAuth form differ, hence the json.RawMessage.
type wireError struct {
	Error            json.RawMessage `json:"error"`
	ErrorDescription string          `json:"error_description"`
	Message          string          `json:"message"`
	// Some endpoints answer with {"result":false} instead of an error object.
	Result *FlexBool `json:"result"`
}

type wireErrorObject struct {
	Code             FlexInt `json:"code"`
	Message          string  `json:"message"`
	Error            string  `json:"error"`
	ErrorDescription string  `json:"error_description"`
}

// parseError converts an upstream non-2xx response into an APIError. body may
// be empty; url must already be redacted.
func parseError(status int, header http.Header, body []byte, op, url string) *APIError {
	e := &APIError{HTTPCode: status, Op: op, URL: url}

	var env wireError
	if len(body) > 0 && json.Unmarshal(body, &env) == nil {
		var obj wireErrorObject
		switch {
		case len(env.Error) > 0 && env.Error[0] == '{':
			_ = json.Unmarshal(env.Error, &obj)
		case len(env.Error) > 0 && env.Error[0] == '"':
			// OAuth grant shape: {"error":"invalid_grant","error_description":…}
			var s string
			_ = json.Unmarshal(env.Error, &s)
			obj.Error = s
			obj.ErrorDescription = env.ErrorDescription
		default:
			obj.Message = env.Message
		}
		e.UpstreamCode = obj.Code.Int()
		e.OAuthError = obj.Error
		e.UpstreamMsg = firstNonEmpty(obj.Message, obj.ErrorDescription, env.ErrorDescription, env.Message)
	}
	if e.UpstreamMsg == "" && len(body) > 0 && !isJSON(body) {
		e.UpstreamMsg = truncate(strings.TrimSpace(string(body)), 300)
	}
	e.retryAfter = parseRetryAfter(header)
	e.Kind = classify(status, e.OAuthError, e.UpstreamCode, e.UpstreamMsg)
	return e
}

// classify maps an upstream response onto a Kind. The message sniffing is a
// deliberate concession: upstream reuses HTTP 403 for captcha, quota and
// bandwidth conditions that callers must treat very differently (§11).
func classify(status int, oauthErr string, upstreamCode int, msg string) Kind {
	lower := strings.ToLower(msg)
	switch {
	case oauthErr == "invalid_token", oauthErr == "expired_token":
		return KindTokenExpired
	case oauthErr == "invalid_grant":
		return KindRefreshTokenFailed
	}
	switch {
	case strings.Contains(lower, "captcha"):
		return KindCaptchaRequired
	case strings.Contains(lower, "bandwidth"):
		return KindBandwidthExceeded
	case strings.Contains(lower, "quota"), strings.Contains(lower, "storage limit"),
		strings.Contains(lower, "not enough space"), strings.Contains(lower, "space limit"):
		return KindQuotaExceeded
	case strings.Contains(lower, "invalid file name"), strings.Contains(lower, "invalid folder name"),
		strings.Contains(lower, "illegal character"):
		return KindInvalidName
	}

	code := status
	if code == 0 {
		code = upstreamCode
	}
	switch code {
	case http.StatusUnauthorized:
		return KindUnauthorized
	case http.StatusForbidden:
		return KindUnauthorized
	case http.StatusNotFound:
		return KindNotFound
	case http.StatusConflict:
		return KindConflict
	case http.StatusTooManyRequests:
		return KindRateLimited
	case http.StatusRequestEntityTooLarge:
		return KindQuotaExceeded
	}
	return KindUpstreamError
}

func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// networkError wraps a transport failure.
func networkError(op, url string, err error) *APIError {
	return &APIError{Kind: KindNetwork, Op: op, URL: url, Err: err}
}

// invalidResponse marks a body the SDK could not decode.
func invalidResponse(op, url string, err error) *APIError {
	return &APIError{Kind: KindInvalidResponse, Op: op, URL: url, Err: err}
}

// invalidRequest marks a caller mistake that never reaches the network.
func invalidRequest(format string, args ...any) *APIError {
	return &APIError{Kind: KindInvalidRequest, UpstreamMsg: fmt.Sprintf(format, args...)}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isJSON(b []byte) bool {
	b = []byte(strings.TrimSpace(string(b)))
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
