package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The failure this file guards against was found by the Windows smoke test, not
// by a unit test, and the reason is worth stating: nothing on a Mac or a Linux
// runner rewrites arguments, so on every machine the project develops on, the
// bug is invisible. These tests reproduce the rewritten value directly.

func TestAWindowsPathIsNotAnOpenDrivePath(t *testing.T) {
	// Exactly what Git Bash hands odctl when the person typed "ls /Smoke".
	for _, arg := range []string{
		`C:/Program Files/Git/Smoke`,
		`C:\Program Files\Git\Smoke`,
		`c:/msys64/Documents`,
		`\\fileserver\share\Documents`,
	} {
		err := checkRemotePath(arg)
		if err == nil {
			t.Fatalf("%s was accepted as a path in the account", arg)
		}
		if !strings.Contains(err.Error(), "not in your OpenDrive account") {
			t.Errorf("%s: the message does not say where the path is: %v", arg, err)
		}
		if code := exitCodeOf(t, err); code != ExitUsage {
			t.Errorf("%s: exit code %d, want %d", arg, code, ExitUsage)
		}
	}
}

func TestTheRewrittenPathMessageNamesTheCause(t *testing.T) {
	// Windows-only: the hint is about a Windows shell, and printing it on a Mac
	// would send someone looking for a cause that cannot apply.
	if !rewritingShell() {
		hint := rewriteHint()
		if hint != "" {
			t.Fatalf("the Git Bash explanation was offered off Windows: %q", hint)
		}
		t.Skip("the rewriting only happens under a Windows shell")
	}
	err := checkRemotePath(`C:/Program Files/Git/Smoke`)
	msg := err.Error()
	for _, want := range []string{"this shell changed it", "MSYS_NO_PATHCONV=1", "PowerShell"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q:\n%s", want, msg)
		}
	}
}

func TestRelativeAndTildePathsAreRefusedWithAnExample(t *testing.T) {
	for _, arg := range []string{"Documents/report.pdf", "report.pdf", "~/Documents", "~"} {
		err := checkRemotePath(arg)
		if err == nil {
			t.Fatalf("%q was accepted", arg)
		}
		if !strings.Contains(err.Error(), "/Documents/report.pdf") {
			t.Errorf("%q: the message shows no example of a correct path: %v", arg, err)
		}
	}
}

func TestOpenDrivePathsAreAccepted(t *testing.T) {
	// Including the ones that only look odd: a folder may legitimately be named
	// with a colon or a backslash inside it, and a check that refused those
	// would break paths the account really has.
	for _, arg := range []string{"/", "/Documents", "/Documents/report.pdf", "/a:b", `/a\b`, "/Program Files"} {
		if err := checkRemotePath(arg); err != nil {
			t.Errorf("%q was refused: %v", arg, err)
		}
	}
}

// remoteArgs must check the remote position and leave the local one alone —
// `odctl up C:\report.pdf /Documents/report.pdf` is correct on Windows, and an
// over-eager check would reject the very platform it was written for.
func TestOnlyTheRemoteArgumentIsChecked(t *testing.T) {
	up := remoteArgs(cobra.ExactArgs(2), 1)
	if err := up(nil, []string{`C:\report.pdf`, "/Documents/report.pdf"}); err != nil {
		t.Errorf("a Windows local file was refused: %v", err)
	}
	if err := up(nil, []string{`C:\report.pdf`, `C:\Documents\report.pdf`}); err == nil {
		t.Error("a Windows path in the remote position was accepted")
	}

	// An optional path that was not given must not be checked into existence.
	ls := remoteArgs(cobra.MaximumNArgs(1), 0)
	if err := ls(nil, nil); err != nil {
		t.Errorf("ls with no argument was refused: %v", err)
	}

	// The wrapped validator still runs, and runs first: a count mistake is a
	// clearer thing to report than the shape of an argument that may not belong
	// there at all.
	if err := ls(nil, []string{"/a", "/b"}); err == nil {
		t.Error("too many arguments were accepted")
	}
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	c, ok := err.(coded)
	if !ok {
		t.Fatalf("%T carries no exit code", err)
	}
	return c.ExitCode()
}
