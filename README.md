# OpenDrive Bridge

A local REST proxy (`opendrived`) and CLI (`odctl`) for the [OpenDrive.com](https://www.opendrive.com) cloud storage REST API, written in Go.

把 OpenDrive 官方 REST API 封装为本地代理服务与命令行工具:内部处理 OAuth2 token 生命周期、四步分块上传、MD5 秒传、断点续传与重试,对外提供统一简化的现代 REST 接口。

## 状态 Status

**Phase P0 — 脚手架阶段。尚未实现功能。**

开发的**唯一权威设计文档**是 [`OpenDrive-Bridge-Whitepaper.md`](./OpenDrive-Bridge-Whitepaper.md),包含:

- 官方 API 深度分析(15 模块、认证体系、上传/下载管线、14 条已知陷阱)
- 系统架构与仓库结构(§3)
- Bridge 对外 REST 接口设计(§4)
- 分阶段开发流程 P0–P6 与出口标准(§5)
- 测试、QA、安全、性能、打包(6 平台 + Docker 双架构)要求(§6–§10)

**开发 AI 请先完整阅读白皮书,并遵守其附录 D「执行须知」。**

## 仓库结构

```
cmd/opendrived/   守护进程入口          pkg/opendrive/   Go SDK(核心)
cmd/odctl/        CLI 入口              internal/        server / jobs / keystore / cache / config
tools/fetch-spec/ 线上 Swagger 规格抓取  deploy/          docker / systemd / launchd / windows
Doc/              官方 API 指引 PDF 与官方 PHP/C# 样本(参考资料,勿修改)
docs/             Bridge OpenAPI 规格与部署手册(待生成)
```

## 参考资料 References

- `Doc/OpenDrive_API_guide.pdf` — 官方 REST API Guide v1.1.7
- `Doc/api-samples/` — 官方示例代码(MIT,© OpenDrive Inc.)
- 线上 API Explorer: https://dev.opendrive.com/api/explorer/
- 线上机器可读规格: `https://dev.opendrive.com/api/v1/resources.json`

## License

MIT (仓库自研代码)。`Doc/OpenDrive_API_guide.pdf` 版权归 OpenDrive, Inc. 所有,仅作本项目内部开发参考,本仓库为私有仓库,请勿公开或转发该文件。
