# Running the OpenDrive Bridge

This guide assumes you are comfortable in a terminal and know nothing else. You
do not need to know Go, and you do not need to know anything about OpenDrive's
API — that is the whole point of this program.

**What it is.** `opendrived` is a small server that runs on your own machine and
speaks to your OpenDrive account for you. `odctl` is a command that talks to it.
Once you sign in the first time, the daemon keeps itself signed in, so scripts
and other programs can use your files without ever handling your password.

**Where your password goes.** Nowhere you have to prepare. You sign in once — on
the bridge's web page or with `odctl login` — and the daemon encrypts what you
typed into one file, `credentials.key`, in the same folder as the programs (in
`./data` for a container). Restarting asks for nothing. If OpenDrive stops
accepting the password, because you changed it, the bridge asks you to sign in
again; nothing needs restarting.

**What that protects you from, and what it does not.** The encryption keeps your
password out of git, out of a cloud backup's plain text, off your screen and out of
logs and process listings. The key is in the same file, because a daemon that
restarts on its own cannot ask anybody for a passphrase — so it does **not**
protect you from someone who can already read your files as you. On a shared
machine, use full-disk encryption and a separate account rather than relying on
this.

---

## 1. Install

Download the archive for your machine from the
[releases page](https://github.com/echotreez/opendrive-bridge/releases), then
check it against the published checksums before you run anything:

```bash
sha256sum --check --ignore-missing opendrive-bridge_*_SHA256SUMS
```

On macOS use `shasum -a 256 -c` instead. If the check does not say `OK`, stop and
download again.

Unpack it. The archive creates a folder called `opendrive-bridge/` and the
programs run from there — nothing is copied into `/usr/local/bin`, so there is no
`sudo` anywhere in this guide and uninstalling is deleting the folder:

```bash
tar xzf opendrive-bridge_*_linux_amd64.tar.gz
cd opendrive-bridge
```

Put that folder somewhere permanent: your sign-in will be kept in it.

### macOS will probably refuse the first time

If you see *"cannot be opened because the developer cannot be verified"*, macOS
has quarantined the download. That happens to every program not signed with a
paid Apple certificate; it is not a sign that anything is wrong with the file —
but do check the checksum above before doing this:

```bash
xattr -d com.apple.quarantine ./opendrived ./odctl
```

Releases built with a certificate available are signed and notarised, and need
none of this.

---

## 2. Sign in

Start the daemon. There is nothing to set up first:

```bash
./opendrived
```

Open `http://127.0.0.1:9750/ui` and sign in with your OpenDrive account — or, in
another terminal in the same folder:

```bash
./odctl login you@example.com     # prompts for the password, so it stays out of your history
./odctl status
```

It should say `Signed in as you@example.com.` The daemon has written
`credentials.key` beside itself and keeps itself signed in from here on; you will
only be asked again if you change your OpenDrive password somewhere else.

Try a couple of things:

```bash
./odctl ls /
./odctl up ./report.pdf /Documents/report.pdf
./odctl ls /Documents
./odctl down /Documents/report.pdf ./copy.pdf
```

If typing `./` grates, `odctl daemon install` prints the one line to add to your
shell profile, or adds it for you with `--modify-shell-profile`.

Then stop the daemon with Ctrl-C and set it up properly.

---

## 3. Run it in the background

From inside the folder you unpacked:

```bash
./odctl daemon install
./odctl daemon start
./odctl status
```

That registers the daemon with whatever your system uses — systemd on Linux,
launchd on macOS — so it starts when you log in. It runs **from this
folder**, with the absolute path written into the service definition, because no
service manager reads your shell configuration.

`odctl daemon stop` and `odctl daemon uninstall` undo it. Uninstalling leaves
`credentials.key` alone: removing a service is not the same as throwing away your
sign-in, and reinstalling a newer version should not ask you to do it again.

`install` also prints the one line that puts this folder on your `PATH`. It does
not write it unless you pass `--modify-shell-profile`, in which case it backs the
file up first and wraps its addition in a marked block that `uninstall` removes
again. Your shell profile is yours.

The rest of this section is only interesting if you want to know what it did, or
would rather do it by hand.

### Linux (systemd, as your own user)

`deploy/systemd/opendrived.service` is the unit. It is a **user** unit — no
`sudo`, no system-wide install:

```bash
mkdir -p ~/.config/systemd/user
sed "s#/path/to/opendrive-bridge#$PWD#g" \
  deploy/systemd/opendrived.service > ~/.config/systemd/user/opendrived.service
systemctl --user daemon-reload
systemctl --user enable --now opendrived
systemctl --user status opendrived
journalctl --user -u opendrived -f      # the log
```

Two paths are substituted there and both matter: `ExecStart`, and the
`ReadWritePaths` that lets the daemon write `credentials.key` when you sign in and
when a token rotates.

It used to be a system unit with `DynamicUser=yes`, which is a good shape for a
service with no user data and the wrong one here — a throwaway account cannot
write to the folder in your home directory. `ProtectHome=yes` was in that file too,
and would have hidden the folder from the daemon just as effectively.

**If you want it running when you are not logged in**, ask systemd to keep your
user manager alive:

```bash
sudo loginctl enable-linger $USER
```

That is the only `sudo` on this page, and it is optional.

### macOS (launchd)

```bash
cp deploy/launchd/com.opendrive.bridge.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.opendrive.bridge.plist
```

Edit the plist first: it needs the full path to `opendrived` in the folder you
unpacked. A LaunchAgent runs as you, which is what lets it keep `credentials.key`
in your folder. A LaunchDaemon would start earlier, run as root, and keep your
sign-in in a file you would need `sudo` to back up. Nothing prompts you for
anything — macOS is not holding the credentials.

---

## 4. Containers

The image is on `ghcr.io/echotreez/opendrive-bridge`, built for x86-64 and ARM64. It
contains the two programs and a set of CA certificates, and nothing else — no
shell, no package manager.

```bash
docker compose up -d        # with deploy/docker/docker-compose.yaml, in a folder of its own
```

Then open `http://127.0.0.1:9750/ui` and sign in, or:

```bash
docker compose exec opendrive-bridge odctl login you@example.com
```

That is the whole setup. Docker creates `./data` beside the compose file on the
first start, and the bridge keeps everything there: `credentials.key` once you sign
in, transfer state in `jobs/`, and the cache in `cache/`. It survives `docker
compose down`, an image upgrade and a new container; a password change is handled
by signing in again, with no restart.

Without compose:

```bash
docker run -d --name opendrive-bridge -p 127.0.0.1:9750:9750 \
  -e ODB_CACHE_DIR=/data/cache -v "$PWD/data:/data" \
  ghcr.io/echotreez/opendrive-bridge:latest
```

Things worth knowing:

- **It runs as root**, as containers ordinarily do, so no folder mounted into it
  has to be `chown`ed first. On Linux the files in `./data` belong to root as a
  result; reading them for a backup takes `sudo`. The root filesystem is read-only and `no-new-privileges` is set.
- **No API key by default.** The container listens on all interfaces, because
  loopback inside a container is unreachable, and the compose file publishes the
  port to `127.0.0.1` so only this machine can reach it. If you publish it more
  widely, set `ODB_API_KEY`; every caller then has to send it. The daemon logs a
  warning when it listens beyond loopback without one.
- **Mount a folder at `/data`, not a file.** Everything is written there by the
  bridge; a single file mounted on its own cannot be replaced atomically, and the
  bridge will say so rather than lose a sign-in.
- **`odctl up` and `odctl down` cannot move files through a containerised bridge**:
  they send the bridge a path on your machine, which the container cannot see. Use
  the `PUT /v1/upload/stream` and `GET /v1/download/stream` endpoints.
- **Docker Desktop for Mac or Windows** is unmeasured for bind mounts: whether fsync
  crosses its file-sharing layer has not been checked. Keep the cache on a named
  volume there (the compose file has the line, commented).
- **Apple's `container` on macOS** was measured: an upload acknowledged
  with 202 survived `container kill --signal KILL` and was delivered intact after a
  restart. What survives a power cut on the Mac itself is not measured. The cache
  directory lock does **not** hold between two containers there, because each is
  its own virtual machine: run one per folder.

---

## 5. The local cache

Off by default. Turn it on by giving it a directory:

```bash
./opendrived --cache-dir ./cache
```

It does two different things, and the second one changes what a successful upload
means, so it is worth a minute.

**Reading** is the easy half. A file you have read once is served from your own
disk the next time, and `X-Cache: HIT` on the response says so. Losing the cache
costs a download; nothing else.

**Writing** is the half to understand. With write-back on — the default when the
cache is on — a file you upload is copied to the bridge's own disk first and sent to
OpenDrive afterwards. `PUT /v1/upload/stream` answers 202 as soon as that first step
is done; `odctl up` waits for both, and `odctl jobs` shows which leg it is on.

The point is not that `odctl up` returns sooner. It is that once the first leg is
done the file is safe on the bridge: a crash, a network outage or OpenDrive having a
bad afternoon no longer loses it, because the bridge keeps retrying and picks up
again after a restart. It also means a file you just wrote can be read back
immediately, which OpenDrive itself does not guarantee.

A file too large for `--cache-max-dirty-bytes` skips the cache and goes straight up,
so the cache being full never stops an upload.

The cost is that for a while, **the bridge is the only place that file exists.**
Everything below follows from that one sentence.

### Is it safe to stop?

```bash
./odctl cache status
```

The last line says so in words — not a number to interpret:

```
Not yet on OpenDrive: 41.2 MB in 3 file(s), limit 5.0 GB
Longest wait:         12s

NOT safe to stop the bridge yet: 41.2 MB has not reached OpenDrive.
Run `odctl cache flush --wait` to send it now.
```

`odctl cache flush --wait` returns when there is nothing left. `odctl cache
objects --unsent` lists what is outstanding, and why, if an upload keeps failing.

Stopping the service is handled for you: on SIGTERM the daemon stops accepting
writes, finishes uploading what it can, and only then exits. If it runs out of
time it writes one log line per unfinished file, naming each one and where its
data is, so nothing vanishes without a record. `TimeoutStopSec` in the systemd
unit is set high enough to let that happen.

### What it will not do

- **It never discards a file to make room.** Under capacity pressure only files
  OpenDrive already has are evicted. If the unsent data reaches
  `--cache-max-dirty-bytes`, new writes are **refused** — HTTP 507, and a message
  saying nothing was lost — rather than something being thrown away.
- **`cache refresh` and `cache clear` refuse to touch anything unsent.** They
  answer 409 and say what is in the way.
- **A crash loses nothing that was acknowledged.** Every write is recorded in a
  journal that is flushed to the platter before the bridge answers, so a restart
  finds the unsent files and re-queues them. The only thing a crash costs is a
  write that was still in flight, which was never acknowledged.

### The cache directory is not encrypted

`credentials.key` is encrypted. **The cache is not.** It holds your files as they are,
protected by the directory's permissions — the bridge sets 0700, so only the
account running the bridge can read it — and by whatever encryption the disk
itself provides. If that is not enough for the files you work with, either leave
the cache off or put it on an encrypted volume.

### In a container

Keep the cache outside the container. A container's own filesystem is disposable and
recreating a container is routine, so a cache inside it would take unsent files with
it. The compose file puts it in `./data/cache`, on the host, and that is the most
robust option: Docker's clean-up commands (`docker compose down -v`, `docker volume
prune`) leave host directories alone; the journal and the unsent files are somewhere
you can see and back up; and after a crash, starting the container again replays the
journal and sends what had not gone up. An interrupted file is sent again from the
start rather than resumed mid-way — if it had actually arrived, OpenDrive recognises
it by hash and the resend is nearly free.

**On Docker Desktop for Mac or Windows, use a named volume** for the cache (`-v
odb-cache:/data/cache`) until somebody measures the alternative: a host directory
there goes through the VM's file-sharing layer, and whether an fsync inside the
container reaches the host's disk through it has not been checked.

**One bridge per cache directory.** The daemon takes a lock on the directory at
startup, so a second bridge pointed at the same folder — another container, or
`opendrived` on the host — refuses to start instead of two of them writing one
journal. It is a kernel lock, released when the holder exits however it exits. It
holds between processes on the same Linux kernel; it does **not** hold across
virtual machines (Apple's `container`, measured) or on a network filesystem.

The daemon also checks at startup and reports `durable: false` on
`/v1/cache/status` if the cache directory is not a mount at all, which `odctl cache
status` prints as a warning.

### Turning it off again

`--cache-write-back=false` keeps the read cache and sends writes straight to
OpenDrive, which is the honest setting anywhere the cache directory might not
survive. Removing `--cache-dir` switches the whole thing off. Neither loses
anything: if files are still waiting when you turn write-back off, the daemon says
so at startup and they stay on disk until you turn it back on.

---

## 6. The web interface

`http://127.0.0.1:9750/ui`, served by the daemon itself. Nothing to install, nothing
to build, and no new files in the release: it is compiled into `opendrived`.

Five things, all of them the same figures `odctl` reports:

- the account, the sign-in state and how much of your storage is used
- somewhere to sign in, which is the natural place to fix it if your password changed
  elsewhere
- a transfer-rate line
- the transfers in progress, with the leg each one is on and a button to stop it
- the cache, with **whether it is safe to stop the bridge** in the largest text on the
  page

**It fetches nothing from the internet.** No fonts, no chart library, no analytics.
A local service holding your OpenDrive password has no business making outbound
requests because you opened a page, and a machine with no internet should not have a
broken interface. A `Content-Security-Policy` makes the browser enforce that rather
than leaving it to good intentions.

**There is nothing to configure.** With no API key set — the default — the page
just works. If you set `ODB_API_KEY`, the bridge will not answer without it, and the
page asks for it — kept for that browser tab only, sent as a header, and never put in
a web address, because a URL reaches the browser history and every log in between.

**It is where you sign in.** When the bridge has no account yet, or OpenDrive stops
accepting the saved password, the page says so and puts the cursor in the sign-in
form.

**It is a client, not a back door.** The page has no other route to OpenDrive: every
number on it arrives over the same HTTP API `odctl` uses, and every error it shows is
the daemon's own wording, unedited.

If you would rather it were not there, bind the daemon to loopback — which is the
default — and it is reachable only from that machine. There is no switch to remove
it, because there is nothing to remove: it is a few static files inside a binary you
are already running.

---

## 7. The credential file

One file, `credentials.key`, in the folder you unpacked (or `./data` in a
container), 0600. You never create or edit it: the bridge writes it when you sign
in and when a token rotates, and removes it when you sign out.

Inside, after a comment, is one line holding a random key and then your credentials
encrypted with it in OpenSSL's own format, so you are not locked in to this program.
The comment at the top of the file carries the command, and it is:

```bash
grep -v '^#' credentials.key | head -1 > /tmp/k
grep -v '^#' credentials.key | tail -n +2 | \
  openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a -pass file:/tmp/k
```

That is run, exactly as written, against the real `openssl` in this project's test
suite.

**If it is lost or damaged**, sign in again; the bridge writes a new one. Nothing in
your OpenDrive account is affected.

`--ephemeral` keeps the sign-in in memory and forgets it on exit. It exists for
tests and one-off runs; the bridge never chooses it for you.

---

## 8. When something is wrong

Every message the bridge produces is meant to be actionable on its own. If one
is not, that is a bug worth reporting.

**`odctl status` first.** It answers without touching the network, so it works
even when OpenDrive is unreachable, and it will say which of these you are in:

| what it says | what to do |
|---|---|
| Not signed in yet | sign in on the web page, or `odctl login you@example.com` |
| Signed in as … | nothing; it is working |
| Your saved password is no longer accepted | you changed it on the website; sign in again (page or `odctl login`) |
| OpenDrive is asking for a captcha | sign in once at opendrive.com, then retry |
| The bridge cannot read its saved sign-in | `credentials.key` is damaged or unreadable; sign in again to replace it |

**Exit codes**, for scripts:

| code | meaning |
|---|---|
| 0 | it worked |
| 2 | something in the command was wrong |
| 3 | the bridge needs you to sign in again |
| 4 | OpenDrive failed; trying later is reasonable |
| 5 | there is nothing at that path |
| 6 | no bridge is running to talk to |
| 7 | OpenDrive refused this and will refuse it again; retrying will not help |

**Logs.** `journalctl -u opendrived` on Linux, `log show --predicate 'process ==
"opendrived"'` on macOS, `docker logs opendrive-bridge` for a container. Every
line has a `request_id`; quote it in a bug report and it can be matched to the
exact request.

Passwords and tokens are removed from log output before it is written. If you
ever see one in a log, that is a security bug — please report it.

**Transfers.** `odctl jobs` lists them; `odctl jobs <id>` shows one, including
why it failed. Cancelling with `odctl jobs cancel <id>` also removes the
half-finished file from your OpenDrive account, so a cancelled upload leaves
nothing behind.

---

## 9. Using it from your own programs

The daemon is an ordinary HTTP API on `127.0.0.1:9750`; the full specification is
`docs/bridge-openapi.yaml`, which you can hand to most code generators.

```bash
curl 127.0.0.1:9750/v1/auth/status
curl '127.0.0.1:9750/v1/ls?path=/Documents'
curl -X POST 127.0.0.1:9750/v1/mkdir \
  -H 'Content-Type: application/json' -d '{"path":"/Documents/2026"}'
```

Failures come back in one shape:

```json
{"error": {"code": "not_found", "http": 404,
           "message": "There is nothing at /Documents/nope.pdf."}}
```

Switch on `code` — it is a fixed list, documented in the specification. Show
`message` to people. There is also an `upstream` field holding what OpenDrive
itself said; it is there for bug reports, and it is deliberately *not* what you
should display, because OpenDrive's own wording is often misleading. (If you are
curious about how often: `docs/discrepancies.md` lists 53 measured cases.)

One paging quirk you will meet if you list a large folder: `/v1/ls` returns
`dir_update_time`, and to fetch the next page you must send it back along with
`offset`. Sending `offset` alone is rejected rather than silently giving you the
first page again.

---

## Appendix: what the security scanners say

`govulncheck`, `gosec` and `gitleaks` run on every change; `trivy` scans the
container image before it is pushed. Nothing is published if any of them objects.

`.gitleaksignore` in the repository root lists every finding that has been looked
at and dismissed, one line each with the reason — including one that was not a
false positive: a test written to prove that access tokens never leak had
committed the live token that leaked. Nothing is allowlisted by directory,
because recorded API fixtures are the likeliest place for a real credential to
hide.

A few findings are expected and reviewed. Listing them in a document turned out
not to be enough — the first real run of the release workflow failed on them,
because a note in a document is not something a scanner can read. They now carry
`#nosec` annotations in the code itself, with the reasons below, and the gate
fails on anything medium or higher, so these pass and a new one does not.

| finding | why it is there |
|---|---|
| `G101` × several — "hardcoded credentials" in `internal/keystore/env.go` | they are the *names* of the fields in `credentials.key` (`ODB_PASSWORD`, `ODB_ACCESS_TOKEN`), not values. A constant naming a field looks exactly like a constant holding one. |
| `G117` — a struct field called `Password` is serialised | that is the point: the credential store persists your password so the bridge can stay signed in without you. Whitepaper §9.2 records this as a deliberate deviation from "never store a password", and `persist_password: false` is the way out. |
| `G115` — an int converted to a byte in the PKCS#7 padding | the value is bounded to 1–255 by a check three lines above; the analyser cannot follow it. |

One more is suppressed at the call site: the retry backoff uses a
non-cryptographic random source for jitter. Jitter exists so that many clients do
not retry in lockstep, which needs spread rather than unpredictability.
