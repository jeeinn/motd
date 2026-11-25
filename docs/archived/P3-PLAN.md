# P3 阶段计划（稳定运营 + 占位缺口收口）

> 状态：**P3 全部完成（M1→M6 六项均已实施、发版）**。2026-11-21 收口：M1~M5 随 v0.5.0 发布；M6 彩蛋先落 v0.5.1 内容（未单独发 tag），与 0.6.0 扩展（签到/数字雨/Konami 解锁）一并随 **v0.6.0** 发布。全流程遵守"验证逻辑 → 自测 → ADR → 结构化汇报"，发版均经用户当次明确确认。ADR-056~062 已记，全部 commit / tag / push / Release 完成。
> 基线：v0.4.3（2026-11-20），测试 905，迁移止于 0009，ADR 止于 055。
> 收口账本：测试 **935**（+30），迁移止于 **0010**，ADR 止于 **062**，版本 v0.5.0 → v0.6.0。待执行观察项见 §11。
> 依据：`docs/design/命令行论坛整体设计方案-评审稿v0.2.md`（原始稿）、`docs/P2-PLAN.md`（Recent plan 2）、`docs/DECISIONS.md` ADR-001~062、代码核实。

## 0. 结论先行

用户 2026-11-20 拍板：**方向一（稳定运营线）为主线，A 类占位缺口一并补全，压测取最小集合**。

这里存在两套 "P3" 口径，先厘清（否则方案会错位）：

| 来源 | "P3" 定义 | 本方案处理 |
| --- | --- | --- |
| 原始设计稿 §6 | 稳定运营：压测、带宽监控、自动备份恢复、容量限制与降级、Passkey | ✅ 作为主线（Passkey 除外，见"明确不做"） |
| P2-PLAN §6/:205 | 被推后的功能集合：节点合并、白名单热重载、分权、@提及、任何 Web 面 | ❌ 不进本阶段，逐项给出理由 |

**P3 一句话范围**：让已在真实 VPS 运行的论坛"稳得住、数据不丢、展示的数字是真的"，用最小压测集给出核心闭环的延迟基线与并发正确性问题，不碰对外契约、不开新入口。

## 1. 范围与目标

| 优先级 | 工作项 | 体量 | 性质 | 里程碑 |
| --- | --- | --- | --- | --- |
| **P0** | 成就落 SQLite（替换内存 Store） | S | A 类缺口 | P3-M1 |
| **P0** | karma 计分接线（发帖/被赞 → 真实值） | M | A 类缺口 | P3-M2 |
| **P0** | CI 加 `-race` job | S | 并发正确性 | P3-M3 |
| **P1** | 最小压测集合（4 核心场景） | M | 稳定运营 | P3-M4 |
| **P1** | 自动备份调度 + cron/systemd 模板 + FTS 复核 | M | 稳定运营 | P3-M5 |
| P1 | 彩蛋命令接入（最终 6 条本地命令，/xkcd 走本地清单不联网） | M | A 类缺口 | P3-M6 ✅ |
| **不做** | 带宽监控的运行时指标（M4 后判定最小压测不需要字节级计量，留 P4；见 ADR-059） | — | 稳定运营 | P4 |
| **不做** | 节点合并 / 热重载 / 分权 / @提及 / Web 面 / Passkey | — | 见 §8 | — |

目标（可验收）：
1. 用户看到的 karma / 等级 / 成就**是真实的、重启不丢的**；
2. CI 每次跑全量测试带竞态检测，并发缺陷有发现通道；
3. 有一组可重复的压测基线（核心闭环延迟 + 背压行为），不追 1000 在线；
4. 备份有调度模板与演练记录，恢复流程补上 FTS 重建这一步。

## 2. P3-M1：成就落 SQLite

### 2.1 现状（代码核实）

- 端口 `AchievementStore` 在 `internal/application/user/achievement.go:42-46`（`Has` / `Grant` / `GrantedCount`）；写门面 `AchievementGate.Apply`（`:36-38`），校验 `AchievementService.Apply`（`:69-94`）。
- 内存实现 `MemoryAchievementStore`：`internal/application/user/memory_store.go:24`，`grantKey()`（`:41`），测试注入 `SetGrantFailure`（`:35`）、观测 `Entries()`（`:79`）。
- 装配点 `cmd/server/wiring.go:41-42` 注入 `NewMemoryAchievementStore()`；`forumRuntime.Store` 字段声明为具体类型 `*appuser.MemoryAchievementStore`（`:25`）。
- 表 `achievements` 已在 `0001_init.sql`：`id / user_id / code / unlocked_at`，`UNIQUE(user_id,code)`。

### 2.2 DoD

1. 新增 `internal/infrastructure/sqlite/repo_achievement.go` 实现 `appuser.AchievementStore`：`Has` 走读池；`Grant` 经 `Writer.Submit` 走唯一写连接，`INSERT ... ON CONFLICT(user_id, code) DO NOTHING` 保证幂等；`GrantedCount` 读池 `SELECT COUNT(*)`。
2. `wiring.go` 换 SQLite 实现；`forumRuntime.Store` 放宽为 `appuser.AchievementStore` 接口（保留测试观测手段，避免 `Entries()` 依赖测试编译失败）。
3. 端口仍留 application 侧，plugin 不 import infrastructure（ADR-015）。
4. **无新迁移**——0001 表结构已够用。
5. 新增 `repo_achievement_test.go`（Has/Grant 幂等/计数/失败不留半状态）+ 集成测试证明重启后成就仍在。
6. 全量测试绿，ADR-056 记录"成就持久化"决策。

## 3. P3-M2：karma 计分接线

### 3.1 现状（代码核实）

- 字段：DB `users.karma`（`0001_init.sql`，注册硬编码 0：`repo_login.go:125-126`）；领域 `domain/user/user.go:48`；视图 `viewmodel.StatusBar.Karma`（`viewmodel.go:129`）、`forum.ViewContext.Karma`（`forum/viewmodel.go:25`）；渲染 `terminal/view.go:226-227`。
- `Level()`：`domain/user/level.go:13-24`，阈值 L2=10 / L3=50 / L4=200，**只显示不参与权限**（ADR-014）。
- 真实缺口：`page_controller.go:146-156` 的 `viewContext()` **从不填 Karma** → 状态栏恒 0；`pageController.users` 是 `appuser.StatusReader`（只有 `Status`）。
- 写用例：`CreateThread`（`service.go:214`）、`CreatePost`（`:294`）、`applyVote`（`vote.go:57`，delta=新−旧），都在 `s.repos.Write` 事务闭包内；`WriteTx` 已有 `ThreadOwner/PostOwner`（`ports.go:213-216`）。
- ADR 约束：`ADR-013`（不臆造 P0 计分语义）、`ADR-014`（karma 只显示）、`ADR-031`（投票幂等/插件只感知首次）。

### 3.2 计分规则（用户 2026-11-20 已确认，按此实施）

| 动作 | karma | 说明 |
| --- | --- | --- |
| 发主题 | `+2` | 原创内容 |
| 发回复 | `+1` | 参与内容 |
| 被 upvote | `+1 / 票` | 与 `threads.score` 同向 |
| 被 downvote | **不扣作者** | 防恶意刷踩，仅 `threads.score` 变 |
| 撤票 / 改票 | 反向 / 按 delta 差 | 与投票幂等键一致（ADR-031） |
| 删除 / 隐藏内容 | 不回退 | 简单，不制造负状态 |
| 等级 | 仅显示 | L2=10/L3=50/L4=200，**绝不做权限谓词**（ADR-014） |

### 3.3 DoD

1. `WriteTx` 增 `AdjustKarma(ctx, userID, delta) error`（`ports.go`），SQLite 实现 `UPDATE users SET karma = karma + ? WHERE id = ?`（新 `repo_karma.go` 或并入 `repo_user.go`）。
2. `CreateThread/CreatePost` 事务内（插入后、`AppendOutbox` 前）给作者加分；`applyVote` 的 `delta≠0` 分支用已解析 threadID + `ThreadOwner/PostOwner` 给目标作者加分。
3. `StatusReader` 扩 `Karma(ctx, userID)`（或新增只读端口），`page_controller.go:146-156 viewContext()` 填充 Karma；同步所有 fake（`page_controller_test.go` / `login_screen_test.go` / `viewmodel_test.go`）。
4. **无新迁移**（列已存在，DEFAULT 0）。
5. 测试：`level_test.go`（阈值）、`viewmodel_test.go`（Karma→Level）、`service_test.go`、`vote_test.go`、`vote_integration_test.go`、`forum_integration_test.go`、`page_controller_test.go` 补 viewContext 断言；验证投票改票 karma 不重复加、downvote 不扣作者。
6. ADR-057 记录计分规则（用户拍板后）与"等级仅显示"重申。

## 4. P3-M3：CI 加 `-race` job

### 4.1 事实澄清（重要）

前一轮口头说"本机无 gcc 所以 -race 跑不了"——需更正：`go.mod` 只有 `modernc.org/sqlite`（纯 Go）与 `golang.org/x/crypto`，**无 CGO 依赖**；`-race` 的限制是 `CGO_ENABLED=1` + C 编译器，**本机 Windows 无 gcc 才跑不了**（`DECISIONS.md:99` 的报错），Linux CI（ubuntu-latest 预装 gcc）完全可行。纯 Go 依赖反而让 CI 开 CGO 无额外安装成本。

`PHASE1-PLAN.md:75` 写的"纯 Go（无 CGO，故 -race 不可用）"因果不成立，本次在 ADR 中澄清（不回改历史文档，只标注）。

### 4.2 DoD

1. `.github/workflows/ci.yml` **追加独立 `race` job**（不改现有 `check`），`CGO_ENABLED=1`、`GORACE=halt_on_error=1`、`go test -race -count=1 -timeout 15m ./...`。
2. cast 初期**不设为 required check**，观察 2–3 轮稳定后再提为门禁。
3. 预计耗时：首次编译 4–8 分钟、缓存命中 2–5 分钟；内存 5–10×，4C/16GB runner 足够。
4. 若暴露 data race，优先排查 19 个并发测试覆盖路径：`presence.Registry`（锁内 map）、`sqlite.Writer`（chan + atomic）、`plugin/events.DomainBus`、`SSH Server.conns/sessions` 双锁、`atomic.Pointer` 时区。
5. 时间敏感断言在 race 下若超时（如 `write_test.go:136` 的 60ms、`server_test.go:151` 的 3s、healthz 2s Ping），**调测试容差而非跳过 race**。
6. ADR-058 记录 race job 与文档因果澄清。

## 5. P3-M4：最小压测集合

### 5.1 原则

用户明确：**最小集合，对核心场景稳定测试即可**——不做 100/300/500/1000 全量。压测是 P3 质量活动，**不进 CI 默认 job**（共享 runner 延迟噪声大、避免时长失控），用 `workflow_dispatch` 或发版前本地跑。

复用现成钩子（无需新增导出 API）：`sqlite.Writer.Submit/Depth/Cap/Commits` + `WithQueueSize`（`write.go:160-165,56-77`）、`presence.Registry` 全套（`registry.go:68-251`）、`ssh.New/Serve/Addr + Config.MaxConns`（`server.go:102,146,234`）、`forum.Service.ListThreads/CreatePost`（`service.go:144,214,294`）、`probeMux`（`main.go:327-350`）。**现成 benchmark 0 个**，需新增。

### 5.2 四个核心场景

| # | 场景 | 并发档 | 通过标准（初值，首测后转回归基线） | 就近文件 |
| --- | --- | --- | --- | --- |
| 1 | SSH 连接建立/拆除 | 8 / 50（≤ MaxConns 100） | 握手 p99 ≤ 200ms、错误率 0、`presence.Online()`=成功数、goroutine 回落 | `internal/transport/ssh/*_bench_test.go` |
| 2 | 浏览列表（读多） | 16 / 64 | p99 ≤ 10ms、错误率 0、无 `SQLITE_BUSY` | `internal/application/forum/*_bench_test.go` |
| 3 | 发帖写队列（背压） | 8 / 32，`WithQueueSize(8)` | 成功数==`Commits()` 增量；`ErrQueueFull` 视为预期背压；无卡死 | `internal/infrastructure/sqlite/*_bench_test.go` |
| 4 | 并发读+渲染 + presence | 100 会话，读写 10:1 | 0 错误、`Active/Online` 一致、`Peak()==100`；配合 -race 0 race | `internal/presentation/terminal/*_bench_test.go` |

形式：Go `testing.B`，就近同包 `*_bench_test.go`（复用私有 test helper）；`-benchtime=3s -count=3` 取中位数，总时长约 1–2 分钟；可选 `scripts/bench.sh` 仅做参数封装。判定从绝对阈值过渡到"相对基线劣化 ≤ 20%"，避免机器噪声误判。

### 5.3 诚实口径（必须写进结论）

> 本轮最小压测给出的是核心闭环在**回环环境**下的功能正确性、并发安全性与相对延迟基线；它**不构成** 1 Mbps / 1000 在线的验收证据。1000 在线与 1 Mbps 仍是设计目标（设计稿 §1.2、§6；MVP-DELIVERY A3），需后续在目标机器、真实网络与提升后的 `MaxConns` 配置下分档实测后方可给出运行容量上限。

未验证项清单：1 Mbps 真实上下行（回环不过网卡、未计 SSH/TLS/心跳/帧开销）、1000 在线的 CPU/内存与 500–700MB 资源账本、扇出 6/60 MB/s、公网 RTT/丢包、小时级长稳、压测下 kill-9/WAL 恢复。

### 5.4 带宽监控（视 M4 结果定）

`/healthz` 已暴露 `queue`、`online`、`active`、`commits`、`audit_*`（`main.go:327-350`），但没有字节级带宽计量。最小做法是在探针加 `tx_bytes/rx_bytes` 原子计数（Session.Write/Read 处累加、纯内存），不改领域模型。

**结论（M4 后判定）**：最小压测不需要字节级带宽数据，本阶段**明确不做，留 P4**。ADR-059 已把"未验证项"（1 Mbps 真实上下行、1000 在线资源账本、扇出带宽、公网 RTT/丢包、长稳、压测下 kill-9/WAL）列为诚实口径，不制造虚假验收。

## 6. P3-M5：自动备份调度

### 6.1 现状（代码核实）

- API：`sqlite.Backup(ctx, pools, dir)`（`backup.go:62`，VACUUM INTO + sha256 sidecar）、`VerifyBackup`（`:99`，校验和 + integrity_check + criticalTables）、`Restore`（`:157`，先验、拒绝覆盖、清 WAL/SHM、复制后复检）。
- CLI：`cmd/motdctl/main.go` 的 `cmdBackup/cmdVerify/cmdRestore`（`:64/:93/:120`）；脚本 `scripts/backup.sh`、`scripts/restore.sh`。
- **仓库无任何 cron/systemd/timer 文件**；OPS §9.5 明确"首次部署默认不建立每日 cron/timer，由用户拍板"；OPS §2 已有 cron 示例（`:50-54`），§2 口径 RPO≤24h / RTO≤15min，演练实测技术 RTO≈1.3s / 端到端≈50.6s（`:309`）。
- **发现缺口**：OPS §4 恢复流程漏写 `motdctl fts-rebuild --db <path>`，而 OPS §3（`:65`）与 ADR-033 要求恢复后必须重建 FTS 索引，否则搜索返回空。

### 6.2 DoD

1. 新增 `deploy/motd-backup.cron`（或 `.service` + `.timer`，Calendar `*-*-* 03:00:00`），`ExecStart` 调 `/opt/motd/scripts/backup.sh`；cron 内显式 export `MOTD_DB_PATH / MOTD_BACKUP_DIR / MOTDCTL_BIN`（脚本不加载 `.env`，OPS `:81,:568`）。
2. 备份后立即 `VerifyBackup`，失败告警（脚本当前无告警逻辑，最小加一个退出码判断 + 日志）。
3. OPS §4 恢复流程补 `fts-rebuild` 步骤；§3 备份段引用新模板。
4. 演练沿用 §4/§5：停服 → 保留现场 → restore → fts-rebuild → 启动 → /healthz → 抽查 → 记录。
5. **保留策略不自动删除**（OPS §6），需人工清理；RPO/RTO 是否收紧到小时级由用户拍板（OPS `:311`）。
6. 无新迁移、无 Go 代码行为变更；ADR-059 记录调度方案与 FTS 步骤补齐。

## 7. P3-M6 ✅ 已完成：彩蛋命令接入

### 7.1 立项时的现状（代码核实，历史记录）

- 命令注册表 `application/command/registry.go`：`defaultCommands`，命令名**不含前导 `/`**，`validateCommand` 拒绝 `/` 开头；`Group` 仅排版。
- 插件命令 `plugin/registry.Command` 名字**带 `/`**，注释"P0 只登记不执行"；demo 在 `init()` 登记 `/fortune`。
- **为何不生效**：两套注册表独立——`Plugin.Commands` 从未合并进 `command.Default()`，且格式不兼容（带 `/` 会被 `validateCommand` 拒），`/fortune` 在 Parse 阶段即 `ErrUnknownCommand`。

### 7.2 实施结果（用户 2026-11-20 拍板纳入；分两阶段发布）

1. **命令合并**：实际落地为 `command.WithCommands(extra)` + cmd 装配层 `eggRegistry()`（剥前导 `/`、`Description→Summary`、`EggCommands()` 兜底防漂移），经 `inputHandler.withCommands()` 注入。**未采用**原计划的 `MergeDefaults` 命名；原因是依赖方向约束——command 不 import plugin，转换只能在 cmd 装配层（ADR-015）。
2. **六条本地彩蛋命令**（非原计划 5 条）：`/fortune` `/cowsay` `/xkcd` `/blame` `/sudo` `/karma`，另加 `/quit` 退出彩蛋。全部固定语料，经 `safe.RenderSystem` 输出，不回显参数（ADR-026）。
3. **`/xkcd` 走本地清单**（12 条编号+标题），**未做** HTTP 实现——用户拍板"本地清单"，避免网络失败态与新依赖。原 DoD 的 `application` 端口 + `httptest.Server` 方案作废。
4. **状态类成就**：新建 `plugin/social` 订阅 `PostCreated` / `VoteCast`；扩展 `ReadOnlyQuery` 加作者 ID 查询（只返回整数、无 PII）；`night_owl` 时段按 `MOTD_DISPLAY_TIMEZONE` 计算（用户 2026-11-20 追加要求）。
5. **0.6.0 扩展**（用户 2026-11-21 追加）：0010 迁移 + 每日签到 `streak_7` 成就、合规 `/matrix` 滚动雨帘、Konami 码按用户解锁 `/dev/null`（`ViewContext.UnlockedHiddenNodes` 注入方案）。
6. 决策记录：彩蛋接入 **ADR-061**，扩展三项 **ADR-062**；后续多色/节日主题评估见 `docs/ROADMAP.md R-01`。

## 8. 明确不做（本阶段）

| 项 | 理由 | 来源 |
| --- | --- | --- |
| 浏览器 TUI / Frame / WebSocket | 三个包仅 `doc.go` 空壳；开 3 个新入口与 0.x 冻结契约冲突，需先压测再谈 | PHASE1-PLAN §1.2、P2-PLAN:27 |
| @提及与未读 | `mentions` 表不存在、`RowMeta.Unread` 从不赋值；需推翻 R11 + 新 ADR + 触显示契约 | P2-PLAN:262,278 |
| 节点移动 / 合并 | 需快照 + dry-run + kill-9/WAL 演练级门槛；当前 5 seed 无痛点 | P2-PLAN:185、ADR-048 |
| 白名单 SIGHUP 热重载 | 分钟级窗口可接受，优先级低 | P2-PLAN R8 |
| 分权 / role / RBAC | 已明确否决；白名单 + 作者权两点足够 | P2-PLAN:160、ADR-049 |
| 任何 Web 面 / Web 管理页 | 重议触发四条一条不满足（P2-PLAN:169）；HTTP 面会焊死契约 | P2-PLAN:164-169 |
| Passkey / WebAuthn | 设计稿列 P2 增强，无真实需求证据 | 设计稿 §4.9 |
| RSS / 只读 API / JSON 输出 / AI 摘要 / 原生 CLI token / 主题 ASCII Art | 设计稿 P2 扩展能力，0.x 冻结；与"Agent 可参与"预留相关但留存价值未验证 | P2-PLAN:27 |
| 100/300/500/1000 全量压测 | 用户明确"最小集合、核心场景稳定测试即可" | 用户 2026-11-20 拍板 |

## 9. 风险与缓解

| # | 风险 | 缓解 |
| --- | --- | --- |
| R1 | karma 计分规则未经用户拍板即实现，违反 ADR-013"不臆造" | §3.2 规则表已列推荐默认值，**实施前必须用户确认**；ADR-057 记录 |
| R2 | -race 暴露真实 data race，阻塞发版 | race job 初期非 required，观察 2–3 轮；优先修并发缺陷而非跳过 |
| R3 | 时间敏感测试在 race 下超时 | 调容差不跳过 race（见 §4.2） |
| R4 | 彩蛋 Help 输出变化冲击固定文案断言 | 测试同步更新；`/xkcd` 失败固定文案 |
| R5 | cron 环境变量缺失导致备错库 | 显式 export + 备份后立即 Verify + 告警 |
| R6 | 恢复流程漏 fts-rebuild 致搜索空 | OPS §4 补步骤，演练必查 |
| R7 | 压测基线受机器噪声影响 | `-count=3` 取中位数 + 相对劣化 ≤20% 判定 |
| R8 | 自动备份 VACUUM INTO IO 峰值触发 ErrQueueFull | 保留策略不自动删除；Online Backup API 切换条件（OPS §7）仍未满足，不改 |

**风险收口（2026-11-21）**：R1 已按 §3.2 表经用户确认后实施（ADR-057）；R2 真实发生——race job 首次上线即抓到 4 处 data race（3 处测试假对象 + 1 处 `session.go` 生产代码真 bug），全部修复后 CI 全绿，并去掉 `halt_on_error=1` 让 detector 一轮报全（ADR-058）；R3/R4 未发生，测试同步更新通过；R5 已通过 `backup.sh` 内 verify + 模板 `EnvironmentFile=` 收口（ADR-060）；R6 已在 OPS §4 补 fts-rebuild 步骤（ADR-060）。R7/R8 为持续观察项。

## 10. 版本节奏（已执行完毕）

| 版本 | 内容 | 状态 |
| --- | --- | --- |
| v0.4.3 | 运维双形态与智能体部署指南（纯文档） | **已发布**（P3 基线） |
| **v0.5.0** | P3-M1 成就 + M2 karma + M3 race job + M4 压测 + M5 备份 | **已发布**（2026-11-20） |
| v0.5.1 | P3-M6 彩蛋接入（6 命令 + 2 状态成就） | 已实施，**未单独发 tag**——内容随 v0.6.0 一并发布 |
| **v0.6.0** | P3-M6 扩展：签到 7 天成就 + 合规数字雨 + Konami 解锁 /dev/null | **已发布**（2026-11-21） |
| P4 | @提及 / 浏览器 TUI / RSS/API / Passkey / Web 面 / 节点合并 | 不排期 |

> 三次发版（v0.5.0 / v0.6.0）均经用户当次明确确认，CI check + race 两 job 全绿后打 tag、建 Release（AGENTS.md §5）。本文档已随 0.6.0 之后的文档更新提交。

## 11. 收口状态与遗留观察项

**P3 实施已全部完成**，以下为待执行的观察/后续事项（不再是阻塞项）：

1. **里程碑回访（待执行）**：核对当前版本（v0.6.0）上线后 CI 的 race job 连续运行状态；确认备份 timer 首次执行成功且 `motdctl verify` 通过（OPS §2）。
2. **race job 转 required（待评估）**：目前非 required，已连续多轮全绿；观察 2–3 轮稳定后可提为强制门禁（ADR-058）。
3. **容量验收（未做，非欠账）**：1 Mbps / 1000 在线分档实测需目标机器与真实网络环境；按用户 2026-11-20 拍板，本阶段只做最小压测集，诚实口径见 ADR-059 / `ROADMAP.md R-03`。
4. **带宽监控留 P4**：M4 后判定不需要字节级计量（§5.4）。
5. **后续方向见 `docs/ROADMAP.md`**：终端多色扩展（含节日主题，R-01）、延后彩蛋（R-02）。
