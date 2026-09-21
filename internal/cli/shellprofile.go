package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Putting the programs on the user's PATH is a convenience, and §8.2 is
// deliberate about how much of it to do on their behalf: print the line, and
// write it only when asked in so many words.
//
// The reason is not squeamishness. A shell profile is a file the user owns and
// reads, other tools also edit it, and a mistake in it means their next terminal
// does not open properly. Something that edits it silently, on a command whose
// name is "install a service", is doing more than it said.
//
// So --modify-shell-profile is opt-in, the edit is a marked block, the file is
// backed up first, and uninstall removes exactly the block and nothing else.
const (
	profileBegin = "# >>> opendrive-bridge >>>"
	profileEnd   = "# <<< opendrive-bridge <<<"
)

// shellProfilePath picks the file to edit.
//
// Every return is the user's own home directory joined with a constant. Nothing
// from the environment reaches the path except through filepath.Base, and even
// that only chooses between fixed names — a SHELL of "/tmp/../../etc/passwd"
// selects the default branch, not a file called passwd. gosec's taint analysis
// (G703) cannot see that, which is why the writes below are annotated rather
// than the analysis switched off.
func shellProfilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// The login shell decides, not the platform: plenty of people run bash on
	// macOS and zsh on Linux.
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "bash":
		// .bash_profile is what a login shell reads on macOS; .bashrc on Linux.
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish")
	}
	// Unknown shell: guess by platform rather than refuse, and say so.
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, ".zshrc")
	}
	return filepath.Join(home, ".bashrc")
}

// pathLine is what the user is told to add, or what is added for them.
func pathLine(dir string) string {
	if filepath.Base(shellProfilePath()) == "config.fish" {
		return fmt.Sprintf("fish_add_path %q", dir)
	}
	return fmt.Sprintf("export PATH=%q:$PATH", dir)
}

// addToShellProfile writes the marked block, after taking a backup. It is a
// no-op when the block is already there, so running install twice does not
// leave two copies.
func addToShellProfile(dir string) (path string, changed bool, err error) {
	path = shellProfilePath()
	if path == "" {
		return "", false, fmt.Errorf("cannot work out which shell profile to edit; " +
			"add this line to yours by hand:\n  " + pathLine(dir))
	}

	// #nosec G304,G703 -- reviewed: path is $HOME joined with one of four
	// constants (see shellProfilePath), and this is the user's own file, edited
	// only because they asked for it with --modify-shell-profile.
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return path, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if strings.Contains(string(existing), profileBegin) {
		return path, false, nil
	}

	if len(existing) > 0 {
		backup := fmt.Sprintf("%s.opendrive-bridge-backup-%s", path, time.Now().Format("20060102-150405"))
		// #nosec G703 -- as above: a constant suffix on a path under $HOME.
		if err := os.WriteFile(backup, existing, 0o600); err != nil {
			return path, false, fmt.Errorf("cannot back up %s before editing it: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, false, fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}

	block := "\n" + profileBegin + "\n" +
		"# Added by 'odctl daemon install --modify-shell-profile'.\n" +
		"# 'odctl daemon uninstall' removes this block and nothing else.\n" +
		pathLine(dir) + "\n" + profileEnd + "\n"

	updated := append(existing, block...)
	// #nosec G703 -- as above.
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return path, false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return path, true, nil
}

// removeFromShellProfile takes the block out again, leaving everything else
// exactly as it was. Anything outside the markers is untouched, including a line
// the user added themselves that happens to look the same.
func removeFromShellProfile() (path string, changed bool, err error) {
	path = shellProfilePath()
	if path == "" {
		return "", false, nil
	}
	// #nosec G304,G703 -- as in addToShellProfile.
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return path, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	content := string(raw)
	start := strings.Index(content, profileBegin)
	if start < 0 {
		return path, false, nil
	}
	end := strings.Index(content[start:], profileEnd)
	if end < 0 {
		// The block was opened and never closed, which means somebody edited it
		// by hand. Removing to the end of the file would take their work with
		// it, so this stops and says what to do.
		return path, false, fmt.Errorf("%s has %q but no %q, so it has been edited by hand; "+
			"remove the block yourself and nothing will be lost", path, profileBegin, profileEnd)
	}
	end += start + len(profileEnd)
	// Take the newline that follows the block, and the blank line before it.
	if end < len(content) && content[end] == '\n' {
		end++
	}
	trimmed := content[:start] + content[end:]
	trimmed = strings.TrimRight(trimmed, "\n") + "\n"

	// #nosec G703 -- as above.
	if err := os.WriteFile(path, []byte(trimmed), 0o600); err != nil {
		return path, false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return path, true, nil
}
