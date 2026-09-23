#!/usr/bin/env bash
# Release smoke test: prove the binaries that were just built actually run.
#
# Compiling is not evidence. A cross-compiled binary can build cleanly and then
# fail on its target for reasons no compiler sees — a missing syscall, a keystore
# backend that cannot initialise, a linker flag that broke the version string. So
# this starts the real daemon against a mock OpenDrive and drives it with the
# real odctl, on the platform the binary is for.
#
# Usage: scripts/smoke.sh <dir-containing-opendrived-and-odctl>
set -uo pipefail

# This script used to carry a second personality for Windows: an .exe suffix, a
# pair of MSYS variables to stop Git Bash rewriting arguments that begin with a
# slash, and WORK/WORKN — the same temporary directory spelled twice, because
# mktemp hands back /tmp/tmp.XXXX and only the shell understands that. All of it
# was correct and all of it is gone with Windows support (§8.1). It is worth a
# sentence here because that second personality is what §8.1 means when it says
# Windows was the only platform needing a parallel code path: even the smoke test
# had one.
BIN_DIR="${1:-.}"
OPENDRIVED="$BIN_DIR/opendrived"
ODCTL="$BIN_DIR/odctl"
WORK="$(mktemp -d)"
FAIL=0

cleanup() {
  # Keep the evidence when something went wrong. A smoke test that deletes the
  # logs of its own failure is a smoke test you debug twice.
  if [ "${FAIL:-0}" -ne 0 ]; then
    echo "smoke failed; leaving the working directory at $WORK"
    KEEP_WORK=1
  fi
  # Both waits are there to keep the shell from printing "Terminated" after the
  # result line, where it reads like a failure in a run that passed.
  if [ -n "${DPID:-}" ]; then kill "$DPID" 2>/dev/null; wait "$DPID" 2>/dev/null; fi
  if [ -n "${MPID:-}" ]; then kill "$MPID" 2>/dev/null; wait "$MPID" 2>/dev/null; fi
  [ -n "${KEEP_WORK:-}" ] || rm -rf "$WORK"
}
trap cleanup EXIT

say()   { printf '\n--- %s\n' "$1"; }
check() { if [ "$1" = "0" ]; then echo "    ok: $2"; else echo "    FAIL: $2"; FAIL=1; fi }

for f in "$OPENDRIVED" "$ODCTL"; do
  if [ ! -x "$f" ]; then echo "FAIL: $f is missing or not executable"; exit 1; fi
done

say "the binaries report a version"
"$OPENDRIVED" --version | tee "$WORK/v1.txt"
grep -q "opendrived" "$WORK/v1.txt"; check $? "opendrived --version"
"$ODCTL" --version | tee "$WORK/v2.txt"
grep -qi "odctl\|version" "$WORK/v2.txt"; check $? "odctl --version"

# A release build must not report itself as a development build: that would mean
# the version never reached the linker.
if [ "${SMOKE_EXPECT_VERSION:-}" != "" ]; then
  grep -q "$SMOKE_EXPECT_VERSION" "$WORK/v1.txt"; check $? "opendrived reports $SMOKE_EXPECT_VERSION"
  grep -q "$SMOKE_EXPECT_VERSION" "$WORK/v2.txt"; check $? "odctl reports $SMOKE_EXPECT_VERSION"
fi

say "a mock OpenDrive"
"$BIN_DIR/mockupstream" --addr 127.0.0.1:0 --port-file "$WORK/upstream.addr" &
MPID=$!
for _ in $(seq 1 50); do [ -s "$WORK/upstream.addr" ] && break; sleep 0.2; done
UPSTREAM="$(cat "$WORK/upstream.addr" 2>/dev/null)"
[ -n "$UPSTREAM" ]; check $? "mock upstream is listening on ${UPSTREAM:-nothing}"
[ -z "$UPSTREAM" ] && exit 1

say "the daemon starts and signs in"
PORT=9761
# An ephemeral credential store, chosen explicitly: this is a throwaway run and
# the daemon refuses to guess that for itself (§9.2).
# The caching gateway is on for this run. "It compiles" is not evidence that it
# works on a platform (§8.1's standing rule), and the gateway is the first part of
# this program that holds the user's data — so it is exercised here, on the real
# daemon, on every platform that has a runner, rather than only in unit tests on
# whatever machine happened to run them.
ODB_BASE_URL="http://$UPSTREAM/api/v1" \
  "$OPENDRIVED" --addr "127.0.0.1:$PORT" --keystore ephemeral --ephemeral \
  --state-dir "$WORK/jobs" --cache-dir "$WORK/cache" --log-level error &
DPID=$!

for _ in $(seq 1 50); do
  "$ODCTL" --addr "127.0.0.1:$PORT" status >/dev/null 2>&1 && break
  sleep 0.2
done

"$ODCTL" --addr "127.0.0.1:$PORT" status | tee "$WORK/status.txt"
grep -qi "not signed in" "$WORK/status.txt"; check $? "status answers before signing in"

"$ODCTL" --addr "127.0.0.1:$PORT" login smoke@example.com --password whatever \
  > "$WORK/login.txt" 2>&1
check $? "login"
grep -qi "signed in as smoke@example.com" "$WORK/login.txt"; check $? "login names the account"

say "a listing"
"$ODCTL" --addr "127.0.0.1:$PORT" ls / | tee "$WORK/ls.txt"
grep -q "Smoke" "$WORK/ls.txt"; check $? "ls / shows the folder"

"$ODCTL" --addr "127.0.0.1:$PORT" ls /Smoke | tee "$WORK/ls2.txt"
grep -q "hello.txt" "$WORK/ls2.txt"; check $? "ls /Smoke shows the file"

say "the caching gateway"
# Both routes into the gateway are exercised: the streaming endpoints with curl, and
# the job endpoints through odctl. The second used to bypass the cache entirely,
# which meant the feature was unreachable from the command line — see the two legs
# in internal/jobs/cache.go.
if ! command -v curl >/dev/null 2>&1; then
  say "curl is not available; skipping the cache checks"
else
  API="http://127.0.0.1:$PORT/v1"

  # Nothing written yet, so nothing is outstanding and it says so. The wording
  # matters as much as the number: this is the line a person reads before turning a
  # machine off (§3.5.2 rule 3).
  "$ODCTL" --addr "127.0.0.1:$PORT" cache status | tee "$WORK/cache0.txt"
  grep -qi "safe to stop the bridge" "$WORK/cache0.txt"
  check $? "an empty cache reports itself safe to stop"

  # A write-back upload: the body lands in the cache and the answer is 202 —
  # accepted here, not yet on OpenDrive. A 201 would mean the opposite, so the
  # status code is checked rather than just the success.
  printf 'cached content for the smoke test\n' > "$WORK/cached.txt"
  code=$(curl -sS -o "$WORK/cacheup.json" -w '%{http_code}' \
    -X PUT --data-binary "@$WORK/cached.txt" \
    "$API/upload/stream?path=/Smoke/cached.txt&overwrite=true")
  [ "$code" = "202" ]
  check $? "a write through the gateway answers 202 (got $code)"
  sed 's/^/      /' "$WORK/cacheup.json"
  grep -q '"cache_state":"dirty"' "$WORK/cacheup.json"
  check $? "the response says the file is not on OpenDrive yet"

  # It is readable at once, which is the read-after-write consistency §3.5.3 offers
  # against upstream's own delay (D44) — and it is a HIT, from this disk.
  hdr=$(curl -sS -D - -o "$WORK/readback.txt" "$API/download/stream?path=/Smoke/cached.txt")
  printf '%s' "$hdr" | grep -qi "^X-Cache: HIT"
  check $? "a just-written file reads back from the cache"
  cmp -s "$WORK/cached.txt" "$WORK/readback.txt"
  check $? "the bytes read back are the bytes written"

  # The object listing names it. Deliberately not `--unsent`: against a mock on
  # loopback the flush can finish before this line runs, so requiring it to still be
  # outstanding would be a race — and the flusher being fast is not a failure. What
  # is worth asserting is that the listing describes state in words a person can
  # read rather than in the API's own vocabulary.
  "$ODCTL" --addr "127.0.0.1:$PORT" cache objects | tee "$WORK/cacheobjs.txt"
  grep -q "/Smoke/cached.txt" "$WORK/cacheobjs.txt"
  check $? "the object listing names the file"
  grep -qE "NOT uploaded yet|on OpenDrive" "$WORK/cacheobjs.txt"
  check $? "the listing says what state it is in, in plain words"
  grep -q "dirty" "$WORK/cacheobjs.txt" && { echo "    FAIL: an API state name leaked into the listing"; FAIL=1; }

  # Flushing with --wait is the contract: it returns when nothing is left.
  "$ODCTL" --addr "127.0.0.1:$PORT" cache flush --wait > "$WORK/cacheflush.txt" 2>&1
  check $? "cache flush --wait returns"
  grep -qi "safe to stop the bridge" "$WORK/cacheflush.txt"
  check $? "after a flush it reports itself safe to stop"

  "$ODCTL" --addr "127.0.0.1:$PORT" cache status > "$WORK/cache2.txt" 2>&1
  grep -qi "NOT safe" "$WORK/cache2.txt" && { echo "    FAIL: still not safe after a flush"; FAIL=1; }
  grep -q "1 file" "$WORK/cache2.txt" || true

  # A read of something that was never written is a MISS, and fills the cache, so
  # the next read of the same path is a HIT. That is the read-through half.
  hdr=$(curl -sS -D - -o "$WORK/first.txt" "$API/download/stream?path=/Smoke/hello.txt")
  printf '%s' "$hdr" | grep -qi "^X-Cache: MISS"
  check $? "the first read of a file is a miss"
  hdr=$(curl -sS -D - -o "$WORK/second.txt" "$API/download/stream?path=/Smoke/hello.txt")
  printf '%s' "$hdr" | grep -qi "^X-Cache: HIT"
  check $? "the second read is served from the cache"
  cmp -s "$WORK/first.txt" "$WORK/second.txt"
  check $? "the cached copy matches what was downloaded"

  # And now through odctl, which is how almost everybody will actually use this.
  # `up` copies the file into the cache (leg one) and waits for the gateway to send
  # it (leg two); the job reports which leg it is on throughout.
  printf 'uploaded by odctl through the cache\n' > "$WORK/viaodctl.txt"
  "$ODCTL" --addr "127.0.0.1:$PORT" up "$WORK/viaodctl.txt" /Smoke/viaodctl.txt \
    > "$WORK/odctlup.txt" 2>&1
  check $? "odctl up goes through the cache"

  "$ODCTL" --addr "127.0.0.1:$PORT" --json jobs > "$WORK/jobs.json" 2>&1
  grep -q '"phase"' "$WORK/jobs.json"
  check $? "the job reports which leg it was on"

  "$ODCTL" --addr "127.0.0.1:$PORT" cache objects > "$WORK/objs2.txt" 2>&1
  grep -q "/Smoke/viaodctl.txt" "$WORK/objs2.txt"
  check $? "the file odctl uploaded is in the cache"

  # And a download of it is served locally: the file is already here, so this costs
  # no request to OpenDrive at all.
  "$ODCTL" --addr "127.0.0.1:$PORT" down /Smoke/viaodctl.txt "$WORK/viaodctl-back.txt" \
    > "$WORK/odctldown.txt" 2>&1
  check $? "odctl down of a cached file"
  cmp -s "$WORK/viaodctl.txt" "$WORK/viaodctl-back.txt"
  check $? "the round trip through odctl preserved the bytes"

  # Clearing works once nothing is outstanding.
  "$ODCTL" --addr "127.0.0.1:$PORT" cache clear > "$WORK/cacheclear.txt" 2>&1
  check $? "cache clear"

  # The directory holds the user's files in the clear, so the mode is the whole of
  # its protection (§3.5.4).
  # GNU stat first, BSD second. -f is "format" on BSD and "filesystem status" on
  # GNU, so putting BSD first made GNU *succeed* at printing filesystem details and
  # the fallback never ran — the check then compared "700" against a block of text
  # and failed on Linux only.
  mode=$(stat -c '%a' "$WORK/cache" 2>/dev/null || stat -f '%Lp' "$WORK/cache" 2>/dev/null)
  [ "$mode" = "700" ]
  check $? "the cache directory is 0700 (got ${mode:-unknown})"
fi

say "a failure reports itself properly"
"$ODCTL" --addr "127.0.0.1:$PORT" stat /Smoke/nope.txt > "$WORK/err.txt" 2>&1
code=$?
[ "$code" = "5" ]; check $? "a missing path exits 5 (got $code)"
sed 's/^/      /' "$WORK/err.txt"

printf '\n=== smoke %s\n' "$([ $FAIL -eq 0 ] && echo PASSED || echo FAILED)"
exit $FAIL
