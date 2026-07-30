# Running the OpenDrive Bridge

This guide assumes you are comfortable in a terminal and know nothing else. You
do not need to know Go, and you do not need to know anything about OpenDrive's
API — that is the whole point of this program.

**What it is.** `opendrived` is a small server that runs on your own machine and
speaks to your OpenDrive account for you. `odctl` is a command that talks to it.
Once you sign in the first time, the daemon keeps itself signed in, so scripts
and other programs can use your files without ever handling your password.

**Where your password goes.** Into your operating system's own credential store
— macOS Keychain, GNOME Keyring or KWallet on Linux, Credential Manager on
Windows. Never into a config file. On a server or in a container, where there is
no such store, you supply an encryption key instead and the bridge keeps an
encrypted file; that is covered under [Containers](#containers).

---

## 1. Install

Download the archive for your machine from the
[releases page](https://github.com/StormRealm/opendrive-bridge/releases), then
check it against the published checksums before you run anything:

```bash
sha256sum --check --ignore-missing opendrive-bridge_1.0.0_SHA256SUMS
```

On macOS use `shasum -a 256 -c` instead. If the check does not say `OK`, stop and
download again.

Unpack it and put the two programs somewhere on your `PATH`:

```bash
tar xzf opendrive-bridge_1.0.0_linux_amd64.tar.gz
sudo install -m 0755 opendrived odctl /usr/local/bin/
```

### macOS will probably refuse the first time

If you see *"cannot be opened because the developer cannot be verified"*, macOS
has quarantined the download. That happens to every program not signed with a
paid Apple certificate; it is not a sign that anything is wrong with the file —
but do check the checksum above before doing this:

```bash
xattr -d com.apple.quarantine /usr/local/bin/opendrived /usr/local/bin/odctl
```

Releases built with a certificate available are signed and notarised, and need
none of this.

---

## 2. Sign in

Start the daemon in a terminal, just to see it work:

```bash
opendrived
```

In another terminal:

```bash
odctl login you@example.com
odctl status
```

`status` should say `Signed in as you@example.com.` From here on the daemon keeps
itself signed in; you will only be asked again if you change your OpenDrive
password.

Try a couple of things:

```bash
odctl ls /
odctl up ./report.pdf /Documents/report.pdf
odctl ls /Documents
odctl down /Documents/report.pdf ./copy.pdf
```

Then stop the daemon with Ctrl-C and set it up properly.

---

## 3. Run it in the background

```bash
odctl daemon install
odctl daemon start
odctl status
```

That registers the daemon with whatever your system uses — systemd, launchd or
the Windows Service Manager — so it starts when you log in. `odctl daemon stop`
and `odctl daemon uninstall` undo it. If it says the machine will not let you
change its services, run the same command with administrator rights.

The rest of this section is only interesting if you want to know what it did, or
you would rather do it by hand.

### Linux (systemd)

`deploy/systemd/opendrived.service` is the unit file. It runs the daemon under a
throwaway user account with most of the filesystem read-only, which limits what a
bug in this program could reach:

```bash
sudo install -m 0644 deploy/systemd/opendrived.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now opendrived
systemctl status opendrived
journalctl -u opendrived -f      # the log
```

**One thing to know about servers.** A systemd service usually has no desktop
session, and without one there is no credential store to unlock. If
`odctl status` reports that the bridge cannot reach its credential store, that is
why — use the encrypted-file store instead:

```bash
# 32 random bytes, base64. Keep a copy somewhere safe: without it the
# stored credentials cannot be read back.
openssl rand -base64 32
sudo systemctl edit opendrived
```

and add:

```ini
[Service]
Environment=ODB_STATE_KEY=<the value you generated>
Environment=ODB_KEYSTORE=encrypted_file
```

### macOS (launchd)

```bash
cp deploy/launchd/com.opendrive.bridge.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.opendrive.bridge.plist
```

A LaunchAgent runs inside your login session, so it can use the Keychain and
nothing extra is needed. The first time it reads the stored password macOS will
ask you to allow it; choose **Always Allow** or you will be asked on every
restart.

### Windows

`odctl daemon install` from an Administrator prompt registers a Windows Service.
`deploy/windows/README.txt` has the manual equivalent. Credentials go to
Credential Manager, and the encrypted file — if you use one — is locked down with
`icacls` so that only your account can read it.

---

## 4. Containers

The image is on `ghcr.io/stormrealm/opendrive-bridge`. It is built for both
Intel and ARM, runs as a non-root user, and contains nothing but the two
programs and a set of CA certificates.

**A container has no keychain,** so you must give the bridge a key to encrypt its
credential file with. If you do not, it refuses to start and says so — that is
deliberate. A bridge that started, looked configured, and forgot your password on
the next restart would be worse than one that never started.

```bash
docker run -d --name opendrive-bridge \
  -p 127.0.0.1:9750:9750 \
  -e ODB_LISTEN=0.0.0.0:9750 \
  -e ODB_STATE_KEY="$(openssl rand -base64 32)" \
  -e ODB_API_KEY="$(openssl rand -hex 32)" \
  -v odb-state:/data \
  ghcr.io/stormrealm/opendrive-bridge:1.0.0
```

Then sign in once, from your own machine:

```bash
odctl --addr 127.0.0.1:9750 --api-key <the ODB_API_KEY value> login you@example.com
```

`deploy/docker/docker-compose.yaml` is the same thing written down, with a
healthcheck.

Three things worth getting right:

- **Keep `ODB_STATE_KEY` somewhere safe and unchanged.** Change it and the stored
  credentials become unreadable; you will need to sign in again.
- **Keep the volume.** `/data` is where the encrypted credentials and the
  transfer state live. Without it, every restart is a fresh install.
- **`ODB_LISTEN=0.0.0.0:9750` is required inside a container** — the default
  listens on loopback, which inside a container means "nothing outside can reach
  it". Because that address is not loopback, the daemon **will not start without
  `ODB_API_KEY`.** Publish the port to `127.0.0.1` as above so only your machine
  can reach it.

---

## 5. Choosing a credential store

| where you are running | store | what you need |
|---|---|---|
| your own desktop or laptop | your OS keychain (the default) | nothing |
| a Linux server with no desktop session | `encrypted_file` | `ODB_STATE_KEY` |
| a container | `encrypted_file` | `ODB_STATE_KEY` and a volume for `/data` |
| a throwaway test run | `ephemeral` | `--ephemeral`, and expect to sign in again after every restart |

Set it with `--keystore` or `ODB_KEYSTORE`. `auto` — the default — uses your OS
keychain and falls back to the encrypted file only when you have supplied a key.

The bridge will not silently pick a store that forgets everything on restart. If
none is usable it stops and tells you which of the above to choose.

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
| The bridge cannot reach its credential store | unlock your keychain, or see §5 for servers |

**Exit codes**, for scripts:

| code | meaning |
|---|---|
| 0 | it worked |
| 2 | something in the command was wrong |
| 3 | the bridge needs you to sign in again |
| 4 | OpenDrive failed; trying later is reasonable |
| 5 | there is nothing at that path |
| 6 | no bridge is running to talk to |

**On Windows, if you use Git Bash.** Git Bash changes any argument that starts
with a slash into a Windows path before `odctl` ever runs, so

```
odctl ls /Documents
```

reaches the program as `odctl ls "C:/Program Files/Git/Documents"` and cannot
work. `odctl` recognises the result and says so, but it cannot undo it. Either

```
MSYS_NO_PATHCONV=1 odctl ls /Documents
```

or use PowerShell or Command Prompt, where nothing is rewritten. This affects
only paths in your OpenDrive account; local filenames are unaffected, so
`odctl up report.pdf /Documents/report.pdf` needs the same treatment while
`odctl up C:\reports\report.pdf .` does not. (If you use MSYS2 rather than Git
for Windows, the equivalent is `MSYS2_ARG_CONV_EXCL='*'`.)

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

Every release runs `govulncheck`, `gosec`, `gitleaks` and `trivy`, and will not
publish if any of them reports something high severity. Three medium findings are
expected and reviewed, listed here so nobody has to rediscover them:

| finding | why it is there |
|---|---|
| `G204` — subprocess with variable arguments (`internal/keystore/keyring.go`) | reading your keychain means running `security` or `secret-tool`. The secret goes in on **stdin**, never as an argument, because arguments are visible to every process on the machine. |
| `G117` × 2 — a struct field named `Password` is serialised | that is the point: the credential store persists your password so the bridge can stay signed in. It goes to your OS keychain, or to an AES-256-GCM encrypted file. Whitepaper §9.2 records this as a deliberate deviation from "never store a password". |

One finding is suppressed at the call site with a stated reason: the retry backoff
uses a non-cryptographic random source for jitter. Jitter exists so that many
clients do not retry in lockstep, which needs spread rather than
unpredictability.
