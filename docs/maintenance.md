# Maintaining this after 1.0

Whitepaper §12 in operational form. Everything here is a standing obligation
rather than a suggestion, because the whole project rests on one claim — that the
Bridge tells you the truth about an upstream that often does not — and that claim
decays the moment upstream changes and nobody notices.

## Watching for upstream drift

**Weekly**, automatically: `scripts/fetch-spec.sh` re-fetches OpenDrive's Swagger
specs and diffs them against `testdata/spec/`. A difference opens an issue.

Two things about that job that are easy to get wrong:

- **It must run signed in.** Anonymously the explorer exposes 21 operations out
  of 220, so an unauthenticated diff would report the other 199 as "removed" on
  its first run and as nothing at all thereafter. The script reads the sandbox
  credentials from the OS keychain (service `opendrive-bridge-test`).
- **A spec diff is a lead, not a finding.** The specs have been wrong before —
  D26 was a documented boolean the API refuses outright. A changed spec means
  "go and measure this", and the result goes in `docs/discrepancies.md` whichever
  way it comes out.

**A failing contract test is a P1.** The contract tests are the ones that assert
the shape of a real recorded response; when one fails, upstream changed something
under a running installation, not just in a document.

**Quarterly**, by hand: ask OpenDrive whether the API guide has moved past v1.1.7
(<support@opendrive.com>). The document is not in this repository and cannot be —
see [official-api-reference.md](./official-api-reference.md) — so this is the
only way to notice a revision.

## Dependencies

- **Every day**: `govulncheck`, which already runs in CI on every change.
- **Every month**: one dependency upgrade pull request, all modules together, so
  that the diff is reviewable as a single decision rather than dribbling in.
- **Go**: track the two supported releases, N and N−1. The container builds with
  `golang:alpine` and CI with `go-version: stable` deliberately — a hand-pinned
  version next to something that floats drifts silently, which is exactly how the
  image ended up shipping a standard library with twelve known HIGH findings.
- **The module floor stays at Go 1.22** unless there is a reason to move it. Some
  dependencies were pinned to older releases to hold that floor; check
  `go.mod` before upgrading anything that raises it.

**Upgrading is not the same as being current.** After any dependency change, the
transfer benchmarks must still pass `scripts/check-bench.sh` — a new version of a
library in the read path can cost 20% without anybody noticing until a user with
a 40 GB file does.

## Support window

| version | what it gets |
|---|---|
| newest minor | everything: features, fixes, security |
| the minor before it | security fixes only |
| anything older | nothing; upgrade |

So when 1.1 ships, 1.0 gets security fixes and nothing else, and when 1.2 ships,
1.0 gets nothing.

**SemVer, read strictly:**

- A breaking change in the *Bridge's own* HTTP API or in `pkg/opendrive` needs a
  major version. `docs/bridge-openapi.yaml` is the contract, and CI already fails
  if the router and the spec disagree in either direction.
- An upstream incompatibility that forces our hand is a **minor** with a
  migration note, not a major. It is not our break, but the caller still has to
  be told.
- Anything a caller can observe belongs in the release notes, which are generated
  from Conventional Commit subjects. A change described only in a commit body is
  a change nobody downstream will read about.

## Routine checks that only a person can do

- **Every quarter**, install a release by hand on each platform and run the five
  minutes in [first-run.md](./first-run.md) exactly as written. CI proves the
  binaries run; it does not prove the instructions are still true.
- **Watch the certificate.** A macOS notarisation certificate expires on its own
  schedule, and the failure shows up as "the developer cannot be verified" for
  users, not as a red build.
- **Watch the runners.** GitHub retires runner images, and a job whose image no
  longer exists does not fail — it queues until the run times out, which reads as
  a green tick beside a job that never ran. `macos-15-intel` is the last x86_64
  macOS image and support ends in autumn 2027; when it goes, darwin/amd64 is
  compiled but no longer smoke-tested.

## What is deliberately not automated

The sandbox account's credentials, and anything that would put them in a file. CI
runs the whole suite against a mock upstream; the live suite
(`scripts/integration-test.sh`) runs on a developer's machine, reading from the
OS keychain. That is why the live tests are not a CI gate, and it is a trade made
knowingly: the alternative is a long-lived credential in a secret store, and the
project's own history includes a live token committed by a test written to prove
tokens do not leak.

## Where the next work is

From whitepaper §12.1, unchanged by 1.0 shipping:

- **1.1** — Users write operations, Account Users management, User Groups. Note
  that the sandbox account is an *account user*, not an owner (D33), so the whole
  sharing module answers 403 to it. Testing this needs an owner account, and
  `TestSandboxSharing` is already written to run the full lifecycle when it gets
  one.
- **1.2** — Secure Folders, password-protected files end to end, usage reporting.
  D32 and D4 are the groundwork: no API route can consume a file password, and
  the gate is an `od.lk` HTML form.
- **2.0 candidates** — WebDAV or FUSE mounting, and an rclone backend. This is
  the direction that changes who can use the project: today it serves people who
  will run a daemon and type commands; a mount point serves everyone else, and an
  rclone backend puts OpenDrive behind tooling people already have. The path
  cache, the resumable transfers and the job engine were all built in shapes that
  a filesystem layer can sit on.
