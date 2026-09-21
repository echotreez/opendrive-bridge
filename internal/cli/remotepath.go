package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// Paths that name something in the account are checked before a request goes
// anywhere, so that a mistake is answered by the program the person is talking to
// rather than by a daemon two layers away.
//
// This file began as a Windows fix. The Windows smoke test printed
//
//	$ odctl ls /Smoke
//	Paths must start with a slash, for example /Documents/report.pdf.
//
// which was true about the value odctl received and useless to the person who
// typed it, because they had plainly typed a slash: Git Bash rewrites arguments
// beginning with a slash into Windows paths before the program starts, so
// `ls /Smoke` arrived as `ls "C:/Program Files/Git/Smoke"`. An accurate message
// about the wrong thing — the mistake this project keeps finding upstream.
//
// Windows is no longer supported (§8.1) and the explanation of MSYS argument
// rewriting has gone with it, along with the regular expression that recognised
// a volume-rooted path. That check turned out to be redundant rather than merely
// unneeded: "C:\\x", "C:/x" and "\\\\server\\share" all fail the plain
// "starts with a slash" test below, which was always the check doing the work.
// What is left applies on every platform, because a path that does not start at
// the account root is wrong wherever it was typed.

// remoteArgs adds a check for OpenDrive paths to an existing cobra argument
// validator. The positions are the arguments that name something in the account
// rather than on this computer, and they have to be named one at a time: `up`
// takes a local file first and a remote path second, so only position 1 is
// checked, and "./report.pdf" is correct in position 0 and wrong in position 1.
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
// paths too, because the two are answering different questions. The Bridge
// decides whether a path is acceptable; odctl can say what the person appears to
// have meant, having seen the argument as they typed it. A daemon on another
// machine — or in a container — knows nothing about the shell it came from.
func checkRemotePath(p string) error {
	switch {
	case p == "":
		return usage("A path is needed here, for example /Documents/report.pdf.")

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
