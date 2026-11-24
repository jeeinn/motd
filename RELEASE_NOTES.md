# Release Notes — v0.8.2

**发布日期**：2026-11-24
**Git tag**：`v0.8.2`
**Module**：`github.com/jeeinn/motd`

> 补丁版本：提交被限流 / 只读 / 冻结拒绝时保留输入草稿（仅本次连接内存），不再逐字重打。无 schema 迁移、无新依赖、无新环境变量、无键位/渲染改动；测试函数 1014 → 1028。

## 本版变化

### Added

- **提交失败保留临时草稿**。此前在编辑器写完标题/正文，提交命中限流（60 秒 1 帖 / 1 小时 10 帖）或只读/冻结拒绝时，编辑器被整体摘除、输入丢失，用户需等冷却后逐字重打。本版把内容存为**会话内存草稿**，分两条路径恢复：

  - **路径 A（限流，原地保留）**：失败后编辑器原地重建，正文回显一次，打印「约 X 秒后按 Ctrl-D（或单独一行 `.`）重发」。等待期间按 **Esc 挂起**草稿回浏览屏，回到原帖按 `r` 即恢复草稿；按 **Ctrl-C** 则取消并清草稿。
  - **路径 B（只读 / 冻结，回屏兜底）**：失败后回浏览屏并保留输入，完成登录 / 解封后，重新 `/reply` / `/new` 或回原帖按 `r` 即预填恢复。

  A、B 共用同一份 `pageController` 会话草稿；`/edit` 不检出草稿（避免 `EditPost` 覆盖原文）。

## 设计取舍

- **只做内存版**：草稿仅存在当前 SSH 连接的控制器内存，**断连即失、不落库、不进日志**，文案统一「临时草稿（仅本次连接）」管理预期。持久化落库（新 `drafts` 表 + TTL + 写队列）登记为 P1 backlog，另立 ADR 评估写队列背压（ADR-010），本期不实现（ADR-067）。
- **A/B 双路径**：编辑器态只吃按键、不分发斜杠命令，`ErrReadOnly` 需 `/login`、`ErrForbidden` 需等解封，无法原地解决，故走 B；只有限流是"原地等一会儿即可重发"，走 A。Esc 在 A 草稿态语义扩展为"挂起保草稿"（不新增键位），覆盖原"取消"语义。
- **防重复回显 / 哨兵误判**：路径 A 不调 `pasteBlock`（避免第二遍正文与 `echo.cols` 叠加致退格错乱）；`Composer.Load` 整块构造 lines、绝不逐字符 Feed（防正文里独立的 `.` 行误触提交哨兵）。
- **账号不存在口径统一**：`repo_user.go` 的 `Status()` 在记录不存在时改为包装 `forum.ErrNotFound`，`actor()` 据此收敛为 `ErrForbidden`（与事务内写权限检查一致，同样可留草稿）；另两个 `Status()` 调用点已核对无回归。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖、无键位变化。替换二进制并重启即可，配置与数据零改动。命令集合、权限、渲染契约不变。

## 已知问题与限制

- 草稿断连即失（内存版既定边界）；如需跨会话恢复，待 P1 落库方案。
- 倒计时不自动刷新（守 ADR-027）：等待秒数为静态文案「约 X 秒、以服务端为准」，提前提交会再次被限流拒绝。
- 沿用 v0.8.1：`/stats` 在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出（含未跟踪新增文件）、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **1028**。
- 新增 `composer_draft_test.go` 9 项集成测试（覆盖 TC-A01~A11 P0 断言）与 `composer_test.go` 5 项 `Load` 纯单测；含挂起→按 r 恢复逐字节一致、注入净化白名单 ANSI、`echo.cols` 不叠加等断言。
- 两条闭环专项 `TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData` 绿；archtest 绿。

## 破坏性变更

无。命令集合、权限、数据契约、键位完全不变；仅新增提交失败时的草稿保留行为（既有 Esc 在草稿态语义扩展为"挂起"，无草稿时维持原"取消"）。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.8.2]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-067，以及设计稿 [`docs/DESIGN-composer-draft.md`](docs/DESIGN-composer-draft.md) v2。
