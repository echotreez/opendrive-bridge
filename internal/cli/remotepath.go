package cli

import (
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// This file exists because of one line of output from the Windows smoke test:
//
//	$ odctl ls /Smoke
//	Paths must start with a slash, for example /Documents/report.pdf.
//
// The complaint was true about the value odctl received and useless to the
// person who typed it, because they had plainly typed a slash. Git Bash — and
// MSYS2, and Cygwin — rewrite arguments that begin with a slash into Windows
// paths before the program starts, so `ls /Smoke` arrives as
// `ls "C:/Program Files/Git/Smoke"`. The rewriting happens in the shell and is
// finished before odctl has run a single instruction, so odctl cannot undo it.
// What it can do is recognise the result and name the cause.
//
// The general shape of the mistake is the one this project keeps finding
// upstream: an accurate message about the wrong thing.

// volumeRooted matches a path anchored to a Windows volume — C:\..., C:/... or
// a \\server\share name. None of those can name anything in an OpenDrive
// account, whose paths always start at a single root.
var volumeRooted = regexp.MustCompile(`^([A-Za-z]:[\\/]|\\\\)`)

// remoteArgs adds a check for OpenDrive paths to an existing cobra argument
// validator. The positions are the arguments that name something in the
// account rather than on this computer: `up` takes a local file first and a
// remote path second, so only position 1 is checked, and a Windows path there
// is a mistake while the same string in position 0 is correct.
//
// Positions past the end of args are skipped, so an optional trailing path
// (`ls` with no argument) needs no special case.
func remoteArgs(inner cobra.PositionalArgs, positions ...int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if inner != nil {
			if err := inner(cmd, args); err != nil {
				return err
			}
		}
		for _, i := range positions {
			if i < 0 || i >= len(args) {
				continue
			}
			if err := checkRemotePath(args[i]); err != nil {
				return err
			}
		}
		return nil
	}
}

// checkRemotePath rejects arguments that cannot be paths in an OpenDrive
// account, before a request goes anywhere.
//
// This is deliberately a client-side check even though the Bridge validates
// paths too. The Bridge cannot write this message: the shell that rewrote the
// argument is the caller's, and a daemon on another machine — or in a
// container — has no way to know it was involved.
func checkRemotePath(p string) error {
	switch {
	case p == "":
		return usage("A path is needed here, for example /Documents/report.pdf.")

	case volumeRooted.MatchString(p):
		return usage("%s is a path on this computer, not in your OpenDrive account.%s", p, rewriteHint())

	case strings.HasPrefix(p, "~"):
		// The shell expands ~ before odctl sees it only when it is a real home
		// directory; when it is not, the tilde arrives literally and would be
		// looked up as a folder with that name.
		return usage("Paths in your OpenDrive account start with a slash, not with ~, "+
			"for example /Documents/report.pdf. Got %s.", p)

	case !strings.HasPrefix(p, "/"):
		return usage("Paths in your OpenDrive account start at the top, with a slash, "+
			"for example /Documents/report.pdf. Got %s.", p)
	}
	return nil
}

// rewriteHint explains the Git Bash argument rewriting, but only where it can
// actually be the cause. On a Mac or a Linux box a Windows path in a command is
// simply a typo, and a paragraph about MSYS would be noise.
func rewriteHint() string {
	if !rewritingShell() {
		return ""
	}
	return "\n\nIf you typed a path beginning with a slash, this shell changed it: " +
		"Git Bash rewrites arguments like /Documents into Windows paths before odctl runs. " +
		"Put MSYS_NO_PATHCONV=1 in front of the command, or run odctl from PowerShell " +
		"or Command Prompt instead."
}

// rewritingShell reports whether odctl was started by a shell that performs the
// rewriting. Git Bash and MSYS2 both set MSYSTEM; Cygwin does not, but sets
// TERM and a Unix-shaped HOME, and the conservative test is enough for the
// shells people actually use on Windows.
func rewritingShell() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if os.Getenv("MSYSTEM") != "" {
		return true
	}
	return strings.Contains(strings.ToLower(os.Getenv("SHELL")), "sh")
}
