# Security

## Reporting a vulnerability

Please report privately rather than in a public issue: use GitHub's
[**Report a vulnerability**](https://github.com/echotreez/opendrive-bridge/security/advisories/new)
button on the Security tab, which opens a private advisory only the maintainers
can see.

Include what you did, what happened, and what you expected. A proof of concept
helps but is not required — a clear description of the mistake is enough.

This is a small project maintained by one person. You can expect an
acknowledgement within a week. If something is exploitable and being used, say so
in the first line and it will jump the queue. There is no bounty programme.

If you would rather not use GitHub, the maintainer's address is in the commit
history.

## What this protects you from, and what it does not

The bridge holds a password that unlocks somebody's entire cloud storage, so it
is worth being exact about the threat model rather than reassuring.

**Where your credentials live.** In `.env`, in the folder you unpacked,
encrypted with AES-256-CBC. The key is 32 random bytes in `.env.key`, generated
on first run, sitting next to it. Both files are 0600.

**It protects you from:**

- committing your password to git — `.env` and `.env.key` are in `.gitignore`,
  and the release archives and container images are checked in CI for both;
- a cloud backup or file sync carrying your password away in the clear;
- someone reading it over your shoulder, or finding it in a screen recording;
- your password appearing in a log file, a crash report or a process listing.
  Redaction of `access_token`, `session_id` and `passwd` is not optional and
  cannot be switched off;
- a `cat .env` typed by habit.

**It does not protect you from:**

- **anyone who can already read your files as you.** `.env.key` is next to
  `.env`, so they get both. This is the important one, and it is deliberate: any
  scheme that asks for a passphrase at startup cannot restart the daemon after a
  reboot or a crash, and unattended operation is the product requirement the
  whole design rests on. It is a conscious trade, not an oversight.
- **another program running as you.** The bridge listens on `127.0.0.1` and, on
  loopback, treats the operating system's decision about who may connect as the
  authority. If that is not good enough for your machine, configure an API key
  explicitly with `--api-key` and it will be required for every request.
- **someone with root or Administrator.** They can read anything.

If you are on a shared machine, or the account matters more than a personal one,
use system-level measures instead of relying on this: full-disk encryption, a
separate user account for the bridge, and least privilege.

## Deliberate deviations worth knowing about

**The bridge stores your password, not just a token.** OpenDrive's OAuth2 terms
ask applications not to. The reason it does anyway: the refresh token lasts 30
days and only rolls when it is used, so a bridge left alone over a long holiday
comes back to two dead tokens and no way to recover without a person. Storing the
password is what makes "set it up once and forget it" true. `persist_password:
false` turns it off, at the cost of signing in again after a long idle period.

**The credential file is AES-256-CBC, not an AEAD.** The whitepaper originally
asked for GCM *and* for the file to be readable with `openssl enc -d`, and those
are mutually exclusive — `openssl enc` refuses AEAD ciphers. Keeping the
`openssl` compatibility was chosen deliberately, so that you can always recover
your own credentials without this program:

```bash
openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -a -pass file:.env.key -in .env
```

That command is tested against the real `openssl`, in both directions, on every
change. Integrity is not dropped: the plaintext carries a checksum, so an altered
file fails to load rather than loading something subtly different.

## What is checked, and when

Every one of these runs on every change, not only at release time — a rule this
project learned by having gates that only ran when someone tried to ship:

| | |
|---|---|
| `govulncheck` | known vulnerabilities in anything reachable from our code |
| `gosec` | fails on medium and above; every suppression names its reason at the line |
| `gitleaks` | the working tree and the full history |
| `trivy` | the container image, before it is pushed |

Releases are built by GitHub Actions from a tag, with SHA256 checksums published
alongside. **The macOS binaries are not code-signed**, because that needs a paid
developer certificate — which is why macOS refuses to run them until you clear the
quarantine flag. Verify the checksum before you run anything; `docs/first-run.md`
has the command for each platform.

## Supported versions

The newest minor release gets everything. The one before it gets security fixes
only. Older than that: please upgrade. See
[docs/maintenance.md](./docs/maintenance.md).
