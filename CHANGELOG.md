# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

### Changed

### Fixed

### Security

## [0.3.0] - 2026-11-19

### Added

- **邮箱注册/登录/密码找回**：`/register` / `/verify <code>` / `/resend` / `/recover` / `/login email`，自带 163 SMTP 隐式 TLS 通道；验证码只存 HMAC 散列、单事务原子消费；两段式发信（写事务提交后才触网）；argon2id(19MiB, t=2, p=1) 本地口令；复用 M2 双维限流 / 审计 / 会话撤销。无新迁移（表已在 0001）。

### Changed

- `/login` 无参提示改为「仅支持 /login github 或 /login email」。
- `/verify` 的密码阶段（注册设密码 / 找回新密码）改为两次输入确认；两次不一致时回退重输、不消费验证码、不计尝试次数。`/login email` 仍单次输入密码。

### Fixed

- 修复邮箱流程结束后密码掩码（`setSecret(true)`）未复位、泄漏到后续命令输入的问题；done / cancelled / 新命令取代旧流程三类出口全部复位。
- SSH banner 现在始终下发登录引导（非 root 用户名 + 公钥、未绑定则访客、禁 root 禁密码），解决 root / 密码登录被拒时只见 `Permission denied (publickey)`、不知如何操作的问题；满员时叠加满员提示。

### Notes

- 所有交互项均已完成；邮箱凭据仅在本地 `.env` 配置，不进仓库。

## [0.3.0] - 2026-11-19

### Added

- **正文跨行退格合并**：空行行首按退格现在删除该空行并把上一行文本并入，光标停在拼接处可继续输入（此前无动作）；首行空退格保持无动作。
- 新增白名单内 CUU+EL 整行重绘回显，ADR-027 白名单零改动，未放宽 CUP/CUD/ED。

### Changed

- `ComposerOp` 扩展跨行合并信息（`MergedLines` / `MergedText`）；标题阶段维持单行语义。

### Notes

- 本段仅含交互改进；邮箱注册/登录/找回另行发布。行内左右移动、超宽自动折行不在本版。

## [0.2.1] - 2026-11-19

低风险体验补齐，无 schema 迁移。

### Added

- 主题详情页标题与正文之间新增虚线分隔（服务端固定文案 + 暗色样式，无新增光标控制）。
- 新增 `/delete [id]`：作者可软删自己的主题或回复；无参删当前主题，带参删指定回复；非作者、访客及目标不存在统一提示「仅作者可删除」。

### Changed

- 软删不硬删、不级联：被删主题下的回复行保留，并随主题从详情、列表与全文检索中一并隐藏。检索 SQL 增加所属主题 `status='active'` 过滤，避免已删主题内容从搜索结果泄漏。

### Fixed

- 修复删除失败时写事务内部错误细节可能回显到终端的问题，统一为固定提示文案。

## [0.2.0] - 2026-11-19

第二阶段发布。补齐认证安全、论坛互动与编辑体验，迁移扩展到 0007，测试从 456 增至 632。

### Added

- **论坛互动**：投票/点赞（`/vote up|down|rm`，幂等、改票按差值计分）、全文搜索（`/search`，FTS5 trigram + 短词 LIKE 回退 + 特殊字符字面量处理）、搜索结果页（`n/p` 翻页、Enter 打开主题）。
- **认证安全**：`auth_events` 审计（有界缓冲 + 批量落库 + 溢出 fail-closed）、按 IP/用户/邮箱的认证限流（纯内存滑动窗口）、`/logout` 注销并撤销该账号全部在线会话、`/keys <指纹>` 公钥详情。
- **多行编辑**：正文 Enter 换行、单独一行 `.` 或 Ctrl-D 提交；粘贴整块进正文、内部换行不误提交；标题仍是 Enter 确认。
- **运维**：`device_auth_states` 过期清理、SSH 公钥 `last_used_at` 节流更新、`motdctl fts-rebuild` 恢复后重建索引、`.env.sample` 全量环境变量示例（17 项，带中文注释）。
- **数据完整性前置**：`threads.status` 主题软删、回复父子同 thread 约束、领域事件 outbox（事务内落库、提交后投递、崩溃可重扫）。
- 新增迁移 0004~0007；全量测试 632 个函数。

### Changed

- `/login` 发起屏改为论坛同款 Screen 与共享渲染器，状态栏与版块页一致；渐进状态行仍为固定文本。
- `TouchThread` 仅供发帖/回复；投票改用独立的 `AdjustScore`，不再影响主题排序（`last_reply_at`）。
- 恢复备份后 FTS 索引不再自动重建，需显式执行 `motdctl fts-rebuild`。

### Security

- 投票分数与主题活跃度正交，评价行为不再污染排序。
- 审计缓冲溢出 fail-closed：审计缺失期间拒绝认证动作，不做 fail-open 放行。
- `/keys` 详情将 userID 纳入查询条件，跨用户与不存在返回同一错误，防止指纹枚举。
- FTS 检索的 MATCH 串参数化并转义，用户输入的 `*`/`"`/`%`/`_` 按字面量处理。

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

[Unreleased]: https://github.com/jeeinn/motd/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/jeeinn/motd/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/jeeinn/motd/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/jeeinn/motd/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/jeeinn/motd/releases/tag/v0.1.0
