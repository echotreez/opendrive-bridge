#!/usr/bin/env bash
# Run tools/fetch-spec with credentials taken from the OS keychain instead of
# the shell history, a file or a command line (whitepaper §9.2).
#
# One-time setup on macOS:
#
#   security add-generic-password -U \
#     -s opendrive-bridge-test -a <account-email> -w '<password>'
#
# Then just:
#
#   scripts/fetch-spec.sh                 # refresh testdata/spec/
#   scripts/fetch-spec.sh -out /tmp/spec  # any fetch-spec flag is passed through
#
# In CI, set ODB_SPEC_USER and ODB_SPEC_PASS from the secret store instead; the
# keychain lookup is skipped when both are already present.
set -euo pipefail

SERVICE="${ODB_SPEC_KEYCHAIN_SERVICE:-opendrive-bridge-test}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${ODB_SPEC_USER:-}" || -z "${ODB_SPEC_PASS:-}" ]]; then
  if ! command -v security >/dev/null 2>&1; then
    echo "no keychain available; set ODB_SPEC_USER and ODB_SPEC_PASS yourself" >&2
    exit 2
  fi
  # The account (login email) is stored on the keychain item itself, so only the
  # service name has to be known here.
  ODB_SPEC_USER="$(security find-generic-password -s "$SERVICE" 2>/dev/null |
    awk -F'"' '/"acct"<blob>/ {print $4}')"
  ODB_SPEC_PASS="$(security find-generic-password -s "$SERVICE" -w 2>/dev/null || true)"

  if [[ -z "$ODB_SPEC_USER" || -z "$ODB_SPEC_PASS" ]]; then
    cat >&2 <<EOF
no credentials in keychain service "$SERVICE".

Add them once with:
  security add-generic-password -U -s $SERVICE -a <account-email> -w '<password>'
EOF
    exit 2
  fi
  echo "using credentials from keychain service \"$SERVICE\" (account ${ODB_SPEC_USER%%@*}@...)"
fi

export ODB_SPEC_USER ODB_SPEC_PASS
cd "$repo_root"
exec go run ./tools/fetch-spec "$@"
