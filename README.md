# OpenDrive Bridge

**Use your [OpenDrive](https://www.opendrive.com) storage from the command line,
or from your own programs, without touching OpenDrive's API.**

OpenDrive's REST API is workable but awkward: uploads take four calls in a fixed
order, tokens expire on their own schedule, and the API often reports success for
things that did not happen. This puts a small server on your own machine that
deals with all of that, and gives you two ordinary things instead:

```bash
odctl up ./report.pdf /Documents/report.pdf     # a command line
curl 127.0.0.1:9750/v1/ls?path=/Documents       # and a plain local HTTP API
```

**Who it is for.** Anyone who wants OpenDrive in a script, a backup job or a
program of their own — and does not want to write an API client to get there.
You need a terminal; you do not need Go, and you do not need to have heard of
OpenDrive's API.

**Sign in once.** Your username and password go into a `.env` file, once. The
first run encrypts it and the plaintext disappears; after that the daemon keeps
itself signed in and nothing else ever handles your password.
[What that protects you from, and what it does not.](./SECURITY.md#what-this-protects-you-from-and-what-it-does-not)

Linux and macOS, or as a container. MIT licensed.

## Five minutes

```bash
# 1. Download the archive for your machine from the releases page, and check it
sha256sum --check --ignore-missing opendrive-bridge_*_SHA256SUMS

# 2. Unpack. This creates opendrive-bridge/ — nothing is installed system-wide
tar xzf opendrive-bridge_*_linux_amd64.tar.gz
cd opendrive-bridge

# 3. Put your OpenDrive username and password in, once
cp .env.example .env
$EDITOR .env

# 4. Start it once. This encrypts .env and your password stops being readable
./opendrived

# 5. In another terminal, in the same folder
./odctl ls /
./odctl up ./report.pdf /Documents/report.pdf
./odctl down /Documents/report.pdf ./back.pdf
```

> **On macOS, do [the quarantine step](./docs/first-run.md#macos) first.** These
> builds are not signed with an Apple certificate, so macOS refuses to run them
> until you clear one flag. It is one command, it is not a sign that anything is
> wrong with the download, and it is the single most common reason a first run
> fails.

**[docs/first-run.md](./docs/first-run.md)** is the same path written out
properly, with a section per platform and one for Docker.

## Documentation

| | |
|---|---|
| [docs/first-run.md](./docs/first-run.md) | download → first upload, per platform |
| [docs/deployment.md](./docs/deployment.md) | running it permanently, choosing a credential store, troubleshooting |
| [docs/bridge-openapi.yaml](./docs/bridge-openapi.yaml) | the HTTP API, for driving it from your own programs |
| [OpenDrive-Bridge-Whitepaper.md](./OpenDrive-Bridge-Whitepaper.md) | design, architecture, phases and quality gates |

## What it does about OpenDrive's API

Most of the work here is not wrapping endpoints; it is refusing to pass on things
upstream says that are not true. Forty-five of them are recorded in
[docs/discrepancies.md](./docs/discrepancies.md), measured against the live API
rather than the PDF, and
[docs/bridge-boundary-audit.md](./docs/bridge-boundary-audit.md) checks each one
against the Bridge API: every one is either stopped at the boundary or written
down in the OpenAPI spec so a caller can plan for it.

Three that shape the design:

- **A 200 proves nothing.** `download/all.json` answers a request for files with
  a valid, empty ZIP. `folder/info.json` still describes a folder you deleted.
  `filesettings` accepts a misspelt parameter, changes nothing, and returns the
  whole object. The bridge verifies effects, never statuses.
- **The error text is not the error.** A 403 saying "permission" is byte-for-byte
  identical whether the cause is a real denial or a transient refusal. Failures
  go through one classification layer
  ([docs/error-taxonomy.md](./docs/error-taxonomy.md)) that reads evidence —
  status, body shape, content type, which endpoint — rather than taking
  upstream's word for what happened.
- **Your password is persisted, deliberately.** OpenDrive has no refresh token
  that survives a password change, and unattended operation needs one or the
  other. Whitepaper §9.2 records the decision and its cost, and
  `persist_password: false` is the way out. It goes to the OS credential store
  and nowhere else — never a config file, never a process argument.

## Repository layout

```
cmd/opendrived/   守护进程入口            pkg/opendrive/   Go SDK(核心)
cmd/odctl/        CLI 入口                internal/        server / cli / jobs / keystore / cache
tools/            mock upstream、线上 Swagger 规格抓取
deploy/           docker / systemd / launchd
docs/             使用指南、OpenAPI 规格,以及官方 API 参考资料(只读)
```

## Building it yourself

Go 1.22 or newer.

```bash
go test ./...                 # unit and contract tests, no network
./scripts/check-coverage.sh   # per-package coverage gates
./scripts/integration-test.sh # the live suite; needs sandbox credentials
```

Conventional Commits, tests with every change, and every gate that runs at
release time also runs on every pull request — that last rule was earned rather
than chosen, and [CLAUDE.md](./CLAUDE.md) says how.

## 参考资料 References

- **[docs/official-api-reference.md](./docs/official-api-reference.md)** — 官方文档
  从哪里取,以及为什么它不在本仓库里
- 线上 API Explorer: https://dev.opendrive.com/api/explorer/
- 线上机器可读规格: `https://dev.opendrive.com/api/v1/resources.json`
- `docs/api-samples/` — OpenDrive 官方示例代码(PHP / C# / JavaScript),
  由 OpenDrive, Inc. 以 **MIT** 许可发布,© OpenDrive, Inc.,原样保留并注明来源

OpenDrive 的 REST API Guide(PDF)**不在本仓库中**,因为它的版权页写明未经书面
许可不得以任何形式复制或传播,并且明确把"转换格式"也算作复制。本项目的所有结论
来自对线上 API 的实测,记录在 `docs/discrepancies.md`(46 条)与
`docs/error-taxonomy.md` 中 —— 那是运行结果的记录,不是文档的转述。

## Licence

**MIT**, for everything written for this project — see [LICENSE](./LICENSE).
Copyright © 2026 Derek Zhang.

`docs/api-samples/` is OpenDrive's own sample code (PHP, C#, JavaScript),
republished unchanged under the MIT licence OpenDrive publishes it with.
Copyright © OpenDrive, Inc. — see
[docs/api-samples/LICENSE](./docs/api-samples/LICENSE). It is not compiled into
the binaries.

OpenDrive's REST API Guide (the PDF) is **not** in this repository and cannot be:
its copyright page forbids reproducing or transmitting it in any form, and says
that reproducing includes converting it to another format. Where to request it,
and which sources outrank it, is in
[docs/official-api-reference.md](./docs/official-api-reference.md).

## Security

Report vulnerabilities privately through
[GitHub's advisory form](https://github.com/echotreez/opendrive-bridge/security/advisories/new),
not a public issue. [SECURITY.md](./SECURITY.md) has the threat model in full —
including a plain account of what the credential file does **not** protect you
from, which is worth reading before you decide to trust it.
