# Release Notes — v0.5.1

**发布日期**：2026-11-20
**Git tag**：`v0.5.1`（待打，代码与文档已就绪）
**Module**：`github.com/jeeinn/motd`

> 彩蛋系统版本。P3-M6 落地，打通「彩蛋→成就」闭环。无 schema 迁移、无新依赖、无新环境变量；测试 914 → 924。

## 本版变化

### Added

- **六条彩蛋命令**（全部本地语料、零网络、无新依赖）：
  - `/fortune` — 随机极客箴言（20 条本地语料）
  - `/cowsay` — ASCII 牛说固定短句
  - `/xkcd` — 随机 xkcd 风格编号+标题（12 条本地清单，**不联网**，避免失败态）
  - `/blame @user` — 玩笑式甩锅，固定模板、**绝不回显参数**
  - `/sudo` — 假提权失败文案，呼应公钥白名单与审计日志
  - `/karma` — 显示自己的声望、等级与距下一级进度（复用 v0.5.0 的 `KarmaReader`）
  - `/quit` 退出时约 12% 概率多说一句固定告别语
- **状态类成就**（接 0001 白名单里已有的两个成就码，首次真正生效）：
  - `night_owl`「夜行者」— 02:00–05:00 发帖解锁，**按 `MOTD_DISPLAY_TIMEZONE` 计算**（默认 Asia/Shanghai，与终端显示时间一致）
  - `first_vote`「欢迎来到互联网」— 内容首次被投票时给**作者**（非投票者）；自投票不解锁
- `command.WithCommands` 命令合并机制与 `internal/plugin/social` 插件。

## 安全与边界

- 彩蛋文案全部是编译期常量，经 `safe.RenderSystem`（服务端可信路径）输出；渲染失败退化为不输出，绝不裸写原文（ADR-019/026）。
- 彩蛋不改数据库、不改计分；插件仍只有「只读查询 + 申请成就」两条受限能力（ADR-015）。
- `first_vote` 通过扩展的只读端口取作者 ID（只有整数、无 PII）；内容已软删查不到作者时静默跳过，不让插件读失败阻塞 outbox 事件投递。
- `night_owl` 按 `MOTD_DISPLAY_TIMEZONE`（默认 Asia/Shanghai）计算本地小时，与终端展示时间一致；时区由 cmd 装配层注入插件 Context，插件不反向依赖 presentation 层。

## 升级说明

从 v0.5.0 升级无需迁移、无需改配置：替换二进制重启即可。无新环境变量。

## 已知问题与限制

- 彩蛋文案固定，重复触发内容不变；`/xkcd` 是本地清单不是实时漫画。
- 延后到 v0.5.2：动态数字雨 `/matrix`（ANSI 白名单风险）、连续签到 7 天（需迁移）、Konami 码解锁隐藏版块 `/dev/null`（需输入序列检测）。
- 沿用前版：会话最长 6 小时、最小压测不构成 1 Mbps/1000 在线验收、race job 非 required 观察中。

## 验证

- `go build ./...` / `go vet ./...` 通过；`go test -count=1 ./...` 全绿，**0 FAIL**，测试函数 **924**（v0.5.0 为 914）。
- 专项测试：命令合并与语料安全、夜行者时段边界、投票成就给作者、安全渲染、等级阈值；archtest 全绿（插件未越权 import infrastructure）。
- 格式门禁零输出。

## 破坏性变更

无。新增命令与成就均为增量能力，对外契约、配置与数据结构兼容 v0.5.0。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.5.1]` 段、[`DECISIONS.md`](docs/DECISIONS.md) 的 ADR-061、[`P3-PLAN.md`](docs/P3-PLAN.md) §7。
