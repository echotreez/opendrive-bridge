package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A path that is not anchored at the account root is refused, whatever kind of
// path it happens to be.
//
// The strings below are what Git Bash used to hand odctl when somebody typed
// "ls /Smoke" on Windows. Windows is gone (§8.1) and so is the regular
// expression that recognised them specially — this test stays because removing
// that expression must not have opened a hole, and it had not: every one of them
// fails the plain "starts with a slash" rule, which was always the check doing
// the work. Keeping the cases is cheaper than re-deriving that argument later.
func TestAPathOnSomeOtherComputerIsNotAnOpenDrivePath(t *testing.T) {
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
		// Whatever the message says, it has to show what a right one looks like.
		if !strings.Contains(err.Error(), "/Documents/report.pdf") {
			t.Errorf("%s: the message shows no example of a correct path: %v", arg, err)
		}
		if code := exitCodeOf(t, err); code != ExitUsage {
			t.Errorf("%s: exit code %d, want %d", arg, code, ExitUsage)
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

// remoteArgs must check the remote position and leave the local one alone. The
// local argument is a path on this computer and none of the account's rules apply
// to it: an over-eager check would refuse perfectly ordinary local filenames.
func TestOnlyTheRemoteArgumentIsChecked(t *testing.T) {
	up := remoteArgs(cobra.ExactArgs(2), 1)
	if err := up(nil, []string{"./report.pdf", "/Documents/report.pdf"}); err != nil {
		t.Errorf("a relative local file was refused: %v", err)
	}
	if err := up(nil, []string{"./report.pdf", "./Documents/report.pdf"}); err == nil {
		t.Error("a local-looking path in the remote position was accepted")
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
