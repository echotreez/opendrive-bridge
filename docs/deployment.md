# Running the OpenDrive Bridge

This guide assumes you are comfortable in a terminal and know nothing else. You
do not need to know Go, and you do not need to know anything about OpenDrive's
API — that is the whole point of this program.

**What it is.** `opendrived` is a small server that runs on your own machine and
speaks to your OpenDrive account for you. `odctl` is a command that talks to it.
Once you sign in the first time, the daemon keeps itself signed in, so scripts
and other programs can use your files without ever handling your password.

**Where your password goes.** Into a file called `.env`, in the same folder as
the programs, encrypted with a key in `.env.key` beside it. You put it there in
the clear once; the first run of the daemon encrypts the file and the plaintext
is gone. The same two files are used on every platform and in containers — there
is nothing to choose.

**What that protects you from, and what it does not.** It protects you from
committing your password to git, from syncing it to a cloud backup in the clear,
from someone reading it over your shoulder, and from it appearing in a log or a
process listing. It does **not** protect you from someone who can already read
your files as you, because `.env.key` sits next to `.env` and they would get
both. That is the price of a daemon that restarts on its own without anybody
typing a passphrase. On a shared machine, use full-disk encryption and a separate
account rather than relying on this.

Back up `.env` and `.env.key` together, or neither is any use.

---

## 1. Install

Download the archive for your machine from the
[releases page](https://github.com/echotreez/opendrive-bridge/releases), then
check it against the published checksums before you run anything:

```bash
sha256sum --check --ignore-missing opendrive-bridge_1.1.0_SHA256SUMS
```

On macOS use `shasum -a 256 -c` instead. If the check does not say `OK`, stop and
download again.

Unpack it. The archive creates a folder called `opendrive-bridge/` and the
programs run from there — nothing is copied into `/usr/local/bin`, so there is no
`sudo` anywhere in this guide and uninstalling is deleting the folder:

```bash
tar xzf opendrive-bridge_1.1.0_linux_amd64.tar.gz
cd opendrive-bridge
```

Put that folder somewhere permanent: your credentials are about to live in it.

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

Copy the template and put your OpenDrive username and password in it:

```bash
cp .env.example .env
$EDITOR .env
```

Now start the daemon once. This is the moment the plaintext goes away: it creates
`.env.key`, generates the bridge's own API key, and rewrites `.env` encrypted.

```bash
./opendrived
```

In another terminal, in the same folder:

```bash
./odctl status
```

It should say `Signed in as you@example.com.` From here on the daemon keeps
itself signed in; you will only be asked again if you change your OpenDrive
password somewhere else.

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
`.env` and `.env.key` alone: removing a service is not the same as throwing away
your sign-in, and reinstalling a newer version should not ask you to do it again.

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
`ReadWritePaths` that lets the daemon write `.env` when a token rotates.

It used to be a system unit with `DynamicUser=yes`, which is a good shape for a
service with no user data and the wrong one here — a throwaway account cannot
read a `.env` in your home directory. `ProtectHome=yes` was in that file too, and
would have hidden the credentials from the daemon just as effectively.

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
unpacked. A LaunchAgent runs as you, which is what lets it read `.env`. A
LaunchDaemon would start earlier, run as root, and be looking for a file it has
no business reading. Nothing prompts you for anything — macOS is not holding the
credentials.

---

## 4. Containers

The image is on `ghcr.io/echotreez/opendrive-bridge`. It is built for both Intel
and ARM, runs as a non-root user, and contains nothing but the two programs and a
set of CA certificates.

**The container takes the same two files as a host install.** There is no
container-specific credential backend any more: you prepare `.env` exactly as you
would on a laptop and mount it in.

```bash
cp .env.example .env      # then fill in your OpenDrive username and password

docker run -d --name opendrive-bridge \
  -p 127.0.0.1:9750:9750 \
  -e ODB_API_KEY="$(openssl rand -hex 32)" \
  -v "$PWD/.env:/data/.env" \
  -v odb-state:/data/jobs \
  ghcr.io/echotreez/opendrive-bridge:1.1.0
```

The first start rewrites `.env` encrypted and creates `.env.key` beside it, on
your host, through the mount. After that both files are yours to back up.

`deploy/docker/docker-compose.yaml` is the same thing written down, with a
healthcheck and `.env.key` mounted read-only.

Four things worth getting right:

- **`.env` must be mounted writable.** The first run rewrites it, and so does
  every token rotation. Mounted `:ro` the bridge works until the first refresh
  and then starts failing in a way that looks like an OpenDrive outage. `.env.key`
  is only ever read, so that one can be `:ro`.
- **Back up `.env` and `.env.key` together.** Either alone is useless.
- **Keep the `/data/jobs` volume** if you care about resuming interrupted
  transfers across restarts.
- **`ODB_API_KEY` is required here.** The image binds `0.0.0.0`, because inside a
  container loopback means "nothing outside can reach it" — and since that is not
  loopback, the daemon insists on a key. On a host install it generates one into
  `.env` itself and you never see it. Publish the port to `127.0.0.1` as above so
  only your machine can reach it.

---

## 5. The credential files

Two files, in the folder you unpacked, on every platform:

| file | what it is | permissions |
|---|---|---|
| `.env.example` | the template that ships in the archive | 0644 |
| `.env` | your credentials and the bridge's API key, encrypted | 0600 |
| `.env.key` | the 32 random bytes that decrypt it, made on first run | 0600 |

You are not locked in to this program. `.env` is written in OpenSSL's own
format, so you can always read your credentials back yourself:

```bash
openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a -pass file:.env.key -in .env
```

That command is checked against the real `openssl` in this project's test suite,
in both directions, because a claim like that is only worth making if something
proves it.

**The one thing that can go permanently wrong** is losing `.env.key`. It is not a
password you can reset — without it the credentials in `.env` cannot be read, and
you would have to start again from `.env.example`. No data in OpenDrive is at
risk either way; it is your sign-in that goes.

`--ephemeral` keeps everything in memory and forgets it on exit. It exists for
tests and one-off runs, and the bridge will never choose it for you: a daemon
that looks configured and forgets on reboot is worse than one that refuses to
start.

---

## 6. When something is wrong

Every message the bridge produces is meant to be actionable on its own. If one
is not, that is a bug worth reporting.

**`odctl status` first.** It answers without touching the network, so it works
even when OpenDrive is unreachable, and it will say which of these you are in:

| what it says | what to do |
|---|---|
| Not signed in yet | `odctl login you@example.com` |
| Signed in as … | nothing; it is working |
| Your saved password is no longer accepted | you changed it on the website; `odctl login` again |
| OpenDrive is asking for a captcha | sign in once at opendrive.com, then retry |
| The bridge cannot reach its credential store | `.env.key` is missing, or `.env` cannot be decrypted with it — see §5 |

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

## 7. Using it from your own programs

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
curious about how often: `docs/discrepancies.md` lists 45 measured cases.)

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
| `G101` × several — "hardcoded credentials" in `internal/keystore/env.go` | they are the *names* of the fields in `.env` (`ODB_PASSWORD`, `ODB_ACCESS_TOKEN`), not values. A constant naming a field looks exactly like a constant holding one. |
| `G117` — a struct field called `Password` is serialised | that is the point: the credential store persists your password so the bridge can stay signed in without you. Whitepaper §9.2 records this as a deliberate deviation from "never store a password", and `persist_password: false` is the way out. |
| `G115` — an int converted to a byte in the PKCS#7 padding | the value is bounded to 1–255 by a check three lines above; the analyser cannot follow it. |

One more is suppressed at the call site: the retry backoff uses a
non-cryptographic random source for jitter. Jitter exists so that many clients do
not retry in lockstep, which needs spread rather than unpredictability.

The three OS keyring backends that used to appear here are gone — v1.1 removed
them along with about a thousand lines of code and the CI jobs that tested each
vault on its own runner.
