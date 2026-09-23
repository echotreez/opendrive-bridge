package datacache

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// One gateway per cache directory. See lock.go for why, and for why flock rather
// than a pid file.

// A second gateway on the same directory is refused before it reads the journal.
func TestASecondGatewayOnTheSameDirectoryIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	first, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("the first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Open(Config{Dir: dir})
	if err == nil {
		_ = second.Close()
		t.Fatal("a second gateway opened a directory the first one holds; two writers on one " +
			"journal would lose files")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("the refusal = %v, want ErrLocked", err)
	}
	// The refusal says who has it and what to do, because a person reading it at
	// startup has to decide which of two bridges to stop.
	msg := err.Error()
	for _, want := range []string{dir, "pid " + strconv.Itoa(os.Getpid()), "--cache-dir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// Closing gives the directory back.
func TestClosingReleasesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	first, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("reopening after Close: %v", err)
	}
	_ = again.Close()
}

// A start that fails after taking the lock must give it back. Otherwise one bad
// start — an impossible configuration, a journal that cannot be read — would leave
// every later attempt refused for a reason that has nothing to do with the problem.
func TestAFailedStartDoesNotKeepTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A journal that is a directory cannot be read, so Open fails after the lock.
	if err := os.Mkdir(filepath.Join(dir, journalName), 0o700); err != nil {
		t.Fatal(err)
	}
	if c, err := Open(Config{Dir: dir}); err == nil {
		_ = c.Close()
		t.Fatal("Open succeeded over an unreadable journal")
	}
	// Fix it and try again: the lock must not be what stops this one.
	if err := os.Remove(filepath.Join(dir, journalName)); err != nil {
		t.Fatal(err)
	}
	c, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("after fixing the journal, Open = %v; the failed start kept the lock", err)
	}
	_ = c.Close()
}

// ---------------------------------------------------------------- across processes

const lockChildEnv = "ODB_DATACACHE_LOCK_CHILD"

// lockChild opens a gateway and waits to be killed.
func lockChild(dir string) {
	c, err := Open(Config{Dir: dir})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: open:", err)
		os.Exit(2)
	}
	_ = c // held open deliberately; the parent kills this process
	fmt.Println("ready")
	time.Sleep(5 * time.Minute)
}

// The lock holds across processes, and a crash does not leave it behind.
//
// The second half is the reason this is flock and not a pid file. After a crash is
// exactly when the next start matters most — that is when unsent objects are
// waiting to be re-queued — and a lock that outlived the process would block it. The
// kernel releases a flock when its holder dies, however it dies, so a SIGKILLed
// gateway leaves a directory the next one can open straight away.
func TestTheLockHoldsAcrossProcessesAndDiesWithItsHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess and kills it")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "cache")

	cmd := exec.Command(exe, "-test.run=XXX_NO_SUCH_TEST") // #nosec G204 -- this test binary
	cmd.Env = append(os.Environ(), lockChildEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	ready := make(chan bool, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			if s.Text() == "ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("the child stopped before holding the lock: %s", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the child never became ready: %s", stderr.String())
	}

	// While the child is alive, this process is refused, and told which pid.
	c, err := Open(Config{Dir: dir})
	if err == nil {
		_ = c.Close()
		t.Fatal("a second process opened a directory another process holds")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("refusal = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "pid "+strconv.Itoa(cmd.Process.Pid)) {
		t.Errorf("the refusal does not name the child's pid %d:\n%v", cmd.Process.Pid, err)
	}

	// SIGKILL: the holder gets no chance to release anything.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed = true
	_, _ = cmd.Process.Wait()

	// And the directory is free at once, with no staleness to wait out.
	c, err = Open(Config{Dir: dir})
	if err != nil {
		t.Fatalf("after the holder was killed, Open = %v; the lock outlived its process", err)
	}
	_ = c.Close()
}
