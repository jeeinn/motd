# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

### Changed

### Fixed

### Security

## [0.1.0] - 2026-11-19

首个开源版本。将原本本地可用的 MVP 正式以 `v0.1.0` 公开，代码与功能未做破坏性调整。

### Added

- **SSH 优先的终端论坛入口**：用户通过 `ssh` 连入，用键盘与斜杠命令浏览版块 / 主题、发帖、回复；仅公钥认证，默认端口 `:2222`。
- **论坛业务内核**：5 个版块种子、keyset 分页、软删除过滤、访客只读、Q6 限流、统一 ViewModel。
- **GitHub Device Flow 登录**：公钥三状态解析、设备码状态机、账号登录事务、公钥绑定与软吊销、`guest → user` 会话升级。
- **四页终端页面**：版块 / 主题 / 详情 / 编辑，原地重绘导航、键位收尾与分层安全渲染（22 条恶意样本验证）。
- **M0 地基**：配置 / 日志 / 优雅停机 / `/healthz`、SQLite 双池与 PRAGMA 自检、版本化迁移（3 个）、写入队列（容量 256 快速失败、3s 超时）、备份恢复、依赖方向守卫。
- **端到端验证**：真实 SSH 客户端闭环冒烟（D1/D3/D4）、带真实数据的重启持久化与备份恢复演练（D6/D7）。
- 全量 `go build` / `go vet` / `go test -count=1 ./...` 通过，456 个测试函数。

### Changed

- README 由内部交付文档改写为开源版本，移除内部演练代号与私有文档引用，补充安装与贡献入口。

### Security

- 终端输入文本经净化后回显，仅 Secret 字段隐藏，避免终端注入与凭据回显。
- SSH 入口仅公钥认证、禁 root；host key 在数据目录首次启动时生成，不随仓库分发。
- 令牌不落库、不进日志、不进呈现结构。

[Unreleased]: https://github.com/jeeinn/motd/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jeeinn/motd/releases/tag/v0.1.0
