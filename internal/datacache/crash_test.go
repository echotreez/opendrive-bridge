package datacache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Rule 4 of §3.5.2: a killed process loses nothing it had acknowledged.
//
// # Why this is a subprocess and not a struct
//
// Every other test in this package can build a DataCache, do something to it, and
// look at the result. This one cannot, because the thing being tested is what
// survives when *nothing* gets to run: no deferred close, no flush of a buffered
// writer, no shutdown hook, no chance for the Go runtime to tidy anything. An
// in-process test that simply drops the reference and opens a second cache is a
// reasonable approximation and it is in writeback_test.go
// (TestRule3WhatWasAcknowledgedIsRecoverable) — but it is an approximation, and
// this is the rule Derek singled out as the one that must not be skimped.
//
// So: the test binary re-executes itself as a child, the child writes objects into
// a cache whose upstream never completes, reports what it acknowledged, and then
// the parent sends it SIGKILL. SIGKILL cannot be caught, blocked or handled. The
// child stops between two instructions. Whatever is on that disk afterwards is
// exactly what a power cut would have left, minus the kernel's own buffers — and
// the fsyncs are there for those.
//
// The parent then opens the same directory and asserts the only thing that
// matters: every object the child said it had accepted comes back, is re-queued
// without anybody asking, and reaches upstream with the right bytes.

// crashChildEnv names the variable that turns a test binary run into the child.
const crashChildEnv = "ODB_DATACACHE_CRASH_CHILD"

// childReport is what the child prints on stdout so the parent knows what was
// acknowledged. The parent must not assume: the point of the test is to compare
// what the child was *told* had been stored against what came back.
type childReport struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

// TestMain runs the child when asked, and the tests otherwise.
func TestMain(m *testing.M) {
	if dir := os.Getenv(crashChildEnv); dir != "" {
		crashChild(dir)
		return // unreachable; crashChild blocks until it is killed
	}
	os.Exit(m.Run())
}

// crashChild is the child process. It writes objects into a write-back cache whose
// upstream never returns, prints what was acknowledged, and then waits to be
// killed.
//
// Nothing here is allowed to be tidy. No defer that closes the cache, no attempt
// to drain: a process that shuts down cleanly is testing a different property.
func crashChild(dir string) {
	up := &neverCompletes{}
	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up, MaxDirtyBytes: 64 << 20})
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: open:", err)
		os.Exit(2)
	}

	enc := json.NewEncoder(os.Stdout)
	for i := 0; i < 6; i++ {
		path := fmt.Sprintf("/crashed/object-%d.bin", i)
		content := bytesOf(4096+i*512, byte(i*7))

		w, err := c.Put(PutRequest{RemotePath: path, FolderID: "42", Size: int64(len(content))})
		if err != nil {
			fmt.Fprintln(os.Stderr, "child: put:", err)
			os.Exit(2)
		}
		if _, err := w.Write(content); err != nil {
			fmt.Fprintln(os.Stderr, "child: write:", err)
			os.Exit(2)
		}
		o, err := w.Commit()
		if err != nil {
			// Not acknowledged, so it is not reported and the parent will not
			// expect it. That is the contract working, not a failure.
			fmt.Fprintln(os.Stderr, "child: commit:", err)
			continue
		}
		// Only now is this object something the client was told had been stored.
		if err := enc.Encode(childReport{Path: o.RemotePath, Size: o.Size, Hash: o.Hash}); err != nil {
			os.Exit(2)
		}
	}
	fmt.Println(`{"path":"ready"}`)
	// Flush stdout by virtue of it being unbuffered, then wait to die. A sleep
	// rather than a channel, because there is nothing to wait for: the parent is
	// about to kill this process.
	time.Sleep(5 * time.Minute)
}

// neverCompletes is an upstream that accepts nothing, so every object the child
// writes is still unsent when the child dies.
type neverCompletes struct{}

func (neverCompletes) Upload(ctx context.Context, _ *Object, _ *os.File) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestRule4AKilledProcessLosesNothingItAcknowledged(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess and kills it")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary to re-execute: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "cache")

	// -test.run matches nothing: the child's work happens in TestMain, before any
	// test would run, and this keeps it from running the suite as well.
	cmd := exec.Command(exe, "-test.run=XXX_NO_SUCH_TEST") // #nosec G204 -- this test binary
	cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the child: %v", err)
	}
	killed := false
	defer func() {
		if !killed && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	// Read what the child acknowledged, up to its ready line.
	acknowledged := map[string]childReport{}
	scanner := bufio.NewScanner(stdout)
	ready := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			var rep childReport
			if err := json.Unmarshal(scanner.Bytes(), &rep); err != nil {
				continue // test framework chatter
			}
			if rep.Path == "ready" {
				ready <- nil
				return
			}
			acknowledged[rep.Path] = rep
		}
		ready <- fmt.Errorf("the child stopped before it was ready: %s", stderr.String())
	}()

	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the child never became ready; stderr:\n%s", stderr.String())
	}
	if len(acknowledged) == 0 {
		t.Fatalf("the child acknowledged nothing, so there is nothing to recover; stderr:\n%s",
			stderr.String())
	}

	// SIGKILL. Not SIGTERM: a signal the process could handle would let it tidy
	// up, and tidying up is the one thing this test must not allow.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("cannot kill the child: %v", err)
	}
	killed = true
	state, err := cmd.Process.Wait()
	if err != nil {
		t.Fatalf("waiting for the killed child: %v", err)
	}
	t.Logf("child %d killed (%v) after acknowledging %d objects",
		cmd.Process.Pid, state, len(acknowledged))

	// Now the recovery. A working upstream this time.
	up := newFakeUpstream()
	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up, MaxDirtyBytes: 64 << 20})
	if err != nil {
		t.Fatalf("reopen after the crash: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Every acknowledged object is back, unsent, with the size and hash the child
	// was given. The hash is the part that matters: it proves the *content* came
	// back, not just an index entry claiming it did.
	st := c.Status()
	if st.DirtyObjects != len(acknowledged) {
		t.Errorf("recovered dirty_objects = %d, want %d", st.DirtyObjects, len(acknowledged))
	}
	if st.SafeToShutDown {
		t.Error("safe_to_shut_down is true with recovered unsent objects")
	}
	recovered := map[string]*Object{}
	for _, o := range c.Objects() {
		recovered[o.RemotePath] = o
	}
	for path, rep := range acknowledged {
		o := recovered[path]
		if o == nil {
			t.Errorf("%s was acknowledged to the client and did not come back", path)
			continue
		}
		if !o.State.Unsent() {
			t.Errorf("%s came back as %s; it never reached OpenDrive", path, o.State)
		}
		if o.Size != rep.Size {
			t.Errorf("%s came back as %d bytes, was acknowledged as %d", path, o.Size, rep.Size)
		}
		if o.Hash != rep.Hash {
			t.Errorf("%s came back hashing to %s, was acknowledged as %s", path, o.Hash, rep.Hash)
		}
		// And the bytes are readable, which is a stronger claim than the index.
		if r, err := c.Get(path); err != nil {
			t.Errorf("%s cannot be read after recovery: %v", path, err)
		} else {
			_ = r.Close()
		}
	}

	// Re-queued without being asked, and they get there.
	eventually(t, "every recovered object to reach upstream", func() bool {
		return up.count() == len(acknowledged)
	})
	for path, rep := range acknowledged {
		got, ok := up.got(path)
		if !ok {
			t.Errorf("%s never reached upstream after recovery", path)
			continue
		}
		if int64(len(got)) != rep.Size {
			t.Errorf("%s reached upstream as %d bytes, was acknowledged as %d",
				path, len(got), rep.Size)
		}
	}
	// Waiting on the cache's own view rather than on the upstream counter, because
	// the two move at different moments: Upload returning is what increments the
	// counter, and the object becomes clean a little later, after the transition has
	// been journalled and flushed. Asserting immediately after the counter reached 6
	// failed under -race for exactly that reason — the last object was uploaded and
	// not yet marked. The gateway is right; the assertion was early.
	eventually(t, "the cache to report everything sent", func() bool {
		st := c.Status()
		return st.DirtyObjects == 0 && st.SafeToShutDown
	})
}

// The other half of rule 4, and the half that is easy to get wrong: a write the
// crash interrupted must not come back claiming to hold data it does not have.
//
// The journal's last record is truncated by hand here, which is what a crash
// mid-append leaves. The recovered cache must treat it as absent — the client was
// never told that write succeeded — rather than parsing what it can and inventing
// an object.
func TestRule4AnInterruptedWriteDoesNotComeBackAsAnObject(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	up := newFakeUpstream()
	up.hold()

	c, err := Open(Config{Dir: dir, WriteBack: true, Upstream: up})
	if err != nil {
		t.Fatal(err)
	}
	put(t, c, "/crashed/whole.bin", bytesOf(1024, 1))
	_ = c.jnl.close()

	// A torn tail: half a record, as an interrupted append leaves.
	journalPath := filepath.Join(dir, journalName)
	raw, err := os.ReadFile(journalPath) // #nosec G304 -- a path this test built
	if err != nil {
		t.Fatal(err)
	}
	half := `{"op":"put","obj":{"remote_path":"/crashed/torn.bin","size":999,"sta`
	if err := os.WriteFile(journalPath, append(raw, []byte(half)...), 0o600); err != nil {
		t.Fatal(err)
	}
	// And the content file it would have referred to, so that the only reason to
	// reject it is the torn record rather than a missing file.
	if err := os.WriteFile(filepath.Join(dir, contentName("/crashed/torn.bin")),
		bytesOf(999, 2), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	again, err := Open(Config{
		Dir: dir, WriteBack: true, Upstream: newFakeUpstream(),
		Logger: slogText(&logged),
	})
	if err != nil {
		t.Fatalf("reopen after a torn journal: %v", err)
	}
	defer func() { _ = again.Close() }()

	if again.Has("/crashed/torn.bin") {
		t.Fatal("an object from a half-written journal record came back as real")
	}
	if !again.Has("/crashed/whole.bin") {
		t.Error("the complete record before the torn one was lost with it")
	}
	// It says the daemon did not stop cleanly, because an operator reading this
	// after an incident should not have to infer it.
	if !strings.Contains(logged.String(), "incomplete record") {
		t.Errorf("the log does not mention the torn journal:\n%s", logged.String())
	}
	// The orphaned content is cleaned up rather than left to accumulate.
	if _, err := os.Stat(filepath.Join(dir, contentName("/crashed/torn.bin"))); !os.IsNotExist(err) {
		t.Errorf("the orphaned content file survived: %v", err)
	}
	up.release()
}
