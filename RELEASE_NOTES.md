# Release Notes — v0.4.2

**发布日期**：2026-11-20
**Git tag**：`v0.4.2`（已发布）
**Module**：`github.com/jeeinn/motd`

> 契约收敛版本。移除死配置 `MOTD_SSH_IDLE_TIMEOUT` 与整套 idle timer 机制，会话生存期只由 6 小时绝对上限约束。无 schema 迁移、无新依赖；测试 907 → 905。

## 本版变化

### 移除：会话空闲超时配置

`MOTD_SSH_IDLE_TIMEOUT` 及其整套 idle timer 机制已彻底删除。

**为什么删**：v0.4.1 把空闲计时器改为由连接级心跳每 10s 重置后，它在正常配置下**永不触发**——心跳只要会话存活就一直运行。真正兜住「客户端静默消失」的是 6 小时绝对上限（`sessionMaxLifetime`）。于是这个配置成了"看起来能配、改了没效果"的死配置：只有在被误设成小于心跳周期（10s）时才会偶尔触发，等于「配小了才产生 bug」。留着弊大于利。

**删了什么**：环境变量 `MOTD_SSH_IDLE_TIMEOUT`、`Config.SSHIdleTimeout`、`sshd.Config.IdleTimeout`、`Session.idle` / `Session.timer` / `resetIdle()` / `idleCloseNotice`、`newSession` 的 idle 参数、相关测试。环境变量总数从 25 项减为 24 项。

**会话生存期现在的唯一规则**：

- 无按键不断连——停止浏览但连接仍在时不会被断开；
- 最长在线 **6 小时**（硬编码，不可配置），到点下发固定提示「motd: 会话达到最长在线时长（6 小时），连接已关闭」并回 exit-status（退出码 0）后关闭；
- 关闭序列仍是幂等的「固定文案 → exit-status → 关闭 channel」（ADR-054 的机制保留），只是收口路径从三条减为两条。

### 理由与边界

死配置比没有配置更有害——它让用户以为能控制断连时机，实际不能。移除后语义单一清晰。不提供"自定义最长在线时长"配置，6 小时固定；若未来有长会话需求再单独评估开放配置项。ADR-054 原文保留供追溯，本次收敛记在 ADR-055。

## 升级说明

从 v0.4.1 / v0.4.0 升级无需迁移、无需改配置：替换二进制重启即可。

若 `.env` 里仍有 `MOTD_SSH_IDLE_TIMEOUT`，会被**静默忽略**（不报错、不影响启动），建议手动删除该行。从更早版本升级同理，迁移会自动顺序补齐（0001 → 0009）。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.4.2
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)。

## 已知问题与限制

- 会话最长在线时长固定 6 小时、不可配置。
- 沿用前版：白名单变更需重启生效；审计缓冲 queue full 时 DomainBus 去重可能吞掉重投；回复计数 `INDEXED BY` 索引名硬绑定为技术债。
- 终端 VT 兼容、限流纯内存、验证码 pepper 重启失效等前版限制不变。

## 验证

- `go build ./...` / `go vet ./...` 通过。
- `go test -count=1 ./...` 全绿，0 FAIL；测试函数 **905**（v0.4.1 为 907：删 3 个 idle 测试、增 1 个 `TestNoIdlePathWithoutInputOrHeartbeat` 反证 idle 路径确实消失）。
- archtest 全绿；CI 门禁在 push 后确认。

## 破坏性变更

配置项 `MOTD_SSH_IDLE_TIMEOUT` 被移除（0.x 阶段允许的契约收敛）。终端行为完全兼容：停止浏览不再断连的体验与 v0.4.1 一致，旧客户端无需改动；残留该配置的环境变量被静默忽略，不导致启动失败。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.4.2]` 段落，技术决策见 [`DECISIONS.md`](docs/DECISIONS.md) 的 ADR-054 / ADR-055。
