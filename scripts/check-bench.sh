#!/usr/bin/env bash
# Transfer performance gate (whitepaper §10).
#
# Runs the 1 GB upload and download benchmarks and fails if either has got more
# than 15% slower than the recorded baseline.
#
# The comparison is a *ratio*, not a throughput. Each run also measures
# BenchmarkReferenceMD5 — an MD5 over the same bytes, the one piece of work the
# upload pipeline cannot avoid — and the gate compares
#
#     pipeline ns/op ÷ reference ns/op
#
# against the recorded ratio. A fixed MB/s threshold would have to hold on a
# shared CI runner whose speed varies by more than 15% between mornings, so it
# would fail for reasons that have nothing to do with the code, and the first
# response to a gate that cries wolf is to stop believing it. The ratio is a
# property of the code: it moves when the pipeline does more work per byte, and
# stays put when the machine is having a bad day.
#
# Each benchmark runs several times and the *fastest* run is used, which is not
# cherry-picking: benchmark noise is one-sided — scheduling, GC and a noisy
# neighbour can only ever add time — so the minimum is the closest estimate of
# what the code actually costs.
#
# That correction was earned. The ratio alone removed clock-speed differences
# but not contention, and the upload pipeline feels contention far more than the
# yardstick does: it moves 50 MB chunks through an HTTP server, a multipart
# writer and the garbage collector, while the reference is one tight hashing
# loop. On a four-core shared runner the same unchanged code measured 1.319x,
# 1.411x and 1.673x on consecutive days, and the third of those tripped a 15%
# gate that had nothing to report.
#
# Usage:
#   scripts/check-bench.sh                 compare against the baseline
#   scripts/check-bench.sh --update        record the current numbers as the baseline
set -euo pipefail

cd "$(dirname "$0")/.."

BASELINE="testdata/benchmarks/baseline.json"
TOLERANCE="${ODB_BENCH_TOLERANCE:-15}"   # percent
SIZE="${ODB_BENCH_SIZE:-}"               # empty means 1 GB, the benchmark's default
UPDATE=0
# Written as an if rather than `[ ... ] && UPDATE=1`, which under `set -e` exits
# the script whenever the test is false — that is, on every run that is not
# --update, which is every run that matters.
if [ "${1:-}" = "--update" ]; then UPDATE=1; fi

echo "running the transfer benchmarks (${SIZE:-1GiB} per iteration)..."
raw="$(ODB_BENCH_SIZE="$SIZE" go test ./pkg/opendrive/ \
        -run '^$' -bench '^(BenchmarkReferenceMD5|BenchmarkTransfer)' \
        -benchtime=1x -count="${ODB_BENCH_COUNT:-5}" -timeout 30m)"
echo "$raw"

printf %s "$raw" | python3 scripts/bench-compare.py "$BASELINE" "$TOLERANCE" "$UPDATE"
