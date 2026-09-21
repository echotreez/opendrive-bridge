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
  # Both waits are there to keep the shell from printing "Terminated" after the
  # result line, where it reads like a failure in a run that passed.
  if [ -n "${DPID:-}" ]; then kill "$DPID" 2>/dev/null; wait "$DPID" 2>/dev/null; fi
  if [ -n "${MPID:-}" ]; then kill "$MPID" 2>/dev/null; wait "$MPID" 2>/dev/null; fi
  rm -rf "$WORK"
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
ODB_BASE_URL="http://$UPSTREAM/api/v1" \
  "$OPENDRIVED" --addr "127.0.0.1:$PORT" --keystore ephemeral --ephemeral \
  --state-dir "$WORK/jobs" --log-level error &
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

say "a failure reports itself properly"
"$ODCTL" --addr "127.0.0.1:$PORT" stat /Smoke/nope.txt > "$WORK/err.txt" 2>&1
code=$?
[ "$code" = "5" ]; check $? "a missing path exits 5 (got $code)"
sed 's/^/      /' "$WORK/err.txt"

printf '\n=== smoke %s\n' "$([ $FAIL -eq 0 ] && echo PASSED || echo FAILED)"
exit $FAIL
