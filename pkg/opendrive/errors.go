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

// Error kinds, mapping one-to-one onto the Bridge API error enumeration of
// whitepaper §4.5 (v1.1).
//
// The three credential kinds are deliberately distinct, because the daemon has
// to react differently to each:
//
//	KindKeystoreUnavailable — the credential store cannot be read. No upstream
//	    request may be made at all: retrying with no credentials would walk the
//	    account into a captcha lock. Recovery is local (unlock the keyring).
//	KindReauthRequired      — upstream rejected the stored credentials, so the
//	    password has changed (or the bridge was never configured). All automatic
//	    attempts stop until the user supplies a new password.
//	KindTokenExpired        — transient: the SDK refreshes or silently logs in
//	    again, so callers rarely observe it.
//
// KindUnauthorized is reserved for the Bridge API's own authentication (a wrong
// API key on a non-loopback listener). The SDK never produces it for an
// upstream credential problem.
const (
	KindKeystoreUnavailable Kind = "keystore_unavailable"
	KindReauthRequired      Kind = "reauth_required"
	KindTokenExpired        Kind = "token_expired"
	KindCaptchaRequired     Kind = "captcha_required"
	KindUnauthorized        Kind = "unauthorized"
	// KindEdgeRejected means the response did not come from the API at all: a
	// proxy in front of it refused the request and answered in HTML. Nothing in
	// such a response says anything about credentials, so it is kept separate
	// from every credential kind on purpose (docs/error-taxonomy.md T1, D38).
	KindEdgeRejected      Kind = "edge_rejected"
	KindNotFound          Kind = "not_found"
	KindConflict          Kind = "conflict"
	KindQuotaExceeded     Kind = "quota_exceeded"
	KindBandwidthExceeded Kind = "bandwidth_exceeded"
	KindInvalidName       Kind = "invalid_name"
	KindUpstreamError     Kind = "upstream_error"
	KindRateLimited       Kind = "rate_limited"
	KindNetwork           Kind = "network"
	KindInvalidResponse   Kind = "invalid_response"
	KindInvalidRequest    Kind = "invalid_request"
	// KindRefreshTokenFailed is internal and transient: it means the refresh
	// grant was rejected, which sends the authenticator down the silent
	// re-login path (§2.2 #3). Once that succeeds the caller never sees it; if
	// it fails the error becomes KindReauthRequired.
	KindRefreshTokenFailed Kind = "refresh_token_failed"
)

// Sentinel errors for errors.Is. Every APIError reports itself as the sentinel
// matching its Kind, so callers can write errors.Is(err, ErrTokenExpired).
var (
	ErrKeystoreUnavailable = &APIError{Kind: KindKeystoreUnavailable}
	ErrReauthRequired      = &APIError{Kind: KindReauthRequired}
	ErrUnauthorized        = &APIError{Kind: KindUnauthorized}
	ErrTokenExpired        = &APIError{Kind: KindTokenExpired}
	ErrCaptchaRequired     = &APIError{Kind: KindCaptchaRequired}
	ErrEdgeRejected        = &APIError{Kind: KindEdgeRejected}
	ErrNotFound            = &APIError{Kind: KindNotFound}
	ErrConflict            = &APIError{Kind: KindConflict}
	ErrQuotaExceeded       = &APIError{Kind: KindQuotaExceeded}
	ErrBandwidthExceeded   = &APIError{Kind: KindBandwidthExceeded}
	ErrInvalidName         = &APIError{Kind: KindInvalidName}
	ErrUpstream            = &APIError{Kind: KindUpstreamError}
	ErrRateLimited         = &APIError{Kind: KindRateLimited}
	ErrNetwork             = &APIError{Kind: KindNetwork}
	ErrInvalidResponse     = &APIError{Kind: KindInvalidResponse}
	ErrInvalidRequest      = &APIError{Kind: KindInvalidRequest}
	ErrRefreshTokenFailed  = &APIError{Kind: KindRefreshTokenFailed}
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

	// shape is what the response body actually was. ShapeUnset means the SDK
	// built this error itself rather than reading it off the wire.
	shape BodyShape
	// scope is the resource the failed call acted on, for the success witness.
	scope string
	// ambiguous marks an error whose cause the response does not determine —
	// the permission-shaped 403 above all (docs/error-taxonomy.md T2). It is
	// barred from the auth state machine until resolved.
	ambiguous bool
	// resolved records that disambiguation has run, so it runs once.
	resolved bool
	// retry is the classifier's verdict on retryability, overriding the
	// kind-based default when set.
	retry retryState
	// retryBudget caps the attempts for a verdict the evidence supports only
	// weakly. 0 means the client's normal policy applies.
	retryBudget int
	// diagnosis explains, in words fit for a user, what the classifier
	// concluded when the upstream message was misleading.
	diagnosis string

	// Err is an optional wrapped cause (transport or decode error).
	Err error
}

// BodyShape reports what the response body was. It is exported because the
// distinction between an API answer and a proxy's HTML page is the difference
// between a credential problem and something that merely looks like one.
func (e *APIError) BodyShape() BodyShape { return e.shape }

// Ambiguous reports whether the response left the cause undetermined.
func (e *APIError) Ambiguous() bool { return e.ambiguous }

// Diagnosis returns the classifier's explanation when upstream's own message is
// known to be misleading, or "" when the message can be taken at face value.
// The Bridge REST layer and the job engine present this instead of the raw
// upstream text, so that resource pressure is never reported as a permission
// failure (docs/error-taxonomy.md T2).
func (e *APIError) Diagnosis() string { return e.diagnosis }

// decisive reports whether this error is trustworthy enough to act on: it came
// from the API in JSON (or the SDK built it), and nothing about it is
// unresolved.
func (e *APIError) decisive() bool {
	return (e.shape == ShapeUnset || e.shape == ShapeJSON) && (!e.ambiguous || e.resolved)
}

// drivesAuth reports whether this error may reach the authentication state
// machine. This is the invariant of docs/error-taxonomy.md, and the reason D38
// cannot recur: an HTML 401 from a proxy has ShapeHTML, so it can never trigger
// a refresh, a silent re-login or a reauth_required verdict.
func (e *APIError) drivesAuth() bool {
	if e == nil {
		return false
	}
	if e.ambiguous && !e.resolved {
		return false
	}
	return e.shape == ShapeUnset || e.shape == ShapeJSON
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
		// A wrapped cause is not ours and may carry anything. Go's own
		// *url.Error, for one, quotes the full request URL — access token and
		// all — so the message is redacted on the way out (§9.4). Unwrap still
		// returns the original for callers that need to inspect it.
		b.WriteString(RedactString(e.Err.Error()))
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
//
// This is the only retry authority in the codebase: the transfer pipeline, the
// job engine and the Bridge REST layer consume it and never re-derive it from a
// status code or a message (docs/error-taxonomy.md).
func (e *APIError) Temporary() bool {
	if e.retry != retryDefault {
		return e.retry == retryYes
	}
	switch e.Kind {
	case KindRateLimited, KindNetwork:
		return true
	case KindUpstreamError, KindEdgeRejected:
		return e.HTTPCode == 0 || e.HTTPCode >= 500
	default:
		return false
	}
}

// RetryAfter returns the delay requested by the server, or 0 when it gave none.
func (e *APIError) RetryAfter() time.Duration { return e.retryAfter }

// RetryBudget returns the maximum number of retries this particular error
// justifies, or 0 when the caller's normal policy applies. It is set when the
// evidence supports retrying only weakly (docs/error-taxonomy.md T2).
func (e *APIError) RetryBudget() int { return e.retryBudget }

// replaySafe reports whether the request may be repeated whatever its method.
//
// A refusal is the one failure that is safe to replay unconditionally: upstream
// declined to act, so nothing happened and a second attempt cannot duplicate
// anything. That is not true of a 5xx or a timeout, where the write may well
// have landed — which is why those still respect Request.Retryable.
func (e *APIError) replaySafe() bool {
	return e.HTTPCode == http.StatusForbidden || e.HTTPCode == http.StatusTooManyRequests
}

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

// parseError converts an upstream response into an APIError. It is a thin
// adapter onto classify.go, which holds every rule; nothing here decides
// anything (docs/error-taxonomy.md).
func parseError(status int, header http.Header, body []byte, op, url string) *APIError {
	var contentType string
	if header != nil {
		contentType = header.Get("Content-Type")
	}
	return classifyResponse(Evidence{
		Status:      status,
		Header:      header,
		Body:        body,
		ContentType: contentType,
		Op:          op,
		Path:        endpointOf(op),
		URL:         url,
	})
}

// endpointOf extracts the endpoint identity from an op string ("METHOD /path").
// Which front end answers, and which parameter conventions apply, depend on it
// (docs/error-taxonomy.md, D38).
func endpointOf(op string) string {
	if i := strings.IndexByte(op, ' '); i >= 0 {
		return op[i+1:]
	}
	return op
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
