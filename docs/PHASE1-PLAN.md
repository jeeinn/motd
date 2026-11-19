# P1 实施计划（内核安全与数据完整性）

> 基线：v0.1.0 已开源发布，MVP（P0）全部交付、456 个测试函数全绿。
> 本文是 P0 之后下一阶段的单一执行入口。已知限制的原始记录见 `MVP-DELIVERY.md §6`，设计分期见 `docs/design/命令行论坛整体设计方案-评审稿v0.2.md`。
> 状态：**全部定稿**。三项拍板（D-a/b/c）按推荐执行；用户三条建议已评估纳入；D2 真实 `/login github` 用户确认已实测可用、结清；公开邮箱注册已确认开放（配 IP/邮箱双限流）。

## 0. 结论先行

下一阶段主线：**把内核做安全、做扎实**——以「认证安全补齐 + 三项数据完整性前置」为骨架，顺带收割轨道已铺好、且不触碰对外契约的低成本体验项（投票、搜索 FTS5、多行编辑器）。

**本阶段不新增任何对外公共接口。** 二轮讨论后新增三处范围调整：
1. **邮箱注册/登录/找回纳入 P1**，但独立为末位里程碑 P1-M5（L 体量），用**自带 SMTP（163），配置放 `.env`**；
2. **节点（版块）创建与管理明确放 P2**，P1 只落三条不变量到 ADR；
3. **Ctrl+Enter 提交不可靠**（通用 SSH 终端字节级不可区分），P1 保持哨兵 `.` / Ctrl-D + 正文 Enter 换行，Ctrl+Enter 增强挪 P2。

D6 kill-9 演练仍是所有迁移之前的信心门；D2 已结清，`/login` 屏替换改为搭车进 P1-M2。

## 1. 范围界定

### 1.1 进下一阶段（8 组）

| 组 | 内容 | 依赖 |
| --- | --- | --- |
| A | D6 kill-9 + WAL 恢复演练（前置信心门） | 无，必须最先 |
| B | 三项强制前置技术债：R12 outbox、R9 threads.status、R8 parent 同 thread 约束 | A |
| C | 认证安全补齐：审计事件、三类限流、会话撤销、注销边界；**搭车 `/login` 屏呈现层替换（R18/R20）** | B |
| D | 投票/点赞（votes 表已建、score 路径就位） | B（R8） |
| E | 搜索 FTS5 | B |
| F | 多行编辑器（哨兵 `.` / Ctrl-D + 正文 Enter 换行） | B |
| G | 数据/会话运维：device_auth_states 清理、last_used_at、/keys 详情 | C |
| **H** | **邮箱注册/登录/密码找回（独立里程碑 P1-M5，L）** | **C（复用限流/审计/会话撤销）** |

### 1.2 明确不做（范围守门）

| 候选 | 不做的理由 |
| --- | --- |
| 节点（版块）创建与管理 | **放 P2**：顶层导航创建是治理动作，无管理员角色；六层 + 迁移的整里程碑体量；5 个种子版块对开源小社区够用，无扩张痛点证据 |
| karma / 等级展示、作者名投影（R11） | 留存价值未验证；R11 触显示契约，0.x 阶段不宜固化 |
| Ctrl+Enter 作为 P1 提交键 | **通用 SSH 终端不可可靠区分**（Enter / Shift+Enter / Ctrl+Enter 多为同一字节 `0x0D`）；挪 P2 做能力探测增强，且永不作为唯一方式 |
| 浏览器 TUI / Frame 增量 / WebSocket / RSS / 只读 API / 原生 CLI token / Passkey | 全部是**对外公共接口**；0.x 接口不稳 + 1 Mbps + 1000 在线未验证，现在做等于焊死契约 |
| P2 全部（含节点管理、Ctrl+Enter 增强）、P3 全部 | 依赖稳定对外契约或真实部署后的验证/运维增强 |

### 1.3 RICE 排序（相对判断，非精确数字；二轮已更新结论列）

| 排 | 候选 | R | I | C | E | 分 | 结论 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 认证安全补齐 | 5 | 3 | 1.0 | 3 | 5.00 | 进（含 `/login` 屏搭车） |
| 2 | 投票/点赞 | 4 | 2 | 0.8 | 2 | 3.20 | 进，依赖 R8 |
| 3 | 三项前置技术债 R12/R9/R8 | 4 | 2 | 1.0 | 3 | 2.67 | 进，**强制前置先于投票** |
| 4 | 搜索 FTS5 | 4 | 2 | 0.8 | 3 | 2.13 | 进 |
| 5 | 多行编辑器 | 3 | 1 | 1.0 | 2 | 1.50 | 进（哨兵方案，Ctrl+Enter 挪 P2） |
| 6 | 数据/会话运维 | 2 | 1 | 1.0 | 2 | 1.00 | 进，随安全项顺带 |
| 7 | karma / 作者名（R11） | 3 | 1 | 0.8 | 3 | 0.80 | 不做 |
| 8 | `/login` 屏替换 | 2 | 1 | 0.8 | 2 | 0.80 | **进（D2 结清，搭车 M2）** |
| 9 | 邮箱注册与恢复 | 3 | 2 | 0.5 | 5 | 0.60 | **进（用户拍板，独立 M5）** |
| 10 | 浏览器 TUI / Frame / WebSocket | 2 | 2 | 0.5 | 5 | 0.40 | 明确不做 |

### 1.4 决策台账（含二轮新增，均为已定）

| # | 决策 | 结论 | 代价 / 回报 |
| --- | --- | --- | --- |
| 1 | 先安全后体验 | 认证安全 + 前置债先行，对外接口冻结 | 等级/展示延后一迭代；换来开源部署敢上公网 |
| 2 | 前置债不走纯 RICE，强制插入 | R12/R9/R8 直接排最前 | 一段无用户可见交付的工期；避免体验项返工 |
| 3 | 公共接口冻结 | 不做浏览器/WebSocket/RSS/API/token/Passkey | 暂不覆盖浏览器用户；换回改成本可控 |
| 4 | D6 提前 | 所有 schema 迁移前先跑 kill-9 + WAL 恢复演练 | 少量测试工时；换来迁移的回滚信心 |
| 5 | **邮箱纳入 P1，自带 SMTP** | 独立 P1-M5；用 163 SMTP，配置放 `.env`；不接第三方邮件 API | L 体量 + 外部账号；换来账号自助与找回能力 |
| 6 | **节点管理放 P2** | P1 只写 slug 规则 / 归档不物理删 / seed 不可变三条 ADR | 不引管理员角色、不占 P1 工期 |
| 7 | **Ctrl+Enter 不做 P1 提交键** | P1 哨兵 `.` / Ctrl-D + 正文 Enter 换行；Ctrl+Enter 增强挪 P2 | 放弃"全终端 Ctrl+Enter"幻觉；换来零返工与全终端可达 |
| 8 | **D2 结清，`/login` 屏替换搭车 M2** | 用户确认 MVP 已实测可用，解禁 | 低风险 S–M，不单列里程碑 |

## 2. 技术 Spec

技术基线已对真实代码核实：module `github.com/jeeinn/motd` / Go 1.26.3 / `modernc.org/sqlite` 纯 Go（无 CGO，故 `-race` 不可用）；SQLite 3.54.1，FTS5 与 trigram 分词可用；迁移 0001–0003 单文件单事务、只向前、版本高于二进制拒绝启动；写队列单 worker + chan 256 + 3s + `_txlock=immediate` + `busy_timeout 5000`。

**关键事实**：`WriteTx` 已预留 `TouchThread(threadID, at, scoreDelta)` 的 `scoreDelta` 参数；`votes` 表有 `UNIQUE(user_id, target_type, target_id)`；`DomainEvent.ID` 注释已写明供 outbox 去重，`EventVoteCast` 已声明；`local_credentials` / `email_tokens` 表已建未写（`0001_init.sql`）；`cmd/server/line_echo.go:38` 的 `setSecret()` 已能隐藏回显。**P1 是「接线」不是重造**——唯一例外是邮箱投递，email 包目前只有 `doc.go`，需从零建通道。

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

### 2.7 多行编辑器（二轮定稿）

- **归属**：`application/input/composer.go`，**P1 主提交方式 = 独立一行 `.` 或 Ctrl-D**（`KeyCtrlD` = `0x04`，全终端可达）。
- **正文 Enter 换行**：`transport/ssh/decoder.go:175` 把 `\r`/`\n` 都映射 `KeyEnter`；编辑器态下 `KeyEnter` 进正文换行，不作为提交。标题仍是单行字段、Enter 确认——**两套 Enter 语义须在 UI 明示**（"标题 Enter 确认 / 正文 Enter 换行、单独一行 . 结束"）。
- **粘贴守门**：`decoder.go:109-127` 已把 bracketed paste 整块作为单个 `KeyPaste`（内部换行不拆 `KeyEnter`）；composer 只把 `KeyEnter`/`KeyCtrlD` 当控制键，`KeyPaste.Text` 整块进正文。新增 archtest 禁止把 paste 字节 re-split 成按键。
- **Ctrl+Enter 为何不做（P1）**：传统 VT/xterm/conhost 下 Enter / Shift+Enter / Ctrl+Enter 都发 `0x0D`、字节级不可区分；Windows Terminal 部分配置的 `\n` 是 Win32 特性、与 Ctrl+J 撞车、跨 SSH 不成立；只有 kitty keyboard protocol `ESC[13;5u` / xterm `modifyOtherKeys` `ESC[27;5;13~` 可区分，但需终端支持 + 服务端 `CSI ? u` 主动探测（`decoder.go:225-245` 目前把非方向键 CSI 降级 Unknown）。**增强方案挪 P2**（工作量 M，含跨终端矩阵接近 L），且 Ctrl-D 兜底常驻。
- **P1-M4 守卫**：composer 落地时加呈现文案测试，断言正文提示串含哨兵 / Ctrl-D、**不含 Ctrl+Enter**。

### 2.8 数据/会话运维

- `maintenance.go`：定时（启动 + 每小时）清 `device_auth_states` 终态过期行（`pending` 留到过期）。
- SSH 认证成功后**异步节流**（每钥 ≥ 5min）更新 `ssh_keys.last_used_at`，避免写放大。
- `/keys` 详情：扩 `KeyBindingReader.GetByFingerprint` + ViewModel + 终端渲染；`label` 仍空（无输入口，记为已知限制）。

### 2.9 邮箱注册/登录/找回（二轮新增，P1-M5）

- **通道（用户已定）**：**自带 SMTP，163 邮箱，配置放 `.env`**。不接第三方邮件 API。`email.Sender` 接口只写一个实现，不预留第二 Provider（孤儿能力）。163 SMTP 需用户提供**授权码**（非登录密码）；配置显式 resolve：`MOTD_EMAIL_ENABLED=false` 默认，开启时校验 `MOTD_SMTP_HOST` / `MOTD_SMTP_PORT` / `MOTD_SMTP_USER` / `MOTD_SMTP_PASS` / `MOTD_EMAIL_FROM`，缺哪项报哪项、不静默兜底。凭据不进仓库与 systemd unit（沿用 `client_id` 惯例）。
- **发信是网络调用，绝不进写事务**（3s 写超时扛不住 SMTP）：两段式——先 `Writer.Submit` 一个 Tx 写 `email_tokens(code_hash, purpose, expires_at, attempts=0)`，提交成功后**在事务外**发信；发信失败保留 token 自然过期 + 回"发送失败"，`/resend` 另写新 token 并把旧 token 置 `consumed_at`，**不做补偿删除**。发信有界并发 4、独立 10s 超时。
- **`/verify <code>` 单 Tx 原子消费**：`UPDATE email_tokens SET consumed_at=? WHERE id=? AND consumed_at IS NULL` + `RowsAffected==1` 抢消费 → 校验 purpose / expires_at / attempts<5 → 同 Tx 建 `users`/`identities`/`local_credentials`；错码在 Tx 内 `attempts++`。argon2id 19MiB / t=2 / p=1 依赖已在（`x/crypto v0.59.0`）；密码隐藏回显复用 `cmd/server/line_echo.go:38 setSecret()`。
- **`/recover`**：重置 hash 后撤销该用户全部 `sessions`，复用 M2 的 `SessionRevoker` 提交后调用模式；响应统一"若邮箱已注册则已发送"防枚举。R7 按 D-c：`primary_email` 仍无 UNIQUE + 守卫，命中占用回固定提示"先解绑旧号"，绝不自动合并。
- **命令面**：只做终端 `/register /verify /resend /recover /login email`；不做设计稿的 HTTP `POST /auth/email/*`（继续冻结对外接口）。
- **限流**：复用 M2 的 `application/auth/ratelimit.go`，键 `hash(email)`：60s 重发冷却、每小时 5 封、5 次尝试；叠加 IP 维度防滥号。
- **开放范围（用户已确认）**：**开放公开邮箱注册**。公开注册是公网 SSH 服务的滥号入口，必须配 IP/邮箱双限流（已含在上面）。不设邀请码机制。
- **守卫**：新增 `archtest/email_test.go` 校验发信不在写事务内、`email_tokens` 只存 `code_hash` 不存明文。

### 2.10 节点管理（二轮定稿：P1 只落不变量，不做实现）

- P1 在 ADR 固化三条：① slug 规则（形如 `/dev/`、字符白名单、层级深度）；② **禁止物理删**（`threads.node_id` 有 FK），有帖节点只 `status='archived'` 归档、列表过滤，与 posts/threads 软删一致；③ seed 节点不可删不可改名（`0002` 是 `INSERT OR IGNORE` + slug UNIQUE，重放会插回原名）。
- **不写无调用者的 `ValidateSlug` 函数**（孤儿能力）。P2 做 CRUD 时权限推荐 `MOTD_NODE_ADMIN_FINGERPRINTS` 公钥指纹白名单，比对**已认证会话的指纹**（不得用用户提交文本，遵循 §4.6）。

## 3. 里程碑与依赖顺序

P0 已占用 M0–M4 编号，本阶段用 **P1-M0…P1-M5** 避免混淆。二轮后主线顺序**不重排**，仅两处调整：`/login` 屏并入 M2；邮箱独立为末位 M5。

| 里程碑 | 内容 | 前置 | DoD |
| --- | --- | --- | --- |
| **P1-M0 前置信心门**（✅ 2026-11-19 完成） | kill-9 + WAL 恢复演练、`backup.sh` + `restore --verify` + `integrity_check`，写清 RPO/RTO 与备份六类 | 无 | **已达成**：隔离环境带数据硬杀 + WAL 重放 + 恢复精确回到备份点；证据与 RPO/RTO 见 `OPS.md §5` 2026-11-19 条；零 schema 变更 |
| **P1-M1 三项强制前置**（✅ 2026-11-19 完成） | 迁移 0004/0005/0006 + `WriteTx` 扩展 + outbox 调度器 + 守卫 | P1-M0 | **已达成**：v3 历史库迁移到 v6（status 回填 / 两条触发器 / outbox 表），回滚精确回 v3；全量测试 456→506 绿；证据见 `OPS.md §5` 2026-11-19 14:32 条、`ADR-028/029` |
| **P1-M2 认证安全 + `/login` 屏** | 限流/审计/密码变更与公钥解绑后会话撤销（≤1 刷新周期，复用现有 `SessionRevoker` 提交后调用模式）/注销边界；搭车 `/login` 屏替换（保持 RenderSystem 固定 / RenderUser 外部分离，ADR-021「token/device_code 不进 view」不回退） | P1-M1 | 可复现脚本；旧会话失效断言；守卫通过；`/login` 屏在真实链路上呈现四页形态 |
| **P1-M3 投票 + 搜索** | 投票幂等与分数一致；FTS5 空结果/特殊字符/1 Mbps | P1-M1 | 并发确定性用例（无 `-race`）；注入用例绿 |
| **P1-M4 编辑器 + 数据运维** | 多行粘贴、哨兵/Ctrl-D 提交、正文 Enter 换行、清理幂等、/keys 详情、1 Mbps 无超时 | P1-M2 / P1-M3 | 上述验收项全过；文案测试断言不含 Ctrl+Enter |
| **P1-M5 邮箱（L，独立）** | SMTP 通道 + `/register /verify /resend /recover /login email` + 会话撤销联动 + 双限流 | P1-M2 | 163 `.env` 授权码接入；验证码 hash 存储 + 5 次 + 60s 冷却；发信在写事务外；公开注册双限流生效 |

## 4. 文件影响

**新增**：
- `internal/application/forum/{vote.go, search.go, outbox.go}`
- `internal/application/auth/{ratelimit.go, audit.go, email_auth.go}`
- `internal/application/input/composer.go`
- `internal/infrastructure/email/{sender.go, smtp.go}`
- `internal/infrastructure/sqlite/{repo_vote.go, repo_search.go, repo_outbox.go, repo_auth_events.go, repo_email_token.go, maintenance.go}`
- `migrations/0004~0007.sql`（邮箱 token 表已在 0001，无新迁移）
- `archtest/{vote, outbox, ratelimit, fts, email}_test.go`

**修改**：
- `internal/application/forum/ports.go`（`WriteTx` 加 `GetVote`/`UpsertVote`/`AdjustScore`/`AppendOutbox`/`InsertPostWithParent`/`PostThreadID`）
- `internal/application/forum/service.go`、`config/config.go`、`transport/ssh/{decoder.go, session.go}`、`presentation/terminal/*`、`cmd/server/{wiring*.go, line_echo.go}`、`docs/OPS.md`（163 SMTP 配置与授权码注入）

## 5. 风险与缓解

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | 单写连接在限流/投票/审计下饱和 | 内存限流判定；审计异步批量 + 有界；队列深度入 `/healthz`；补 100/1000 连接压测 |
| R2 | FTS 索引漂移 / 注入 / 短词无结果 | 外部内容 + 触发器 + `fts-rebuild`；MATCH 引号转义 + LIKE 回退；负向注入守卫 |
| R3 | outbox 投递语义 | at-least-once + `event.ID` 幂等；状态 CAS；失败 observer；启动 pending 重扫；handler 绝不进写事务 |
| R4 | 0.x 冻结 vs 内部改动 | 全落内部包与终端命令，不动传输协议与 ViewModel 渲染无关性；CHANGELOG 标注 |
| R5 | `-race` 不可用（无 CGO/gcc） | 限流用互斥 + 注入时钟，`go test -count=10` 确定性验证；文档声明「未做竞态检测」，不声称 race-free |
| R6 | 迁移对历史库 | 先 P1-M0 演练；每迁移在含历史数据副本上跑 + 回滚；迁移前强制备份 |
| R7 | **163 SMTP 出站被限 / 授权码失效** | 部署前预检 (`smtp.163.com` 连通 + 认证)；发信在写事务外、有界并发 4、独立 10s 超时；失败保留 token 自然过期，回明确错误；`.env` 0600 root:root，不进仓库 |
| R8 | **公开邮箱注册被滥用** | IP + `hash(email)` 双限流（60s 冷却 / 每小时 5 封 / 5 次尝试）；验证码 hash 存储；公开注册为已确认决策，双限流是硬性前置 |
| R9 | **Ctrl+Enter 跨终端不可区分** | P1 不承诺；哨兵 `.` / Ctrl-D 常驻兜底；增强挪 P2 且需能力探测；UI 永不把 Ctrl+Enter 写成唯一方式 |

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
- 编辑器：文案测试断言正文提示不含 Ctrl+Enter；paste 不拆键守卫
- 邮箱（M5）：163 SMTP 连通与认证预检；验证码 hash 不落明文；`/resend` 作废旧 token；公开注册双限流复现脚本
- 每个里程碑：archtest 负向注入必须失败

**ADR**：从 ADR-028 起，按 `docs/DECISIONS.md` 既有 `## ADR-0NN` 五段格式追加，不另建单文件，沿用项目惯例。本阶段应新增：ADR-028 outbox 投递边界、ADR-029 投票 score 与 TouchThread 分离、ADR-030 节点 slug/归档/seed 不变量、ADR-031 正文提交键与终端兼容性。

## 7. 验收标准

1. 连续失败认证触发按 IP/用户/邮箱限流，超限返回明确错误且每次失败写入 `auth_events`；有脚本可复现。
2. 密码变更 / 公钥解绑后旧会话在 ≤1 个刷新周期内失效；注销后会话不可复用；`/login` 屏在真实链路呈现四页形态。
3. 三项前置无回归：outbox 有写入与投递用例；`threads.status` 迁移在含历史数据上跑通且可回滚；违反 `parent_post_id` 同 thread 约束的写入被拒；原 456 个测试全绿且新增覆盖上述路径。
4. 投票幂等且分数事务一致（重复投票不加分）；FTS5 搜索在空结果/特殊字符/模拟 1 Mbps 下返回正确、无注入。
5. 编辑器：正文 Enter 换行、单独一行 `.`/Ctrl-D 提交、标题 Enter 确认；粘贴含多行文本不误提交；提示文案不出现 Ctrl+Enter。
6. 邮箱：163 SMTP 经 `.env` 授权码可发信；验证码 hash 存储、5 次上限、60s 重发冷却；`/recover` 后旧会话失效；公开注册下 IP/邮箱双限流生效。

## 8. 已拍板事项（二轮定稿，非待办）

| # | 决策点 | 结论 |
| --- | --- | --- |
| **D-a** | 「每次失败写 auth_events」与单写连接容量冲突 | 正常负载逐条、洪峰批量 + 溢出 fail-closed（记 dropped 计数） |
| **D-b** | FTS 1–2 字中文 | trigram + LIKE 回退并转义 `%` / `_` |
| **D-c** | R7 同邮箱占用 | 维持 `primary_email` 无 UNIQUE + 守卫；命中占用固定提示"先解绑旧号"，绝不自动合并 |
| **D1** | D2 状态 | 用户确认 MVP 已实测 `/login github` 可用 → 结清；`/login` 屏替换搭车 M2 |
| **D2** | 邮箱通道 | 自带 SMTP（163），配置放 `.env`；第三方邮件 API 暂不接入 |
| **D3** | Ctrl+Enter | 不可靠，P1 不做主提交键；哨兵/Ctrl-D + 正文 Enter 换行；增强挪 P2 |
| **D4** | 节点管理 | 放 P2；P1 只落三条 ADR 不变量 |

| **D5** | 邮箱注册开放范围 | **开放公开注册**，配 IP + `hash(email)` 双限流；不设邀请码机制 |

## 9. 依赖与外部阻塞

- **D2 已结清**：用户确认 `/login github` 在 MVP 阶段已实测可用。`/login` 屏替换据此解禁、搭车 P1-M2；不再阻塞。
- **163 SMTP 凭据（新增外部依赖）**：需用户提供 163 邮箱账号 + **授权码**（非登录密码），写入 `.env`（0600 root:root，不进仓库与 unit）；部署前预检 `smtp.163.com` 连通与认证。通道未就位则 P1-M5 无法验收。
- **公开注册（已定）**：开放公开邮箱注册，IP/邮箱双限流为硬性前置（§2.9、R8）。
- **D6 WAL 恢复演练**阻塞所有 schema 迁移的安全落地，已前置 P1-M0。
- **未验证约束**：1000 在线 / 1 Mbps 尚未压测（P3），但限流与搜索须按此约束设计，禁全表扫描与大响应体。
