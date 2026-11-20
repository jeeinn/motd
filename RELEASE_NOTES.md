# Release Notes — v0.4.0

**发布日期**：2026-11-20
**Git tag**：`v0.4.0`（待打，代码与文档已就绪）
**Module**：`github.com/jeeinn/motd`

> 社区治理版本。P2 五个里程碑（M0/M1/M1b/M2/M3）合并一次发版：版块可管理、内容可修正、终端提交更可靠。迁移 0007 → 0009，ADR-048 → 053，测试 749 → 903（双口径实测一致）。无新依赖。

## 本版变化

### 节点（版块）管理

管理员用 `/node list|create|rename|archive` 列出 / 创建 / 改名 / 归档版块。归档按软删处理——只改 `node.status`，不动 threads，历史帖详情页仍显示版块名，规避 `threads.node_id` 外键冲突。5 个 seed 节点可归档、不可改名（slug/name 保持稳定，0002 的 `INSERT OR IGNORE` 保证迁移重放不覆盖）。

权限来自 `MOTD_ADMIN_FINGERPRINTS`（逗号分隔的 `SHA256:` 公钥指纹），比对的是 **SSH 认证阶段算出的会话指纹**，绝不是用户命令文本——公开邮箱注册无法自我提权。白名单留空 = 无人可管；变更需重启生效（热重载推 P3）。不引入 `users.role` / RBAC，不做 Web 管理页，CLI 仅作灾后恢复补充。治理动作经 outbox 异步写入 `auth_events` 审计（谁 / 何时 / 命令 / 目标 / 结果）。

### 内容治理

`/hide [id]` 隐藏他人主题或回复，`hidden ≠ deleted`——是可治理的隐藏状态，不物理删数据。用户实测反馈"担心误隐藏无法恢复"后补 `/unhide [id]`（恢复当前主题/回复）与 `/unthread <thread-id>`（从任意页面恢复隐藏主题，解决 `/unhide` 无参对已隐藏主题不可达的问题）。命令复用与节点治理同一套 `requireAdmin` 权限与审计通道。6 条管理员命令在帮助里单独归组展示。

### 作者编辑

`/edit [id]` 或无参编辑首帖，仅作者本人可操作（复用 `canModify` 单点所有权判定，与治理权严格分离）。详情页显示「已编辑 MM-DD HH:MM」，走 `RenderSystem` 固定路径、复用 `MOTD_DISPLAY_TIMEZONE`，不进用户内容净化链、不做第二套时区逻辑。策略为「已编辑标记」而非版本历史：保留原投票与回复，接受"投票对象可能是改前内容"的语义代价；编辑**不推进 `last_reply_at`**，避免改内容把旧帖顶到最新。

### 回复数显示

主题列表真实显示紧凑 `rN`（此前 v0.3.3 已就位格式，但 `RowMeta.Replies` 生产路径从未赋值、实际不显示）。本版读时批量聚合 `ReplyCountsByThreadIDs`，`COUNT-1` 去掉首帖得实际回复数；分页 20 行的 EXPLAIN 走 `idx_posts_thread`，无全表扫（可复现对照见 ADR-052）。选择读时聚合而非写时维护列，与项目"写事务稀缺"约束同向，零迁移、零写路径改动。

### 用户实测反馈修复

- 浏览态连按 `Esc` 不再向终端追加「（取消）」——改为静默 `actionNone`；编辑 / 命令 / 邮箱态的取消提示保留。
- `/vote rm` "未生效"疑点经诊断确认非 bug：`/top` 后 `threadID` 清零，须先打开主题；守卫提示被吞属显示层，未改。

## 升级说明

从 v0.3.2 / v0.3.3 升级需执行两个新迁移（`0008_nodes_status`、`0009_edited_at`）：服务启动时自动应用，无需手工操作。建议先按 `OPS.md §2` 备份。v0.3.1 及更早版本同理，迁移会自动顺序补齐（0001 → 0009）。

开启治理能力需在 `.env` 配置 `MOTD_ADMIN_FINGERPRINTS`（0600、不进仓库）：

```bash
ssh-keygen -lf ~/.ssh/id_rsa.pub
# 把输出的 SHA256:... 指纹填入 .env，多个用逗号分隔
MOTD_ADMIN_FINGERPRINTS=SHA256:<43 位 base64>
```

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.4.0
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)。

## 已知问题与限制

- 白名单变更需重启才生效（SIGHUP 热重载推 P3）。
- 审计缓冲 queue full 时，DomainBus 去重可能吞掉重投；`/healthz` 的 `audit_dropped > 0` 应立即告警。
- 回复计数使用 `INDEXED BY` 锁计划，索引名硬绑定为技术债。
- 沿用前版：限流计数纯内存；验证码 pepper 重启失效；`/recover` 对未注册邮箱回统一文案但实际不发信；正文超宽折行与行内左右移动不做；正文提交用 `.` 或 Ctrl-D；窄终端下元信息会折行。

## 验证

- `go build ./...` / `go vet ./...` 通过。
- `go test -count=1 ./...` 全绿，0 FAIL；测试函数 **903**（`go test -list` 与 `grep '^func Test'` 双口径一致）。
- 5 个里程碑 commit + 1 个实测修复 commit 共 7 个，隔离演练均用 `data/` 副本，生产库全程未触碰（见 `OPS.md §5`）。

## 破坏性变更

无。全部新增命令与字段向后兼容；旧客户端不受影响。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.4.0]` 段落，技术决策见 [`DECISIONS.md`](docs/DECISIONS.md) 的 ADR-048 ~ 053。
