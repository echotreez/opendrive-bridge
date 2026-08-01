# Your first five minutes

This gets you from a downloaded file to your own file sitting in OpenDrive. You
need a terminal and an OpenDrive account. You do not need to know anything about
OpenDrive's API, and you do not need Go.

**What you are installing.** Two programs in one folder. `opendrived` is a small
server that runs on your own machine and talks to OpenDrive for you; `odctl` is
the command you type. They run from wherever you unpack them — nothing is
installed system-wide, and nothing on this page needs `sudo`.

**How signing in works.** You write your OpenDrive username and password into a
file called `.env`, once, in the clear. The first time you start the daemon it
encrypts that file, puts the key in `.env.key` beside it, and the plaintext is
gone. After that the bridge keeps itself signed in and you never type the
password again.

**What that protects you from, and what it does not.** It protects you from
committing your password to git, from syncing it to a cloud backup in the clear,
from someone reading it over your shoulder, and from it turning up in a log or a
process listing. It does **not** protect you from someone who can already read
your files as you — `.env.key` sits next to `.env`, so they would get both. That
is the price of a daemon that restarts by itself without anybody typing a
passphrase. On a shared machine, use full-disk encryption and a separate account
rather than relying on this.

Back up `.env` and `.env.key` together, or neither is any use.

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

When you first run these programs, macOS is likely to say **"cannot be opened
because the developer cannot be verified"**, or simply kill them. That is not a
sign that anything is wrong with the file. macOS marks *everything* downloaded
from the internet and refuses anything not signed with a paid Apple certificate.

```bash
# 1. Download the darwin_arm64 archive (Apple Silicon) from
#    https://github.com/echotreez/opendrive-bridge/releases

# 2. Check it is the file we published, before you trust it:
shasum -a 256 -c opendrive-bridge_*_SHA256SUMS --ignore-missing
# expect: opendrive-bridge_1.1.0_darwin_arm64.tar.gz: OK

# 3. Unpack. This creates a folder called opendrive-bridge/ — keep it
#    somewhere permanent, because your credentials will live in it.
tar xzf opendrive-bridge_*_darwin_arm64.tar.gz
cd opendrive-bridge

# 4. Now clear the quarantine mark:
xattr -d com.apple.quarantine ./opendrived ./odctl
```

If step 4 says `No such xattr`, the mark was not there and everything is fine.

Releases built when a signing certificate was available are signed and notarised
and need none of this.

### Then: sign in and upload something

```bash
cp .env.example .env
$EDITOR .env          # put your OpenDrive username and password in
```

Start the daemon. Leave this window open:

```bash
./opendrived
```

That first start encrypts `.env`, creates `.env.key` beside it, and generates the
bridge's own API key. Your password is no longer readable on disk.

In a second terminal, in the same folder:

```bash
./odctl status                   # Signed in as you@example.com.
./odctl ls /                     # your OpenDrive, from the top
echo "hello from my Mac" > hello.txt
./odctl up hello.txt /hello.txt
./odctl ls /
./odctl down /hello.txt back.txt
```

That is the whole loop. `Ctrl-C` in the first window stops the daemon; your
sign-in survives, and starting it again asks for nothing.

To have it start on its own when you log in:

```bash
./odctl daemon install
./odctl daemon start
```

It runs from this folder and needs no `sudo`. `install` also prints the one line
that puts the folder on your `PATH`, so you can drop the `./` — it will add it
for you if you pass `--modify-shell-profile`.

---

## Linux

```bash
# Download the archive for your machine:
#   64-bit PC                 → linux_amd64
#   Raspberry Pi, ARM servers → linux_arm64

sha256sum --check --ignore-missing opendrive-bridge_*_SHA256SUMS
tar xzf opendrive-bridge_*_linux_*.tar.gz
cd opendrive-bridge

cp .env.example .env
$EDITOR .env          # put your OpenDrive username and password in
```

Start it once, in one terminal:

```bash
./opendrived
```

And in another, in the same folder:

```bash
./odctl status
./odctl ls /
echo "hello from Linux" > hello.txt
./odctl up hello.txt /hello.txt
./odctl down /hello.txt back.txt
```

To keep it running in the background:

```bash
./odctl daemon install     # a systemd *user* service — no sudo
./odctl daemon start
systemctl --user status opendrived
```

If you want it to keep running when you are not logged in, that is the one thing
that needs root:

```bash
sudo loginctl enable-linger $USER
```

---

## Windows

Download the `windows_amd64` zip and unpack it — it creates an
`opendrive-bridge` folder. Keep it somewhere permanent.

In **PowerShell**, inside that folder:

```powershell
Get-FileHash .\opendrive-bridge_1.1.0_windows_amd64.zip -Algorithm SHA256
# compare that against the line for this file in the SHA256SUMS file

Copy-Item .env.example .env
notepad .env            # put your OpenDrive username and password in

.\opendrived.exe
```

In a second PowerShell window, in the same folder:

```powershell
.\odctl.exe status
.\odctl.exe ls /
"hello from Windows" | Out-File -Encoding utf8 hello.txt
.\odctl.exe up hello.txt /hello.txt
.\odctl.exe down /hello.txt back.txt
```

`.env` and `.env.key` are locked down with `icacls` so that only your account can
read them — NTFS ignores the Unix permission bits, so the bridge sets the access
control list explicitly.

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

The container takes **the same two files** as everywhere else. There is no
container-specific setup any more — you prepare `.env` exactly as you would on a
laptop and mount it in.

```bash
cp .env.example .env
$EDITOR .env          # put your OpenDrive username and password in

docker run -d --name opendrive-bridge \
  -p 127.0.0.1:9750:9750 \
  -e ODB_API_KEY="$(openssl rand -hex 32)" \
  -v "$PWD/.env:/data/.env" \
  -v odb-state:/data/jobs \
  ghcr.io/echotreez/opendrive-bridge:1.1.0
```

The first start rewrites `.env` encrypted and creates `.env.key` next to it — on
your machine, through the mount. Nothing is baked into the image.

`deploy/docker/docker-compose.yaml` is the same thing written down, with a
healthcheck.

Three things to get right:

- **`.env` has to be writable.** The first run rewrites it, and so does every
  token refresh. Mount it `:ro` and the bridge will work until the first refresh
  and then fail in a way that looks like an OpenDrive outage. `.env.key` is only
  ever read, so that one can be `:ro`.
- **`ODB_API_KEY` is required here** and only here. The image listens on
  `0.0.0.0`, because inside a container loopback means "nothing can reach it", and
  a non-loopback address makes the daemon insist on a key. On your own machine it
  generates one into `.env` and you never see it.
- **Back up `.env` and `.env.key` together.** Either one alone is useless.

Then, from your own machine:

```bash
export ODB_ADDR=127.0.0.1:9750
export ODB_API_KEY=<the value you generated above>
odctl ls /
```

---

## When something goes wrong

`odctl status` answers without touching the network, so it works even when
OpenDrive does not, and it will say which situation you are in:

| what it says | what to do |
|---|---|
| Not signed in yet | put your username and password in `.env` and start the daemon once |
| Signed in as … | nothing; it is working |
| Your saved password is no longer accepted | you changed it on the website; put the new one in `.env` and restart |
| OpenDrive is asking for a captcha | sign in once at opendrive.com in a browser, then retry |
| The bridge cannot reach its credential store | `.env.key` is missing, or `.env` cannot be decrypted with it |

A transfer that exits **7** was refused by OpenDrive for good — most often an
account that is not allowed to write where it was asked to. Retrying will not
help; the account's administrator controls it.

`no bridge is running to talk to` means `opendrived` is not started, or is
listening somewhere other than where `odctl` is looking — check `--addr` and
`ODB_ADDR`.

**If you lose `.env.key`**, the credentials in `.env` cannot be read back. It is
not a password you can reset. Delete both files, copy `.env.example` to `.env`
again and sign in once more; nothing in your OpenDrive account is affected.

Every message this program prints is meant to tell you what to do next. If one
does not, that is a bug worth reporting.

## Where to go next

- [deployment.md](./deployment.md) — running it in the background permanently, on
  all three platforms and in containers, and what the two credential files are
- [bridge-openapi.yaml](./bridge-openapi.yaml) — the HTTP API, if you want to
  drive it from your own programs rather than from `odctl`
