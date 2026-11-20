# Release Notes — v0.5.0

**发布日期**：2026-11-20
**Git tag**：`v0.5.0`（已发布）
**Module**：`github.com/jeeinn/motd`

> P3 稳定运营版本。成就与 karma 从占位变为真实数据，CI 加竞态检测，最小压测集落地，备份调度可复制。无 schema 迁移、无新依赖、无新环境变量；测试 905 → 914。彩蛋命令（P3-M6）拆到 v0.5.1。

## 本版变化

### Added

- **CI 竞态检测 job**：独立 `race` job（`CGO_ENABLED=1`、`GORACE=halt_on_error=1`、`go test -race -timeout 15m`），初期非 required，观察 2–3 轮稳定后再提为门禁。更正了此前"纯 Go 依赖故 -race 不可用"的因果——本机 Windows 无 gcc 才跑不了，Linux CI 开 CGO 零成本（ADR-058）。
- **最小压测集合**：4 个同包 benchmark（SSH 握手 8/50、浏览列表 16/64、写队列背压、并发渲染+presence 100）+ `scripts/bench.sh`。不进 CI 默认 job，发版前本地或 `workflow_dispatch` 跑。
- **备份调度模板**：`deploy/motd-backup.service` + `motd-backup.timer`（每日 03:00，`Persistent=true`）。这是**模板**，需部署方复制到 `/etc/systemd/system/`、替换 `<ENV_FILE>` 后自行启用——不擅自替用户开启调度。`backup.sh` 现在生成备份后立即 `motdctl verify`，失败即非零退出便于告警（ADR-060）。
- `docs/P3-PLAN.md`：P3 完整方案与实施记录（M1~M5 已实施，M6 拆 v0.5.1）。

### Changed

- **成就落 SQLite**：替换 P0 的内存占位 Store。此前插件申请成就成功、进程重启全丢；现在写 `achievements` 表，重启不丢。`Grant` 经唯一写队列、`ON CONFLICT(user_id, code) DO NOTHING` 保证幂等（ADR-056）。
- **karma 计分接线**：规则由用户确认——发主题 +2、回复 +1、被 upvote +1；被 downvote 不扣作者、自投票不给、改票按 upvote 状态差精确同步、删/隐藏内容不回退。状态栏 karma/等级从恒 0/L1 变为真实值（L2=10/L3=50/L4=200）。**等级仅用于显示，绝不参与权限判断**（重申 ADR-014）。显示侧用独立 `KarmaReader` 端口 + 可选接口断言，既有调用方零改动（ADR-057）。
- `docs/OPS.md` §4 恢复流程补上必须的 `motdctl fts-rebuild --db` 步骤——此前遗漏会导致恢复后 `/search` 返回空；抽查步补做 `/search` 确认。

## 升级说明

从 v0.4.x 升级无需迁移、无需改配置：替换二进制重启即可。无新环境变量、无 schema 变更。

成就与 karma 从本版起开始累积：此前内存占位的成就不会保留，karma 从 0 开始按新规则计分。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.5.0
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)；在线体验 `ssh -p 2222 guest@jeeinn.com`。

## 已知问题与限制

- 会话最长在线时长固定 6 小时、不可配置（v0.4.2 起）。
- 最小压测给的是回环环境相对延迟基线，**不构成 1 Mbps / 1000 在线验收证据**；容量上限仍需在目标机器与真实网络下分档实测。
- `race` job 初期非 required；备份调度模板不自动启用、不自动删除旧备份（人工清理）。
- 沿用前版：白名单变更需重启生效（SIGHUP 热重载推 P3）；审计 queue full 时 DomainBus 去重可能吞重投；回复计数 `INDEXED BY` 索引名硬绑定为技术债。
- 彩蛋命令本版未接入，已确认拆到 v0.5.1。

## 验证

- `go build ./...` / `go vet ./...` 通过；`go test -count=1 ./...` 全绿，**0 FAIL**，测试函数 **914**（v0.4.3 为 905）。
- 格式门禁零输出；四个 benchmark 实跑通过。
- 隔离环境实跑 `backup.sh`：输出 `backup=`/`sha256=` 后紧接 verify，`integrity=ok`，证明备份验收闭环。
- CI（含新 race job）在 push 后确认；race job 本身为首次上线。

## 破坏性变更

无。本版为能力增强与稳定性收敛，对外契约、配置与数据结构均兼容 v0.4.x。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.5.0]` 段落，技术决策见 [`DECISIONS.md`](docs/DECISIONS.md) 的 ADR-056 ~ ADR-060，P3 方案见 [`P3-PLAN.md`](docs/P3-PLAN.md)。
