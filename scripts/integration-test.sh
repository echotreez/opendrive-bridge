#!/usr/bin/env bash
# Run the sandbox integration tests with credentials from the OS keychain
# (whitepaper §6.2, §9.2 — never from the shell history or a file).
#
#   scripts/integration-test.sh                    # the whole suite
#   scripts/integration-test.sh -run TestSandboxWrite -v
set -euo pipefail

SERVICE="${ODB_SPEC_KEYCHAIN_SERVICE:-opendrive-bridge-test}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${ODB_SPEC_USER:-}" || -z "${ODB_SPEC_PASS:-}" ]]; then
  if ! command -v security >/dev/null 2>&1; then
    echo "no keychain available; set ODB_SPEC_USER and ODB_SPEC_PASS yourself" >&2
    exit 2
  fi
  ODB_SPEC_USER="$(security find-generic-password -s "$SERVICE" 2>/dev/null |
    awk -F'"' '/"acct"<blob>/ {print $4}')"
  ODB_SPEC_PASS="$(security find-generic-password -s "$SERVICE" -w 2>/dev/null)"
  if [[ -z "$ODB_SPEC_USER" || -z "$ODB_SPEC_PASS" ]]; then
    echo "no keychain item for service '$SERVICE'; see scripts/fetch-spec.sh for setup" >&2
    exit 2
  fi
fi
export ODB_SPEC_USER ODB_SPEC_PASS

cd "$repo_root"
exec go test -tags=integration -count=1 ./pkg/opendrive/ "$@"
