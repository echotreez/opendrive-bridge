package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// stubAuth is a fixed session, enough to drive the classifier.
type stubAuth struct{}

func (stubAuth) Credentials(context.Context) (opendrive.Credentials, error) {
	return opendrive.Credentials{SessionID: "s1"}, nil
}
func (stubAuth) Refresh(context.Context) error { return nil }
func (stubAuth) Identity() opendrive.Identity {
	return opendrive.Identity{Username: "test", AuthMode: opendrive.AuthModeOAuth2, Seamless: true}
}
func (stubAuth) AuthState() opendrive.AuthState { return opendrive.StateAuthenticated }

func renderError(t *testing.T, err error) (int, Error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/ls", nil)
	WriteError(rec, req, err)

	var env errorEnvelope
	if decErr := json.Unmarshal(rec.Body.Bytes(), &env); decErr != nil {
		t.Fatalf("the envelope is not valid JSON: %v (%s)", decErr, rec.Body.String())
	}
	return rec.Code, env.Error
}

// jargon is wording a user cannot act on: the vocabulary of OpenDrive's API and
// of the bridge's own internals.
//
// It deliberately does not ban every technical word. "API key" is the right
// term for somebody configuring a daemon — it is what their config file calls
// it — so banning it would push the message towards vagueness, which is the
// opposite of the goal. What is banned is anything a user has no way to see or
// act on: OpenDrive's endpoints, its parameter names, our classifier's
// vocabulary.
var jargon = []string{
	"upstream", "the api", "opendrive api", "endpoint", "json", "http status",
	"session_id", "file_id", "folder_id", "temp_location", "chunk_offset",
	"classif", "kind:", "not_found", "upstream_error", "invalid_response",
}

func assertUserReadable(t *testing.T, msg string) {
	t.Helper()
	if msg == "" {
		t.Fatal("no message: a user is shown nothing")
	}
	if !strings.HasSuffix(strings.TrimSpace(msg), ".") {
		t.Errorf("message is not a sentence: %q", msg)
	}
	low := strings.ToLower(msg)
	for _, j := range jargon {
		if strings.Contains(low, j) {
			t.Errorf("message contains %q, which means nothing to a user: %q", j, msg)
		}
	}
}

// Every message a user can be shown has to pass the same test: would somebody
// who has never heard of the OpenDrive API know what to do after reading it?
func TestEveryMessageIsWrittenForAPerson(t *testing.T) {
	for kind, msg := range messages {
		t.Run(string(kind), func(t *testing.T) {
			assertUserReadable(t, msg)
		})
	}
}

// ------------------------------------------------------------------ the lies
//
// The four upstream behaviours P4 must not let through, checked where the
// decision is made: the mapping from an SDK error to what a user reads.

// permissionShaped403 is the message upstream sends for a transient refusal and
// for a real denial alike, byte for byte (docs/discrepancies.md D40).
const permissionShaped403 = `{"error":{"code":403,"message":"Your user access enables you only to ` +
	`view this folder, please contact your administrator to discuss your user permissions."}}`

// classifiedRefusal produces a *real* classified error by driving the SDK
// against a mock upstream, rather than hand-building one. That matters: the
// whole chain under test is classifier → diagnosis → Bridge message, and a
// hand-made error would skip the part most likely to change.
//
// withWitness decides which of the two identical refusals comes back: a call
// that has succeeded here before is judged transient; one that has not is
// confirmed by a second attempt and judged a real restriction.
func classifiedRefusal(t *testing.T, withWitness bool) error {
	t.Helper()

	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			_, _ = w.Write([]byte(`{"UserID":"1"}`)) // the access probe: credential is fine
			return
		}
		served++
		if withWitness && served == 1 {
			_, _ = w.Write([]byte(`{"FolderID":"f1"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(permissionShaped403))
	}))
	t.Cleanup(srv.Close)

	c, err := opendrive.New(
		opendrive.WithBaseURL(srv.URL+"/api/v1"),
		opendrive.WithHTTPClient(srv.Client()),
		opendrive.WithAuthenticator(stubAuth{}),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 2, Base: time.Millisecond, Cap: time.Millisecond,
			Jitter: func(d time.Duration) time.Duration { return d }}),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := opendrive.Request{
		Method: http.MethodPost, Path: "/folder.json",
		SessionPlacement: opendrive.SessionInBody, Scope: "dest",
	}
	if withWitness {
		if err := c.Do(context.Background(), req, nil); err != nil {
			t.Fatalf("seeding the witness: %v", err)
		}
	}
	err = c.Do(context.Background(), req, nil)
	if err == nil {
		t.Fatal("want a refusal")
	}
	return err
}

// Lie 1 (D39/D40): a transient refusal wears the words of a permission denial,
// byte for byte identical to a real one. The Bridge must not repeat them.
func TestPseudoPermission403DoesNotReachTheUserAsPermissionDenied(t *testing.T) {
	transient := classifiedRefusal(t, true)
	if !opendrive.IsTemporary(transient) {
		t.Fatal("the classifier did not judge this transient; the test is not exercising the case")
	}

	status, got := renderError(t, transient)

	assertUserReadable(t, got.Message)
	low := strings.ToLower(got.Message)
	for _, forbidden := range []string{"permission", "administrator", "not allowed", "denied"} {
		if strings.Contains(low, forbidden) {
			t.Errorf("a transient refusal was described with %q: %q", forbidden, got.Message)
		}
	}
	if !strings.Contains(low, "moment") && !strings.Contains(low, "again") {
		t.Errorf("the message does not tell the user it will clear up: %q", got.Message)
	}
	if status == http.StatusForbidden {
		t.Error("the bridge repeated upstream's 403, which tells a client to stop trying")
	}
	// Upstream's own wording is kept for operators, but out of `message`.
	if got.Upstream == nil || !strings.Contains(got.Upstream.Message, "administrator") {
		t.Error("upstream's text was discarded; operators need it for diagnosis")
	}
}

// The other half of the same lie: a real denial must be reported as one, and
// must not promise it will clear up.
func TestRealDenialIsReportedAsARestriction(t *testing.T) {
	denied := classifiedRefusal(t, false)
	if opendrive.IsTemporary(denied) {
		t.Fatal("the classifier judged this transient; the test is not exercising the case")
	}

	_, got := renderError(t, denied)

	assertUserReadable(t, got.Message)
	low := strings.ToLower(got.Message)
	if !strings.Contains(low, "not allowed") {
		t.Errorf("a real denial was not described as one: %q", got.Message)
	}
	if strings.Contains(low, "try again") {
		t.Errorf("a permanent restriction promises a retry: %q", got.Message)
	}
}

// Lie 2 (D42): an empty archive arrives as a 200. The SDK turns that into an
// invalid_response rather than a successful download; the Bridge must describe
// it as a request that did not happen, not as a completed one.
func TestEmptyArchiveIsNotReportedAsSuccess(t *testing.T) {
	empty := &opendrive.APIError{
		Kind: opendrive.KindInvalidResponse, HTTPCode: http.StatusOK,
		UpstreamMsg: "the archive upstream produced contains no entries",
	}
	status, got := renderError(t, empty)

	if status == http.StatusOK {
		t.Fatal("a failed archive download was answered with 200")
	}
	assertUserReadable(t, got.Message)
	low := strings.ToLower(got.Message)
	if !strings.Contains(low, "not completed") && !strings.Contains(low, "nothing has been changed") {
		t.Errorf("the message does not make clear the download did not happen: %q", got.Message)
	}
}

// Lie 3 (D27/D43): a 200 is not evidence anything happened. When the SDK
// reports that upstream's answer could not be believed, the Bridge must say the
// request was not completed rather than reporting the 200 it saw.
func TestA200IsNotTreatedAsProof(t *testing.T) {
	_, got := renderError(t, &opendrive.APIError{
		Kind: opendrive.KindInvalidResponse, HTTPCode: http.StatusOK,
		UpstreamMsg: "upstream answered 200 but the change did not take effect",
	})
	if got.Code != string(opendrive.KindInvalidResponse) {
		t.Errorf("code = %q, want the classifier's verdict", got.Code)
	}
	assertUserReadable(t, got.Message)
	if !strings.Contains(strings.ToLower(got.Message), "not completed") {
		t.Errorf("a 200 that proved nothing was not reported as an incomplete request: %q", got.Message)
	}
}

// Lie 4 (D44): a file that was just uploaded is briefly reported as missing.
// The engine already refuses to call that a retryable error; the Bridge must
// not tell the user their file does not exist either. The engine's own sentence
// is what travels, and it says the upload completed.
func TestFreshUploadInvisibleIsNotReportedAsMissing(t *testing.T) {
	// What internal/jobs produces when the visibility wait runs out: the
	// classifier's Kind, the engine's own description of its own timeout.
	invisible := &opendrive.APIError{
		Kind:        opendrive.KindNotFound,
		UpstreamMsg: "the upload completed but upstream did not make the file visible within 30s",
	}
	_, got := renderError(t, invisible)

	// The operator detail must carry the truth: the upload completed.
	if got.Upstream == nil || !strings.Contains(got.Upstream.Message, "the upload completed") {
		t.Fatalf("the engine's account of what happened was lost: %+v", got.Upstream)
	}
	// And it must never be described as a file that was never there.
	if strings.Contains(strings.ToLower(got.Upstream.Message), "does not exist") {
		t.Error("upstream's 'File does not exist' reached the response for a completed upload")
	}
}

// ------------------------------------------------------------------ mapping

func TestStatusMappingNeverRepeatsAMisleadingUpstreamStatus(t *testing.T) {
	cases := []struct {
		kind opendrive.Kind
		want int
	}{
		{opendrive.KindNotFound, http.StatusNotFound},
		{opendrive.KindConflict, http.StatusConflict},
		{opendrive.KindInvalidName, http.StatusBadRequest},
		{opendrive.KindReauthRequired, http.StatusPreconditionRequired},
		{opendrive.KindCaptchaRequired, http.StatusPreconditionRequired},
		{opendrive.KindKeystoreUnavailable, http.StatusServiceUnavailable},
		{opendrive.KindQuotaExceeded, http.StatusInsufficientStorage},
		{opendrive.KindBandwidthExceeded, http.StatusTooManyRequests},
		{opendrive.KindRateLimited, http.StatusTooManyRequests},
		{opendrive.KindNetwork, http.StatusBadGateway},
		{opendrive.KindEdgeRejected, http.StatusBadGateway},
		{opendrive.KindUpstreamError, http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := statusFor(tc.kind); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEnvelopeCarriesTheStableCode(t *testing.T) {
	_, got := renderError(t, &opendrive.APIError{
		Kind: opendrive.KindQuotaExceeded, HTTPCode: 413, UpstreamMsg: "Storage limit reached",
	})
	if got.Code != "quota_exceeded" {
		t.Errorf("code = %q, want the classifier's stable enum", got.Code)
	}
	if got.HTTP != http.StatusInsufficientStorage {
		t.Errorf("http = %d", got.HTTP)
	}
	if got.Upstream == nil || got.Upstream.Code != 413 {
		t.Errorf("upstream detail was lost: %+v", got.Upstream)
	}
}

// A request the caller got wrong carries its own message and never mentions
// upstream, because upstream was never involved.
func TestRequestErrorsAreReportedAsTheirOwn(t *testing.T) {
	status, got := renderError(t, BadRequest("The path must start with a slash."))
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if got.Upstream != nil {
		t.Errorf("a client mistake blamed upstream: %+v", got.Upstream)
	}
	assertUserReadable(t, got.Message)
}

// A credential in an upstream message must not reach the response body (§9.4).
func TestUpstreamDetailIsRedacted(t *testing.T) {
	_, got := renderError(t, &opendrive.APIError{
		Kind:        opendrive.KindUpstreamError,
		UpstreamMsg: `refused for https://x/api/v1/file.json?access_token=abcdef0123456789`,
	})
	if got.Upstream != nil && strings.Contains(got.Upstream.Message, "abcdef0123456789") {
		t.Errorf("a token reached the response body: %q", got.Upstream.Message)
	}
}

func TestNilErrorStillProducesAnEnvelope(t *testing.T) {
	status, got := renderError(t, nil)
	if status != http.StatusInternalServerError || got.Code == "" {
		t.Errorf("status = %d code = %q", status, got.Code)
	}
}
