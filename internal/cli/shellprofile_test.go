package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Editing a user's shell profile is the most intrusive thing odctl does, so the
// tests are about restraint: what it writes, that it backs up first, and that
// removing it takes exactly the block and nothing else.

func withHomeAndShell(t *testing.T, shell string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", shell)
	return home
}

func TestTheBlockIsAddedRemovedAndLeavesTheRestAlone(t *testing.T) {
	home := withHomeAndShell(t, "/bin/zsh")
	profile := filepath.Join(home, ".zshrc")
	original := "# the user's own file\nexport EDITOR=vim\nalias ll='ls -la'\n"
	if err := os.WriteFile(profile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	path, changed, err := addToShellProfile("/opt/opendrive-bridge")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !changed || path != profile {
		t.Fatalf("changed=%v path=%q", changed, path)
	}

	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "/opt/opendrive-bridge") {
		t.Error("the directory did not reach the profile")
	}
	if !strings.HasPrefix(string(after), original) {
		t.Error("the user's own lines were disturbed")
	}

	// A backup exists, holding what was there before.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var backups int
	for _, e := range entries {
		if strings.Contains(e.Name(), "opendrive-bridge-backup") {
			backups++
			raw, err := os.ReadFile(filepath.Join(home, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != original {
				t.Error("the backup does not hold the original file")
			}
		}
	}
	if backups != 1 {
		t.Errorf("backups = %d, want 1", backups)
	}

	// Removing puts it back exactly.
	if _, changed, err := removeFromShellProfile(); err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	restored, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Errorf("after removal the profile is:\n%q\nwant:\n%q", restored, original)
	}
}

// Running install twice must not leave two blocks.
func TestAddingTwiceChangesNothingTheSecondTime(t *testing.T) {
	withHomeAndShell(t, "/bin/bash")
	if _, changed, err := addToShellProfile("/opt/odb"); err != nil || !changed {
		t.Fatalf("first add: changed=%v err=%v", changed, err)
	}
	_, changed, err := addToShellProfile("/opt/odb")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("the block was added a second time")
	}
}

// A line the user wrote themselves that happens to mention the directory is not
// ours to remove. Only the marked block is.
func TestRemovalLeavesAnUnmarkedLineAlone(t *testing.T) {
	home := withHomeAndShell(t, "/bin/zsh")
	profile := filepath.Join(home, ".zshrc")
	mine := "export PATH=\"/opt/opendrive-bridge\":$PATH  # I added this myself\n"
	if err := os.WriteFile(profile, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := removeFromShellProfile(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != mine {
		t.Errorf("the user's own line was touched: %q", after)
	}
}

// If somebody edited the block by hand and lost the end marker, removing to the
// end of the file would take their work with it. Stop and say so instead.
func TestAHalfEditedBlockIsReportedNotGuessedAt(t *testing.T) {
	home := withHomeAndShell(t, "/bin/zsh")
	profile := filepath.Join(home, ".zshrc")
	broken := "export EDITOR=vim\n" + profileBegin + "\nexport PATH=/opt/odb:$PATH\n" +
		"# ... and the user deleted the end marker\nalias ll='ls -la'\n"
	if err := os.WriteFile(profile, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := removeFromShellProfile(); err == nil {
		t.Fatal("a block with no end marker was removed anyway")
	} else if changed {
		t.Error("the file was changed despite the error")
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != broken {
		t.Error("the file was modified")
	}
}

// fish needs its own syntax; export PATH= would simply not work there.
func TestFishGetsFishSyntax(t *testing.T) {
	withHomeAndShell(t, "/usr/local/bin/fish")
	if got := pathLine("/opt/odb"); !strings.HasPrefix(got, "fish_add_path") {
		t.Errorf("fish got %q", got)
	}
	withHomeAndShell(t, "/bin/zsh")
	if got := pathLine("/opt/odb"); !strings.HasPrefix(got, "export PATH=") {
		t.Errorf("zsh got %q", got)
	}
}
