package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `odctl cache` tests, and they are almost entirely about wording.
//
// The command's job is to turn numbers into a decision somebody can act on. "3
// dirty objects" is a number; "NOT safe to stop the bridge yet" is the thing the
// person came to find out. So these assert the sentences rather than the fields: if
// the phrasing changes, the test should have to be read and agreed with, not pass
// because the JSON still parses.
//
// They drive the fakeBridge in cli_test.go rather than a second harness. A first
// draft of this file brought its own, which is the mistake this project keeps
// finding in other people's code — two implementations of the same thing, one of
// which will be wrong.

// The one line the command exists for, in both directions.
func TestCacheStatusSaysWhetherItIsSafeToStop(t *testing.T) {
	t.Run("safe", func(t *testing.T) {
		b := newFakeBridge(t)
		b.withCache(map[string]any{
			"enabled": true, "write_back": true, "dir": "/opt/bridge/cache",
			"bytes": 1024, "max_bytes": 2048, "objects": 1,
			"dirty_bytes": 0, "dirty_objects": 0, "max_dirty_bytes": 512,
			"safe_to_shut_down": true, "durable": true,
			"hits": 3, "misses": 1, "hit_rate": 0.75,
		}, nil)

		code, out, _ := b.run("cache", "status")
		if code != ExitOK {
			t.Fatalf("exit = %d: %s", code, out)
		}
		if !strings.Contains(out, "safe to stop the bridge") {
			t.Errorf("the verdict is missing:\n%s", out)
		}
		if strings.Contains(out, "NOT safe") {
			t.Errorf("an empty cache was reported as unsafe:\n%s", out)
		}
		if !strings.Contains(out, "75% local") {
			t.Errorf("the hit rate is not reported:\n%s", out)
		}
	})

	t.Run("not safe", func(t *testing.T) {
		b := newFakeBridge(t)
		b.withCache(map[string]any{
			"enabled": true, "write_back": true, "dir": "/opt/bridge/cache",
			"bytes": 50 << 20, "max_bytes": 1 << 30, "objects": 4,
			"dirty_bytes": 43200000, "dirty_objects": 3, "max_dirty_bytes": 1 << 30,
			"oldest_dirty_age_seconds": 12, "safe_to_shut_down": false, "durable": true,
		}, nil)

		code, out, _ := b.run("cache", "status")
		if code != ExitOK {
			t.Fatalf("exit = %d: %s", code, out)
		}
		if !strings.Contains(out, "NOT safe to stop the bridge yet") {
			t.Errorf("the warning is missing:\n%s", out)
		}
		// And it says what to do, rather than leaving the reader to find the right
		// subcommand for themselves.
		if !strings.Contains(out, "odctl cache flush --wait") {
			t.Errorf("no way out is offered:\n%s", out)
		}
		if !strings.Contains(out, "Longest wait") {
			t.Errorf("the age of the oldest unsent file is not reported:\n%s", out)
		}
	})

	// §8.3.1: a container whose cache is not on a volume gets told, last, so it is
	// the thing left on screen.
	t.Run("not durable", func(t *testing.T) {
		note := "The cache directory /data/cache is inside the container's own writable layer."
		b := newFakeBridge(t)
		b.withCache(map[string]any{
			"enabled": true, "write_back": true, "dir": "/data/cache",
			"safe_to_shut_down": true, "durable": false, "durability_note": note,
		}, nil)

		_, out, _ := b.run("cache", "status")
		if !strings.Contains(out, "Warning:") || !strings.Contains(out, note) {
			t.Errorf("the durability warning is missing:\n%s", out)
		}
		warn, verdict := strings.Index(out, "Warning:"), strings.Index(out, "safe to stop")
		if warn >= 0 && verdict >= 0 && warn < verdict {
			t.Error("the warning came before the verdict; it should be the last thing on screen")
		}
	})
}

// Switched off, it says so in one line rather than printing a table of zeroes.
func TestCacheStatusSaysWhenTheCacheIsOff(t *testing.T) {
	b := newFakeBridge(t) // no withCache: the default is "off"
	code, out, _ := b.run("cache", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "switched off") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "Holding:") {
		t.Errorf("a switched-off cache printed a size table:\n%s", out)
	}
}

// Without write-back there is nothing to wait for, and saying so saves the reader
// wondering where the "not yet uploaded" figure went.
func TestCacheStatusExplainsAReadOnlyCache(t *testing.T) {
	b := newFakeBridge(t)
	b.withCache(map[string]any{
		"enabled": true, "write_back": false, "dir": "/opt/bridge/cache",
		"bytes": 100, "max_bytes": 1000, "objects": 2,
		"safe_to_shut_down": true, "durable": true,
	}, nil)

	_, out, _ := b.run("cache", "status")
	if !strings.Contains(out, "straight to OpenDrive") {
		t.Errorf("a read-only cache does not explain itself:\n%s", out)
	}
	if strings.Contains(out, "Not yet on OpenDrive") {
		t.Errorf("a read-only cache reported an unsent figure:\n%s", out)
	}
}

// The object list uses words, not the API's state names: somebody looking at a
// list of their own files should be told what it means for them.
func TestCacheObjectsSaysWhichFilesAreNotUploaded(t *testing.T) {
	b := newFakeBridge(t)
	b.withCache(nil, []map[string]any{
		{"remote_path": "/Docs/waiting.bin", "size": 2048, "state": "dirty",
			"flush_attempts": 2, "last_error": "OpenDrive turned this away for a moment"},
		{"remote_path": "/Docs/done.bin", "size": 1024, "state": "clean"},
	})

	_, out, _ := b.run("cache", "objects")
	if !strings.Contains(out, "NOT uploaded yet") {
		t.Errorf("the unsent file is not called out:\n%s", out)
	}
	if !strings.Contains(out, "on OpenDrive") {
		t.Errorf("the uploaded file is not described:\n%s", out)
	}
	// "dirty" is an API word. It must not reach a list of somebody's own files.
	if strings.Contains(out, "dirty") {
		t.Errorf("the API's state name leaked into a user-facing list:\n%s", out)
	}
	// A file that keeps failing shows why, in the classifier's wording.
	if !strings.Contains(out, "turned this away") {
		t.Errorf("the reason for a stuck upload is not shown:\n%s", out)
	}

	_, only, _ := b.run("cache", "objects", "--unsent")
	if strings.Contains(only, "/Docs/done.bin") {
		t.Errorf("--unsent listed an uploaded file:\n%s", only)
	}
	if !strings.Contains(only, "/Docs/waiting.bin") {
		t.Errorf("--unsent omitted the unsent file:\n%s", only)
	}
}

func TestCacheObjectsSaysWhenThereIsNothing(t *testing.T) {
	b := newFakeBridge(t)
	b.withCache(nil, []map[string]any{})

	_, out, _ := b.run("cache", "objects")
	if !strings.Contains(out, "cache is empty") {
		t.Errorf("out = %q", out)
	}
	_, unsent, _ := b.run("cache", "objects", "--unsent")
	if !strings.Contains(unsent, "Nothing is waiting") {
		t.Errorf("out = %q", unsent)
	}
}

// A completed flush ends on the reassurance, because that is the point of running
// it. An incomplete one says how to block.
func TestCacheFlushReportsWhatHappened(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		b := newFakeBridge(t)
		b.withCache(map[string]any{
			"enabled": true, "write_back": true, "dirty_objects": 0, "dirty_bytes": 0,
			"safe_to_shut_down": true, "durable": true,
		}, nil)

		code, out, _ := b.run("cache", "flush", "--wait")
		if code != ExitOK {
			t.Fatalf("exit = %d: %s", code, out)
		}
		if !strings.Contains(out, "safe to stop the bridge") {
			t.Errorf("out = %q", out)
		}
	})

	t.Run("started", func(t *testing.T) {
		b := newFakeBridge(t)
		b.withCache(map[string]any{
			"enabled": true, "write_back": true, "dirty_objects": 2, "dirty_bytes": 4096,
			"safe_to_shut_down": false, "durable": true,
		}, nil)

		_, out, _ := b.run("cache", "flush")
		if !strings.Contains(out, "Uploading") || !strings.Contains(out, "--wait") {
			t.Errorf("a non-blocking flush does not say how to block:\n%s", out)
		}
	})
}

// A 409 from the daemon reaches the user as the daemon's own sentence. The CLI
// must not paraphrase it: §4.5's message field is already written for a person, and
// a second rewording here would be a second thing to keep right — and the one more
// likely to drift, since it is further from the code that knows what happened.
func TestCacheRefusalsReachTheUserUnchanged(t *testing.T) {
	const message = "that would throw away data this cache is still the only copy of; flush it first"
	b := newFakeBridge(t)
	b.failOn("/v1/cache/refresh", failure{
		status: http.StatusConflict, code: "conflict", msg: message,
	})

	code, _, errOut := b.run("cache", "refresh", "/Docs/precious.bin")
	if code == ExitOK {
		t.Fatal("a 409 exited 0")
	}
	if !strings.Contains(errOut, message) {
		t.Errorf("the daemon's own wording did not reach the user:\n%s", errOut)
	}
}

// The remote path is checked before a request goes anywhere, the same as every
// other command that names something in the account.
func TestCacheRefreshChecksThePath(t *testing.T) {
	b := newFakeBridge(t)
	for _, bad := range []string{"Docs/relative", "~/Docs"} {
		code, _, errOut := b.run("cache", "refresh", bad)
		if code != ExitUsage {
			t.Errorf("refresh %q exited %d, want %d (%s)", bad, code, ExitUsage, errOut)
		}
	}
}

// --json prints the daemon's answer rather than the prose, for scripts.
func TestCacheStatusJSONIsTheDaemonsAnswer(t *testing.T) {
	b := newFakeBridge(t)
	b.withCache(map[string]any{
		"enabled": true, "write_back": true, "dirty_objects": 2,
		"safe_to_shut_down": false, "durable": true,
	}, nil)

	code, out, _ := b.runJSON("cache", "status")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json did not print JSON: %v\n%s", err, out)
	}
	if got["safe_to_shut_down"] != false {
		t.Errorf("safe_to_shut_down = %v", got["safe_to_shut_down"])
	}
	// The prose must not be mixed in with it.
	if strings.Contains(out, "NOT safe") {
		t.Errorf("--json printed the human wording too:\n%s", out)
	}
}

// Clearing reports the size left, so a caller can see it worked.
func TestCacheClearReportsWhatIsLeft(t *testing.T) {
	b := newFakeBridge(t)
	b.withCache(map[string]any{
		"enabled": true, "write_back": true, "bytes": 0, "objects": 0,
		"safe_to_shut_down": true, "durable": true,
	}, nil)

	code, out, _ := b.run("cache", "clear")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "Cleared") {
		t.Errorf("out = %q", out)
	}
}
