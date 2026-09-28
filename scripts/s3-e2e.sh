#!/usr/bin/env bash
# S3 end to end: real backup programs against the real daemon (§3.6, S1).
#
# Unit tests drive the gateway with minio-go. That proves the protocol; it does
# not prove that restic, which checks every byte it wrote, and rclone, which
# lists and compares, can run a backup and a restore through the bridge. This
# does both, against internal/odfake (an OpenDrive that keeps what it is given)
# so that the delivery half — cache to OpenDrive — is in the loop too, and then
# reads everything back from OpenDrive with the cache emptied.
#
# Usage: scripts/s3-e2e.sh <dir with opendrived, odctl and mockupstream>
# Needs restic and rclone on PATH, and python3 for reading JSON.
set -uo pipefail

BIN_DIR="${1:-.}"
WORK="$(mktemp -d)"
FAIL=0
API=127.0.0.1:19850
S3HTTP=127.0.0.1:19852
S3HTTPS=127.0.0.1:19851

cleanup() {
  if [ "${FAIL:-0}" -ne 0 ]; then
    echo "s3-e2e failed; leaving $WORK"; tail -50 "$WORK/daemon.log" 2>/dev/null
  fi
  if [ -n "${DPID:-}" ]; then kill "$DPID" 2>/dev/null; wait "$DPID" 2>/dev/null; fi
  if [ -n "${MPID:-}" ]; then kill "$MPID" 2>/dev/null; wait "$MPID" 2>/dev/null; fi
  [ "${FAIL:-0}" -ne 0 ] || rm -rf "$WORK"
}
trap cleanup EXIT

say()   { printf '\n--- %s\n' "$1"; }
check() { if [ "$1" = "0" ]; then echo "    ok: $2"; else echo "    FAIL: $2"; FAIL=1; fi }
need()  { command -v "$1" >/dev/null || { echo "FAIL: $1 is not installed"; exit 1; }; }
need restic; need rclone; need python3
ODCTL="$BIN_DIR/odctl --addr $API"

say "an OpenDrive that keeps what it is given, and the bridge in front of it"
"$BIN_DIR/mockupstream" --stateful --addr 127.0.0.1:0 --port-file "$WORK/up.addr" > "$WORK/mock.log" 2>&1 &
MPID=$!
for _ in $(seq 1 50); do [ -s "$WORK/up.addr" ] && break; sleep 0.2; done
mkdir -p "$WORK/inst" && cp "$BIN_DIR/opendrived" "$WORK/inst/"
HOME="$WORK" ODB_BASE_URL="http://$(cat "$WORK/up.addr")/api/v1" "$WORK/inst/opendrived" \
  --addr "$API" --cache-dir "$WORK/data/cache" \
  --s3-https-addr "$S3HTTPS" --s3-http-addr "$S3HTTP" --log-level info > "$WORK/daemon.log" 2>&1 &
DPID=$!
for _ in $(seq 1 50); do $ODCTL status >/dev/null 2>&1 && break; sleep 0.2; done
$ODCTL login e2e@example.com --password whatever > /dev/null; check $? "signed in"

curl -s "http://$API/v1/s3" > "$WORK/s3.json"
field() { python3 -c "import json,sys; print(json.load(open('$WORK/s3.json'))['$1'])"; }
export AWS_ACCESS_KEY_ID="$(field access_key_id)"
export AWS_SECRET_ACCESS_KEY="$(field secret_access_key)"
[ -n "$AWS_ACCESS_KEY_ID" ]; check $? "the daemon made S3 keys on its own"
! grep -q "$AWS_SECRET_ACCESS_KEY" "$WORK/daemon.log"; check $? "the secret key is not in the log"

say "data to back up"
mkdir -p "$WORK/src/deep/er" "$WORK/src/empty"
python3 - "$WORK/src" <<'PY'
import os, random, sys
root = sys.argv[1]
r = random.Random(7)
for i in range(40):
    sub = ["", "deep", "deep/er"][i % 3]
    size = r.choice([0, 1, 100, 4096, 70_000, 1_500_000])
    with open(os.path.join(root, sub, f"file-{i:02d}.bin"), "wb") as f:
        f.write(r.randbytes(size))
with open(os.path.join(root, "big.bin"), "wb") as f:
    f.write(r.randbytes(24 << 20))
with open(os.path.join(root, "name with :*? and spaces.txt"), "w") as f:
    f.write("awkward name\n")
PY

say "restic: init, backup, check, restore"
export RESTIC_PASSWORD=e2e-password
REPO="s3:http://$S3HTTP/restic-e2e/repo"
restic -r "$REPO" init > "$WORK/r-init.log" 2>&1; check $? "restic init"
restic -r "$REPO" backup "$WORK/src" > "$WORK/r-backup.log" 2>&1; check $? "restic backup"
restic -r "$REPO" check --read-data > "$WORK/r-check.log" 2>&1; check $? "restic check --read-data (from the cache)"

$ODCTL cache flush --wait > "$WORK/flush.log" 2>&1; check $? "everything delivered to OpenDrive"
$ODCTL cache clear > /dev/null 2>&1; check $? "cache emptied, so reads come from OpenDrive"

restic -r "$REPO" check --read-data > "$WORK/r-check2.log" 2>&1; check $? "restic check --read-data (from OpenDrive)"
restic -r "$REPO" restore latest --target "$WORK/restore" > "$WORK/r-restore.log" 2>&1; check $? "restic restore"
diff -r "$WORK/src" "$WORK/restore$WORK/src" > "$WORK/r-diff.log" 2>&1; check $? "restored tree is identical"

echo "changed" > "$WORK/src/deep/changed.txt"
restic -r "$REPO" backup "$WORK/src" > /dev/null 2>&1; check $? "second backup"
restic -r "$REPO" forget --keep-last 1 --prune > "$WORK/r-prune.log" 2>&1; check $? "forget --prune (deletes through S3)"
restic -r "$REPO" check > /dev/null 2>&1; check $? "check after prune"

say "rclone: copy with multipart, check, delete"
export RCLONE_CONFIG_BR_TYPE=s3 RCLONE_CONFIG_BR_PROVIDER=Other
export RCLONE_CONFIG_BR_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" RCLONE_CONFIG_BR_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_BR_ENDPOINT="http://$S3HTTP" RCLONE_CONFIG_BR_FORCE_PATH_STYLE=true
RC="rclone --s3-upload-cutoff 5M --s3-chunk-size 5M --retries 1 --low-level-retries 2"
$RC mkdir br:rclone-e2e > "$WORK/rc.log" 2>&1; check $? "rclone mkdir"
$RC copy "$WORK/src" br:rclone-e2e/copy >> "$WORK/rc.log" 2>&1; check $? "rclone copy (big.bin goes up in parts)"
$RC check --download "$WORK/src" br:rclone-e2e/copy >> "$WORK/rc.log" 2>&1; check $? "rclone check --download"
$ODCTL cache flush --wait > /dev/null 2>&1 && $ODCTL cache clear > /dev/null 2>&1
$RC check --download "$WORK/src" br:rclone-e2e/copy >> "$WORK/rc.log" 2>&1; check $? "rclone check --download (from OpenDrive)"
$RC delete br:rclone-e2e/copy/deep >> "$WORK/rc.log" 2>&1; check $? "rclone delete"
[ -z "$($RC lsf -R br:rclone-e2e/copy/deep 2>/dev/null | grep -v '/$')" ]; check $? "deleted files are gone"

say "HTTPS with the bridge's own certificate"
export RCLONE_CONFIG_BRS_TYPE=s3 RCLONE_CONFIG_BRS_PROVIDER=Other
export RCLONE_CONFIG_BRS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" RCLONE_CONFIG_BRS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_BRS_ENDPOINT="https://$S3HTTPS" RCLONE_CONFIG_BRS_FORCE_PATH_STYLE=true
rclone lsf brs:rclone-e2e > /dev/null 2>&1
[ $? -ne 0 ]; check $? "an unknown certificate is refused by default"
rclone --no-check-certificate lsf brs:rclone-e2e > /dev/null 2>&1; check $? "accepted once told to trust it"
FP="$(field certificate_sha256)"
GOT="$(openssl s_client -connect "$S3HTTPS" </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2)"
[ "$FP" = "$GOT" ]; check $? "the certificate served is the one /v1/s3 describes"

say "nothing unsigned gets in"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://$S3HTTP/restic-e2e/repo/config")
[ "$code" = "403" ]; check $? "unsigned read refused ($code)"

$ODCTL cache status | tail -3

if [ "$FAIL" -ne 0 ]; then echo; echo "=== s3-e2e FAILED"; exit 1; fi
echo; echo "=== s3-e2e PASSED"
