package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBridge is a Bridge daemon good enough to drive odctl end to end: it
// answers the endpoints the commands use and can be told to fail with the real
// error envelope.
type fakeBridge struct {
	srv *httptest.Server

	mu       sync.Mutex
	jobs     map[string]map[string]any
	fail     map[string]failure
	requests []string
	files    map[string]int64
}

type failure struct {
	status int
	code   string
	msg    string
}

func newFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	b := &fakeBridge{
		jobs:  map[string]map[string]any{},
		fail:  map[string]failure{},
		files: map[string]int64{"/Docs/report.pdf": 2048},
	}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *fakeBridge) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, r.Method+" "+r.URL.Path)
	f, failing := b.fail[r.URL.Path]
	b.mu.Unlock()

	if failing {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"http":%d,"message":%q,
			"upstream":{"code":403,"message":"the raw upstream wording"}}}`, f.code, f.status, f.msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case path == "/v1/auth/login", path == "/v1/auth/status":
		_, _ = io.WriteString(w, `{"account":{"username":"derek@example.com","user_id":"1"},
			"auth_mode":"oauth2","state":"authenticated","seamless":true,
			"token_expires_at":null,"keystore":{"backend":"keyring","available":true},
			"quota":{"storage_used":1048576,"storage_max":1073741824,"bw_used":0,"bw_max":0}}`)

	case path == "/v1/auth/logout":
		_, _ = io.WriteString(w, `{"status":"signed out","detail":"The bridge has forgotten your password."}`)

	case path == "/v1/ls":
		_, _ = io.WriteString(w, `{"path":"/Docs","dir_update_time":1785000001,"next_offset":null,
			"entries":[{"name":"2026","path":"/Docs/2026","kind":"folder","size":0,"modified":null,"public":false},
			{"name":"report.pdf","path":"/Docs/report.pdf","kind":"file","size":2048,
			 "modified":"2026-07-29T10:00:00Z","public":false}]}`)

	case path == "/v1/stat":
		_, _ = io.WriteString(w, `{"name":"report.pdf","path":"/Docs/report.pdf","kind":"file",
			"size":2048,"modified":"2026-07-29T10:00:00Z","public":false}`)

	case path == "/v1/mkdir":
		_, _ = io.WriteString(w, `{"name":"new","path":"/Docs/new","kind":"folder","size":0,
			"modified":null,"public":false}`)

	case path == "/v1/mv", path == "/v1/cp":
		_, _ = io.WriteString(w, `{"src":"/Docs/report.pdf","dst":"/Docs/2026/report.pdf","moved":true}`)

	case path == "/v1/rename":
		_, _ = io.WriteString(w, `{"path":"/Docs/renamed.pdf","renamed_from":"/Docs/report.pdf"}`)

	case path == "/v1/rm":
		_, _ = io.WriteString(w, `{"path":"/Docs/report.pdf","permanent":false,
			"detail":"Moved to the trash. Restore it from there if you change your mind."}`)

	case path == "/v1/trash":
		_, _ = io.WriteString(w, `{"entries":[]}`)

	case path == "/v1/versions":
		_, _ = io.WriteString(w, `{"path":"/Docs/report.pdf","versions":[{"version":"2","name":"report.pdf"}]}`)

	case path == "/v1/share/link":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"path":"/Docs/report.pdf","url":"https://od.lk/f/ABC",
			"expires_at":"2026-12-31","max_uses":0}`)

	case path == "/v1/share/list":
		_, _ = io.WriteString(w, `{"path":"/Docs/report.pdf","shares":[
			{"path":"/Docs/report.pdf","url":"https://od.lk/f/ABC","expires_at":"2026-12-31"}]}`)

	case path == "/v1/share":
		_, _ = io.WriteString(w, `{"path":"/Docs/report.pdf","detail":"The share link no longer works."}`)

	case path == "/v1/upload", path == "/v1/download":
		id := fmt.Sprintf("job%d", len(b.jobs)+1)
		kind := "upload"
		if path == "/v1/download" {
			kind = "download"
		}
		j := map[string]any{
			"id": id, "kind": kind, "state": "succeeded",
			"bytes_done": 2048, "bytes_total": 2048, "speed": 1024.0,
			"remote_path": "/Docs/report.pdf", "attempts": 1,
		}
		b.mu.Lock()
		b.jobs[id] = j
		b.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(j)

	case strings.HasPrefix(path, "/v1/jobs/"):
		id := strings.TrimPrefix(path, "/v1/jobs/")
		b.mu.Lock()
		j, ok := b.jobs[id]
		b.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","http":404,
				"message":"There is no transfer with that id."}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(j)

	case path == "/v1/jobs":
		b.mu.Lock()
		list := make([]map[string]any, 0, len(b.jobs))
		for _, j := range b.jobs {
			list = append(list, j)
		}
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": list})

	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","http":404,
			"message":"This bridge has nothing at that address."}}`)
	}
}

func (b *fakeBridge) failOn(path string, f failure) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail[path] = f
}

// run executes odctl against the fake bridge and returns the exit code and both
// streams — which is what a script sees, so it is what the tests assert on.
func (b *fakeBridge) run(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	full := append([]string{"--addr", b.srv.URL}, args...)
	code := Execute(full, opts)
	return code, out.String(), errOut.String()
}

// ---------------------------------------------------------------- exit codes

// A script branches on the exit code, so each class of failure has to have its
// own. This is the table that makes odctl usable without parsing prose.
func TestExitCodesSeparateTheKindsOfFailure(t *testing.T) {
	cases := []struct {
		name string
		code string
		want int
	}{
		{"a path that is not there", "not_found", ExitNotFound},
		{"a command the user got wrong", "invalid_request", ExitUsage},
		{"a name OpenDrive will not take", "invalid_name", ExitUsage},
		{"something already there", "conflict", ExitUsage},
		{"the password changed", "reauth_required", ExitAuth},
		{"a captcha is waiting", "captcha_required", ExitAuth},
		{"the keychain is locked", "keystore_unavailable", ExitAuth},
		{"OpenDrive failed", "upstream_error", ExitUpstream},
		{"the network failed", "network", ExitUpstream},
		{"bandwidth is spent", "bandwidth_exceeded", ExitUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newFakeBridge(t)
			b.failOn("/v1/stat", failure{status: 400, code: tc.code, msg: "something happened."})

			code, _, errOut := b.run("stat", "/Docs/report.pdf")
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
			if !strings.Contains(errOut, "something happened.") {
				t.Errorf("the bridge's message did not reach stderr: %q", errOut)
			}
		})
	}
}

// The daemon not running is its own case: a script should be able to tell "no
// bridge" from "the bridge said no".
func TestNoDaemonHasItsOwnExitCode(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	// A port nothing is listening on.
	code := Execute([]string{"--addr", "127.0.0.1:1", "status"}, opts)

	if code != ExitUnavailable {
		t.Errorf("exit code = %d, want %d", code, ExitUnavailable)
	}
	msg := errOut.String()
	for _, want := range []string{"No bridge is answering", "odctl daemon start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q: %q", want, msg)
		}
	}
}

// ---------------------------------------------------------------- wording

// The daemon already decided what a person reads. odctl repeats it; a second
// wording here would drift from the first.
func TestTheBridgesMessageIsPrintedUnchanged(t *testing.T) {
	const message = "Your OpenDrive account is not allowed to do this here. " +
		"If you expect to be, the account's administrator controls it."

	b := newFakeBridge(t)
	b.failOn("/v1/mkdir", failure{status: 502, code: "upstream_error", msg: message})

	code, _, errOut := b.run("mkdir", "/nope")
	if code != ExitUpstream {
		t.Errorf("exit code = %d", code)
	}
	if strings.TrimSpace(errOut) != message {
		t.Errorf("stderr = %q,\nwant exactly the bridge's message", errOut)
	}
	// And upstream's own wording stays hidden unless asked for.
	if strings.Contains(errOut, "the raw upstream wording") {
		t.Error("upstream's wording leaked into the default output")
	}
}

func TestVerboseAddsUpstreamDetailForBugReports(t *testing.T) {
	b := newFakeBridge(t)
	b.failOn("/v1/stat", failure{status: 502, code: "upstream_error", msg: "OpenDrive refused."})

	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	Execute([]string{"--addr", b.srv.URL, "-v", "stat", "/x"}, opts)

	if !strings.Contains(errOut.String(), "the raw upstream wording") {
		t.Errorf("--verbose did not show the upstream detail: %q", errOut.String())
	}
}

// Everything odctl prints has to pass the same test the daemon's messages do.
func TestOdctlOutputIsWrittenForAPerson(t *testing.T) {
	b := newFakeBridge(t)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"ls", []string{"ls", "/Docs"}},
		{"stat", []string{"stat", "/Docs/report.pdf"}},
		{"mkdir", []string{"mkdir", "/Docs/new"}},
		{"rm", []string{"rm", "/Docs/report.pdf"}},
		{"share", []string{"share", "link", "/Docs/report.pdf"}},
		{"trash", []string{"trash"}},
		{"jobs", []string{"jobs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := b.run(tc.args...)
			if code != ExitOK {
				t.Fatalf("exit code = %d: %s", code, errOut)
			}
			assertReadable(t, out)
		})
	}
}

// assertReadable holds odctl's own prose to the rule the daemon's messages
// follow: no vocabulary a user cannot act on.
func assertReadable(t *testing.T, s string) {
	t.Helper()
	if strings.TrimSpace(s) == "" {
		t.Fatal("the command printed nothing at all")
	}
	for _, jargon := range []string{
		"upstream", "endpoint", "http 4", "http 5", "json", "session_id",
		"folder_id", "file_id", "invalid_request", "upstream_error", "not_found",
	} {
		if strings.Contains(strings.ToLower(s), jargon) {
			t.Errorf("output contains %q, which means nothing to a user:\n%s", jargon, s)
		}
	}
}

// ---------------------------------------------------------------- behaviour

func TestListPrintsFoldersFirst(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("ls", "/Docs")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "2026/") {
		t.Errorf("folders are not marked with a slash:\n%s", out)
	}
	if strings.Index(out, "2026/") > strings.Index(out, "report.pdf") {
		t.Errorf("folders should come first:\n%s", out)
	}
	if !strings.Contains(out, "2.0 KB") {
		t.Errorf("sizes are not human readable:\n%s", out)
	}
}

func TestStatusSpeaksInSentences(t *testing.T) {
	b := newFakeBridge(t)
	_, out, _ := b.run("status")
	if !strings.Contains(out, "Signed in as derek@example.com") {
		t.Errorf("status = %q", out)
	}
	// The quota is reported in units a person reads.
	if !strings.Contains(out, "1.0 MB") || !strings.Contains(out, "1.0 GB") {
		t.Errorf("the quota is not human readable: %q", out)
	}
}

// Each authentication state gets a sentence saying what to do about it, because
// that is the entire reason somebody runs status.
func TestStatusExplainsEveryState(t *testing.T) {
	states := map[string]string{
		"not_configured":       "odctl login",
		"reauth_required":      "current password",
		"captcha_required":     "opendrive.com",
		"keystore_unavailable": "keychain",
	}
	for state, want := range states {
		t.Run(state, func(t *testing.T) {
			b := newFakeBridge(t)
			b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"account":{"username":"d@example.com"},"auth_mode":"oauth2",
					"state":%q,"seamless":false,"token_expires_at":null,
					"keystore":{"backend":"keyring","available":true},"quota":null}`, state)
			})

			code, out, errOut := b.run("status")
			if code != ExitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if !strings.Contains(out, want) {
				t.Errorf("state %s does not tell the user about %q:\n%s", state, want, out)
			}
			assertReadable(t, out)
		})
	}
}

func TestUploadReportsProgressAndFinishes(t *testing.T) {
	b := newFakeBridge(t)
	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := b.run("up", local, "/Docs/payload.bin")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// Not a terminal in a test, so the bar announces itself once instead of
	// filling the output with carriage returns.
	if !strings.Contains(out, "Uploading") {
		t.Errorf("no indication the upload started:\n%s", out)
	}
}

func TestUploadRefusesAMissingFileBeforeCallingTheBridge(t *testing.T) {
	b := newFakeBridge(t)
	code, _, errOut := b.run("up", filepath.Join(t.TempDir(), "nope.bin"), "/Docs/x.bin")

	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut, "no readable file") {
		t.Errorf("stderr = %q", errOut)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.requests {
		if strings.Contains(req, "/v1/upload") {
			t.Error("a missing local file still reached the bridge")
		}
	}
}

// A failed transfer must report the daemon's reason, and exit accordingly.
func TestFollowReportsAFailedJobInItsOwnWords(t *testing.T) {
	b := newFakeBridge(t)
	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The job is accepted, then reports failure with the classifier's wording.
	base := b.srv.Config.Handler
	b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"job1","kind":"upload","state":"failed",
				"bytes_done":0,"bytes_total":1,"speed":0,"remote_path":"/Docs/x",
				"error":{"code":"quota_exceeded","message":"Your OpenDrive account is out of storage space.",
				"retryable":false}}`)
			return
		}
		base.ServeHTTP(w, r)
	})

	code, _, errOut := b.run("up", local, "/Docs/x")
	// A full account is not a "try again later" failure, and this used to exit 4
	// as though it were, because the exit code ignored the retryable flag the
	// daemon had just worked out. The fixture has said "retryable": false since
	// the day it was written; only the assertion was wrong.
	if code != ExitRefused {
		t.Errorf("exit code = %d, want %d", code, ExitRefused)
	}
	if !strings.Contains(errOut, "out of storage space") {
		t.Errorf("the daemon's reason did not reach the user: %q", errOut)
	}
}

// The other half of the same rule: a failure the daemon says may be retried must
// still exit 4, or a script would stop on something that clears up by itself.
func TestFollowExitsRetryableForATransientFailure(t *testing.T) {
	b := newFakeBridge(t)
	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := b.srv.Config.Handler
	b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/jobs/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"job1","kind":"upload","state":"failed",
				"bytes_done":0,"bytes_total":1,"speed":0,"remote_path":"/Docs/x",
				"error":{"code":"upstream_error","message":"OpenDrive turned this request away for a moment.",
				"retryable":true}}`)
			return
		}
		base.ServeHTTP(w, r)
	})

	code, _, _ := b.run("up", local, "/Docs/x")
	if code != ExitUpstream {
		t.Errorf("exit code = %d, want %d", code, ExitUpstream)
	}
}

func TestJobsCancelSaysNothingWasLeftBehind(t *testing.T) {
	b := newFakeBridge(t)
	b.mu.Lock()
	b.jobs["job1"] = map[string]any{"id": "job1", "kind": "upload", "state": "cancelled",
		"bytes_done": 0, "bytes_total": 10, "speed": 0.0, "remote_path": "/x"}
	b.mu.Unlock()

	code, out, errOut := b.run("jobs", "cancel", "job1")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// D39: this is the guarantee the engine makes, and a user who cancels wants
	// to know it holds.
	if !strings.Contains(out, "Nothing was left behind") {
		t.Errorf("cancel does not reassure the user: %q", out)
	}
}

func TestJSONOutputIsMachineReadable(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("--json", "ls", "/Docs")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("--json did not produce JSON: %v\n%s", err, out)
	}
	if parsed["entries"] == nil {
		t.Errorf("the JSON is not the bridge's own shape: %v", parsed)
	}
}

// ---------------------------------------------------------------- units

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KB",
		1048576: "1.0 MB", 1073741824: "1.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestServiceErrorsAreActionable(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"permission denied", "administrator rights"},
		{"Access is denied.", "administrator rights"},
		{"service already exists", "already installed"},
		{"the service is not installed", "daemon install"},
	} {
		err := serviceError(fmt.Errorf("%s", tc.raw))
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q produced %q, want it to mention %q", tc.raw, err.Error(), tc.want)
		}
		assertReadable(t, err.Error())
	}
}

func TestProgressStaysQuietWhenNobodyIsWatching(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "Uploading thing")
	p.update(50, 100, 1024)
	p.update(100, 100, 1024)
	p.done()

	out := buf.String()
	if strings.Contains(out, "\r") {
		t.Errorf("a redirected stream got carriage returns:\n%q", out)
	}
	if !strings.Contains(out, "Uploading thing") {
		t.Errorf("nothing was announced: %q", out)
	}
}

func TestUnknownCommandExitsAsUsage(t *testing.T) {
	b := newFakeBridge(t)
	code, _, _ := b.run("frobnicate")
	if code != ExitUsage {
		t.Errorf("exit code = %d, want %d for an unknown command", code, ExitUsage)
	}
}
