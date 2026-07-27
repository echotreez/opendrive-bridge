package opendrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults for a Client created with New.
const (
	DefaultBaseURL   = "https://dev.opendrive.com/api/v1"
	DefaultUserAgent = "opendrive-bridge/0.1 (+https://github.com/StormRealm/opendrive-bridge)"

	// OAuthSessionID is the magic value the session_id parameter must carry
	// when the call is authenticated with an OAuth2 access token
	// (whitepaper §2.2 B, §2.6 #8).
	OAuthSessionID = "OAUTH"

	// DefaultSessionParam is the name of the session parameter on nearly every
	// endpoint. download/all.json is the known exception and uses
	// "session_key" instead (§2.6 #2).
	DefaultSessionParam = "session_id"

	maxResponseBytes = 64 << 20
)

// SessionPlacement says where the session parameter belongs for an endpoint.
// Upstream is not consistent about this, which is why it is per request
// (§1.2, §2.6 #4).
type SessionPlacement int

// Session placements.
const (
	// SessionAuto puts the session in the JSON body when the request has one
	// and in the query string otherwise.
	SessionAuto SessionPlacement = iota
	// SessionInBody puts it in the JSON body.
	SessionInBody
	// SessionInQuery puts it in the query string.
	SessionInQuery
	// SessionInPath appends it as a path segment, as
	// /folder/list.json/{session_id}/{folder_id} does.
	SessionInPath
	// SessionOmit sends no session at all (login, oauth2/grant).
	SessionOmit
)

// Request describes one upstream call. Zero values are sensible: a GET with no
// body carries the session in the query string, a POST carries it in the body.
type Request struct {
	// Method is an HTTP method. Required.
	Method string
	// Path is the endpoint path relative to the base URL, e.g.
	// "/folder/list.json". A leading "/v1" is tolerated and de-duplicated
	// against the base URL (§2.6 #12).
	Path string
	// PathSegments are appended after the path (and after the session segment
	// when SessionInPath is used), already unescaped.
	PathSegments []string
	// Query holds additional query parameters.
	Query url.Values
	// Body is marshalled as JSON. Use a map or a struct; nil sends no body.
	Body any
	// SessionParam overrides the name of the session parameter
	// (DefaultSessionParam when empty; "session_key" for download/all.json).
	SessionParam string
	// SessionPlacement says where the session parameter goes.
	SessionPlacement SessionPlacement
	// Retryable marks the call as safe to replay after a temporary failure.
	// When nil, GET and HEAD are retryable and nothing else is.
	Retryable *bool
	// Accept overrides the Accept header.
	Accept string
	// RawResponse returns the successful response body as []byte instead of
	// decoding it as JSON. It is for endpoints such as file/thumb.json that
	// return image bytes, while retaining the normal authentication, retry and
	// upstream-error handling path.
	//
	// When set, out must be a *[]byte.
	RawResponse bool
}

// Retryable returns a pointer to v, for use with Request.Retryable.
func Retryable(v bool) *bool { return &v }

func (r *Request) retryable() bool {
	if r.Retryable != nil {
		return *r.Retryable
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

// RetryPolicy controls the exponential backoff applied to temporary failures
// (whitepaper §10.1: 1s, 2s, 4s ... capped, with jitter).
type RetryPolicy struct {
	// Max is the number of retries after the first attempt. 0 disables retrying.
	Max int
	// Base is the first backoff interval.
	Base time.Duration
	// Cap is the upper bound for a single backoff interval.
	Cap time.Duration
	// Jitter randomises a computed interval. Defaults to full jitter; tests
	// can supply an identity function for determinism.
	Jitter func(time.Duration) time.Duration
}

// DefaultRetryPolicy is the policy a Client uses unless told otherwise.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Max: 5, Base: time.Second, Cap: time.Minute}
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	base := p.Base
	if base <= 0 {
		base = time.Second
	}
	capped := p.Cap
	if capped <= 0 {
		capped = time.Minute
	}
	d := base << attempt //nolint:gosec // attempt is bounded by Max
	if d > capped || d <= 0 {
		d = capped
	}
	if p.Jitter != nil {
		return p.Jitter(d)
	}
	// Full jitter: sleep for a random duration in [d/2, d].
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// Client is the HTTP core of the SDK: it builds requests, injects credentials,
// normalises errors, retries temporary failures and replays a request once
// after refreshing an expired token (whitepaper §3.2 pkg/opendrive/client.go).
//
// A Client is safe for concurrent use.
type Client struct {
	base      *url.URL
	hc        *http.Client
	ua        string
	log       *slog.Logger
	auth      Authenticator
	pathCache PathCache
	retry     RetryPolicy
	sleep     func(context.Context, time.Duration) error
	newReqID  func() string
	debugBody bool
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the upstream base URL.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if u, err := url.Parse(strings.TrimRight(raw, "/")); err == nil {
			c.base = u
		}
	}
}

// WithHTTPClient supplies the underlying http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.ua = ua
		}
	}
}

// WithLogger sets the structured logger. Tokens are redacted before anything
// is logged (§9.4).
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithAuthenticator attaches an Authenticator.
func WithAuthenticator(a Authenticator) Option {
	return func(c *Client) { c.auth = a }
}

// WithRetryPolicy overrides the retry policy.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *Client) { c.retry = p }
}

// WithSleepFunc replaces the backoff sleep, for tests.
func WithSleepFunc(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if f != nil {
			c.sleep = f
		}
	}
}

// WithBodyLogging enables debug-level logging of response bodies. Bodies are
// still redacted, and the option is off by default (§9.4).
func WithBodyLogging(on bool) Option {
	return func(c *Client) { c.debugBody = on }
}

// New creates a Client.
func New(opts ...Option) (*Client, error) {
	base, err := url.Parse(DefaultBaseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base:     base,
		hc:       &http.Client{Timeout: 5 * time.Minute},
		ua:       DefaultUserAgent,
		log:      slog.New(discardHandler{}),
		retry:    DefaultRetryPolicy(),
		sleep:    sleepCtx,
		newReqID: newRequestID,
	}
	for _, o := range opts {
		o(c)
	}
	if c.base == nil || c.base.Scheme == "" || c.base.Host == "" {
		return nil, invalidRequest("base URL must be absolute")
	}
	return c, nil
}

// BaseURL returns the configured upstream base URL.
func (c *Client) BaseURL() string { return c.base.String() }

// SetAuthenticator attaches an Authenticator after construction. It exists
// because an Authenticator needs the Client to perform its own calls.
func (c *Client) SetAuthenticator(a Authenticator) { c.auth = a }

// Logger returns the client logger.
func (c *Client) Logger() *slog.Logger { return c.log }

// Do performs a request, decoding a successful JSON response into out (which
// may be nil). Errors are always *APIError.
func (c *Client) Do(ctx context.Context, r Request, out any) error {
	if r.Method == "" {
		return invalidRequest("request method must not be empty")
	}
	if r.Path == "" {
		return invalidRequest("request path must not be empty")
	}
	if ctx == nil {
		return invalidRequest("context must not be nil")
	}

	var refreshed bool
	for attempt := 0; ; {
		creds, apiErr := c.attempt(ctx, r, out)
		if apiErr == nil {
			return nil
		}

		// An expired access token is not a retry: renew once, then replay the
		// very same request (§2.2 #3). Requests that carry no session at all —
		// login and grant — are excluded: renewing in response to their 401
		// would recurse straight back into the same call.
		if apiErr.Kind == KindTokenExpired && c.auth != nil && !refreshed &&
			r.SessionPlacement != SessionOmit {
			refreshed = true
			if err := c.refreshAuth(ctx, creds.AccessToken); err != nil {
				return err
			}
			continue
		}

		if !r.retryable() || !apiErr.Temporary() || attempt >= c.retry.Max {
			return apiErr
		}
		delay := apiErr.RetryAfter()
		if delay <= 0 {
			delay = c.retry.backoff(attempt)
		}
		c.log.Debug("retrying upstream call",
			slog.String("op", apiErr.Op), slog.Int("attempt", attempt+1),
			slog.Duration("delay", delay), slog.String("kind", string(apiErr.Kind)))
		if err := c.sleep(ctx, delay); err != nil {
			return networkError(apiErr.Op, apiErr.URL, err)
		}
		attempt++
	}
}

// refreshAuth renews credentials after a 401. When the authenticator can tell
// stale refreshes apart it is told which access token failed, so that parallel
// requests hitting the same expiry only rotate the refresh token once (§9.2).
func (c *Client) refreshAuth(ctx context.Context, staleAccessToken string) error {
	if sr, ok := c.auth.(StaleTokenRefresher); ok {
		return sr.RefreshStale(ctx, staleAccessToken)
	}
	return c.auth.Refresh(ctx)
}

// attempt performs exactly one HTTP round trip, returning the credentials it
// used so that the caller can detect a stale-token refresh.
func (c *Client) attempt(ctx context.Context, r Request, out any) (Credentials, *APIError) {
	op := r.Method + " " + r.Path

	var creds Credentials
	if r.SessionPlacement != SessionOmit && c.auth != nil {
		var err error
		creds, err = c.auth.Credentials(ctx)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) {
				return creds, ae
			}
			return creds, &APIError{Kind: KindUnauthorized, Op: op, Err: err}
		}
	}

	u, payload, err := c.buildURL(r, creds)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			return creds, ae
		}
		return creds, invalidRequest("%v", err)
	}
	safeURL := RedactURL(u.String())

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), reader)
	if err != nil {
		return creds, invalidRequest("%v", err)
	}
	req.Header.Set("User-Agent", c.ua)
	accept := r.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	reqID := c.newReqID()
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log.Warn("upstream call failed",
			slog.String("request_id", reqID), slog.String("op", op),
			slog.String("url", safeURL), slog.Duration("took", time.Since(start)),
			slog.String("error", RedactString(err.Error())))
		return creds, networkError(op, safeURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	took := time.Since(start)

	attrs := []any{
		slog.String("request_id", reqID), slog.String("op", op),
		slog.String("url", safeURL), slog.Int("status", resp.StatusCode),
		slog.Duration("took", took),
	}
	if c.debugBody {
		attrs = append(attrs, slog.String("body", RedactString(truncate(string(raw), 512))))
	}
	c.log.Debug("upstream call", attrs...)

	if readErr != nil {
		return creds, networkError(op, safeURL, readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return creds, parseError(resp.StatusCode, resp.Header, raw, op, safeURL)
	}

	// Some endpoints answer 200 with an error envelope in the body.
	if apiErr := errorInBody(raw, op, safeURL); apiErr != nil {
		return creds, apiErr
	}

	if r.RawResponse {
		if out == nil {
			return creds, nil
		}
		bytesOut, ok := out.(*[]byte)
		if !ok {
			return creds, invalidRequest("raw response for %s needs a *[]byte output", op)
		}
		*bytesOut = append((*bytesOut)[:0], raw...)
		return creds, nil
	}

	if out == nil {
		return creds, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return creds, invalidResponse(op, safeURL, errors.New("empty response body"))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return creds, invalidResponse(op, safeURL, err)
	}
	return creds, nil
}

// errorInBody detects a 200 response that actually carries an error envelope.
func errorInBody(raw []byte, op, url string) *APIError {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil || len(probe.Error) == 0 {
		return nil
	}
	if string(probe.Error) == "null" || string(probe.Error) == `""` {
		return nil
	}
	return parseError(0, nil, trimmed, op, url)
}

// buildURL assembles the final URL and JSON payload, injecting the session
// parameter in the position the endpoint expects and the OAuth access token in
// the query string (§2.2 B, §2.6 #2, #8, #12).
func (c *Client) buildURL(r Request, creds Credentials) (*url.URL, []byte, error) {
	u := *c.base
	u.Path = joinPath(c.base.Path, r.Path)

	query := url.Values{}
	for k, vs := range r.Query {
		for _, v := range vs {
			query.Add(k, v)
		}
	}

	param := r.SessionParam
	if param == "" {
		param = DefaultSessionParam
	}

	placement := r.SessionPlacement
	if placement == SessionAuto {
		if r.Body != nil {
			placement = SessionInBody
		} else {
			placement = SessionInQuery
		}
	}

	sessionValue := creds.SessionID
	if creds.AccessToken != "" {
		// OAuth mode: the session parameter carries the magic value and the
		// token rides in the query string, whatever the verb.
		sessionValue = OAuthSessionID
		query.Set("access_token", creds.AccessToken)
	}

	payload, err := marshalBody(r.Body)
	if err != nil {
		return nil, nil, err
	}

	if placement != SessionOmit && sessionValue != "" {
		switch placement {
		case SessionInQuery:
			query.Set(param, sessionValue)
		case SessionInPath:
			u.Path = strings.TrimRight(u.Path, "/") + "/" + url.PathEscape(sessionValue)
		case SessionInBody:
			payload, err = injectBodyField(payload, param, sessionValue)
			if err != nil {
				return nil, nil, err
			}
		case SessionAuto, SessionOmit:
			// unreachable: resolved above
		}
	}

	for _, seg := range r.PathSegments {
		if seg == "" {
			continue
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/" + url.PathEscape(seg)
	}
	u.RawQuery = query.Encode()
	return &u, payload, nil
}

func marshalBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, invalidRequest("cannot encode request body: %v", err)
	}
	return raw, nil
}

// injectBodyField adds a field to an already marshalled JSON object, creating
// the object when the request had no body.
func injectBodyField(payload []byte, key, value string) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &fields); err != nil {
			return nil, invalidRequest("request body must be a JSON object to carry %s", key)
		}
	}
	if _, exists := fields[key]; !exists {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, invalidRequest("cannot encode %s: %v", key, err)
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

// joinPath concatenates the base path and the endpoint path, collapsing a
// duplicated "/v1" prefix. The PDF writes some endpoints as /v1/file/... even
// though the base URL already ends in /v1 (§2.6 #12).
func joinPath(basePath, endpoint string) string {
	basePath = strings.TrimRight(basePath, "/")
	endpoint = "/" + strings.TrimLeft(endpoint, "/")
	if base := lastSegment(basePath); base != "" {
		if endpoint == "/"+base || strings.HasPrefix(endpoint, "/"+base+"/") {
			endpoint = strings.TrimPrefix(endpoint, "/"+base)
			if endpoint == "" {
				endpoint = "/"
			}
		}
	}
	return basePath + endpoint
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------------------------------------------------------------- helpers

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func newRequestID() string {
	var b [8]byte
	for i := range b {
		b[i] = byte(rand.IntN(256)) //nolint:gosec // ids are for log correlation only
	}
	return fmt.Sprintf("%x", b)
}

// discardHandler is a slog handler that drops everything, so that a Client
// without an explicit logger stays silent.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
