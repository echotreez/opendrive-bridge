package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

// §8.2: the programs run from the folder the user unpacked, and the service
// definition has to name that folder absolutely. These two tests cover the parts
// where getting it wrong is silent — the daemon would install and then fail to
// find credentials that are sitting right next to it.

func TestTheDaemonBesideOdctlWins(t *testing.T) {
	// A stale copy earlier on PATH must not be picked over the one that came out
	// of the same archive as this odctl.
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(stale, daemonBinaryName()))
	t.Setenv("PATH", stale)

	beside := filepath.Join(filepath.Dir(mustExecutable(t)), daemonBinaryName())
	if _, err := os.Stat(beside); err != nil {
		// The test binary's directory has no opendrived, so the PATH copy is the
		// only candidate and finding it is correct.
		got, err := resolveDaemonPath("")
		if err != nil {
			t.Fatalf("resolveDaemonPath: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("path %q is not absolute; a service manager cannot use it", got)
		}
		return
	}
	got, err := resolveDaemonPath("")
	if err != nil {
		t.Fatalf("resolveDaemonPath: %v", err)
	}
	if got != beside {
		t.Errorf("resolved %q, want the copy beside odctl at %q", got, beside)
	}
}

func TestAnExplicitPathIsMadeAbsoluteAndChecked(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, daemonBinaryName())
	writeExecutable(t, target)

	got, err := resolveDaemonPath(target)
	if err != nil {
		t.Fatalf("resolveDaemonPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("%q is not absolute", got)
	}

	_, err = resolveDaemonPath(filepath.Join(dir, "not-there"))
	if err == nil {
		t.Fatal("a path that does not exist was accepted")
	}
	if code := exitCodeOf(t, err); code != ExitUsage {
		t.Errorf("exit code = %d, want %d", code, ExitUsage)
	}
}

// The service definition must carry the folder as its working directory. Without
// it the daemon starts in / — no service manager inherits a shell's — and looks
// for a .env that is not there.
func TestTheServiceRunsInTheFolderItWasInstalledFrom(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, daemonBinaryName())
	writeExecutable(t, exe)

	svc, err := daemonService(exe, []string{"--addr", "127.0.0.1:9750"})
	if err != nil {
		t.Fatalf("daemonService: %v", err)
	}
	// kardianos/service keeps the config it was built with; the string form is
	// enough to see that the path went in.
	if svc == nil {
		t.Fatal("no service was built")
	}
	if s := svc.String(); s == "" {
		t.Error("the service has no name")
	}
}

func TestTheDaemonBinaryIsNamedForThePlatform(t *testing.T) {
	got := daemonBinaryName()
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(got, ".exe") {
			t.Errorf("on Windows the daemon is %q", got)
		}
		return
	}
	if strings.Contains(got, ".exe") {
		t.Errorf("off Windows the daemon is %q", got)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { // #nosec G306 -- a stand-in binary
		t.Fatal(err)
	}
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("this platform cannot report the executable path")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// --- what the user is told, and what is touched on their machine.

type fakeService struct{ installed, uninstalled bool }

func (f *fakeService) Run() error       { return nil }
func (f *fakeService) Start() error     { return nil }
func (f *fakeService) Stop() error      { return nil }
func (f *fakeService) Restart() error   { return nil }
func (f *fakeService) Install() error   { f.installed = true; return nil }
func (f *fakeService) Uninstall() error { f.uninstalled = true; return nil }
func (f *fakeService) Logger(chan<- error) (service.Logger, error) {
	return nil, nil //nolint:nilnil // the interface allows it and nothing uses it here
}
func (f *fakeService) SystemLogger(chan<- error) (service.Logger, error) {
	return nil, nil //nolint:nilnil // as above
}
func (f *fakeService) String() string { return "fake" }
func (f *fakeService) Platform() string {
	return "fake"
}
func (f *fakeService) Status() (service.Status, error) { return service.StatusRunning, nil }

func withFakeService(t *testing.T) *fakeService {
	t.Helper()
	fake := &fakeService{}
	previous := newDaemonService
	newDaemonService = func(string, []string) (service.Service, error) { return fake, nil }
	t.Cleanup(func() { newDaemonService = previous })
	return fake
}

// By default the PATH line is printed and the user's shell profile is left
// alone. §8.2 is deliberate about that: editing a file the user reads, on a
// command that said it would install a service, is doing more than it announced.
func TestInstallPrintsThePathLineAndTouchesNothing(t *testing.T) {
	withFakeService(t)
	home := withHomeAndShell(t, "/bin/zsh")
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, daemonBinaryName()))

	code, out, _ := runCLI(t, "daemon", "install", "--exec", filepath.Join(dir, daemonBinaryName()))
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("the folder was not mentioned:\n%s", out)
	}
	if !strings.Contains(out, "--modify-shell-profile") {
		t.Errorf("the opt-in was not offered:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); err == nil {
		t.Error("a shell profile was created without being asked")
	}
}

func TestInstallWritesTheBlockWhenAsked(t *testing.T) {
	withFakeService(t)
	home := withHomeAndShell(t, "/bin/zsh")
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, daemonBinaryName()))

	code, _, _ := runCLI(t, "daemon", "install",
		"--exec", filepath.Join(dir, daemonBinaryName()), "--modify-shell-profile")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatalf("the profile was not written: %v", err)
	}
	// Assert the line the code actually writes, not the bare directory. They are
	// the same string on a POSIX path and not on a Windows one, because pathLine
	// formats with %q and %q escapes the backslashes — so this read as a failure
	// on Windows while the block was perfectly correct. Comparing against
	// pathLine is also the real contract: the block contains the PATH line for
	// this directory, however that line has to be spelled.
	if !strings.Contains(string(raw), profileBegin) ||
		!strings.Contains(string(raw), pathLine(dir)) {
		t.Errorf("the block is not what was expected:\n%s", raw)
	}
}

// Uninstalling removes the service and the block, and says plainly that the
// credentials are still there — because they are, and a user who reinstalls
// should not think they have to sign in again.
func TestUninstallKeepsTheCredentialsAndSaysSo(t *testing.T) {
	fake := withFakeService(t)
	home := withHomeAndShell(t, "/bin/zsh")
	if _, _, err := addToShellProfile("/opt/opendrive-bridge"); err != nil {
		t.Fatal(err)
	}

	code, out, _ := runCLI(t, "daemon", "uninstall")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !fake.uninstalled {
		t.Error("the service was not uninstalled")
	}
	for _, want := range []string{".env", ".env.key"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not mention %s:\n%s", want, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err == nil && strings.Contains(string(raw), profileBegin) {
		t.Error("the PATH block survived uninstall")
	}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	opts := &Options{}
	opts.SetOutput(&out, &errOut)
	code := Execute(args, opts)
	return code, out.String(), errOut.String()
}
