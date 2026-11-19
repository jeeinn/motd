# P1 实施计划（内核安全与数据完整性）

> 基线：v0.1.0 已开源发布，MVP（P0）全部交付、456 个测试函数全绿。
> 本文是 P0 之后下一阶段的单一执行入口。已知限制的原始记录见 `MVP-DELIVERY.md §6`，设计分期见 `docs/design/命令行论坛整体设计方案-评审稿v0.2.md`。
> 状态：范围与技术方案已定稿，**三项待拍板（见 §8）未替用户决定**，按推荐项执行前请确认。

## 0. 结论先行

下一阶段主线：**把内核做安全、做扎实**——以「认证安全补齐 + 三项数据完整性前置」为骨架，顺带收割轨道已铺好、且不触碰对外契约的低成本体验项（投票、搜索 FTS5、多行编辑器）。

**本阶段不新增任何对外公共接口**。完整 D2 闭环复测与 D6 kill-9 演练作为两个前置信心门排在迁移之前。

## 1. 范围界定

### 1.1 进下一阶段（7 组）

| 组 | 内容 | 依赖 |
| --- | --- | --- |
| A | D6 kill-9 + WAL 恢复演练（前置信心门） | 无，必须最先 |
| B | 三项强制前置技术债：R12 outbox、R9 threads.status、R8 parent 同 thread 约束 | A |
| C | 认证安全补齐：审计事件、三类限流、会话撤销、注销边界 | B |
| D | 投票/点赞（votes 表已建、score 路径就位） | B（R8） |
| E | 搜索 FTS5 | B |
| F | 多行编辑器 | B |
| G | 数据/会话运维：device_auth_states 清理、last_used_at、/keys 详情 | C |

### 1.2 明确不做（范围守门）

| 候选 | 不做的理由 |
| --- | --- |
| karma / 等级展示、作者名投影（R11） | 留存价值未验证；R11 触显示契约，0.x 阶段不宜固化 |
| `/login` 屏呈现层替换（R18/R20） | 受 D2 阻塞，无法真实验证；等链路跑通一次改完 |
| 邮箱注册与账号恢复 | R7「同邮箱占用」未决策 + 邮件外发通道未确认（见 §8 D-c） |
| 浏览器 TUI / Frame 增量 / WebSocket / RSS / 只读 API / 原生 CLI token / Passkey | 全部是**对外公共接口**；0.x 接口不稳 + 1 Mbps + 1000 在线未验证，现在做等于焊死契约 |
| P2 全部、P3 全部 | 依赖稳定对外契约或真实部署后的验证/运维增强 |

### 1.3 RICE 排序（相对判断，非精确数字）

| 排 | 候选 | R | I | C | E | 分 | 结论 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 认证安全补齐 | 5 | 3 | 1.0 | 3 | 5.00 | 进 |
| 2 | 投票/点赞 | 4 | 2 | 0.8 | 2 | 3.20 | 进，依赖 R8 |
| 3 | 三项前置技术债 R12/R9/R8 | 4 | 2 | 1.0 | 3 | 2.67 | 进，**强制前置先于投票** |
| 4 | 搜索 FTS5 | 4 | 2 | 0.8 | 3 | 2.13 | 进 |
| 5 | 多行编辑器 | 3 | 1 | 1.0 | 2 | 1.50 | 进 |
| 6 | 数据/会话运维 | 2 | 1 | 1.0 | 2 | 1.00 | 进，随安全项顺带 |
| 7 | karma / 作者名（R11） | 3 | 1 | 0.8 | 3 | 0.80 | 不做 |
| 8 | `/login` 屏替换 | 2 | 1 | 0.8 | 2 | 0.80 | 不做 |
| 9 | 邮箱注册与恢复 | 3 | 2 | 0.5 | 5 | 0.60 | 条件进入 |
| 10 | 浏览器 TUI / Frame / WebSocket | 2 | 2 | 0.5 | 5 | 0.40 | 明确不做 |

### 1.4 决策台账

| # | 决策 | 推荐项 | 代价 / 回报 |
| --- | --- | --- | --- |
| 1 | 先安全后体验 | 认证安全 + 前置债先行，对外接口冻结 | 等级/展示延后一迭代；换来开源部署敢上公网 |
| 2 | 前置债不走纯 RICE，强制插入 | R12/R9/R8 直接排最前 | 一段无用户可见交付的工期；避免体验项返工 |
| 3 | 公共接口冻结 | 不做浏览器/WebSocket/RSS/API/token/Passkey | 暂不覆盖浏览器用户；换回改成本可控 |
| 4 | 邮箱恢复拆包 + R7 先决策 | 本阶段不做完整邮箱注册，先定占用规则与发信通道 | 账号找回延后一档 |
| 5 | D6 提前 | 所有 schema 迁移前先跑 kill-9 + WAL 恢复演练 | 少量测试工时；换来迁移的回滚信心 |

## 2. 技术 Spec

技术基线已对真实代码核实：module `github.com/jeeinn/motd` / Go 1.26.3 / `modernc.org/sqlite` 纯 Go（无 CGO，故 `-race` 不可用）；SQLite 3.54.1，FTS5 与 trigram 分词可用；迁移 0001–0003 单文件单事务、只向前、版本高于二进制拒绝启动；写队列单 worker + chan 256 + 3s + `_txlock=immediate` + `busy_timeout 5000`。

**关键事实**：`WriteTx` 已预留 `TouchThread(threadID, at, scoreDelta)` 的 `scoreDelta` 参数；`votes` 表有 `UNIQUE(user_id, target_type, target_id)`；`DomainEvent.ID` 注释已写明供 outbox 去重，`EventVoteCast` 已声明。**P1 是「接线」不是重造。**

### 2.1 R12 — domain_event_outbox

- **表/迁移**：`domain_event_outbox(id, event_id UNIQUE, type, payload, status pending|dispatching|delivered|failed, attempts, created_at, delivered_at)` + `idx(status, created_at)`，迁移 `0006`。
- **事务边界**：`INSERT` 必须在 `repos.Write` 闭包内、与业务写同事务（`WriteTx` 新增 `AppendOutbox`）；投递**在提交后**由 `application/forum/outbox.go` 调度器执行——`status` CAS 领取（`UPDATE ... WHERE status='pending'` 查 `RowsAffected`）→ 调 `DomainEventSink.Publish` → 成功 `delivered` / 失败 `failed` + `attempts++`。**绝不在写事务内调 plugin handler。**
- **幂等/恢复**：至少一次投递，订阅者按 `event.ID` 幂等；崩溃残留 `pending` 由启动恢复扫描重投。
- **守卫**：新增 `archtest/outbox_test.go`，校验 `AppendOutbox` 在 `Write` 闭包内、`Publish` 不在闭包内（复用 `TestSessionRevokerIsCalledAfterWriteCommit` 的注入手法）。

### 2.2 R9 — threads.status

- **迁移**：`0004` `ADD COLUMN status TEXT NOT NULL DEFAULT 'active' CHECK IN ('active','deleted','hidden')`，常量默认自动回填历史行。
- **查询**：`ListThreads` / `GetThread` 过滤 `status='active'`。
- **回滚**：恢复到迁移前备份。
- **守卫**：校验 DDL 含 `CHECK` 且查询带 `status` 过滤。

### 2.3 R8 — parent 同 thread 约束

- **迁移**：`0005` 建 `posts` 触发器 `BEFORE INSERT/UPDATE`（`parent_post_id` 非空时校验父帖 `thread_id = NEW.thread_id`，否则 `RAISE(ABORT)`）。不重建表，触发器对历史数据安全。
- **代码**：`NewPost` 加 `ParentPostID`；`WriteTx.InsertPostWithParent` 先查父 `thread_id` 再插。
- **守卫**：新增 `archtest/vote_test.go` 负向注入「缺同 thread 校验」必须失败。

### 2.4 投票/点赞

- **归属**：新增 `application/forum/vote.go`（`CastVote` / `Unvote`）。
- **单 WriteTx 内顺序**：权限 → 目标存在 → `GetVote` 旧值 → `delta = 新值 - 旧值`（无旧 = 0）→ 同值 `delta=0` 直接返回（幂等）→ `UpsertVote(ON CONFLICT(user_id,target_type,target_id) DO UPDATE)` → `AdjustScore(threadID, delta)` → `AppendOutbox(EventVoteCast, id="vote:<uid>:<type>:<id>")`。
- **⚠️ 不要直接复用 `TouchThread`**：其 SQL 为 `UPDATE threads SET last_reply_at=?, score=score+?`，投票调用会把旧主题顶到「最新」，破坏 `SortLatest`。新增只改 score 的 `AdjustScore`，`TouchThread` 仍只给回复用。
- **守卫**：校验 `vote.go` 不出现 `TouchThread` 调用。

### 2.5 限流 + 审计

- **限流**：`application/auth/ratelimit.go` 纯内存滑动窗口，键 = `hash(ip)` / `userID` / `hash(email)`，零 DB（照 `presence.Registry` 模式，archtest 禁 SQL import）。
  - **决策理由**：单写连接仅 1 条、队列 256，若把暴力尝试计数写进 SQLite，攻击者可直接堵死发帖/投票；**内存计数为权威，进程重启清零**（记为已知限制）。
- **审计**：`auth_events` 走有界异步缓冲（环 512 + 1s 批量 flush 进写队列），不阻塞认证判定；溢出 **fail-closed**（拒绝而非放行）+ `dropped` 计数。
- **配置**：`config.go` 用现有 `IntEnv` / `DurationEnv` 加 `MOTD_AUTH_LIMIT_*` / `MOTD_AUDIT_BUFFER`，显式默认。
- **守卫**：新增 `archtest/ratelimit_test.go` 校验 limiter 无 infrastructure import、审计写不在认证临界路径。

### 2.6 搜索 FTS5

- **迁移**：`0007` `posts_fts` 外部内容表 `tokenize='trigram'` + `posts` 的 INSERT/UPDATE/DELETE 触发器（status 转 deleted 时同步删索引）；配 `motdctl fts-rebuild` 供恢复后重建。
- **检索**：`application/forum/search.go`。MATCH 串引号包裹并转义内部引号（禁拼接，防 FTS 语法注入）；**< 3 字符查询回退 LIKE 并转义 `%` 和 `_`**（实测 trigram 对 2 字中文返回 0）；页长 ≤ 20、摘要截断适配 1 Mbps。
- **呈现**：ViewModel 仍放 `application/viewmodel`（保持渲染无关）；终端 `/search` 进现有命令表，不新增对外协议。
- **守卫**：新增 `archtest/fts_test.go` 校验迁移含 fts5+trigram、MATCH 参数化、LIKE 转义，负向注入拼接必失败。

### 2.7 多行编辑器

- **归属**：`application/input/composer.go`，以哨兵行 `.` / Ctrl-D 结束，限最大行数与字节。
- **接线**：`transport/ssh` 把行喂 composer 而非逐行执行；沿用安全渲染三段分离，无新依赖。

### 2.8 数据/会话运维

- `maintenance.go`：定时（启动 + 每小时）清 `device_auth_states` 终态过期行（`pending` 留到过期）。
- SSH 认证成功后**异步节流**（每钥 ≥ 5min）更新 `ssh_keys.last_used_at`，避免写放大。
- `/keys` 详情：扩 `KeyBindingReader.GetByFingerprint` + ViewModel + 终端渲染；`label` 仍空（无输入口，记为已知限制）。

## 3. 里程碑与依赖顺序

P0 已占用 M0–M4 编号，本阶段用 **P1-M0…P1-M4** 避免混淆。

| 里程碑 | 内容 | 前置 | DoD |
| --- | --- | --- | --- |
| **P1-M0 前置信心门** | kill-9 + WAL 恢复演练、`backup.sh` + `restore --verify` + `integrity_check`，写清 RPO/RTO 与备份六类 | 无 | 脚本化演练通过；OPS/DECISIONS 更新；**零 schema 变更** |
| **P1-M1 三项强制前置** | 迁移 0004/0005/0006 + `WriteTx` 扩展 + outbox 调度器 + 守卫 | P1-M0 | 历史数据形态库上迁移与回滚演练；outbox 写入/投递/崩溃恢复用例；同 thread 违规被拒；原 456+ 测试全绿 |
| **P1-M2 认证安全** | 限流/审计/密码变更与公钥解绑后会话撤销（≤1 刷新周期，复用现有 `SessionRevoker` 提交后调用模式）/注销边界 | P1-M1 | 可复现脚本；旧会话失效断言；守卫通过 |
| **P1-M3 投票 + 搜索** | 投票幂等与分数一致；FTS5 空结果/特殊字符/1 Mbps | P1-M1 | 并发确定性用例（无 `-race`）；注入用例绿 |
| **P1-M4 编辑器 + 数据运维** | 多行粘贴、清理幂等、/keys 详情、1 Mbps 无超时 | P1-M2 / P1-M3 | 上述验收项全过 |

## 4. 文件影响

**新增**：
- `internal/application/forum/{vote.go, search.go, outbox.go}`
- `internal/application/auth/{ratelimit.go, audit.go}`
- `internal/application/input/composer.go`
- `internal/infrastructure/sqlite/{repo_vote.go, repo_search.go, repo_outbox.go, repo_auth_events.go, maintenance.go}`
- `migrations/0004~0007.sql`
- `archtest/{vote, outbox, ratelimit, fts}_test.go`

**修改**：
- `internal/application/forum/ports.go`（`WriteTx` 加 `GetVote`/`UpsertVote`/`AdjustScore`/`AppendOutbox`/`InsertPostWithParent`/`PostThreadID`）
- `internal/application/forum/service.go`、`config/config.go`、`transport/ssh/*`、`presentation/terminal/*`、`cmd/server/wiring*.go`

## 5. 风险与缓解

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | 单写连接在限流/投票/审计下饱和 | 内存限流判定；审计异步批量 + 有界；队列深度入 `/healthz`；补 100/1000 连接压测 |
| R2 | FTS 索引漂移 / 注入 / 短词无结果 | 外部内容 + 触发器 + `fts-rebuild`；MATCH 引号转义 + LIKE 回退；负向注入守卫 |
| R3 | outbox 投递语义 | at-least-once + `event.ID` 幂等；状态 CAS；失败 observer；启动 pending 重扫；handler 绝不进写事务 |
| R4 | 0.x 冻结 vs 内部改动 | 全落内部包与终端命令，不动传输协议与 ViewModel 渲染无关性；CHANGELOG 标注 |
| R5 | `-race` 不可用（无 CGO/gcc） | 限流用互斥 + 注入时钟，`go test -count=10` 确定性验证；文档声明「未做竞态检测」，不声称 race-free |
| R6 | 迁移对历史库 | 先 P1-M0 演练；每迁移在含历史数据副本上跑 + 回滚；迁移前强制备份 |

## 6. 门禁清单

**每个里程碑交付前必跑**：

```bash
go build ./... && go vet ./... && go test ./...   # 含全部 archtest
```

**含迁移的里程碑（P1-M0 / M1 / M3）**：
- fresh 库 + 历史形态库双跑
- `scripts/backup.sh`；kill-9 恢复演练；`motdctl verify`
- 恢复到空目标 + `PRAGMA integrity_check`；核对 `schema_migrations` 版本

**专项**：
- 限流：`scripts/repro_auth_limit.sh` 复现 + `auth_events` 计数断言
- 投票：重复投票幂等 + 分数一致并发确定性用例
- 搜索：特殊字符注入套件 + 1 Mbps 限流跑
- 每个里程碑：archtest 负向注入必须失败

**ADR**：从 ADR-028 起，按 `docs/DECISIONS.md` 既有 `## ADR-0NN` 五段格式追加，不另建单文件，沿用项目惯例。

## 7. 验收标准

1. 连续失败认证触发按 IP/用户/邮箱限流，超限返回明确错误且每次失败写入 `auth_events`；有脚本可复现。
2. 密码变更 / 公钥解绑后旧会话在 ≤1 个刷新周期内失效；注销后会话不可复用。
3. 三项前置无回归：outbox 有写入与投递用例；`threads.status` 迁移在含历史数据上跑通且可回滚；违反 `parent_post_id` 同 thread 约束的写入被拒；原 456 个测试全绿且新增覆盖上述路径。
4. 投票幂等且分数事务一致（重复投票不加分）；FTS5 搜索在空结果/特殊字符/模拟 1 Mbps 下返回正确、无注入。

## 8. 待拍板事项（三项）

| # | 决策点 | 推荐项 | 代价 / 备选 |
| --- | --- | --- | --- |
| **D-a** | 「每次失败写 auth_events」与单写连接容量冲突 | 正常负载逐条、洪峰批量 + 溢出 fail-closed | 极端洪峰记 dropped 计数；备选逐条直写，但攻击可堵死全站写入 |
| **D-b** | FTS 1–2 字中文 | trigram + LIKE 回退 | 短查询退化为扫描；备选仅 trigram → 短词无结果 |
| **D-c** | R7 `/link email` 本阶段不做，但债条要求定义「同邮箱占用」 | 维持 `primary_email` 无 UNIQUE + 守卫，记录「未来 /link email 命中占用 → 固定提示先解绑旧号，绝不自动合并」 | 不新增字段/命令 |

## 9. 依赖与外部阻塞

- **D2 真实 `/login github` 复测**：需 VPS 注入 `MOTD_GITHUB_CLIENT_ID` 并放行出站；阻塞 `/login` 屏验证与 GitHub link。请交付总监协调 VPS 窗口。
- **邮件外发通道**（SMTP/凭据）缺失阻塞邮箱恢复，该候选已据此延后。
- **D6 WAL 恢复演练**阻塞所有 schema 迁移的安全落地，已前置 P1-M0。
- **未验证约束**：1000 在线 / 1 Mbps 尚未压测（P3），但限流与搜索须按此约束设计，禁全表扫描与大响应体。
