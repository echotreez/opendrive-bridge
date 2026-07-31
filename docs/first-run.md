# Your first five minutes

This gets you from a downloaded file to your own file sitting in OpenDrive. You
need a terminal and an OpenDrive account. You do not need to know anything about
OpenDrive's API, and you do not need Go.

**What you are installing.** Two programs. `opendrived` is a small server that
runs on your own machine and talks to OpenDrive for you. `odctl` is the command
you type. You sign in once; the server stays signed in afterwards, using your
operating system's own password store, so nothing else ever handles your
password.

Pick your section:

- [macOS](#macos) — **read the quarantine part first, it will stop you otherwise**
- [Linux](#linux)
- [Windows](#windows)
- [Docker](#docker) — different setup, because a container has no password store

For running it permanently in the background, see
[deployment.md](./deployment.md). This page is only about the first five
minutes.

---

## macOS

### First: macOS will refuse to run the download

Do this before anything else, because it is the thing that stops people.

When you first run `odctl`, macOS is likely to say **"cannot be opened because
the developer cannot be verified"**, or simply kill it. That is not a sign that
anything is wrong with the file. macOS marks *everything* downloaded from the
internet, and it refuses anything not signed with a paid Apple certificate.

The fix is one command, and the right order is: check the file, then clear the
mark.

```bash
# 1. Download the macOS archive for your chip from
#    https://github.com/StormRealm/opendrive-bridge/releases
#    Apple Silicon (M1 and later) → darwin_arm64
#    Intel                        → darwin_amd64

# 2. Check it is the file we published, before you trust it:
shasum -a 256 -c opendrive-bridge_*_SHA256SUMS --ignore-missing
# expect: opendrive-bridge_0.1.0_darwin_arm64.tar.gz: OK

# 3. Unpack and install:
tar xzf opendrive-bridge_*_darwin_*.tar.gz
sudo install -m 0755 opendrived odctl /usr/local/bin/

# 4. Now clear the quarantine mark:
sudo xattr -d com.apple.quarantine /usr/local/bin/opendrived /usr/local/bin/odctl
```

If step 4 says `No such xattr`, the mark was not there and everything is fine.

Releases built when a signing certificate was available are signed and notarised
and need none of this.

### Then: sign in and upload something

```bash
# Start the server. Leave this window open for now.
opendrived
```

In a second terminal window:

```bash
odctl login you@example.com
# It asks for your password. It is not shown as you type, and it is not
# stored anywhere except your macOS Keychain.
```

The first time the daemon reads that password back, macOS will ask whether to
allow it. **Choose "Always Allow"** — otherwise you will be asked again after
every restart.

```bash
odctl ls /                      # your OpenDrive, from the top
echo "hello from my Mac" > hello.txt
odctl up hello.txt /hello.txt   # upload
odctl ls /                      # it is there
odctl down /hello.txt back.txt  # and back again
```

That is the whole loop. Press `Ctrl-C` in the first window to stop the server;
your sign-in survives — starting it again does not ask for the password.

To have it start on its own when you log in, `odctl daemon install` and then
`odctl daemon start`. It installs for you alone and needs no `sudo`; the reason
is in [deployment.md](./deployment.md).

---

## Linux

```bash
# Download the linux archive for your machine from
# https://github.com/StormRealm/opendrive-bridge/releases
#   64-bit PC        → linux_amd64
#   Raspberry Pi, ARM servers → linux_arm64

sha256sum --check --ignore-missing opendrive-bridge_*_SHA256SUMS
tar xzf opendrive-bridge_*_linux_*.tar.gz
sudo install -m 0755 opendrived odctl /usr/local/bin/
```

Start it, in one terminal:

```bash
opendrived
```

And in another:

```bash
odctl login you@example.com
odctl ls /
echo "hello from Linux" > hello.txt
odctl up hello.txt /hello.txt
odctl ls /
odctl down /hello.txt back.txt
```

### If it says it cannot reach a credential store

On a desktop Linux machine your password goes into GNOME Keyring or KWallet, and
this just works. On a **server with no desktop session** there is no such store,
and the bridge will tell you so rather than quietly keeping your password
somewhere it should not. That is deliberate — see
[deployment.md §5](./deployment.md) for the encrypted-file setup, which is the
same one the [Docker](#docker) section below uses.

---

## Windows

Download the `windows_amd64` zip from the
[releases page](https://github.com/StormRealm/opendrive-bridge/releases) and
unpack it, for example to `C:\opendrive-bridge`.

In **PowerShell**, check it and start the server:

```powershell
Get-FileHash .\opendrive-bridge_0.1.0_windows_amd64.zip -Algorithm SHA256
# compare that against the line for this file in the SHA256SUMS file

cd C:\opendrive-bridge
.\opendrived.exe
```

In a second PowerShell window:

```powershell
cd C:\opendrive-bridge
.\odctl.exe login you@example.com
.\odctl.exe ls /
"hello from Windows" | Out-File -Encoding utf8 hello.txt
.\odctl.exe up hello.txt /hello.txt
.\odctl.exe ls /
.\odctl.exe down /hello.txt back.txt
```

Your password goes into the Windows Credential Manager, under
`opendrive-bridge`.

### If you use Git Bash

Use PowerShell or Command Prompt if you can. Git Bash rewrites any argument
starting with a slash into a Windows path before `odctl` ever sees it, so

```
odctl ls /Documents
```

arrives as `odctl ls "C:/Program Files/Git/Documents"` and cannot work. `odctl`
recognises the result and tells you what happened, but it cannot undo it. If you
want to stay in Git Bash, put `MSYS_NO_PATHCONV=1` in front of the command:

```bash
MSYS_NO_PATHCONV=1 odctl ls /Documents
```

This affects paths in your OpenDrive account only. Local filenames are fine
either way.

---

## Docker

A container has no Keychain, no Credential Manager and no keyring. So instead of
using one, the bridge encrypts your credentials into a file — and it needs a key
from you to do that. **If you do not give it one, it refuses to start and tells
you so.** That is the intended behaviour, not a fault: the alternative is a
bridge that forgets your sign-in every restart, or one that writes your password
somewhere in the clear.

### Make the two keys

```bash
openssl rand -base64 32 > .odb_state_key   # encrypts your stored credentials
openssl rand -hex 32    > .odb_api_key     # who is allowed to talk to the bridge
chmod 600 .odb_state_key .odb_api_key
```

**Keep the state key.** It is not a password you can reset. If you lose it, the
stored credentials cannot be read back and you will have to sign in again — no
data is lost, but the container will be locked out until you do. Put it in your
password manager now, before you go further.

The API key exists because the container binds `0.0.0.0` — inside a container
loopback is unreachable from outside, so there is no "only this machine" to fall
back on. Anything that can reach the port could otherwise use your OpenDrive
account, so the bridge insists on a key.

### Start it

```bash
export ODB_STATE_KEY="$(cat .odb_state_key)"
export ODB_API_KEY="$(cat .odb_api_key)"

docker run -d --name opendrive-bridge \
  -p 127.0.0.1:9750:9750 \
  -e ODB_STATE_KEY -e ODB_API_KEY \
  -v odb-state:/data \
  ghcr.io/stormrealm/opendrive-bridge:latest
```

Or with the compose file from the repository, which does the same thing and adds
a healthcheck:

```bash
docker compose -f deploy/docker/docker-compose.yaml up -d
```

### Use it

With `odctl` installed on your own machine, pointed at the container:

```bash
odctl --addr 127.0.0.1:9750 --api-key "$(cat .odb_api_key)" login you@example.com
odctl --addr 127.0.0.1:9750 --api-key "$(cat .odb_api_key)" ls /
```

To save repeating those flags:

```bash
export ODB_ADDR=127.0.0.1:9750
export ODB_API_KEY="$(cat .odb_api_key)"
odctl ls /
```

The `odb-state` volume holds the encrypted credentials and the transfer state.
Keep it, along with the state key — either one alone is useless, and you need
both to avoid signing in again.

---

## When something goes wrong

`odctl status` answers without touching the network, so it works even when
OpenDrive does not, and it will say which situation you are in:

| what it says | what to do |
|---|---|
| Not signed in yet | `odctl login you@example.com` |
| Signed in as … | nothing; it is working |
| Your saved password is no longer accepted | you changed it on the website; sign in again |
| OpenDrive is asking for a captcha | sign in once at opendrive.com in a browser, then retry |
| The bridge cannot reach its credential store | unlock your keychain, or see the Docker section above |

A transfer that exits **7** was refused by OpenDrive for good — most often an
account that is not allowed to write where it was asked to. Retrying will not
help; the account's administrator controls it.

`no bridge is running to talk to` means `opendrived` is not started, or is
listening somewhere other than where `odctl` is looking — check `--addr` and
`ODB_ADDR`.

Every message this program prints is meant to tell you what to do next. If one
does not, that is a bug worth reporting.

## Where to go next

- [deployment.md](./deployment.md) — running it in the background permanently,
  on all three platforms and in containers, and how to choose a credential store
- [bridge-openapi.yaml](./bridge-openapi.yaml) — the HTTP API, if you want to
  drive it from your own programs rather than from `odctl`
