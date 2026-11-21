# Release Notes — v0.6.0

**发布日期**：2026-11-21
**Git tag**：`v0.6.0`（待打，代码与文档已就绪）
**Module**：`github.com/jeeinn/motd`

> 彩蛋扩展版本。连续签到成就、合规数字雨、Konami 码解锁隐藏版块。含 schema 迁移（0010），测试 924 → 935。

## 本版变化

### Added

- **每日签到与 7 天成就**：登录成功后幂等记录本地日期，连续 7 天解锁 `streak_7` 成就，断签归零。日期按 `MOTD_DISPLAY_TIMEZONE` 计算（默认 Asia/Shanghai），与终端展示时间一致。GitHub Device Flow 与邮箱登录两条路径都已接。
- **`/matrix` 滚动雨帘**：固定 24 行、50ms/行、SGR 青色 + 固定字符集，连接断开立即停。**合规 ADR-027**：只用 SGR 上色，不使用被禁止的 ED 清屏、CUP 光标定位或备用屏序列；真全屏数字雨评估见路线图。
- **Konami 码解锁 `/dev/null`**：浏览态输入 ↑↑↓↓←→←→BA 解锁隐藏版块，**按用户持久化**，解锁后该用户的版块列表末尾出现 `/dev/null`；未解锁用户不可见。解锁目标由服务端固定，绝不接用户参数。
- `docs/ROADMAP.md`：后续路线图，含终端多色扩展（如节日主题）的完整技术评估。

### Changed

- `ViewContext` 加 `UnlockedHiddenNodes` 字段：入口层注入已解锁节点，forum 层只做去重与并入，不做解锁判权（维持 ADR-015 边界）。
- 迁移版本 9 → 10；成就码白名单加 `streak_7`。

## 升级说明

新增 0010 迁移（`daily_checkins`、`user_node_unlocks` 表 + `/dev/null` 种子），启动时自动应用。从 v0.5.x 升级无需改配置：替换二进制重启即可，无新环境变量。签到数据从本版起开始累积。

## 已知问题与限制

- `/dev/null` 是彩蛋版块，无任何内容；解锁只影响版块列表可见性，不改变权限或计分。
- `/matrix` 是合规的滚动雨帘（24 行），不是真全屏动画——受 ADR-027 安全白名单约束。
- 签到按展示时区的**本地日期**判定；跨时区旅行或系统时区变更可能使连续天数出现空洞（按日期字符串比对）。
- 沿用前版：会话最长 6 小时、最小压测不构成 1 Mbps/1000 在线验收、race job 非 required 观察中。
- 延后项与多色评估见 [`ROADMAP.md`](docs/ROADMAP.md)。

## 验证

- `go build ./...` / `go vet ./...` 通过；`go test -count=1 ./...` 全绿、**0 FAIL**，测试函数 **935**（v0.5.1 为 924）。
- 专项：Konami 状态机（精确序列/错键重置）、签到连续天数与成就（含断签重置）、SQLite 幂等签到与解锁持久化、ViewContext 隐藏节点注入去重、matrix 帧安全。
- 格式门禁零输出；archtest 全绿（插件未越权）。

## 破坏性变更

无。新增能力均为增量，Config / schema 兼容旧版（迁移向前）。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.6.0]` 段、[`DECISIONS.md`](docs/DECISIONS.md) 的 ADR-062、[`ROADMAP.md`](docs/ROADMAP.md)。
