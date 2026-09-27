# Your first five minutes

This gets you from a downloaded file to your own file sitting in OpenDrive. You
need a terminal and an OpenDrive account. You do not need to know anything about
OpenDrive's API, and you do not need Go.

**What you are installing.** Two programs in one folder. `opendrived` is a small
server that runs on your own machine and talks to OpenDrive for you; `odctl` is
the command you type. They run from wherever you unpack them — nothing is
installed system-wide, and nothing on this page needs `sudo`.

**How signing in works.** There is nothing to prepare. Start the bridge, then sign
in once — on its web page at `http://127.0.0.1:9750/ui`, or with
`odctl login you@example.com`. The bridge encrypts what you typed into one file,
`credentials.key`, beside the programs, and keeps itself signed in from then on:
restarting it asks for nothing. If you change your OpenDrive password, the bridge
notices that OpenDrive no longer accepts the old one and asks you to sign in again
— on the page or with `odctl login` — without a restart.

**What that protects you from, and what it does not.** The password in
`credentials.key` is encrypted, so it is not readable over your shoulder, in git,
in a cloud backup's plain text, in a log or in a process listing. The key that
decrypts it is in the same file, because a bridge that restarts by itself cannot
ask anybody for a passphrase — so it does **not** protect you from someone who can
already read your files as you. On a shared machine, use full-disk encryption and
a separate account rather than relying on this.

Back up `credentials.key` if you do not want to sign in again after restoring.
Losing it costs nothing else: sign in again.

Pick your section:

- [macOS](#macos) — **read the quarantine part first, it will stop you otherwise**
- [Linux](#linux)
- [Docker](#docker) — `docker compose up -d`, then sign in
- [The cache](#the-cache-if-you-turn-it-on) — optional, and off on a host until you ask for it

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
# 1. Download the darwin_arm64 archive (Apple silicon) from
#    https://github.com/echotreez/opendrive-bridge/releases

# 2. Check it is the file we published, before you trust it:
shasum -a 256 -c opendrive-bridge_*_SHA256SUMS --ignore-missing
# expect: opendrive-bridge_<version>_darwin_arm64.tar.gz: OK

# 3. Unpack. This creates a folder called opendrive-bridge/ — keep it
#    somewhere permanent, because your sign-in will be kept in it.
tar xzf opendrive-bridge_*_darwin_arm64.tar.gz
cd opendrive-bridge

# 4. Now clear the quarantine mark:
xattr -d com.apple.quarantine ./opendrived ./odctl
```

If step 4 says `No such xattr`, the mark was not there and everything is fine.
Intel Macs are not supported.

### Then: start it, sign in, and upload something

Start the daemon. Leave this window open:

```bash
./opendrived
```

Open `http://127.0.0.1:9750/ui` and sign in — or, in a second terminal in the same
folder:

```bash
./odctl login you@example.com    # asks for the password; it stays out of your shell history
./odctl status                   # Signed in as you@example.com.
./odctl ls /                     # your OpenDrive, from the top
echo "hello from my Mac" > hello.txt
./odctl up hello.txt /hello.txt  # Uploaded to /hello.txt. It is on OpenDrive.
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
./opendrived
```

Then open `http://127.0.0.1:9750/ui` and sign in, or in another terminal, in the
same folder:

```bash
./odctl login you@example.com
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

## Docker

Put `deploy/docker/docker-compose.yaml` (from the archive, or the repository) in a
folder of its own, and:

```bash
docker compose up -d
```

Then open `http://127.0.0.1:9750/ui` and sign in. That is all — there is no file to
create, no folder to prepare, no `chown` and no key to generate. Docker creates
`./data` beside the compose file on the first start; the bridge keeps everything in
it — `credentials.key`, transfer state in `jobs/`, and the cache in `cache/` — so it
survives `docker compose down`, image upgrades and new containers.

If you change your OpenDrive password, the page asks you to sign in again. Nothing
needs restarting or recreating.

From the terminal instead of the page:

```bash
docker compose exec opendrive-bridge odctl login you@example.com
docker compose exec opendrive-bridge odctl status
```

A few things worth knowing:

- **The port is published to `127.0.0.1` only**, so only this machine can reach the
  bridge, and it asks for no API key. If you publish it more widely, set
  `ODB_API_KEY` in the compose file so that every caller must send it; the bridge
  logs a warning when it listens beyond loopback without one.
- **The cache is on, in write-back mode.** An upload made through the bridge's API is
  answered as soon as the file is in `./data/cache`, and sent to OpenDrive in the
  background. For that while `./data` is the only copy — which is why it is on
  the host and not in the container. Before you stop the container or move `./data`:

  ```bash
  docker compose exec opendrive-bridge odctl cache status
  ```

  The last line says whether it is safe. `docker compose stop` gives the bridge
  five minutes to finish on its own, and names anything it could not.
- **The container runs as root**, as containers ordinarily do, so on Linux the files
  in `./data` belong to root; reading them for a backup takes `sudo`.
- **One bridge per `./data`.** On one Linux machine a second container on the same
  folder refuses to start. Where each container is its own virtual machine — Apple's
  `container`, and possibly Docker Desktop — that lock does not reach across, and
  running one per folder is up to you.
- **`odctl up` and `odctl down` cannot move files through a bridge in a container**:
  they hand it a path on your machine, which the container cannot see. Use the API's
  `PUT /v1/upload/stream` and `GET /v1/download/stream` for now.
- **On Docker Desktop**, whether fsync crosses into the host through a bind mount
  has not been measured, and the cache's guarantees rest on it. The compose file
  has a commented line that keeps the cache on a named volume instead.
- **On a Mac with Apple's `container`** (measured with 1.2.0): an upload accepted by
  the cache, followed at once by `container kill --signal KILL`, was sent after the
  next start and arrived byte for byte.

Is it safe to stop? `docker compose exec opendrive-bridge odctl cache status`. If
something is still waiting, `... odctl cache flush --wait` sends it now.

---

## The cache, if you turn it on

On a host the cache is off unless you give the daemon a directory for it, and
everything above works without it. (The Docker compose file turns it on, into
`./data/cache`.) Read this before you switch it on, because it changes what a
finished upload means.

```bash
./opendrived --cache-dir ./cache
```

Two things then happen.

**Reading gets faster.** A file you have read once is served from your own disk next
time instead of being fetched again. Nothing to think about; losing the cache costs a
download.

**Writing gets different, and this is the part to understand.** The bridge copies
your file to its own disk first and sends it to OpenDrive afterwards. For a short
while — usually seconds — **the bridge is the only place that file exists.**

That is a genuinely useful trade: once a file is in the cache, a network drop or a
bad minute at OpenDrive no longer loses it — the bridge keeps trying, and picks up
again after a restart. It does **not** yet help if OpenDrive is already unreachable
when you start the upload: the bridge checks the destination folder with OpenDrive
before it accepts a byte, so that upload fails as it would without the cache. And
it means "the upload finished" and "OpenDrive has it" stop being the same sentence,
so the bridge gives you a way to ask.

`odctl up` does not leave you guessing: even with the cache on, it waits until the
file is on OpenDrive and then says so, so a large upload takes as long as it did
before. What gets faster is everything that does not wait — another program using
the bridge's API gets its answer as soon as the file is on the bridge's disk — and
reading: a file you fetched once comes back from your own disk. For everything the
bridge is still holding:

```bash
./odctl cache status
```

The last line answers it in words:

```
NOT safe to stop the bridge yet: 41.2 MB has not reached OpenDrive.
Run `odctl cache flush --wait` to send it now.
```

`odctl cache flush --wait` returns when there is nothing left. You do not normally
need it — stopping the service gives the bridge time to finish by itself — but it is
there for when you want to be sure before closing a laptop.

Three more things worth knowing:

- **It never throws away a file to make room.** If it runs out of space for things it
  has not uploaded yet, it refuses new writes and says so, rather than discarding
  something OpenDrive does not have.
- **Nothing in the cache directory is encrypted.** `credentials.key` is; this is not.
  It holds your files exactly as they are, protected by the directory's permissions
  (the bridge sets it so only its own account can read it) and by whatever your disk
  provides. If that is not enough for what you work with, leave the cache off or put
  it on an encrypted volume.
- **In a container, keep it outside the container** — the compose file puts it in
  `./data/cache` on the host. This is the one place where getting it wrong loses
  data.

If you would rather keep the faster reads and none of the above,
`--cache-write-back=false` (or `ODB_CACHE_WRITE_BACK=false`) sends writes straight
to OpenDrive as they always were.

[deployment.md](./deployment.md#5-the-local-cache) has the rest.

---

## When something goes wrong

`odctl status` answers without touching the network, so it works even when
OpenDrive does not, and it will say which situation you are in. The web page says
the same things next to its sign-in form.

| what it says | what to do |
|---|---|
| Not signed in yet | sign in: on the web page, or `odctl login you@example.com` |
| Signed in as … | nothing; it is working |
| Your saved password is no longer accepted | you changed it on the website; sign in again with the new one |
| OpenDrive is asking for a captcha | sign in once at opendrive.com in a browser, then retry |
| The bridge cannot read its saved sign-in | `credentials.key` is damaged or unreadable; sign in again to replace it |

A transfer that exits **7** was refused by OpenDrive for good — most often an
account that is not allowed to write where it was asked to. Retrying will not
help; the account's administrator controls it.

`no bridge is running to talk to` means `opendrived` is not started, or is
listening somewhere other than where `odctl` is looking — check `--addr` and
`ODB_ADDR`.

**Coming from 1.1 or 1.2?** Nothing to do. If the folder still has the `.env` (and
`.env.key`) those versions used, the first start reads them into `credentials.key`
and says so in its log. The old files are then unused; delete them when you are
satisfied.

Every message this program prints is meant to tell you what to do next. If one
does not, that is a bug worth reporting.

## Where to go next

- [deployment.md](./deployment.md) — running it in the background permanently, on
  macOS and Linux and in containers, and the cache in full
- [bridge-openapi.yaml](./bridge-openapi.yaml) — the HTTP API, if you want to
  drive it from your own programs rather than from `odctl`
