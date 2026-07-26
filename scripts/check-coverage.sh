#!/usr/bin/env bash
# Enforce the per-package coverage gates from whitepaper §6.3:
#   pkg/opendrive >= 85%, internal/* >= 75%.
#
# Usage: scripts/check-coverage.sh [coverage profile]
set -euo pipefail

profile="${1:-coverage.out}"
if [[ ! -f "$profile" ]]; then
  echo "coverage profile $profile not found; run: go test -coverprofile=$profile ./..." >&2
  exit 2
fi

# gate <path fragment> <minimum percentage>
gate() {
  local fragment="$1" minimum="$2" covered=0 total=0 pct

  # Each profile line is: name.go:from,to statements count
  while read -r _ statements count; do
    total=$((total + statements))
    if [[ "$count" != "0" ]]; then
      covered=$((covered + statements))
    fi
  done < <(grep "$fragment" "$profile" | grep -v '_test.go' || true)

  if [[ "$total" -eq 0 ]]; then
    echo "SKIP  $fragment (no statements yet)"
    return 0
  fi

  pct=$(awk -v c="$covered" -v t="$total" 'BEGIN { printf "%.1f", 100 * c / t }')
  if awk -v p="$pct" -v m="$minimum" 'BEGIN { exit !(p < m) }'; then
    echo "FAIL  $fragment: ${pct}% < ${minimum}% (whitepaper §6.3)"
    return 1
  fi
  echo "OK    $fragment: ${pct}% >= ${minimum}%"
}

status=0
gate "/pkg/opendrive/" 85 || status=1
gate "/internal/" 75 || status=1
exit "$status"
