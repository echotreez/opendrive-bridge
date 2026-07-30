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

BIN_DIR="${1:-.}"
EXE=""
MSYS_SHELL=""
case "$(uname -s 2>/dev/null || echo Windows)" in
  MINGW*|MSYS*|CYGWIN*|Windows*) EXE=".exe"; MSYS_SHELL=1 ;;
esac

# Git Bash rewrites any argument beginning with a slash into a Windows path
# before the program it is starting has run a single instruction, so `ls /Smoke`
# arrives as `ls "C:/Program Files/Git/Smoke"`. That is real and it happens to
# real users, but it is not what most Windows users see — from PowerShell and
# Command Prompt nothing is rewritten, and that is what the documentation tells
# people to use. So the assertions below run with the rewriting switched off,
# and one case at the end switches it back on to check that the error explains
# itself when it does happen.
if [ -n "$MSYS_SHELL" ]; then
  export MSYS_NO_PATHCONV=1
  export MSYS2_ARG_CONV_EXCL='*'
fi

OPENDRIVED="$BIN_DIR/opendrived$EXE"
ODCTL="$BIN_DIR/odctl$EXE"
WORK="$(mktemp -d)"
FAIL=0

# Switching the rewriting off exposed what it had been quietly covering: mktemp
# hands back /tmp/tmp.XXXX, which only this shell understands, and the first run
# without conversion failed with "open /tmp/tmp.BqPQOxV5b5/upstream.addr: The
# system cannot find the path specified."
#
# So the directory needs both spellings. WORK is for this script — redirects,
# grep, mktemp — and WORKN is the same directory as Windows names it, for the
# arguments handed to the binaries. The conversion existed for a reason; the
# only thing wrong with it was applying to remote paths as well.
WORKN="$WORK"
if [ -n "$MSYS_SHELL" ] && command -v cygpath >/dev/null 2>&1; then
  WORKN="$(cygpath -w "$WORK")"
fi

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
"$BIN_DIR/mockupstream$EXE" --addr 127.0.0.1:0 --port-file "$WORKN/upstream.addr" &
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
  --state-dir "$WORKN/jobs" --log-level error &
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

# Only this shell, on this platform, can check the following — which is why the
# problem it covers survived every test written on a Mac.
if [ -n "$MSYS_SHELL" ]; then
  say "a path the shell rewrote says so"
  (
    unset MSYS_NO_PATHCONV MSYS2_ARG_CONV_EXCL
    "$ODCTL" --addr "127.0.0.1:$PORT" ls /Smoke
  ) > "$WORK/rewritten.txt" 2>&1
  code=$?
  [ "$code" = "2" ]; check $? "a rewritten path is a mistake in the command (got $code)"
  grep -qi "not in your OpenDrive account" "$WORK/rewritten.txt"
  check $? "the message says the path is a local one"
  grep -q "MSYS_NO_PATHCONV=1" "$WORK/rewritten.txt"
  check $? "the message names a way out"
  sed 's/^/      /' "$WORK/rewritten.txt"

  # The way out has to work, or the message is only a better-worded lie. Note
  # that this uses MSYS_NO_PATHCONV alone — exactly what the message advises —
  # rather than both variables the top of this script sets.
  (
    unset MSYS2_ARG_CONV_EXCL
    MSYS_NO_PATHCONV=1 "$ODCTL" --addr "127.0.0.1:$PORT" ls /Smoke
  ) > "$WORK/wayout.txt" 2>&1
  grep -q "hello.txt" "$WORK/wayout.txt"
  check $? "MSYS_NO_PATHCONV=1 makes the same command work"
fi

printf '\n=== smoke %s\n' "$([ $FAIL -eq 0 ] && echo PASSED || echo FAILED)"
exit $FAIL
