package datacache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// One gateway per cache directory, enforced (§3.5.4: "single-user local
// accelerator, no multi-instance coherence").
//
// # Why this exists
//
// The journal is an append-only log with one writer's view of the index behind it.
// Two processes appending to it would interleave records each built from a
// different index; either one's next compaction would rewrite the file from its own
// view and silently drop the other's objects. For a clean object that costs a
// re-download. For an unsent one it is the user's file, gone, from a directory that
// looked perfectly healthy.
//
// Nothing made that hard to do. It became easier to do by accident once the
// recommended container setup was a host directory bind-mounted into the container:
// running `docker run` twice, or a host opendrived pointed at the same folder as a
// container, both produce two writers on one journal. So the directory is locked.
//
// # Why flock and not a pid file
//
// A pid file survives the process that wrote it. After a crash — which is exactly
// when the next start matters most, because that is when unsent objects are waiting
// to be re-queued (rule 2) — a pid file would either block the restart or need
// staleness heuristics that guess whether a pid has been reused. flock is released
// by the kernel when the holding process dies, however it dies, SIGKILL included.
// The rule 4 test kills a child with SIGKILL and reopens the same directory; with a
// pid file that test would fail, and it should.
//
// What is written into the lock file is for the person reading the refusal, not for
// the lock: which pid, on which host, since when. The lock itself is the kernel's.
//
// # Where it does not reach
//
// flock is advisory and per kernel. Two processes on one Linux host — two
// containers, or a container and a host process, on the same bind mount — share a
// kernel and are excluded properly. On Docker Desktop for Mac the containers run in
// a Linux VM and the host is macOS, so a macOS process and a container sharing a
// directory through the VM's file sharing are not guaranteed to see each other's
// locks. Network filesystems are unreliable for flock generally. Both are stated in
// the documentation rather than papered over here.

// lockName is the lock file inside the cache directory.
const lockName = "lock"

// dirLock holds the directory for as long as the gateway is open.
type dirLock struct {
	f *os.File
}

// ErrLocked means another gateway already has this directory. It is its own error
// so the daemon's refusal can be recognised and tested, and so its message can say
// what to do rather than just what went wrong.
var ErrLocked = errors.New("the cache directory is already in use by another bridge")

// lockDir takes the directory, or explains who has it.
func lockDir(dir string) (*dirLock, error) {
	path := filepath.Join(dir, lockName)
	// #nosec G304 -- the configured cache directory joined with a constant
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("datacache: cannot open %s: %w", path, err)
	}

	// Non-blocking: a second instance should refuse at once and say why, not hang
	// at startup waiting for a lock the first will hold for weeks.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readHolder(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s is held by %s. Two bridges writing one cache "+
				"journal would lose files, so this one will not start. Stop the other one, "+
				"or give this one its own --cache-dir", ErrLocked, dir, holder)
		}
		return nil, fmt.Errorf("datacache: cannot lock %s: %w", dir, err)
	}

	// Ours now. Record who, for the next refusal to quote.
	host, _ := os.Hostname()
	info := fmt.Sprintf("pid %d on %s since %s\n", os.Getpid(), host,
		time.Now().UTC().Format(time.RFC3339))
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(info), 0)
		_ = f.Sync()
	}
	return &dirLock{f: f}, nil
}

// readHolder reports what the current holder wrote, for the refusal message. A
// missing or unreadable record still gets a sentence rather than an empty string.
func readHolder(f *os.File) string {
	buf := make([]byte, 256)
	n, _ := f.ReadAt(buf, 0)
	text := strings.TrimSpace(string(buf[:n]))
	if text == "" {
		return "another process"
	}
	// Only the first line, and only printable text: the file is ours, but a
	// refusal message is not the place to find out it was not.
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strconv.QuoteToASCII(text)
}

// release gives the directory back. Closing the descriptor releases the flock; the
// file itself stays, because deleting it would race with a new instance that had
// just opened it and would then lock an unlinked inode.
func (l *dirLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
