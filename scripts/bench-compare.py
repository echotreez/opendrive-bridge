"""Compare transfer benchmarks against the recorded baseline.

Reads `go test -bench` output on stdin. See scripts/check-bench.sh for why the
comparison is a ratio against a same-run yardstick rather than a MB/s figure.
"""

import json
import os
import re
import sys

baseline_path, tolerance, update = sys.argv[1], float(sys.argv[2]), sys.argv[3] == "1"
text = sys.stdin.read()

# Benchmark<name>-<cpus>  <iterations>  <ns> ns/op  <mb> MB/s
pattern = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([\d.]+)\s+ns/op\s+([\d.]+)\s+MB/s", re.M)

# The fastest run of each benchmark. Noise only ever adds time, so the minimum
# is the best estimate of the cost of the code; see check-bench.sh for why this
# matters more than it looks.
runs = {}
samples = {}
for m in pattern.finditer(text):
    name, ns, mbps = m.group(1), float(m.group(2)), float(m.group(3))
    samples.setdefault(name, []).append(ns)
    if name not in runs or ns < runs[name]["ns"]:
        runs[name] = {"ns": ns, "mbps": mbps}
for name, xs in sorted(samples.items()):
    if len(xs) > 1:
        spread = (max(xs) - min(xs)) / min(xs) * 100
        print("%-28s %d runs, spread %.1f%%" % (name, len(xs), spread))

ref = runs.get("BenchmarkReferenceMD5")
if not ref:
    sys.exit("the reference benchmark did not run; there is nothing to compare against")

measured = {name: round(r["ns"] / ref["ns"], 4)
            for name, r in runs.items() if name != "BenchmarkReferenceMD5"}
if not measured:
    sys.exit("no transfer benchmarks ran")

print()
print("reference (MD5 over the same bytes): %.0f MB/s" % ref["mbps"])
for name, ratio in sorted(measured.items()):
    print("  %-28s %7.0f MB/s   %.3fx reference" % (name, runs[name]["mbps"], ratio))

if update or not os.path.exists(baseline_path):
    os.makedirs(os.path.dirname(baseline_path), exist_ok=True)
    with open(baseline_path, "w") as fh:
        json.dump({
            "note": "ns/op divided by BenchmarkReferenceMD5, same machine, same run",
            "tolerance_percent": tolerance,
            "ratios": measured,
        }, fh, indent=2, sort_keys=True)
        fh.write("\n")
    print("\nbaseline written to %s" % baseline_path)
    sys.exit(0)

with open(baseline_path) as fh:
    recorded = json.load(fh)["ratios"]

print()
failed = []
for name, ratio in sorted(measured.items()):
    want = recorded.get(name)
    if want is None:
        print("NEW   %s: %.3fx, not in the baseline — run --update to record it" % (name, ratio))
        continue
    # A higher ratio means more time per byte relative to the yardstick.
    change = (ratio - want) / want * 100
    print("%s  %s: %.3fx vs %.3fx baseline (%+.1f%%)"
          % ("OK  " if change <= tolerance else "SLOW", name, ratio, want, change))
    if change > tolerance:
        failed.append((name, want, ratio, change))

if failed:
    print()
    print("This is a performance regression, measured against a yardstick taken on")
    print("the same machine in the same run, so it is not the runner being slow.")
    for name, want, ratio, change in failed:
        print("  %s now costs %.3fx the reference hash, was %.3fx (%+.1f%%)"
              % (name, ratio, want, change))
    print()
    print("If the change is deliberate, run scripts/check-bench.sh --update and say in")
    print("the commit message what is now being done per byte that was not before.")
    sys.exit(1)

print("\nno transfer regression")
