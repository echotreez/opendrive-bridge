package opendrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file is the single authority on what an upstream failure means.
// docs/error-taxonomy.md is its specification: every rule below corresponds to a
// numbered form there, and a fix applied anywhere else — a status check in a
// binding, a message match in the job engine — is a bug in the process
// (whitepaper §4.5, CLAUDE.md rule 9).
//
// The reason for the indirection is that OpenDrive's status codes and messages
// routinely describe something other than what went wrong. Two whole debugging
// rounds went into single instances of that (D38, D39), so classification is
// driven by evidence gathered rather than by claims read off the response.

// BodyShape records what an error body actually was. It is the most important
// single piece of evidence: a body that is not JSON did not come from the API,
// so nothing in it may be believed about credentials (taxonomy T1).
type BodyShape string

// Body shapes.
const (
	// ShapeUnset marks an error the SDK constructed itself rather than parsed
	// from a response. Such errors are trusted, because we wrote them.
	ShapeUnset BodyShape = ""
	ShapeEmpty BodyShape = "empty"
	ShapeJSON  BodyShape = "json"
	ShapeHTML  BodyShape = "html"
	ShapeText  BodyShape = "text"
)

// Evidence is the complete set of inputs classification may consider. Anything
// outside it is off limits by design.
type Evidence struct {
	// Status is the HTTP status, 0 when the error arrived inside a 200 body.
	Status int
	// Header is the response header, used only for Retry-After and Content-Type.
	Header http.Header
	// Body is the raw response body.
	Body []byte
	// ContentType corroborates the body shape.
	ContentType string
	// Op is "METHOD /path"; Path alone is the endpoint identity, which decides
	// which front end answered (taxonomy T1, T9, T10).
	Op   string
	Path string
	// Scope names the resource the call acted on, for the success witness.
	Scope string
	// URL is already redacted (§9.4).
	URL string
}

// witnessKey identifies "this operation, on this resource". An empty scope
// degrades to the operation alone.
func witnessKey(op, scope string) string {
	if scope == "" {
		return op
	}
	return op + "\x00" + scope
}

// retryState is the classifier's verdict on retryability. It exists so that
// Temporary() can be overridden by evidence rather than inferred again by every
// caller.
type retryState uint8

const (
	retryDefault retryState = iota
	retryYes
	retryNo
)

// verdict is what classification produces.
type verdict struct {
	kind      Kind
	ambiguous bool
	retry     retryState
}

// bodyShape decides what kind of body arrived. Content-Type is corroboration
// only: upstream has been observed sending JSON with no Content-Type at all, and
// the HTML 401 of D38 is recognisable from its first byte either way.
func bodyShape(body []byte, contentType string) BodyShape {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ShapeEmpty
	}
	switch trimmed[0] {
	case '{', '[':
		return ShapeJSON
	case '<':
		return ShapeHTML
	}
	if strings.Contains(strings.ToLower(contentType), "html") {
		return ShapeHTML
	}
	return ShapeText
}

// classifyResponse turns one upstream response into an APIError. It is the only
// producer of wire-derived APIErrors in the SDK.
func classifyResponse(ev Evidence) *APIError {
	e := &APIError{HTTPCode: ev.Status, Op: ev.Op, URL: ev.URL}
	e.scope = ev.Scope
	e.shape = bodyShape(ev.Body, ev.ContentType)
	e.retryAfter = parseRetryAfter(ev.Header)

	switch e.shape {
	case ShapeJSON:
		parseErrorEnvelope(e, ev.Body)
	case ShapeHTML, ShapeText:
		// Kept for the operator's benefit only; it is explicitly not evidence
		// about credentials.
		e.UpstreamMsg = truncate(strings.TrimSpace(string(ev.Body)), 300)
	}

	v := classify(ev, e)
	e.Kind, e.ambiguous, e.retry = v.kind, v.ambiguous, v.retry
	return e
}

// parseErrorEnvelope fills in the upstream fields from a JSON error body,
// normalising the three envelope shapes of whitepaper §11.
func parseErrorEnvelope(e *APIError, body []byte) {
	var env wireError
	if json.Unmarshal(body, &env) != nil {
		return
	}
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

// badCredentialPhrases are the messages upstream uses when it rejects the
// account itself rather than a stale token. They are the trigger for
// KindReauthRequired, which stops every automatic attempt (§2.2 #4a).
var badCredentialPhrases = []string{
	"invalid username or password",
	"invalid username",
	"invalid password",
	"wrong password",
	"incorrect password",
	"username or password",
	"account is suspended",
	"account suspended",
}

// permanentDenialPhrases identify a refusal that is a property of the login
// itself, so no probe is spent and no retry is attempted (taxonomy T3).
var permanentDenialPhrases = []string{
	"account users cannot",
	"restricted user",
}

// ambiguousPermissionPhrases identify the 403 whose text says "permission" but
// whose cause may be transient resource pressure. Upstream sends byte-identical
// messages for both, so this phrase set decides nothing on its own — it only
// marks the error as needing disambiguation (taxonomy T2).
var ambiguousPermissionPhrases = []string{
	"your user access enables you only to view",
	"you do not have permission",
	"permission denied",
}

// sessionExpiredPhrases are the one credential signal upstream states plainly,
// and the only message-derived input allowed to reach the auth state machine
// (taxonomy T12).
var sessionExpiredPhrases = []string{
	"session does not exist",
	"session expired",
	"re-login",
	"invalid session",
}

// classify applies the taxonomy. The order of the checks is part of the
// specification: the body shape comes first because it decides whether anything
// else in the response may be believed at all.
func classify(ev Evidence, e *APIError) verdict {
	// T1 — the response did not come from the API. Nothing about credentials
	// may be inferred from it, whatever status it carries.
	if e.shape == ShapeHTML || e.shape == ShapeText {
		return verdict{kind: KindEdgeRejected, retry: edgeRetry(ev.Status)}
	}

	lower := strings.ToLower(e.UpstreamMsg)

	// T11 — captcha is checked before anything else, everywhere. Retrying or
	// re-authenticating is what turns a throttle into a lockout.
	if strings.Contains(lower, "captcha") {
		return verdict{kind: KindCaptchaRequired, retry: retryNo}
	}

	// The OAuth identifiers are structured, machine-generated and unambiguous.
	switch e.OAuthError {
	case "invalid_token", "expired_token":
		return verdict{kind: KindTokenExpired}
	case "invalid_grant":
		// The refresh token was rejected. Transient by design: the caller falls
		// back to a silent password login.
		return verdict{kind: KindRefreshTokenFailed}
	case "invalid_client", "unauthorized_client", "access_denied":
		return verdict{kind: KindReauthRequired, retry: retryNo}
	}

	switch {
	// T3 before T2: a specific message beats a probe.
	case containsAny(lower, permanentDenialPhrases):
		return verdict{kind: KindUpstreamError, retry: retryNo}
	case containsAny(lower, badCredentialPhrases):
		return verdict{kind: KindReauthRequired, retry: retryNo}
	case strings.Contains(lower, "bandwidth"):
		// T14.
		return verdict{kind: KindBandwidthExceeded, retry: retryNo}
	case strings.Contains(lower, "quota"), strings.Contains(lower, "storage limit"),
		strings.Contains(lower, "not enough space"), strings.Contains(lower, "space limit"):
		return verdict{kind: KindQuotaExceeded, retry: retryNo}
	case strings.Contains(lower, "invalid file name"), strings.Contains(lower, "invalid folder name"),
		strings.Contains(lower, "illegal character"):
		return verdict{kind: KindInvalidName, retry: retryNo}
	}

	// T12 — the one credential signal upstream states plainly. It is checked on
	// the message rather than only on the status, because upstream does not
	// always pair it with a 401: the same sentence turns up inside a 200 body on
	// the endpoints that answer errors that way (§11).
	if containsAny(lower, sessionExpiredPhrases) {
		return verdict{kind: KindTokenExpired}
	}

	code := ev.Status
	if code == 0 {
		code = e.UpstreamCode
	}

	switch code {
	case http.StatusUnauthorized:
		// T12 — a JSON 401 from the API is a statement about the session, and
		// the SDK renews silently. An empty-bodied 401 states nothing at all
		// (T13) and must not be allowed to drive a renewal.
		if e.shape == ShapeJSON {
			return verdict{kind: KindTokenExpired}
		}
		return verdict{kind: KindUpstreamError, ambiguous: true, retry: retryNo}

	case http.StatusForbidden:
		// T2 — the form that cannot be settled by reading the response.
		if containsAny(lower, ambiguousPermissionPhrases) {
			return verdict{kind: KindUpstreamError, ambiguous: true, retry: retryNo}
		}
		// A 403 upstream chose to explain differently is a plain refusal:
		// unauthorized stays reserved for the Bridge API's own auth (§4.5).
		return verdict{kind: KindUpstreamError, retry: retryNo}

	case http.StatusBadRequest:
		// T4, T5, T10 — upstream's 400s describe caller mistakes, including the
		// ones whose wording is misleading. Retrying an encoding error is waste.
		// T9 (the chunk resume point) is also a 400 and is recovered by the
		// upload pipeline before it ever reaches a caller.
		return verdict{kind: KindInvalidRequest, retry: retryNo}

	case http.StatusNotFound:
		return verdict{kind: KindNotFound, retry: retryNo}
	case http.StatusConflict:
		return verdict{kind: KindConflict, retry: retryNo}
	case http.StatusTooManyRequests:
		// T15 — the one status that is trustworthy on its own.
		return verdict{kind: KindRateLimited, retry: retryYes}
	case http.StatusRequestEntityTooLarge:
		return verdict{kind: KindQuotaExceeded, retry: retryNo}
	}

	if code >= 500 || code == 0 {
		return verdict{kind: KindUpstreamError, retry: retryYes}
	}
	// T13 — a status we have no rule for and a body that said nothing.
	return verdict{kind: KindUpstreamError, ambiguous: e.shape == ShapeEmpty, retry: retryNo}
}

// edgeRetry decides whether an edge rejection is worth repeating. A 502 from a
// proxy may well clear; a 401 from one will not, because the credential the edge
// wanted is not the credential that was sent (D38).
func edgeRetry(status int) retryState {
	if status == 0 || status >= 500 || status == http.StatusTooManyRequests {
		return retryYes
	}
	return retryNo
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- disambiguation

// AccessProbe answers exactly one question: does this credential still work?
// It must be idempotent, read-only and cheap, because it runs while a real call
// is waiting on its answer.
type AccessProbe func(context.Context) error

// probeCtxKey marks a context as belonging to a probe, so that a probe which
// itself trips an ambiguous error cannot start another one.
type probeCtxKey struct{}

func withinProbe(ctx context.Context) bool {
	v, _ := ctx.Value(probeCtxKey{}).(bool)
	return v
}

// DefaultProbeInterval is the minimum gap between two access probes. A job
// engine pushing hundreds of files that all trip the same condition therefore
// spends one probe, not hundreds.
const DefaultProbeInterval = 30 * time.Second

// disambiguator resolves the errors classification could not settle from the
// response alone. It holds the two pieces of gathered evidence the taxonomy
// calls for: the probe result and the success witness.
type disambiguator struct {
	probe    AccessProbe
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	last    time.Time
	result  error
	have    bool
	wait    chan struct{} // non-nil while a probe is in flight
	probes  int           // observability, and what the rate-limit test asserts
	witness map[string]bool
	denied  map[string]time.Time
}

const maxWitnessed = 512

func newDisambiguator(probe AccessProbe, interval time.Duration, now func() time.Time) *disambiguator {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	if now == nil {
		now = time.Now
	}
	return &disambiguator{probe: probe, interval: interval, now: now,
		witness: map[string]bool{}, denied: map[string]time.Time{}}
}

// noteSuccess records that this exact operation has worked at least once. It is
// the only evidence that separates a transient permission-shaped refusal from a
// permanent one, because their wire forms are identical (taxonomy T2).
func (d *disambiguator) noteSuccess(key string) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// A success revokes any recorded denial: whatever upstream was doing, it
	// has stopped.
	delete(d.denied, key)
	if len(d.witness) >= maxWitnessed {
		return
	}
	d.witness[key] = true
}

// markDenied records a refusal and reports whether it is the first one for this
// key within the interval.
//
// This is what keeps "one confirming attempt" from becoming "one confirming
// attempt per file": a single permission-shaped 403 proves nothing (D40), so the
// first is worth repeating; the second settles it, and further calls on the same
// operation and resource are refused immediately.
//
// The memory expires with the same interval as the probe, and that matters as
// much as the memory itself. Upstream's refusals come in windows — the sandbox
// has been seen refusing one write endpoint for the better part of a minute and
// then behaving perfectly (D39, revised). A verdict kept for the life of the
// process would let one such window disable an operation until restart, which is
// the same mistake as believing upstream's message: treating a temporary state
// as a permanent fact.
func (d *disambiguator) markDenied(key string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if at, ok := d.denied[key]; ok && now.Sub(at) < d.interval {
		return false
	}
	if len(d.denied) < maxWitnessed || d.denied[key] != (time.Time{}) {
		d.denied[key] = now
	}
	return true
}

func (d *disambiguator) sawSuccess(op string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.witness[op]
}

// check runs the access probe, at most once per interval and at most once
// concurrently. It reports the probe's verdict and whether one was available at
// all.
func (d *disambiguator) check(ctx context.Context) (error, bool) {
	if d == nil || d.probe == nil || withinProbe(ctx) {
		return nil, false
	}

	d.mu.Lock()
	if d.have && d.now().Sub(d.last) < d.interval {
		res := d.result
		d.mu.Unlock()
		return res, true
	}
	if d.wait != nil {
		wait := d.wait
		d.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false
		}
		d.mu.Lock()
		res, ok := d.result, d.have
		d.mu.Unlock()
		return res, ok
	}
	wait := make(chan struct{})
	d.wait = wait
	d.probes++
	probe := d.probe
	d.mu.Unlock()

	err := probe(context.WithValue(ctx, probeCtxKey{}, true))

	d.mu.Lock()
	d.result, d.have, d.last, d.wait = err, true, d.now(), nil
	d.mu.Unlock()
	close(wait)
	return err, true
}

func (d *disambiguator) probeCount() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probes
}

// resolve settles an ambiguous error using gathered evidence. It is the only
// place in the SDK permitted to spend a network call on classification.
//
// The outcome is deliberately conservative. When the probe cannot run, or the
// operation has never succeeded, the error stays permanent: failing closed costs
// one avoidable error report, while failing open costs a retry storm against an
// account that is already refusing us.
func (c *Client) resolve(ctx context.Context, e *APIError) {
	if e == nil || !e.ambiguous || e.resolved {
		return
	}
	probeErr, decided := c.amb.check(ctx)
	if !decided {
		return
	}
	e.resolved = true

	if probeErr != nil {
		e.retry = retryNo
		// The probe's own failure is API-shaped evidence about the credential,
		// so it may be propagated — that is the one path by which a credential
		// verdict can come out of an ambiguous error, and it comes from a clean
		// response rather than from a guess.
		var pe *APIError
		if errors.As(probeErr, &pe) && pe.decisive() {
			switch pe.Kind {
			case KindReauthRequired, KindCaptchaRequired, KindKeystoreUnavailable:
				e.Kind = pe.Kind
				e.diagnosis = "the account itself is refusing this credential; " +
					"the original message about permissions was misleading"
				return
			}
		}
		e.diagnosis = "upstream refused a plain read as well, so this is a real restriction on the account"
		return
	}

	// The credential works. Whatever this is, it is not an auth problem — the
	// remaining question is only how much retrying the evidence justifies.
	key := witnessKey(e.Op, e.scope)
	switch {
	case c.amb.sawSuccess(key):
		// This exact call has worked on this exact resource. A refusal now is
		// upstream having a moment, not a rule — measured on the sandbox, about
		// one folder/trash.json call in twenty is refused this way and succeeds
		// again seconds later (D40).
		e.retry = retryYes
		e.diagnosis = "upstream reported a permission problem, but the credential is working and " +
			"this same call has already succeeded on this resource, so it is being treated as " +
			"a transient refusal"

	case c.amb.markDenied(key):
		// Never seen to work here, and refused once. One refusal settles
		// nothing, so it gets exactly one more attempt.
		e.retry, e.retryBudget = retryYes, 1
		e.diagnosis = "upstream reported a permission problem and the credential is working; " +
			"one refusal is not conclusive, so this is being attempted once more"

	default:
		// Refused again. Now it is believed, and remembered: every later call
		// on this operation and resource fails immediately, so a bulk job
		// against something it truly may not touch costs one extra call in
		// total rather than one per item.
		e.retry = retryNo
		e.diagnosis = "the credential is working and upstream refused this twice, so it is a real " +
			"restriction on this operation rather than a credential problem"
	}
}

// defaultAccessProbe is the probe a Client uses unless given another: one
// account-information read, which every authenticated login may perform and
// which changes nothing.
func (c *Client) defaultAccessProbe(ctx context.Context) error {
	if c.auth == nil {
		return nil
	}
	var out AccountInfo
	return c.Do(ctx, Request{
		Method:           http.MethodGet,
		Path:             EndpointUsersInfo,
		SessionPlacement: SessionInPath,
		Retryable:        Retryable(false),
	}, &out)
}
