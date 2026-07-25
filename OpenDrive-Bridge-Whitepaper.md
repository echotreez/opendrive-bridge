# OpenDrive API Bridge 开发白皮书
**Development Whitepaper for the OpenDrive Cloud Storage API Bridge**

| | |
|---|---|
| 文档版本 | 1.0 |
| 日期 | 2026-07-24 |
| 目标读者 | Claude Opus 5 / 其他代码开发 AI / 项目开发者 |
| 依据资料 | OpenDrive REST API Guide v1.1.7 (10/2023)、官方 PHP/C# 代码样本、官方 API Explorer (https://dev.opendrive.com/api/explorer/) |
| 技术栈 | Go (≥1.22) |
| 交付形态 | 本地 REST 代理服务 (daemon) + CLI 命令行工具 |

---

## 目录

1. [项目概述 Project Overview](#1-项目概述)
2. [官方 API 深度分析 API Analysis](#2-官方-api-深度分析)
3. [系统架构设计 Architecture](#3-系统架构设计)
4. [Bridge 对外接口设计 Bridge API Design](#4-bridge-对外接口设计)
5. [开发流程 Development Workflow](#5-开发流程)
6. [测试策略 Testing Strategy](#6-测试策略)
7. [QA 与代码质量 Quality Assurance](#7-qa-与代码质量)
8. [打包与部署 Packaging & Deployment](#8-打包与部署)
9. [安全性考虑 Security](#9-安全性考虑)
10. [性能优化 Performance](#10-性能优化)
11. [错误处理与日志 Errors & Logging](#11-错误处理与日志)
12. [未来扩展与维护计划 Roadmap & Maintenance](#12-未来扩展与维护计划)
13. [附录 Appendix](#13-附录)

---

## 1. 项目概述

### 1.1 目标 Goal

开发一个名为 **OpenDrive Bridge** 的中间层软件,把 OpenDrive.com 的官方 REST API(下称 *upstream API*)封装成:

1. **`opendrived`** — 常驻本地 REST 代理服务 (daemon)。对外暴露一套**简化、统一、现代化**的 REST 接口,内部处理登录、OAuth2 token 生命周期、分块上传、秒传 (hash dedupe)、重试、并发等所有复杂逻辑。任何本地程序(脚本、财务工具、备份任务)只需调用 `http://127.0.0.1:PORT/v1/...` 即可操作 OpenDrive 云存储。
2. **`odctl`** — CLI 工具。既可直接调用 upstream API(单机模式),也可作为 `opendrived` 的客户端。支持上传/下载/列目录/分享/回收站管理等,可脚本化。

### 1.2 为什么需要 Bridge / Why

Upstream API 存在以下"历史包袱",Bridge 的价值就是把它们全部屏蔽掉:

- 认证方式混杂:session_id 走 URL path 或 JSON body;OAuth2 模式下 `session_id` 必须填魔法值 `"OAUTH"` 且 `access_token` 放在 **query string** 里。
- 上传需要 4 步握手(create_file → open_file_upload → upload_file_chunk2 → close_file_upload),还有条件分支(RequireHashOnly 秒传、RequireCompression 压缩)。
- URL 风格不统一(参数有的在 path、有的在 query、有的在 JSON body;POST/PUT/DELETE 混用同一个 `.json` 资源名)。
- 错误返回格式不完全一致(HTTP code + `error` 对象,OAuth 错误另有格式)。
- 文档 (PDF v1.1.7) 与线上 Swagger 规格存在偏差(详见 §2.6),需要以线上为准做防御性编程。

### 1.3 交付物 Deliverables

| # | 交付物 | 说明 |
|---|--------|------|
| D1 | `opendrived` 二进制 | Linux / macOS / Windows × x86_64 / arm64,共 6 个平台组合 |
| D2 | `odctl` 二进制 | 同上 6 个平台组合 |
| D3 | Docker 镜像 | multi-arch (linux/amd64 + linux/arm64),发布到 registry |
| D4 | Go SDK 包 | `pkg/opendrive` 可被其他 Go 程序 import(REST 代理与 CLI 共用的核心) |
| D5 | 文档 | README、Bridge API 的 OpenAPI 3.1 规格、部署手册、CHANGELOG |
| D6 | 测试与 CI | 单元/集成/契约/E2E 测试 + GitHub Actions 全平台矩阵 |

### 1.4 范围 Scope(第一版)

**包含(核心存储)**:Session、OAuth2、File、Folder、Upload、Download、Sharing、Users(只读 info 部分)。
**不包含(后续版本,见 §12)**:Notes、Tasks/Projects、Branding、Secure Folders、Account Users 管理、User Groups、Stats。

---

## 2. 官方 API 深度分析

### 2.1 基本事实 Basic Facts

- **Base URL**: `https://dev.opendrive.com/api/v1`
- **格式**: 请求 `application/json`(上传 chunk 为 `multipart/form-data`),响应 JSON。
- **HTTP 动词**: GET(读取)、POST(创建)、PUT(替换/更新)、DELETE(删除)。
- **API Partner Roles**: `1 - basic`(管理自己账户)、`2 - manager`(可创建/管理 users 和 account users)。每个端点在文档中标注了所需 role。
- **API Explorer**: 基于 PHP Restler 框架的 Swagger 1.1 explorer。机器可读规格位于:
  - 资源索引: `GET https://dev.opendrive.com/api/v1/resources.json`
  - 各模块规格: `GET https://dev.opendrive.com/api/v1/resources/{module}.json`,module ∈ {branding, download, file, folder, session, tasks, upload, users, oauth2}
  - **注意**: 未携带有效 session 时,explorer 只返回公开端点(如 `session/login`、`upload/checkfileexistsbyname`)。要抓取完整规格,须先登录取得 session_id 再请求。开发时应写一个 `tools/fetch-spec` 脚本登录后拉取全量规格存档,作为契约测试的基准。

### 2.2 认证体系 Authentication

Upstream 提供两套并存的认证:

**(A) Session 模式** — `POST /session/login.json`
```json
{ "username": "...", "passwd": "...", "version": "10", "partner_id": "", "captcha_response": "" }
```
返回 `SessionID` 及用户信息(AccType、UserPlan、FVersioning 等)。此后所有调用携带 `session_id`。相关端点:`/session/exists.json` (POST, 检查有效性)、`/session/info.json` (GET)、`/session/logout.json` (POST)。
线上规格还有 PDF 未记载的 `GET /session/captcharequired.json`(判断当前 IP 是否已被限流、需要先渲染 captcha)——登录失败次数多会触发 captcha,Bridge 必须处理该分支。

**(B) OAuth2 模式(推荐,Bridge 默认)** — 简化版 Resource Owner Password Credentials Flow:
- `POST /oauth2/grant.json` with `{grant_type:"password", client_id:"OpenDrive", username, password}` → 返回 `access_token`(有效期 **86400 秒**)+ `refresh_token`(有效期 **30 天**)。
- 刷新:`POST /oauth2/grant.json` with `{grant_type:"refresh_token", client_id:"OpenDrive", refresh_token}` → 返回新的 access_token **和新的 refresh_token**(refresh token 是滚动更新的,必须持久化最新值,旧值作废)。
- 调用业务 API 时:`session_id` 参数填字符串 `"OAUTH"`,并在 **URL query** 追加 `?access_token=...`(对 GET/POST/PUT/DELETE 全部适用)。
- token 过期时返回 HTTP 401,body 为 `{"error":{"code":401,"error":"invalid_token","error_description":"The access token provided has expired"}}`;refresh token 失效返回 `{"error":"invalid_grant", ...}`(注意两种错误格式不同)。
- 官方要求:使用 OAuth2 后**不得在客户端存储用户密码**。

**Bridge 策略**:默认 OAuth2;首次登录用密码换 token 后立即丢弃密码,token 存 OS keyring(§9.2)。access_token 过期前 5 分钟主动刷新;收到 401 invalid_token 时被动刷新并重放请求(最多 1 次);refresh 失败则要求用户重新登录。Session 模式作为 fallback 保留(某些端点如 Secure Folders 可能只认 session)。

### 2.3 核心模块与端点清单 Endpoint Inventory(第一版范围)

**Session / OAuth2**(见 §2.2)

**Folder 模块**(`/folder...`,PDF §5,20 个端点,核心子集):

| 操作 | 端点 | 方法 | 备注 |
|---|---|---|---|
| 创建文件夹 | `/folder.json` | POST | `folder_name`(≤255,禁止 `\ / : * ? " < > \|`)、`folder_sub_parent`(0=root)、`folder_is_public`(0 私有/1 公开/2 隐藏)及 public_upl/display/dnl 开关 |
| 列目录内容 | `/folder/list.json/{session_id}/{folder_id}` | GET | 返回 Folders[] + Files[],是同步/浏览的主力端点。**分页协议**:带 `offset` 参数时每次最多返回 100 条,必须配合 `last_request_time`(首次传 0,后续传上次响应的 `DirUpdateTime`);另支持 `search_query`、`only_subfolders`、`with_breadcrumbs` |
| 文件夹信息 | `/folder/info.json` | GET | |
| 按路径取 ID | `/folder/idbypath.json` | POST | Bridge 的路径→ID 解析靠它 + 缓存 |
| 按名取条目 | `/folder/itembyname.json` | GET | |
| 面包屑 | `/folder/breadcrump.json` | GET | 官方拼写就是 "breadcrump" |
| 改名 | `/folder/rename.json` | POST | |
| 复制/移动 | `/folder/move_copy.json` | POST | |
| 回收站 | `/folder/trash.json` (POST 丢入 / DELETE 清空)、`/folder/trashlist.json`、`/folder/restore.json`、`/folder/remove.json`(彻底删除) | | |
| 访问权限 | `/folder/setaccess.json` | POST | |
| 设置 | `/folder/foldersettings.json` | POST | |
| 过期链接 | `/folder/expiringlink.json/...`、`/folder/folderexpiringlinks.json/...` | GET | 限时/限次分享链接 |
| 邮件分享 | `/folder/sendbyemail.json` | POST | |
| 用户访问模式 | `/folder/useraccessmode.json` | GET | |

**File 模块**(`/file...`,PDF §4,18 个端点,核心子集):

| 操作 | 端点 | 方法 |
|---|---|---|
| 文件信息 | `/file/info.json` | GET |
| 按路径取 ID | `/file/idbypath.json` | POST |
| 文件路径 | `/file/path.json` | GET |
| 改名 | `/file/rename.json` | POST |
| 复制/移动 | `/file/move_copy.json` | POST |
| 回收站 | `/file/trash.json` (POST)、`/file/restore.json` (POST)、`/file/file.json` (DELETE, 从 trash 彻底删除) | |
| 版本 | `/file/fileversions.json`、`/file/removefileversion.json` | GET / POST |
| 缩略图 | `/file/thumb.json` | GET |
| 访问权限 | `/file/access.json` | PUT |
| 文件设置 | `/file/filesettings.json` | POST(可设密码保护等) |
| 密码校验 | `/file/verifypassword.json` | POST |
| 过期链接 | `/file/expiringlink.json/...`、`/file/fileexpiringlinks.json/...` | GET |
| 建空文件 | `/file/file.json` | POST |
| 邮件分享 | `/file/sendbyemail.json` | POST |

**Upload 模块**(`/upload...`,PDF §12)— 见 §2.4 上传管线。另有 `checkfileexistsbyname.json`(POST,批量查文件名是否存在)与 resumable.js 配套的 resumable 端点(样本 `test_upload_resumable.php`)。

**Download 模块**(PDF §3):
- `GET /download/file.json/{file_id}` — 参数:`session_id`、`offset`(字节偏移,**支持断点续传**)、`inline`、`sharing_id`、`test`(仅探测可下载性)、`backup`、`temp_key`(密码保护文件的临时 key)。
- `POST /download/all.json` — 多文件/文件夹打包 zip 下载(`files`、`folders` 逗号分隔 ID;注意此端点的会话参数名是 `session_key`,又一个不一致点)。

**Sharing 模块**(PDF §9):`listsharedfolders.json`、`listsharedusers.json`、`listusers.json`、`sharing.json`(POST 分享 / DELETE 取消)、`setmode.json`。

**Users 模块**(PDF §13,第一版只做只读):`GET /users/info.json`(OAuth 示例即用此端点)。修改类端点(email/password/username/info PUT)列入后续版本。

### 2.4 上传管线 Upload Pipeline(重点!)

官方规定的完整流程,Bridge 必须严格按序实现:

```
┌─────────────────────────────────────────────────────────────┐
│ 0. (可选) POST /upload/checkfileexistsbyname.json/{folder_id}│
│    批量检查目标文件夹内是否有同名文件                          │
├─────────────────────────────────────────────────────────────┤
│ 1. POST /upload/create_file.json                             │
│    {session_id, folder_id, file_name,                        │
│     file_size?, file_hash?(MD5), open_if_exists?}            │
│    返回: FileId, TempLocation,                               │
│          RequireCompression, RequireHash, RequireHashOnly    │
│    ★ 若带 file_hash+file_size 且服务器已有相同内容:          │
│      RequireHashOnly=1 → 跳过 2、3,直接 close(秒传)         │
│    ★ open_if_exists=1: 同名文件存在时返回其信息(覆盖上传);   │
│      =0: 同名返回 409                                        │
├─────────────────────────────────────────────────────────────┤
│ 2. POST /upload/open_file_upload.json                        │
│    {session_id, file_id, file_size, file_hash?}              │
│    返回: TempLocation, RequireCompression(=需 Zlib level 6   │
│    压缩), RequireHash, SpeedLimit                            │
├─────────────────────────────────────────────────────────────┤
│ 3. 循环: POST /upload/upload_file_chunk2.json/{session}/{id} │
│    multipart/form-data, 二进制在 file_data 字段,             │
│    其余参数(temp_location, chunk_offset, chunk_size)在 query │
│    官方样本 chunk 大小: 50 MB;返回 TotalWritten 用于校验     │
│    ★ 官方明确: 用 v2 (upload_file_chunk2),v1 在限速场景不稳  │
├─────────────────────────────────────────────────────────────┤
│ 4. POST /upload/close_file_upload.json                       │
│    {session_id, file_id, file_size, temp_location,           │
│     file_time?, file_hash?(若 RequireHash=1 必须算 MD5),      │
│     file_compressed?, access_folder_id?, sharing_id?}        │
│    返回完整文件元数据(DownloadLink, StreamingLink, ...)       │
└─────────────────────────────────────────────────────────────┘
```

实现要点:
- **秒传 (dedupe)**:上传前流式计算 MD5(与读文件一次完成),优先带 hash 调 create_file,命中 `RequireHashOnly=1` 时零流量完成。
- **压缩**:若 `RequireCompression=1`,chunk 数据须经 Zlib level 6 压缩后上传,close 时置 `file_compressed=1`。
- **断点续传**:TempLocation + chunk_offset 天然支持;Bridge 在本地持久化未完成上传的 (file_id, temp_location, offset) 状态,崩溃后可续传。
- **校验**:每个 chunk 上传后核对 `TotalWritten` 与本地已发送字节数;不一致即从正确 offset 重传。
- **file_time**:close 时传本地文件 mtime(Unix 时间戳),保持时间戳保真。

### 2.5 下载管线 Download Pipeline

- 单文件:`GET /download/file.json/{file_id}?session_id=...&offset=N` → 流式写盘;HTTP 断连后用 `offset` 续传;先用 `test=1` 探测权限/带宽再开始大文件下载。
- 打包:`POST /download/all.json` 拿 zip 流。
- 带宽护栏:文件元数据中 `BWExceeded=1` 表示该文件下载已超带宽限额,应直接报错而不是盲目重试;`download_speed_limit`/`upload_speed_limit` 字段提示服务端限速,客户端重试策略要考虑。

### 2.6 文档与线上规格的已知偏差 & 陷阱 Known Discrepancies & Gotchas

开发 AI 必须注意以下坑(均来自本次对 PDF、样本代码和线上 Swagger 的交叉比对):

1. **以线上 Swagger 为准**:PDF 自称 v1.1.7 (10/2023),但内页版权栏仍写 v1.1.6 (03/2017);线上已出现 PDF 没有的端点(如 `session/captcharequired.json`)。开发前先跑 `tools/fetch-spec` 拉全量线上规格。
2. **参数命名不一致**:`download/all.json` 用 `session_key` 而非 `session_id`;`checkfileexistsbyname` 的文件名参数叫 `name` 且是数组。
3. **拼写即接口**:`breadcrump.json`(官方拼错但必须照用)、PDF 中大量 typo(arrey、bollean、charing 等)只是文档问题,不影响 wire format,但字段名(如 `OwnerSuspendet`)**必须照原样映射**。
4. **同一资源名多动词**:`/file/file.json` POST=建空文件、DELETE=从 trash 删除;`/folder/trash.json` POST=丢回收站、DELETE=清空回收站。路由注册时严禁想当然。
5. **布尔值多为 0/1 整数**,偶有 "True"/"False" 字符串返回(如 branding 的 check 端点);JSON 解码层必须做宽松类型处理(自定义 `FlexBool`/`FlexInt` 类型)。
6. **时间戳混用**:多数为 Unix 秒(integer),个别字段是字符串;统一在 SDK 层归一化为 Go `time.Time`。
7. **成功响应不统一**:有的返回完整对象,有的只返回 `true` / `{"result": true}`。
8. **OAuth 的 access_token 在 query string**(有泄漏到日志的风险,见 §9.4)。
9. **root folder 一律用 `"0"`**(字符串)。
10. **文件/文件夹名非法字符**:`\ / : * ? " < > |`,max 255;Bridge 在本地先验证,给出比 upstream 更友好的报错。
11. **Captcha 限流**:连续登录失败会要求 captcha(`captcha_response` 参数)。Bridge 无法代答 captcha,应把该错误清晰地透传给用户并提示去网页端解锁。
12. **PDF 中 expiringlink 的 URL 写作 `/v1/file/...`**(带了 /v1 前缀)而其他端点不带——实际 base URL 已含 /v1,拼接时注意去重。
13. **布尔参数偶尔是字符串**:如 `file/move_copy.json` 的 `move`、`overwrite_if_exists` 要求传 `"true"/"false"` 字符串而非 JSON 布尔;逐端点核对线上规格。
14. **list.json 分页**:`offset` 模式下单次上限 100 条,且必须与 `last_request_time`/`DirUpdateTime` 配套使用(见 §2.3 表格),否则大目录会拿到不完整列表。

---

## 3. 系统架构设计

### 3.1 总体架构 Overall Architecture

```
                    ┌──────────────────────────────────────────┐
                    │              opendrived (daemon)          │
 本地客户端 ──HTTP──▶│  ┌────────────┐  ┌─────────────────────┐ │
 (脚本/应用/odctl)   │  │ Bridge API │  │  Service Layer       │ │
                    │  │ (chi router│──▶│  files / folders /   │ │
 odctl ──单机模式────┼─▶│  + OpenAPI)│  │  transfers / shares  │ │
   │                │  └────────────┘  └──────────┬──────────┘ │
   │                │  ┌────────────┐             │            │
   │                │  │ Job Engine │◀────────────┤            │
   │                │  │ (上传/下载  │             ▼            │
   │                │  │  队列+断点) │  ┌─────────────────────┐ │
   │                │  └────────────┘  │  pkg/opendrive (SDK) │ │
   │                │                  │  ├ client (HTTP core)│ │
   └────────────────┼─────────────────▶│  ├ auth (OAuth2/sess)│ │
                    │                  │  ├ upload pipeline   │ │
                    │                  │  ├ download pipeline │ │
                    │                  │  └ typed models      │ │
                    │                  └──────────┬──────────┘ │
                    └─────────────────────────────┼────────────┘
                                                  ▼
                                 https://dev.opendrive.com/api/v1
```

### 3.2 仓库结构 Repository Layout

```
opendrive-bridge/
├── cmd/
│   ├── opendrived/main.go        # daemon 入口
│   └── odctl/main.go             # CLI 入口 (spf13/cobra)
├── pkg/opendrive/                # 可复用 Go SDK(公开 API)
│   ├── client.go                 # HTTP core: base URL、重试、限速、UA
│   ├── auth.go                   # OAuth2 grant/refresh、session login、token store 接口
│   ├── session.go  folder.go  file.go  sharing.go  users.go
│   ├── upload.go                 # 4 步上传管线 + 秒传 + 压缩 + 断点
│   ├── download.go               # 流式下载 + offset 续传
│   ├── types.go                  # 全部 wire model(FlexBool/FlexInt/UnixTime)
│   └── errors.go                 # 统一错误类型 (APIError{Code, Message, Kind})
├── internal/
│   ├── server/                   # Bridge REST API (handlers, middleware, OpenAPI)
│   ├── jobs/                     # 传输任务引擎(队列、并发、持久化状态、进度)
│   ├── keystore/                 # OS keyring 封装 (99designs/keyring)
│   ├── cache/                    # 路径→ID 解析缓存 (TTL + DirUpdateTime 失效)
│   └── config/                   # 配置加载 (file + env + flags)
├── tools/fetch-spec/             # 登录后拉取线上 Swagger 全量规格存档
├── testdata/                     # 录制的真实响应 fixture(脱敏)
├── deploy/
│   ├── docker/Dockerfile         # multi-stage, distroless/static
│   ├── systemd/opendrived.service
│   ├── launchd/com.opendrive.bridge.plist
│   └── windows/                  # Windows Service 安装脚本 (kardianos/service)
├── .github/workflows/            # ci.yml, release.yml
├── .goreleaser.yaml
└── docs/                         # bridge-openapi.yaml, 部署手册
```

### 3.3 关键依赖 Key Dependencies(保持最少)

标准库优先。允许引入:`spf13/cobra`(CLI)、`go-chi/chi`(路由)、`99designs/keyring`(凭证存储)、`kardianos/service`(跨平台服务注册)、`golang.org/x/time/rate`(限速)、`golang.org/x/sync/errgroup`(并发)。禁止引入大型框架。压缩用标准库 `compress/zlib`,MD5 用 `crypto/md5`(仅因 upstream 协议要求,非安全用途)。

### 3.4 配置 Configuration

优先级:CLI flags > 环境变量 (`ODB_*`) > 配置文件。配置文件位置:
- Linux: `$XDG_CONFIG_HOME/opendrive-bridge/config.yaml`(默认 `~/.config/...`)
- macOS: `~/Library/Application Support/opendrive-bridge/config.yaml`
- Windows: `%APPDATA%\opendrive-bridge\config.yaml`

```yaml
listen: 127.0.0.1:9750        # 默认只绑 loopback
auth_mode: oauth2             # oauth2 | session
upstream_base: https://dev.opendrive.com/api/v1
transfers:
  chunk_size_mb: 50           # 官方样本值;可调 8–100
  parallel_chunks: 3          # 单文件并发 chunk 数(注意服务端顺序要求,见 §10.2)
  max_concurrent_jobs: 4
  retry_max: 5
log:
  level: info
  redact_tokens: true         # 强制脱敏,不可关闭 access_token 的脱敏
```

---

## 4. Bridge 对外接口设计

Bridge API 是**面向使用者的简化层**,统一 JSON、统一错误、路径式寻址。完整规格随代码生成 OpenAPI 3.1 文档(`docs/bridge-openapi.yaml`),以下为骨架:

### 4.1 认证与账户

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/auth/login` | POST | `{username, password}` → 换取 upstream token 并存 keyring;密码不落盘 |
| `/v1/auth/logout` | POST | 撤销并清除本地 token |
| `/v1/auth/status` | GET | 登录状态、token 到期时间、账户配额 (StorageUsed/Max, BwUsed/Max) |

### 4.2 文件与文件夹(路径式,自动解析为 upstream ID)

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/ls?path=/Docs/2026` | GET | 列目录(合并 upstream Folders[]+Files[],统一分页) |
| `/v1/stat?path=...` | GET | 文件/文件夹元数据 |
| `/v1/mkdir` | POST | `{path, public?: "private"\|"public"\|"hidden"}`,递归创建 |
| `/v1/mv` `/v1/cp` | POST | `{src, dst}` |
| `/v1/rename` | POST | `{path, new_name}`(本地先校验非法字符) |
| `/v1/rm` | POST | `{path, permanent?: bool}` 默认进回收站 |
| `/v1/trash` | GET / POST `/restore` / POST `/empty` | 回收站管理 |
| `/v1/versions?path=...` | GET | 文件版本列表 |

### 4.3 传输(Job 模型,异步 + 进度)

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/upload` | POST | `{local_path, remote_path, overwrite?}` → 返回 `job_id`;大文件走分块管线,自动秒传/压缩/断点 |
| `/v1/upload/stream?path=...` | PUT | 小文件同步直传(请求体即文件内容,适合脚本) |
| `/v1/download` | POST | `{remote_path, local_path}` → `job_id`,offset 断点续传 |
| `/v1/download/stream?path=...` | GET | 同步流式下载(响应体即文件内容) |
| `/v1/jobs` `/v1/jobs/{id}` | GET | 任务列表/详情:state, bytes_done/total, speed, error |
| `/v1/jobs/{id}` | DELETE | 取消任务(保留断点状态可恢复) |

### 4.4 分享

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/share/link` | POST | `{path, expires_at?, max_uses?}` → 包装 upstream expiringlink |
| `/v1/share/list` | GET | 已分享内容 |
| `/v1/share` | DELETE | 取消分享 |

### 4.5 统一错误格式

```json
{ "error": { "code": "not_found", "http": 404,
             "message": "remote path /Docs/x.pdf does not exist",
             "upstream": { "code": 404, "message": "File not exists" } } }
```
`code` 为稳定机器可读枚举:`unauthorized / token_expired / captcha_required / not_found / conflict / quota_exceeded / bandwidth_exceeded / invalid_name / upstream_error / rate_limited`。

### 4.6 odctl 命令面

```
odctl login | logout | status
odctl ls <path>        odctl mkdir <path>
odctl up <local> <remote> [--overwrite]   # 带进度条
odctl down <remote> <local>
odctl mv|cp|rm|rename ...
odctl trash [restore|empty]
odctl share <path> [--expires 7d] [--max-uses 10]
odctl jobs [watch]
odctl daemon install|start|stop|uninstall  # 注册系统服务
```
`odctl` 默认连接本地 daemon;`--direct` 时用内嵌 SDK 直连 upstream(无 daemon 场景)。

---

## 5. 开发流程

采用分阶段 (phase-gated) 开发,每个 phase 有明确出口标准 (exit criteria),全部通过 CI 才进入下一阶段。

| Phase | 内容 | 出口标准 |
|---|---|---|
| **P0 基建** (1) | 仓库脚手架、CI 骨架、`tools/fetch-spec` 拉取并存档线上规格、fixture 录制方案 | CI 绿;规格存档入库 |
| **P1 SDK 核心** (2–3) | `pkg/opendrive`: client core、auth(OAuth2+session)、types(FlexBool 等)、errors | 单测覆盖 ≥85%;mock server 下全部 auth 流转(过期/刷新/重放)通过 |
| **P2 存储操作** (2) | folder/file/sharing/users 全部端点绑定 | 契约测试对照存档规格通过;沙盒账户冒烟通过 |
| **P3 传输管线** (3) | 4 步上传(秒传/压缩/断点)、下载续传、job engine | 大文件 (>1GB)、断网恢复、崩溃续传场景测试通过 |
| **P4 Bridge API + CLI** (2–3) | `internal/server`、`odctl`、OpenAPI 文档 | E2E 测试通过;OpenAPI lint 通过 |
| **P5 打包发布** (1–2) | goreleaser 6 平台矩阵、Docker multi-arch、服务安装脚本 | 6 平台二进制 + 2 架构镜像在 CI 全部构建并冒烟 |
| **P6 硬化** (1–2) | 安全审计、fuzz、性能基准、文档收尾 | §7 QA 清单全项通过 |

(括号内为估计人周;AI 辅助开发可显著压缩,但**出口标准不得压缩**。)

开发纪律:
- Trunk-based + 短分支 PR;每个 PR 必须含测试;`main` 始终可发布。
- Conventional Commits(`feat:` `fix:` `test:` ...),据此自动生成 CHANGELOG。
- 每个 upstream 端点的绑定代码须在注释中标注 PDF 章节号与线上规格字段来源,便于日后比对 API 漂移。

---

## 6. 测试策略

### 6.1 测试金字塔

| 层级 | 工具/方式 | 覆盖目标 |
|---|---|---|
| 单元测试 | `go test` + `net/http/httptest` mock upstream | SDK 每个端点的请求构造、响应解析、错误映射;FlexBool/UnixTime 等类型的全部边界;token 刷新状态机 |
| 契约测试 | 对照 `tools/fetch-spec` 存档的 Swagger 规格自动校验请求路径/方法/参数 | 防止实现偏离规格;规格更新时 CI 自动报警(API 漂移检测) |
| 集成测试 | 真实 OpenDrive 沙盒账户(专用测试账号,凭证走 CI secrets),`-tags=integration` 手动/夜间触发 | login→mkdir→upload(小/大/秒传)→list→download→verify MD5→share→trash→restore→purge 全链路 |
| E2E 测试 | 启动真实 `opendrived` + `odctl` 子进程,黑盒走 Bridge API | §4 全部端点;进度、取消、断点恢复 |
| 故障注入 | mock server 注入 401/429/500、半途断连、慢响应、畸形 JSON | 重试/退避/续传逻辑;确保不重复上传 chunk、不丢数据 |
| Fuzz | Go native fuzzing 打 JSON 解码层与路径解析层 | 不 panic、不越界 |

### 6.2 集成测试注意事项

- 测试账号数据用随机前缀命名 (`odb-test-{run_id}-...`),测试结束强制清理(含清空 trash),失败也要清理(defer)。
- 尊重 upstream 限流:集成测试串行 + 全局速率限制;登录复用 token,避免触发 captcha 限流(一旦触发,CI 标记 warning 而非 fail,并提示人工到网页端解锁)。
- 大文件用例 ≥ 2×chunk_size + 1 字节(保证跨 chunk 边界),校验上传后 `FileHash` 与本地 MD5 一致。

### 6.3 覆盖率与验收指标

`pkg/opendrive` 语句覆盖 ≥ 85%,`internal/*` ≥ 75%;上传/下载管线的分支覆盖(秒传、压缩、断点、重试)必须 100% 显式用例化。CI 阻断低于阈值的 PR。

---

## 7. QA 与代码质量

**静态与规范**:`gofmt`/`goimports` 强制;`golangci-lint`(errcheck, staticcheck, gosec, revive, gocritic);`go vet`。
**并发安全**:全部测试跑 `-race`;job engine 做专门的并发压力用例。
**安全扫描**:`govulncheck`(依赖漏洞)、`gosec`(代码模式)、`gitleaks`(防 secrets 入库)、Docker 镜像 `trivy` 扫描。CI 每日定时跑一次,不只在 PR 时。
**依赖治理**:`go.mod` 最小化;Dependabot/Renovate 自动升级 PR + CI 验证。
**Code Review 清单**(AI 或人工 review 都照此执行):错误是否都被处理且携带上下文;token/密码是否可能进入日志/错误信息;文件句柄/response body 是否关闭;context 是否贯穿并支持取消;chunk 边界与 offset 计算是否有 off-by-one;Windows 路径分隔符与文件名大小写处理。
**发布前 QA 门禁**:6 平台二进制冒烟(`odctl version` + mock server 走一次 login/ls);Docker 镜像双架构冒烟;OpenAPI 文档与实现一致性检查;CHANGELOG 完整;§9 安全清单逐项签收。

---

## 8. 打包与部署

### 8.1 构建矩阵 Build Matrix

Go 交叉编译,`CGO_ENABLED=0`(keyring 在部分平台需 cgo 时,fallback 到加密文件存储,见 §9.2):

| OS | Arch | 产物 |
|---|---|---|
| linux | amd64 | `opendrive-bridge_{ver}_linux_amd64.tar.gz` |
| linux | arm64 | `..._linux_arm64.tar.gz` |
| darwin (macOS) | amd64 | `..._darwin_amd64.tar.gz` |
| darwin (macOS) | arm64 (Apple Silicon) | `..._darwin_arm64.tar.gz` |
| windows | amd64 | `..._windows_amd64.zip` |
| windows | arm64 | `..._windows_arm64.zip` |

每个包内含 `opendrived`、`odctl`、LICENSE、README、服务安装文件。用 **goreleaser** 一条命令产出全矩阵 + checksums (SHA256) + 签名;版本号注入 `main.version`(SemVer,`git tag` 驱动)。macOS 产物做 codesign + notarization(无开发者证书时文档说明 `xattr -d com.apple.quarantine` 方案);Windows 产物条件允许时做 Authenticode 签名。

### 8.2 主机部署 Host Deployment

- **Linux**: `deploy/systemd/opendrived.service`(`DynamicUser=yes`、`ProtectSystem=strict`、`NoNewPrivileges=yes` 等硬化指令);`odctl daemon install` 自动写入。
- **macOS**: launchd plist,`odctl daemon install` 装载到 `~/Library/LaunchAgents`。
- **Windows**: 经 `kardianos/service` 注册 Windows Service,`odctl daemon install` 一键完成。
- 三平台统一由 `odctl daemon start|stop|status` 管理,屏蔽差异。

### 8.3 Docker 部署

`deploy/docker/Dockerfile`:multi-stage,builder 用官方 golang 镜像,runtime 用 `gcr.io/distroless/static`(或 `scratch` + CA 证书),非 root 用户运行,最终镜像 < 25 MB。

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/opendrive-bridge:1.0.0 --push .
docker run -d -p 127.0.0.1:9750:9750 \
  -v odb-state:/data -e ODB_LISTEN=0.0.0.0:9750 \
  <registry>/opendrive-bridge:1.0.0
```

镜像内 keyring 不可用 → 自动切换加密文件 token store(密钥来自 `ODB_STATE_KEY` env 或挂载的 secret)。提供 `docker-compose.yaml` 样例与 healthcheck(`GET /v1/auth/status`)。

### 8.4 发布流程 Release Flow

`git tag vX.Y.Z` → GitHub Actions release workflow:全平台构建 → 全部测试与扫描 → goreleaser 出包 + checksums → Docker buildx 双架构推送 → 生成 Release Notes(自 Conventional Commits)→ 附上产物。任何一步失败即整体失败,不允许部分发布。

---

## 9. 安全性考虑

### 9.1 威胁模型概览

Bridge 持有能完全控制用户云盘的 token,运行在用户主机上。主要威胁:token 泄漏(日志/磁盘/进程列表)、本机其他进程滥用 Bridge API、中间人攻击、恶意文件名/路径注入、供应链攻击。

### 9.2 凭证与 Token 管理

- 密码只存在于登录请求的内存中,用后即弃,**永不落盘、永不入日志**(官方 OAuth2 条款也如此要求)。
- Token 首选 OS keyring(macOS Keychain / Windows Credential Manager / Linux Secret Service);不可用时(Docker、headless Linux)fallback 到 AES-256-GCM 加密文件,密钥由环境变量或首次启动生成的机器密钥提供,文件权限 0600。
- refresh_token 滚动更新必须原子持久化(先写新值成功再作废旧值的本地副本),防止刷新到一半崩溃导致永久掉登录。

### 9.3 Bridge API 自身防护

- 默认只监听 `127.0.0.1`;监听非 loopback 地址时**强制**要求配置 Bridge API key(`Authorization: Bearer`),并在文档中要求配合 TLS 反代。
- 所有输入严格校验:路径规范化后必须仍在预期命名空间内(防 `../` 遍历,本地下载落盘路径同样校验);文件名按 upstream 规则白名单校验。
- CORS 默认关闭;无 cookie、无隐式凭证。
- 对 upstream 只走 HTTPS,启用证书校验(禁止 InsecureSkipVerify),可选 pin OpenDrive 证书链。

### 9.4 Token-in-query 风险缓解

Upstream 要求 `access_token` 放 URL query,这会出现在各种日志里。缓解:HTTP client 层统一在日志/错误信息中对 `access_token`、`session_id`、`passwd` 做正则脱敏(`redact_tokens` 不可关闭);错误对象保存的 URL 一律为脱敏版;禁止把完整 request URL 传给任何第三方(含 crash report)。

### 9.5 供应链与构建安全

依赖锁定 (`go.sum`) + `govulncheck` 日扫;CI 产物带 SHA256 checksums 与(可行时)sigstore/cosign 签名;Docker 基础镜像用 digest 固定;GitHub Actions 用 pinned SHA 引用第三方 action;secrets 仅存 CI secret store,gitleaks 阻断误提交。

---

## 10. 性能优化

### 10.1 传输性能

- **流式处理**:上传读文件与 MD5 计算单遍完成 (`io.TeeReader`);下载直接流式写盘;内存占用与文件大小无关,峰值 ≈ chunk_size × 并发数。
- **秒传优先**:凡本地能取到 size+MD5 一律先试 `RequireHashOnly` 路径,零流量命中。
- **chunk 大小自适应**:默认 50 MB(官方样本值);可配置;弱网环境自动降到 8–16 MB 减少重传代价。
- **重试与退避**:指数退避 + jitter(1s→2s→4s...,cap 60s,默认 5 次);仅对幂等操作与可续传的 chunk 重试;429/`SpeedLimit` 信号触发全局限速器 (`x/time/rate`)。

### 10.2 并发模型

- Job engine:全局 worker pool(默认 4 job 并发),单文件 chunk 默认**串行**(upstream 的 TempLocation+offset 协议未承诺乱序写安全;P3 阶段实测验证后才允许开启 `parallel_chunks>1`,并作为实验特性)。多文件并发是主要吞吐来源。
- HTTP 连接复用:单一 `http.Transport`,合理的 `MaxIdleConnsPerHost`,开启 HTTP/2(若 upstream 支持)。

### 10.3 元数据性能

- 路径→ID 解析缓存:以 `DirUpdateTime` 做失效依据,TTL 兜底(默认 60s);写操作(mv/rm/mkdir)后主动失效相关子树。
- 目录列表缓存同上;`/v1/ls` 支持 `?fresh=1` 强制绕过。
- 基准测试:`go test -bench` 覆盖 JSON 解码热点与路径解析;集成环境记录 1GB 上传/下载耗时基线,回归超 15% 即 CI 报警。

---

## 11. 错误处理与日志

- SDK 层把 upstream 的三种错误形态(HTTP code + `{"error":{code,message}}`、OAuth `{"error","error_description"}`、纯文本/布尔异常体)归一为 `APIError{Kind, HTTPCode, UpstreamMsg}`;`errors.Is/As` 友好。
- 可重试性显式建模:`Temporary()` / `RetryAfter()`;401 invalid_token → auth manager 自动刷新重放(仅一次);captcha_required、quota、bandwidth 类错误绝不自动重试。
- 结构化日志 (`log/slog`,JSON 输出可选):每请求带 request_id、耗时、upstream 端点、脱敏 URL;`debug` 级别才输出 body 摘要(仍脱敏)。
- 崩溃安全:job 状态(含上传断点)以事务方式写本地状态文件 (SQLite 或 JSON WAL);启动时恢复未完成任务为 `paused`,由用户或配置决定是否自动续传。

---

## 12. 未来扩展与维护计划

### 12.1 功能路线图 Roadmap

| 版本 | 内容 |
|---|---|
| v1.0 | 本白皮书第一版范围(核心存储 + daemon + CLI + 6 平台 + Docker) |
| v1.1 | Users 写操作、Account Users 管理、User Groups(role=2 管理场景) |
| v1.2 | Secure Folders(两阶段授权)、文件/文件夹密码保护全流程、Stats 带宽/用量报表 |
| v1.3 | Notes 与 Tasks/Projects 模块(若有实际需求) |
| v2.0 候选 | **WebDAV / FUSE 挂载前端**(把 OpenDrive 挂成本地盘)、**rclone backend 贡献**、S3 兼容网关、双向同步引擎(基于 DirUpdateTime 增量) |

### 12.2 API 漂移监控 Upstream Drift Watch

每周定时 CI job 重新运行 `tools/fetch-spec` 并 diff 存档规格;发现新增/变更端点自动开 issue。PDF 文档更新(官方 support@opendrive.com 渠道)纳入季度检查。契约测试失败视为 P1 事件。

### 12.3 维护制度

- **版本策略**:SemVer;upstream 不兼容变更 → minor + 迁移说明;Bridge API 破坏性变更只在 major。
- **支持窗口**:最新 minor 全量支持,前一 minor 仅安全修复。
- **例行任务**:每月依赖升级 PR;每日 `govulncheck`;每季度全平台手动冒烟(尤其 macOS notarization 与 Windows 签名有效期);Go 版本跟随官方支持窗口(N 与 N-1)。
- **文档维护**:OpenAPI 规格由代码生成,防止文档漂移;README 快速上手 5 分钟内可完成 login→upload。

---

## 13. 附录

### A. 官方资料清单

- `Doc/OpenDrive_API_guide.pdf` — REST API Guide v1.1.7 (10/2023),15 个模块的权威参数说明。
- `Doc/api-samples/api-samples/*.php` — 官方 PHP 样本(2016,PHP 5.3+ / cURL):session_login、folder/file CRUD、chunked upload(50MB chunk)、resumable.js 上传、账户与权限管理等 30+ 文件。
- `Doc/api-samples/C# Upload Sample.txt` — 伪代码级 C# 上传样本,演示 open_if_exists 与 RequireHashOnly 秒传分支。
- 线上 Swagger 1.1 规格:`https://dev.opendrive.com/api/v1/resources.json` 及 `/resources/{module}.json`(登录后可见全量)。
- API Explorer(交互测试):`https://dev.opendrive.com/api/explorer/`。

### B. 典型端到端流程(伪代码)

```
# 首次配置
odctl login                     # → oauth2/grant.json (password) → keyring
# 上传(带秒传)
odctl up ./report.xlsx /Finance/2026/
  → md5(report.xlsx) 流式计算
  → create_file.json {file_size, file_hash, open_if_exists:1}
  → RequireHashOnly=1 ? close_file_upload : (open → chunk2 循环 → close)
  → 校验返回 FileHash == 本地 MD5
# 分享 7 天限 10 次
odctl share /Finance/2026/report.xlsx --expires 7d --max-uses 10
  → file/expiringlink.json → 返回 DownloadLink
```

### C. 术语表 Glossary

| 术语 | 含义 |
|---|---|
| upstream | OpenDrive 官方 REST API (dev.opendrive.com/api/v1) |
| 秒传 / dedupe | create_file 携带 MD5+size 命中服务器已有内容,免传输 (RequireHashOnly=1) |
| TempLocation | upstream 分块上传的服务端临时位置句柄 |
| session 模式 / OAuth 模式 | 两套认证;OAuth 模式下 session_id 固定为 "OAUTH" |
| Job | Bridge 内的异步传输任务(可查询进度、取消、断点恢复) |
| 契约测试 | 实现与存档的线上 Swagger 规格之间的一致性自动校验 |

### D. 给开发 AI 的执行须知 Instructions to the Implementing AI

1. 严格按 §5 的 phase 顺序开发,每个 phase 出口标准是硬门禁。
2. 一切 upstream 行为以 `tools/fetch-spec` 拉取的线上规格 + 真实沙盒响应为准;PDF 仅作参数语义参考。遇到二者矛盾,在代码注释与 `docs/discrepancies.md` 中记录。
3. 不得跳过 §2.6 列出的任何一个陷阱;每个陷阱至少对应一个测试用例。
4. 安全条款(§9)与测试覆盖率(§6.3)不可协商;性能优化(§10)在正确性之后。
5. 所有公开函数写 godoc;错误信息面向使用者,不泄漏内部路径与 token。

---

*本白皮书基于 OpenDrive REST API Guide v1.1.7、官方代码样本及 2026-07-24 抓取的线上 API Explorer 规格编写。*

