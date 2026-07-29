package opendrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bodies below are verbatim from the live API, recorded while writing
// docs/error-taxonomy.md. Editing one to make a test pass would defeat the
// point: they are the evidence.
const (
	// T1 — the openresty page the chunk upload front end answers with (D38).
	edgeHTML401 = "<html>\r\n<head><title>401 Authorization Required</title></head>\r\n" +
		"<body>\r\n<center><h1>401 Authorization Required</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n</body>\r\n</html>\r\n"

	// T2 — the message upstream sends both for real permission denial (D25) and
	// for transient resource pressure (D39), byte for byte.
	permissionShaped403 = `{"error":{"code":403,"message":"Your user access enables you only to view ` +
		`this folder, please contact your administrator to discuss your user permissions."}}`

	// T12 — the one credential signal upstream states plainly.
	sessionGone401 = `{"error":{"code":401,"message":"Session does not exist, please re-login."}}`
)

// Every disguise form in docs/error-taxonomy.md that is decidable from the
// response alone has a case here, named after it.
func TestTaxonomyFormsClassify(t *testing.T) {
	tests := []struct {
		form           string
		status         int
		contentType    string
		body           string
		wantKind       Kind
		wantShape      BodyShape
		wantAmbiguous  bool
		wantTemporary  bool
		wantDrivesAuth bool
	}{
		{
			form: "T1 edge rejection in HTML", status: 401, contentType: "text/html",
			body: edgeHTML401, wantKind: KindEdgeRejected, wantShape: ShapeHTML,
		},
		{
			form: "T1 edge rejection worth retrying", status: 502, contentType: "text/html",
			body: edgeHTML401, wantKind: KindEdgeRejected, wantShape: ShapeHTML, wantTemporary: true,
		},
		{
			form: "T2 permission-shaped 403 is ambiguous", status: 403,
			body: permissionShaped403, wantKind: KindUpstreamError, wantShape: ShapeJSON,
			wantAmbiguous: true,
		},
		{
			form: "T3 account-type gate is permanent", status: 403,
			body:     `{"error":{"code":403,"message":"Account users cannot list shared users"}}`,
			wantKind: KindUpstreamError, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T3 restricted user", status: 403,
			body:     `{"error":{"code":403,"message":"Export failed. Permission denied for restricted user"}}`,
			wantKind: KindUpstreamError, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T4 400 asking for the type it was given", status: 400,
			body:     "{\"error\":{\"code\":400,\"message\":\"Invalid value specified for `move`. Expecting boolean value\"}}",
			wantKind: KindInvalidRequest, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T5 required is not satisfied by an empty string", status: 400,
			body:     "{\"error\":{\"code\":400,\"message\":\"`access_folder_id` is required.\"}}",
			wantKind: KindInvalidRequest, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T9 the chunk resume point", status: 400,
			body:     `{"error":{"code":400,"message":"Incorrect chunk offset: uploaded=0, chunk_offset=999999"}}`,
			wantKind: KindInvalidRequest, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T10 total uploaded=0 while the bytes are stored", status: 400,
			body:     `{"error":{"code":400,"message":"Invalid upload file size. Total uploaded=0. File size=1280"}}`,
			wantKind: KindInvalidRequest, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T11 captcha outside login", status: 403,
			body:     `{"error":{"code":403,"message":"Captcha required"}}`,
			wantKind: KindCaptchaRequired, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T12 a real session expiry", status: 401, body: sessionGone401,
			wantKind: KindTokenExpired, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			// The same sentence, without a 401 to go with it. It is still the
			// session speaking, and the message is what says so.
			form: "T12 a session expiry announced without a 401", status: 200,
			body:     `{"error":{"code":403,"message":"Session does not exist, please re-login."}}`,
			wantKind: KindTokenExpired, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T13 a 401 with no body says nothing", status: 401, body: "",
			wantKind: KindUpstreamError, wantShape: ShapeEmpty, wantAmbiguous: true,
		},
		{
			form: "T14 bandwidth", status: 403,
			body:     `{"error":{"code":403,"message":"Bandwidth limit exceeded"}}`,
			wantKind: KindBandwidthExceeded, wantShape: ShapeJSON, wantDrivesAuth: true,
		},
		{
			form: "T15 a genuine rate limit", status: 429,
			body:     `{"error":{"code":429,"message":"Too many requests"}}`,
			wantKind: KindRateLimited, wantShape: ShapeJSON, wantTemporary: true, wantDrivesAuth: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.form, func(t *testing.T) {
			h := http.Header{}
			if tc.contentType != "" {
				h.Set("Content-Type", tc.contentType)
			}
			e := parseError(tc.status, h, []byte(tc.body), "POST /x.json", "https://x/x.json")

			if e.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", e.Kind, tc.wantKind)
			}
			if e.BodyShape() != tc.wantShape {
				t.Errorf("body shape = %q, want %q", e.BodyShape(), tc.wantShape)
			}
			if e.Ambiguous() != tc.wantAmbiguous {
				t.Errorf("ambiguous = %v, want %v", e.Ambiguous(), tc.wantAmbiguous)
			}
			if e.Temporary() != tc.wantTemporary {
				t.Errorf("temporary = %v, want %v", e.Temporary(), tc.wantTemporary)
			}
			if e.drivesAuth() != tc.wantDrivesAuth {
				t.Errorf("drivesAuth = %v, want %v", e.drivesAuth(), tc.wantDrivesAuth)
			}
		})
	}
}

// The invariant, stated as a test: a response that is not API-shaped may not
// move the authentication state machine, whatever status it carries. This is
// exactly the failure D38 describes — an HTML 401 read as an expired token sends
// the machine chasing a credential that was never the problem.
func TestNonAPIBodyNeverDrivesTheAuthStateMachine(t *testing.T) {
	m := newMockUpstream(t)
	m.pushHeader(http.StatusUnauthorized, edgeHTML401,
		http.Header{"Content-Type": []string{"text/html"}})

	auth := &stubAuth{creds: Credentials{SessionID: "s1"}}
	c := m.client(WithAuthenticator(auth), WithAccessProbe(nil))

	err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "/upload/upload_file_chunk2.json",
		SessionPlacement: SessionInPath,
	}, nil)

	if ErrorKind(err) != KindEdgeRejected {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindEdgeRejected)
	}
	if auth.refreshCount() != 0 {
		t.Fatalf("the auth machine ran %d times for a proxy's HTML page; it must never run",
			auth.refreshCount())
	}
	if m.callCount() != 1 {
		t.Fatalf("made %d calls, want 1: an edge 401 is not worth repeating", m.callCount())
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.drivesAuth() {
		t.Fatal("an HTML body reported itself as fit to drive authentication")
	}
}

// The same invariant for the other half: an error that is still ambiguous is
// barred from the auth machine until something has settled it.
func TestAmbiguousErrorNeverDrivesTheAuthStateMachine(t *testing.T) {
	m := newMockUpstream(t)
	m.push(http.StatusUnauthorized, "") // T13: a 401 that states nothing

	auth := &stubAuth{creds: Credentials{SessionID: "s1"}}
	// No probe, so the ambiguity cannot be resolved and must stay barred.
	c := m.client(WithAuthenticator(auth), WithAccessProbe(nil))

	err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Path: EndpointFolderList, SessionPlacement: SessionInPath,
	}, nil)

	if auth.refreshCount() != 0 {
		t.Fatalf("an empty 401 triggered %d renewals; with no evidence it must trigger none",
			auth.refreshCount())
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %v", err)
	}
	if !ae.Ambiguous() || ae.drivesAuth() {
		t.Fatalf("ambiguous = %v, drivesAuth = %v; want true and false",
			ae.Ambiguous(), ae.drivesAuth())
	}
}

// permissionServer answers the first call to op with 200 and every later one
// with the permission-shaped 403, while answering the probe as told.
func permissionServer(t *testing.T, probeStatus int, probeBody string) *mockUpstream {
	t.Helper()
	m := newMockUpstream(t)
	var mu sync.Mutex
	seen := map[string]int{}
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			w.WriteHeader(probeStatus)
			_, _ = io.WriteString(w, probeBody)
			return
		}
		mu.Lock()
		n := seen[r.URL.Path]
		seen[r.URL.Path]++
		mu.Unlock()
		if n == 0 {
			_, _ = io.WriteString(w, `{"FolderID":"f1"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})
	return m
}

func createFolderRequest() Request {
	return Request{
		Method: http.MethodPost, Path: "/folder.json",
		SessionPlacement: SessionInBody, Retryable: Retryable(false),
	}
}

// T2a: the credential works and this very call has succeeded before, so the
// permission wording is not to be believed and the refusal is transient.
func TestAmbiguous403WithAWorkingCredentialAndAWitnessIsTransient(t *testing.T) {
	m := permissionServer(t, http.StatusOK, `{"UserID":"1"}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, createFolderRequest(), nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	err := c.Do(ctx, createFolderRequest(), nil)

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %v", err)
	}
	if ae.Kind != KindUpstreamError {
		t.Errorf("kind = %q, want %q: it is not a credential problem either way", ae.Kind, KindUpstreamError)
	}
	if !ae.Temporary() {
		t.Error("a permission-shaped 403 with a working credential and a witness must be retryable (D39)")
	}
	if ae.Diagnosis() == "" {
		t.Error("no diagnosis: the user would be shown upstream's misleading permission text")
	}
	if strings.Contains(strings.ToLower(ae.Diagnosis()), "contact your administrator") {
		t.Error("the diagnosis repeats upstream's misleading advice")
	}
	if got := c.amb.probeCount(); got != 1 {
		t.Errorf("ran %d probes, want exactly 1", got)
	}
}

// T2b: the same wire form, but this operation has never worked. Failing closed
// is deliberate — it costs one avoidable error, not a retry storm.
func TestAmbiguous403WithoutAWitnessIsPermanent(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			_, _ = io.WriteString(w, `{"UserID":"1"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))

	err := c.Do(context.Background(), createFolderRequest(), nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %v", err)
	}
	if ae.Temporary() {
		t.Error("a first-ever call refused this way must not be retried")
	}
	if !ae.decisive() {
		t.Error("the error was resolved, so it should now count as decided")
	}
}

// A witness is per operation: succeeding at a listing says nothing about
// whether a create is permitted.
func TestTheSuccessWitnessIsPerOperation(t *testing.T) {
	m := permissionServer(t, http.StatusOK, `{"UserID":"1"}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: EndpointFolderList,
		SessionPlacement: SessionInPath, Retryable: Retryable(false)}, nil); err != nil {
		t.Fatalf("listing: %v", err)
	}

	err := c.Do(ctx, createFolderRequest(), nil)
	if IsTemporary(err) {
		t.Fatal("a successful listing was taken as licence to retry a refused create")
	}
}

// Write rights are held per folder, so the witness is too. Succeeding in one
// folder says nothing about another, and an account may well hold rights in one
// and none in the next.
func TestTheSuccessWitnessIsPerResource(t *testing.T) {
	m := newMockUpstream(t)
	var mu sync.Mutex
	seen := 0
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			_, _ = io.WriteString(w, `{"UserID":"1"}`)
			return
		}
		if r.URL.Query().Get("folder") == "A" {
			mu.Lock()
			n := seen
			seen++
			mu.Unlock()
			if n == 0 {
				_, _ = io.WriteString(w, `{"FolderID":"A"}`)
				return
			}
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})

	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()
	write := func(folder string) error {
		return c.Do(ctx, Request{
			Method: http.MethodPost, Path: "/folder.json", SessionPlacement: SessionInBody,
			Query: map[string][]string{"folder": {folder}}, Scope: folder,
			Retryable: Retryable(false),
		}, nil)
	}

	if err := write("A"); err != nil {
		t.Fatalf("first write to A: %v", err)
	}
	if err := write("A"); !IsTemporary(err) {
		t.Errorf("a refusal in a folder that has accepted writes must be transient, got %v", err)
	}
	if err := write("B"); IsTemporary(err) {
		t.Error("succeeding in folder A was taken as licence to retry in folder B")
	}
}

// When the probe is refused the same way, the restriction is real: the account
// cannot even read, so nothing here is transient.
func TestAmbiguous403WithAFailingProbeStaysPermanent(t *testing.T) {
	m := permissionServer(t, http.StatusForbidden, permissionShaped403)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, createFolderRequest(), nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	err := c.Do(ctx, createFolderRequest(), nil)

	if IsTemporary(err) {
		t.Fatal("the probe was refused too, so this must not be retried")
	}
	var ae *APIError
	if errors.As(err, &ae) && !strings.Contains(ae.Diagnosis(), "real restriction") {
		t.Errorf("diagnosis = %q, want it to say the restriction is real", ae.Diagnosis())
	}
}

// One refusal settles nothing (D40), so it is attempted once more even though
// the call is a POST — a refusal is replay-safe by construction, since upstream
// declined to act. The second refusal settles it and is remembered, so a bulk
// job against something it truly may not touch costs one extra call in total,
// not one per item.
func TestARefusalIsConfirmedOnceAndThenRemembered(t *testing.T) {
	m := newMockUpstream(t)
	var mu sync.Mutex
	writes := 0
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			_, _ = io.WriteString(w, `{"UserID":"1"}`)
			return
		}
		mu.Lock()
		writes++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, createFolderRequest(), nil); err == nil {
		t.Fatal("want a refusal")
	}
	mu.Lock()
	first := writes
	mu.Unlock()
	if first != 2 {
		t.Fatalf("made %d attempts, want 2: one call and one confirming retry", first)
	}

	for i := 0; i < 5; i++ {
		if err := c.Do(ctx, createFolderRequest(), nil); IsTemporary(err) {
			t.Fatal("a confirmed refusal was reported as retryable again")
		}
	}
	mu.Lock()
	total := writes
	mu.Unlock()
	if total != first+5 {
		t.Fatalf("made %d attempts for 5 further calls, want %d: the refusal must be remembered",
			total-first, 5)
	}
}

// A success revokes a remembered refusal: whatever upstream was doing, it has
// stopped.
func TestASuccessRevokesARememberedRefusal(t *testing.T) {
	m := newMockUpstream(t)
	var mu sync.Mutex
	refuse := true
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			_, _ = io.WriteString(w, `{"UserID":"1"}`)
			return
		}
		mu.Lock()
		refusing := refuse
		mu.Unlock()
		if refusing {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, permissionShaped403)
			return
		}
		_, _ = io.WriteString(w, `{"FolderID":"f1"}`)
	})
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, createFolderRequest(), nil); err == nil {
		t.Fatal("want a refusal")
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	if err := c.Do(ctx, createFolderRequest(), nil); err != nil {
		t.Fatalf("after upstream recovered: %v", err)
	}

	mu.Lock()
	refuse = true
	mu.Unlock()
	err := c.Do(ctx, createFolderRequest(), nil)
	if !IsTemporary(err) {
		t.Fatal("after a success on this resource, a refusal must be treated as transient again")
	}
}

// A probe that comes back with a clean credential verdict may promote the
// error's Kind — that is the one path from an ambiguous error to a credential
// one, and it travels on a well-formed API response rather than on a guess.
func TestAFailingProbePromotesACredentialVerdict(t *testing.T) {
	m := permissionServer(t, http.StatusUnauthorized,
		`{"error":{"code":401,"message":"Invalid username or password"}}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	if err := c.Do(ctx, createFolderRequest(), nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	err := c.Do(ctx, createFolderRequest(), nil)

	if ErrorKind(err) != KindReauthRequired {
		t.Fatalf("kind = %q, want %q", ErrorKind(err), KindReauthRequired)
	}
	if IsTemporary(err) {
		t.Error("a rejected credential is not retryable")
	}
}

// One probe, however many callers trip the condition at once, and however often
// they trip it inside the interval. A bulk transfer must not turn one upstream
// hiccup into a probe storm.
func TestTheAccessProbeIsSingleFlightAndRateLimited(t *testing.T) {
	m := newMockUpstream(t)
	var probes int
	var mu sync.Mutex
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.Contains(r.URL.Path, "users/info.json") {
			mu.Lock()
			probes++
			mu.Unlock()
			time.Sleep(5 * time.Millisecond) // hold the flight open
			_, _ = io.WriteString(w, `{"UserID":"1"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})

	clock := newFakeClock()
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}),
		withProbeClock(clock.Now))
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Do(ctx, createFolderRequest(), nil)
		}()
	}
	wg.Wait()

	// A second wave, still inside the interval.
	clock.Advance(DefaultProbeInterval / 2)
	for i := 0; i < 5; i++ {
		_ = c.Do(ctx, createFolderRequest(), nil)
	}

	mu.Lock()
	got := probes
	mu.Unlock()
	if got != 1 {
		t.Fatalf("ran %d probes for 25 ambiguous errors, want 1", got)
	}

	// Past the interval, a fresh look is allowed.
	clock.Advance(DefaultProbeInterval + time.Second)
	_ = c.Do(ctx, createFolderRequest(), nil)
	mu.Lock()
	got = probes
	mu.Unlock()
	if got != 2 {
		t.Fatalf("ran %d probes after the interval elapsed, want 2", got)
	}
}

// A probe that trips the very condition it is investigating must not start
// another probe.
func TestTheAccessProbeCannotRecurse(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, permissionShaped403)
	})
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Do(context.Background(), createFolderRequest(), nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe recursed instead of stopping at one level")
	}
	if got := c.amb.probeCount(); got != 1 {
		t.Fatalf("ran %d probes, want exactly 1", got)
	}
}

// Retryability is decided in one place. A resolved-transient error is retried by
// the client itself, with no caller involved in the judgement.
func TestTheClassifierAloneDecidesRetries(t *testing.T) {
	m := permissionServer(t, http.StatusOK, `{"UserID":"1"}`)
	c := m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}))
	ctx := context.Background()

	req := createFolderRequest()
	if err := c.Do(ctx, req, nil); err != nil {
		t.Fatalf("first call: %v", err)
	}

	req.Retryable = Retryable(true)
	before := m.callCount()
	if err := c.Do(ctx, req, nil); err == nil {
		t.Fatal("want the refusal to survive the retries")
	}
	// 3 retries by the mock's policy, plus the first attempt, plus one probe.
	if got := m.callCount() - before; got < 4 {
		t.Fatalf("made %d calls, want the retry policy to have been applied", got)
	}
}

// A wrapped transport error is not ours and quotes the full request URL. This
// leaked a live access token into an integration-test log, which is exactly what
// §9.4 forbids; the string below is the shape that did it.
func TestAWrappedTransportErrorCannotLeakACredential(t *testing.T) {
	const token = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	wrapped := errors.New(`Post "https://dev.opendrive.com/api/v1/folder/remove.json?access_token=` +
		token + `": context deadline exceeded`)

	err := networkError("POST /folder/remove.json", "https://dev.opendrive.com/api/v1/folder/remove.json", wrapped)

	mustNotContain(t, err.Error(), token, "the error message")
	mustContain(t, err.Error(), "context deadline exceeded", "the error message")
	// Unwrapping still yields the original, for callers that need it.
	if !errors.Is(errors.Unwrap(err), wrapped) {
		t.Error("the original cause is no longer reachable through Unwrap")
	}
}

// Nothing may be classified from a body the SDK never saw: an error the SDK
// builds itself is trusted, because we wrote it.
func TestSDKBuiltErrorsAreTrusted(t *testing.T) {
	e := &APIError{Kind: KindTokenExpired}
	if e.BodyShape() != ShapeUnset {
		t.Fatalf("shape = %q, want the unset shape", e.BodyShape())
	}
	if !e.drivesAuth() || !e.decisive() {
		t.Fatal("an SDK-built error must be usable by the auth machine")
	}
}
