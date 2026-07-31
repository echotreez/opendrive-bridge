# OpenDrive Bridge

A local REST proxy (`opendrived`) and command line (`odctl`) for
[OpenDrive.com](https://www.opendrive.com) cloud storage, written in Go.

把 OpenDrive 官方 REST API 封装为本地代理服务与命令行工具:内部处理 OAuth2 token
生命周期、四步分块上传、MD5 秒传、断点续传与重试,对外提供统一简化的现代 REST 接口。

You sign in once. After that the daemon keeps itself signed in through your
operating system's own credential store, so your scripts and programs can work
with your files without ever handling your password.

## Five minutes

```bash
# 1. Download the archive for your machine from the releases page, and check it
sha256sum --check --ignore-missing opendrive-bridge_*_SHA256SUMS

# 2. Unpack and install
tar xzf opendrive-bridge_*_linux_amd64.tar.gz
sudo install -m 0755 opendrived odctl /usr/local/bin/

# 3. Run the daemon in one terminal
opendrived

# 4. In another, sign in and upload something
odctl login you@example.com
odctl ls /
odctl up ./report.pdf /Documents/report.pdf
odctl down /Documents/report.pdf ./back.pdf
```

**On macOS, do [the quarantine step](./docs/first-run.md#macos) first** — macOS
refuses unsigned downloads, and that is the first thing that stops people.

**[docs/first-run.md](./docs/first-run.md)** is the same path written out
properly, with a section per platform and one for Docker. It assumes you have
never heard of OpenDrive's API, because you do not need to have.

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

## License

MIT。仓库自研代码采用 MIT 许可,见 [LICENSE](./LICENSE)。

`docs/api-samples/` 是 OpenDrive, Inc. 的官方示例代码,同样为 MIT 许可,版权归
OpenDrive, Inc. 所有。
