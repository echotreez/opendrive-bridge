package datacache

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The container check of whitepaper §8.3.1.
//
// §3.5.2 says write-back makes the gateway the only holder of a user's bytes for
// a while. A container sharpens that, and not because containers are fragile:
// because a container's writable layer is *disposable by design* and "delete it
// and start a new one" is a routine operation rather than an accident. `docker
// rm`, `docker compose down`, an image upgrade, an orchestrator rescheduling a
// pod — any one of them destroys whatever is in that layer, including bytes this
// gateway already answered 202 for. The user believes the file is stored. It left
// with the container.
//
// So three layers, per §8.3.1, and this file is the second:
//
//  1. the compose file ships a named volume for the cache by default, because a
//     default configuration has to be a safe one;
//  2. the daemon checks at startup and says so — in the log, and as
//     `durable: false` on /v1/cache/status for the GUI to show in red;
//  3. the documentation puts it first in the Docker section, level with the macOS
//     quarantine note, because both are the first thing that hurts somebody.
//
// # Warn, do not refuse
//
// The detection cannot be certain. Container runtimes differ, bind mounts and
// volumes are indistinguishable from inside in some configurations, and an
// unusual but perfectly safe setup exists for any rule this could apply. Refusing
// to start on a guess would be worse than the risk: a daemon that will not run is
// a certainty, and this is a probability.
//
// But the warning does not hedge. §8.3.1 is explicit — 话要说死. If the check
// fires, the message says that unsent data will be lost when the container is
// removed, without "may" or "might", because that is what happens.
//
// A user who knows better has an exact answer available: `write_back: false`
// turns the gateway into a read cache, nothing is ever dirty, and the warning
// stops on its own rather than needing to be silenced.

// durability answers whether the cache directory looks like it will outlive the
// process, and if not, why.
//
// The question is only interesting for write-back: with it off there is never
// anything here that OpenDrive does not already have, so a disposable directory
// costs a re-download.
func (c *DataCache) durability() (bool, string) {
	if !c.cfg.WriteBack {
		return true, ""
	}
	if !inContainer() {
		return true, ""
	}
	mounted, err := isMountPoint(c.cfg.Dir)
	if err != nil {
		// Unable to tell. Say that rather than guessing either way — an
		// unexplained `durable: false` would be as misleading as a wrong true.
		return true, "Running in a container. Whether " + c.cfg.Dir + " is on a persistent " +
			"volume could not be determined, so check that it is: if it is not, data that has " +
			"not yet reached OpenDrive is lost when the container is removed."
	}
	if mounted {
		return true, ""
	}
	return false, "The cache directory " + c.cfg.Dir + " is inside the container's own " +
		"writable layer, not on a persistent volume. Data that has not yet reached OpenDrive " +
		"is lost when this container is removed — and removing a container is a routine thing " +
		"to do, not an accident. Mount a volume at " + c.cfg.Dir + " (the supplied " +
		"docker-compose.yaml does), or set write_back to false to keep nothing here that " +
		"OpenDrive does not already have."
}

// warnIfNotDurable writes the startup warning. Called once by Open.
func (c *DataCache) warnIfNotDurable() {
	durable, note := c.durability()
	if durable && note == "" {
		return
	}
	level := slog.LevelWarn
	if !durable {
		level = slog.LevelError
	}
	c.log.Log(context.Background(), level, note,
		slog.String("datacache_dir", c.cfg.Dir),
		slog.Bool("datacache_durable", durable))
}

// inContainer reports whether this process looks containerised.
//
// Two signals, both cheap and both well known. /.dockerenv is written by Docker
// itself; the cgroup path mentions the runtime under Docker, containerd,
// Kubernetes and Podman. Neither is documented as an API, which is part of why
// this warns rather than refuses.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil { // Podman
		return true
	}
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	text := string(data)
	for _, marker := range []string{"docker", "containerd", "kubepods", "libpod", "lxc"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// isMountPoint reports whether dir is its own mount — a volume or a bind mount —
// rather than part of the filesystem it sits in.
//
// /proc/self/mountinfo is read rather than comparing st_dev with the parent's,
// because the device-number trick gives a false negative for a bind mount from
// the same filesystem, which is a perfectly ordinary way to mount a host
// directory into a container.
//
// Reading the file and deciding the answer are separate functions, so the decision
// can be tested against a known list instead of against whatever the machine
// running the tests happens to have mounted. The first version of the test asserted
// that anything under / is on a mount, which is exactly what this must *not* say —
// and it passed on macOS and failed on Linux, which is the least useful way to
// learn that an assertion was wrong.
func isMountPoint(dir string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	points, err := readMountPoints(f)
	if err != nil {
		return false, err
	}
	return pathIsUnderAMount(dir, points)
}

// readMountPoints pulls the mount points out of mountinfo.
//
// Field 5 is the mount point. The fields before it are numeric or device ids and
// cannot contain a space; the mount point itself is escaped by the kernel, so a tab
// or space in a path arrives as \011 or \040.
func readMountPoints(r io.Reader) ([]string, error) {
	var out []string
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 5 {
			continue
		}
		out = append(out, unescapeMountPath(fields[4]))
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// pathIsUnderAMount reports whether dir, or any directory above it, is one of the
// given mount points.
//
// The walk goes up because a volume mounted at /data makes /data/cache durable too,
// and checking only the exact path would warn about a setup that is fine.
//
// The root is excluded on purpose, and this is the whole subtlety of the function:
// everything is under /, so counting it would make every possible layout "on a
// mount" and the check would never fire. In a container the interesting ancestors
// are the volumes, and / is the disposable writable layer this is trying to warn
// about.
func pathIsUnderAMount(dir string, points []string) (bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	ancestors := map[string]bool{}
	for p := filepath.Clean(abs); p != "/" && p != "." && p != filepath.Dir(p); p = filepath.Dir(p) {
		ancestors[p] = true
	}
	if len(ancestors) == 0 {
		return false, errors.New("datacache: the cache directory has no mountable ancestor")
	}
	for _, point := range points {
		if ancestors[point] {
			return true, nil
		}
	}
	return false, nil
}

// unescapeMountPath undoes the octal escaping the kernel applies to mount points.
func unescapeMountPath(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	replacements := []struct{ from, to string }{
		{`\011`, "\t"},
		{`\012`, "\n"},
		{`\040`, " "},
		{`\134`, `\`},
	}
	for _, r := range replacements {
		p = strings.ReplaceAll(p, r.from, r.to)
	}
	return p
}
