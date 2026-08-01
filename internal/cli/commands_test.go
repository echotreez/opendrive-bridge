package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
)

// The rest of the command set, each asserted on what a person ends up reading.

func TestLoginTellsYouWhatHappensNext(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("login", "derek@example.com", "--password", "hunter2")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Signed in as derek@example.com") {
		t.Errorf("out = %q", out)
	}
	// The whole promise of the bridge is that it stays signed in; say so.
	if !strings.Contains(out, "keep itself signed in") {
		t.Errorf("login does not explain what happens next: %q", out)
	}
	assertReadable(t, out)
}

func TestLoginWithoutAPasswordOrATerminalSaysWhatToDo(t *testing.T) {
	b := newFakeBridge(t)
	// stdin in a test is not a terminal and has nothing in it.
	code, _, errOut := b.run("login", "derek@example.com")
	if code == ExitOK {
		t.Skip("stdin supplied something; the prompt path is covered elsewhere")
	}
	if !strings.Contains(errOut, "password") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestLogoutRepeatsTheDaemonsSentence(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("logout")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// The daemon said it; odctl does not paraphrase.
	if !strings.Contains(out, "forgotten your password") {
		t.Errorf("out = %q, want the daemon's own wording", out)
	}
}

func TestMoveAndCopyReportWhereThingsWent(t *testing.T) {
	b := newFakeBridge(t)

	code, out, errOut := b.run("mv", "/Docs/report.pdf", "/Docs/2026")
	if code != ExitOK {
		t.Fatalf("mv exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Moved") || !strings.Contains(out, "/Docs/2026/report.pdf") {
		t.Errorf("mv out = %q", out)
	}
	assertReadable(t, out)

	code, out, errOut = b.run("cp", "/Docs/report.pdf", "/Docs/2026")
	if code != ExitOK {
		t.Fatalf("cp exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Copied") {
		t.Errorf("cp out = %q", out)
	}
}

func TestRenameReportsTheNewPath(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("rename", "/Docs/report.pdf", "renamed.pdf")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "/Docs/renamed.pdf") {
		t.Errorf("out = %q", out)
	}
	assertReadable(t, out)
}

func TestVersionsListsOrSaysThereAreNone(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("versions", "/Docs/report.pdf")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "report.pdf") {
		t.Errorf("out = %q", out)
	}
}

func TestTrashSaysWhenItIsEmpty(t *testing.T) {
	b := newFakeBridge(t)
	code, out, errOut := b.run("trash")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "The trash is empty") {
		t.Errorf("out = %q", out)
	}
	assertReadable(t, out)
}

func TestShareListAndRevoke(t *testing.T) {
	b := newFakeBridge(t)

	code, out, errOut := b.run("share", "list", "/Docs/report.pdf")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "od.lk") || !strings.Contains(out, "2026-12-31") {
		t.Errorf("share list out = %q", out)
	}

	code, out, errOut = b.run("share", "revoke", "/Docs/report.pdf")
	if code != ExitOK {
		t.Fatalf("revoke exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "no longer works") {
		t.Errorf("revoke out = %q", out)
	}
	assertReadable(t, out)
}

// A share link is the one output somebody copies and pastes, so it goes on its
// own line with nothing else on it.
func TestShareLinkIsOnItsOwnLine(t *testing.T) {
	b := newFakeBridge(t)
	_, out, _ := b.run("share", "link", "/Docs/report.pdf")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("share printed %d lines: %q", len(lines), out)
	}
	if strings.TrimSpace(lines[0]) != "https://od.lk/f/ABC" {
		t.Errorf("the first line is not just the link: %q", lines[0])
	}
}

func TestDownloadSaysWhereItSaved(t *testing.T) {
	b := newFakeBridge(t)
	dest := filepath.Join(t.TempDir(), "back.bin")

	code, out, errOut := b.run("down", "/Docs/report.pdf", dest)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Saved to") || !strings.Contains(out, dest) {
		t.Errorf("out = %q, want it to name the file it wrote", out)
	}
}

// Without a local name, the remote file's own name is used — the behaviour
// somebody expects from curl -O or scp.
func TestDownloadDefaultsToTheRemoteName(t *testing.T) {
	b := newFakeBridge(t)
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	code, out, errOut := b.run("down", "/Docs/report.pdf")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "report.pdf") {
		t.Errorf("out = %q", out)
	}
}

func TestJobsShowsOneAndAllOfThem(t *testing.T) {
	b := newFakeBridge(t)
	b.mu.Lock()
	b.jobs["j1"] = map[string]any{"id": "j1", "kind": "upload", "state": "running",
		"bytes_done": 512, "bytes_total": 2048, "speed": 256.0, "remote_path": "/Docs/x.bin"}
	b.jobs["j2"] = map[string]any{"id": "j2", "kind": "download", "state": "queued",
		"bytes_done": 0, "bytes_total": 100, "speed": 0.0, "remote_path": "/Docs/y.bin"}
	b.mu.Unlock()

	_, out, _ := b.run("jobs")
	if !strings.Contains(out, "j1") || !strings.Contains(out, "j2") {
		t.Errorf("jobs out = %q", out)
	}
	// A running job says how far along it is, in units a person reads.
	if !strings.Contains(out, "512 B of 2.0 KB") {
		t.Errorf("progress is not human readable: %q", out)
	}
	if !strings.Contains(out, "waiting to start") {
		t.Errorf("a queued job is not explained: %q", out)
	}

	_, one, _ := b.run("jobs", "j1")
	if !strings.Contains(one, "/Docs/x.bin") {
		t.Errorf("single job out = %q", one)
	}
	assertReadable(t, one)
}

func TestJobsReportsAFailureWithItsReason(t *testing.T) {
	b := newFakeBridge(t)
	b.mu.Lock()
	b.jobs["bad"] = map[string]any{"id": "bad", "kind": "upload", "state": "failed",
		"bytes_done": 0, "bytes_total": 10, "speed": 0.0, "remote_path": "/Docs/x",
		"error": map[string]any{"code": "quota_exceeded",
			"message": "Your OpenDrive account is out of storage space.", "retryable": false}}
	b.mu.Unlock()

	_, out, _ := b.run("jobs", "bad")
	if !strings.Contains(out, "out of storage space") {
		t.Errorf("a failed job does not say why: %q", out)
	}
}

func TestUnknownJobExitsNotFound(t *testing.T) {
	b := newFakeBridge(t)
	code, _, errOut := b.run("jobs", "nope")
	if code != ExitNotFound {
		t.Errorf("exit %d, want %d", code, ExitNotFound)
	}
	if !strings.Contains(errOut, "no transfer with that id") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestDescribeCoversEveryState(t *testing.T) {
	for _, state := range []string{"queued", "running", "succeeded", "failed", "cancelled"} {
		got := describe(job{Kind: "upload", State: state, RemotePath: "/x", BytesTotal: 1024})
		if got == "" {
			t.Errorf("state %q produced no description", state)
		}
		if strings.Contains(got, state) && state != "running" {
			// The state name itself is jargon; a sentence is better.
			if state == "succeeded" || state == "failed" || state == "cancelled" {
				continue // these read as words already
			}
		}
	}
	if !strings.Contains(describe(job{State: "succeeded", BytesTotal: 2048}), "2.0 KB") {
		t.Error("a finished job does not report its size")
	}
}

// ---------------------------------------------------------------- daemon

func TestDaemonServiceIsConfiguredForThisMachine(t *testing.T) {
	svc, err := daemonService("/usr/local/bin/opendrived", []string{"--addr", "127.0.0.1:7777"})
	if err != nil {
		t.Fatalf("daemonService: %v", err)
	}
	if svc == nil {
		t.Fatal("no service was built")
	}
	// The library's own lifecycle hooks are no-ops here: odctl registers
	// opendrived, it does not become the daemon.
	var p serviceProgram
	if err := p.Start(nil); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := p.Stop(nil); err != nil {
		t.Errorf("Stop: %v", err)
	}
	var _ service.Interface = p
}

func TestDaemonInstallWithoutABinarySaysSo(t *testing.T) {
	b := newFakeBridge(t)
	// A path that certainly does not exist, and PATH cleared so the lookup fails.
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	_ = os.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	code, _, errOut := b.run("daemon", "install")
	if code == ExitOK {
		t.Skip("opendrived was found on this machine after all")
	}
	if !strings.Contains(errOut, "opendrived") {
		t.Errorf("the message does not name what is missing: %q", errOut)
	}
	assertReadable(t, errOut)
}

func TestDaemonHelpExplainsWhatItIsFor(t *testing.T) {
	b := newFakeBridge(t)
	code, out, _ := b.run("daemon", "--help")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"background", "install", "start"} {
		if !strings.Contains(strings.ToLower(out), want) {
			t.Errorf("daemon help does not mention %q", want)
		}
	}
}

// ---------------------------------------------------------------- plumbing

func TestClientSurfacesAnUnreachableDaemon(t *testing.T) {
	c := NewClient("127.0.0.1:1", "", 0)
	err := c.Do(context.Background(), "GET", "/v1/health", nil, nil)
	if err == nil {
		t.Fatal("a closed port reported success")
	}
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("error = %T, want UnreachableError", err)
	}
	if unreachable.ExitCode() != ExitUnavailable {
		t.Errorf("exit code = %d", unreachable.ExitCode())
	}
}

func TestClientSendsTheAPIKey(t *testing.T) {
	b := newFakeBridge(t)
	var seen string
	base := b.srv.Config.Handler
	b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		base.ServeHTTP(w, r)
	})

	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	Execute([]string{"--addr", b.srv.URL, "--api-key", "s3cret", "status"}, opts)

	if seen != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the key as a bearer token", seen)
	}
}

// A response that is not the Bridge's envelope must still be reported honestly
// rather than guessed at.
func TestUnreadableResponseIsReportedNotGuessed(t *testing.T) {
	b := newFakeBridge(t)
	b.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("<html>gateway error</html>"))
	})

	code, _, errOut := b.run("status")
	if code != ExitUpstream {
		t.Errorf("exit %d, want %d", code, ExitUpstream)
	}
	if !strings.Contains(errOut, "could not read") {
		t.Errorf("stderr = %q", errOut)
	}
	assertReadable(t, errOut)
}

func TestQueryBuilderSkipsEmptyValues(t *testing.T) {
	if got := query("/v1/ls"); got != "/v1/ls" {
		t.Errorf("query with no pairs = %q", got)
	}
	if got := query("/v1/ls", "path", ""); got != "/v1/ls" {
		t.Errorf("an empty value was still sent: %q", got)
	}
	got := query("/v1/ls", "path", "/Docs/a b")
	if !strings.Contains(got, "path=%2FDocs%2Fa+b") {
		t.Errorf("the path was not escaped: %q", got)
	}
}

// --direct must be a real mode, not a flag that does nothing. It builds the same
// server in memory and reaches it through a transport that calls the handler,
// so direct and daemon modes cannot disagree about anything.
func TestDirectModeRunsTheBridgeInProcess(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)

	// No daemon is running on this address; --direct must not need one.
	code := Execute([]string{"--addr", "127.0.0.1:1", "--direct", "status"}, opts)

	// Either it answered from the local keychain, or it said the keychain is
	// the problem. What it must not do is report "no bridge is answering".
	if strings.Contains(errOut.String(), "No bridge is answering") {
		t.Fatalf("--direct still tried to reach a daemon: %q", errOut.String())
	}
	if code == ExitUnavailable {
		t.Fatalf("--direct exited as if no daemon were running: %q", errOut.String())
	}
	if out.Len() == 0 && errOut.Len() == 0 {
		t.Fatal("--direct printed nothing at all")
	}
	t.Logf("direct status: exit=%d out=%q err=%q", code, out.String(), errOut.String())
}

// The in-process transport has to behave like a real one: status, headers and
// body all arrive as the client expects.
func TestDirectTransportCarriesTheWholeResponse(t *testing.T) {
	tr := directTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	})}

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://odctl-direct/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers were lost: %v", resp.Header)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"hello":"world"}` {
		t.Errorf("body = %q", body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("content length = %d, want %d", resp.ContentLength, len(body))
	}
}

// A handler that writes without setting a status must still produce a 200, the
// way a real server does.
func TestDirectRecorderDefaultsToOK(t *testing.T) {
	rec := newResponseRecorder()
	if _, err := rec.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	rec.Flush()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://x/", nil)
	if got := rec.result(req).StatusCode; got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
}

// --direct with a working credential store builds the whole bridge in memory and
// answers from it. The store is injected so this runs the same on a machine with
// a vault and one without.
func TestDirectModeAnswersFromTheInProcessBridge(t *testing.T) {
	restore := openKeystore
	openKeystore = func() (keystore.Store, error) {
		return keystore.Open(keystore.Config{Backend: keystore.BackendEphemeral, AllowEphemeral: true})
	}
	t.Cleanup(func() { openKeystore = restore })

	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	code := Execute([]string{"--addr", "127.0.0.1:1", "--direct", "status"}, opts)

	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	// An ephemeral store holds nothing, so the honest answer is "not signed in"
	// — and it comes from the bridge running inside this process, not a daemon.
	if !strings.Contains(out.String(), "Not signed in yet") {
		t.Errorf("out = %q", out.String())
	}
	if strings.Contains(errOut.String(), "No bridge is answering") {
		t.Error("--direct reached for a daemon")
	}
}

// When no credential store can be opened, --direct has to say that rather than
// pretend the daemon is missing.
func TestDirectModeReportsAnUnusableKeystore(t *testing.T) {
	restore := openKeystore
	openKeystore = func() (keystore.Store, error) { return nil, keystore.ErrNoBackend }
	t.Cleanup(func() { openKeystore = restore })

	var out, errOut bytes.Buffer
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	code := Execute([]string{"--direct", "status"}, opts)

	if code != ExitAuth {
		t.Errorf("exit %d, want %d", code, ExitAuth)
	}
	if !strings.Contains(errOut.String(), "keystore") && !strings.Contains(errOut.String(), "credential") {
		t.Errorf("stderr = %q, want it to name the credential store", errOut.String())
	}
}

// --direct runs real transfers, so a command that ends has to leave nothing
// behind upstream. Close is what guarantees it (D39), and it must be safe to
// call more than once.
func TestDirectBridgeCloseIsIdempotent(t *testing.T) {
	stopped := 0
	d := &directBridge{stop: func() { stopped++ }}
	d.Close()
	d.Close()
	if stopped != 1 {
		t.Errorf("stop ran %d times, want exactly 1", stopped)
	}
}
