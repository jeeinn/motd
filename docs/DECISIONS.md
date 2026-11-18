# 技术决策记录

> 只记录**已经做了的**决定和**已经写进代码的**与设计稿的偏差。
> 尚未拍板的事项记在 `P0-PLAN.md §6`，不在此处占位。

格式：编号 / 状态 / 结论 / 理由 / 代价。

---

## ADR-001 采纳模块化单体，依赖方向单向

- 状态：已采纳（依据评审稿 §2.2）
- 结论：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。不拆 Go Module，靠目录 + 后续的 archtest 约束。
- 理由：单机 1 核 1 GB 的前提下，微服务化只会增加部署面；但「单体」不等于「共享内部实现」，把依赖方向写死可以避免 domain 里长出 SQL。
- 代价：需要一条会失败的测试（P0-03）来守住边界，否则半年后形同虚设。

## ADR-002 统一 ViewModel，两个 Renderer 独立渲染

- 状态：已采纳（依据评审稿 §2.3）
- 结论：Application 层输出与 UI 无关的 ViewModel；终端渲染 ANSI，浏览器渲染语义 Frame，互不复用渲染结果。
- 理由：`View() → ANSI → ANSI Parser → Frame` 等于自研终端解析器，与「放弃 xterm.js 是为了省带宽」的初衷互相打脸。
- 代价：ViewModel 要预先设计得足够语义化（P0-11），字段既不能带颜色也不能带终端概念，早期改结构的成本高于直接渲染字符串。

## ADR-003 SQLite 拆双池，写连接恒为 1

- 状态：已实现（`internal/infrastructure/sqlite/db.go`）
- 结论：写池 `MaxOpenConns=1` + DSN `_txlock=immediate`；读池 `MaxOpenConns=NumCPU`（下限 2）；PRAGMA 通过 DSN `_pragma=` 逐连接注入，启动时查询自检。
- 理由：SQLite 的并发写只有「排队」一种解；WAL 下读不阻塞写，读写分池比让一个池既读又写更可预测。`_txlock=immediate` 让 `BEGIN` 阶段就拿写锁，避免「读事务升级为写事务」时出现不可重试的 `SQLITE_BUSY`。
- 代价：两个句柄指向同一文件，事务不能跨池；写池的 1 个连接一旦被长事务占住，所有写入串行排队——这正是 P0-06 写入队列要限制超时的原因。

## ADR-004 迁移只向前，版本高于二进制时拒绝启动

- 状态：已实现（`migrate.go`）
- 结论：`schema_migrations` 记录版本；每个迁移文件单事务；库版本高于二进制已知版本时拒绝启动，不尝试自动降级。
- 理由：SQLite 没有可靠的自动回滚；「启动时悄悄改表」在出问题时无法定位。回退路径统一走备份恢复（ADR-007）。
- 代价：升级必须严格按二进制版本推进；回滚 = 恢复备份，会丢备份点之后的数据（需在 OPS 文档里写清 RPO）。

## ADR-005 用户名规则：放宽为 Unicode 白名单，校验分两层

- 状态：**已确认**（V 先生 2026-11-18 决策）+ 已实现
- 结论：允许**中文（`\p{Han}`）、ASCII 字母、ASCII 数字、`_`、`-`**；长度 3–20 字符；**不允许任何空白字符**。大小写不敏感唯一保留（`COLLATE NOCASE`；中文无大小写概念，不受影响）。保留名规则不变。
- 与设计稿的差异：评审稿 §4.5 / §5.1 限定 `[a-z0-9_-]`，且 DDL 用 `GLOB '[a-z0-9_-]*'`。按本次决策放宽字符集、长度口径不变，原 GLOB 约束整条废弃。设计稿文件保留为评审稿快照，不回改。
- 为什么分两层：SQLite 内置无正则，`GLOB` 是字节级匹配、会误伤 UTF-8 中文；若注册自定义 SQL 函数来做 Unicode 校验，DDL 就依赖「每个连接必须先注册该函数」，外部工具（`sqlite3` CLI、恢复脚本）打开库写入会报 `no such function`——单机项目不该背这个耦合。因此：
  - `domain/user.ValidateUsername`（`^[\p{Han}a-zA-Z0-9_-]{3,20}$`，按 rune 计数）是**唯一事实来源**，任何写 `users.username` 的路径都必须先过它；
  - DDL `CHECK` 只兜底两件能用内置函数表达的事：长度 3–20（`length(TEXT)` 返回字符数，中文一字算一个，与 Go 按 rune 计数一致）、常见空白字符（`instr` + `char()`，含全角空格 U+3000）。
- 已知边界：NBSP、U+2000 系列等 Unicode 空白 DDL 拦不住，由 domain 层拦。`TestDDLMatchesDomainPolicy` 断言「domain 层接受的名字 DDL 一定不拒绝」，用来防止两层规则日后漂移。
- 代价：绕过 Application 层直接写库即可绕过字符类别校验。P0-06 的写入门面（不向 application 暴露裸 `*sql.DB`）是这条约束的执行保障。

## ADR-006 `/healthz` 是运维探针，不是业务端点

- 状态：已实现（`cmd/server/main.go`）
- 结论：P0 只暴露 `GET /healthz`，返回 `ok <version>`，并做一次真实 DB 读取。
- 理由：systemd / 反向代理 / 部署脚本需要一个不依赖业务的存活判据；同时它也是「HTTP 传输层存在但未承载业务」的最小证据。
- 代价：多一个需要维护的对外表面。规则：任何有业务语义的端点都不许挂在这个 mux 上，业务端点走 `internal/transport/http` 独立注册。

## ADR-007 备份用 `VACUUM INTO`，恢复演练优先于备份脚本

- 状态：已实现脚本方案（P0-07 待落地）
- 结论：P0 备份 = `VACUUM INTO` + sha256 + 时间戳；成功标准是**能恢复**（恢复后 `integrity_check` + 行数一致），不是生成了文件。
- 理由：`VACUUM INTO` 在 WAL 下产出一致性快照且不阻塞读，实现成本最低。
- 代价：大库时会有明显的 IO 峰值；当库体积或锁等待超过阈值时改评估 Online Backup API。阈值待 P0-27 压测后填入 OPS 文档。

## ADR-008 Go module path 定为 `github.com/jeeinn/motd`

- 状态：**已确定**（V 先生 2026-11-18）
- 结论：`go.mod` 中 `module github.com/jeeinn/motd`，内部 import 为 `github.com/jeeinn/motd/internal/...`；远端 `git@github.com:jeeinn/motd.git`。
- 理由：托管仓库已确定，域名形式便于后续被外部引用。趁初始骨架只有 2 处内部 import 时替换，成本最低。
- 代价：无。已全量替换并通过 `go build ./...` / `go vet ./...` / `go test ./...` 验证。

## ADR-009 P0 七项拍板结论（Q1–Q7）

- 状态：**全部同意**（V 先生 2026-11-18 明确答复「Q1–Q7 七项待拍板，我全部同意」）
- 结论：`P0-PLAN.md §6` 不再是待决策清单，以下是已生效的执行口径。

| # | 已拍板结论 | 落到哪里 |
| --- | --- | --- |
| Q1 | §8.2 四项架构决策全部采纳：模块化单体与 Domain 解耦、统一 ViewModel + 双 Renderer、所有持久化写入必经 Application 且插件禁直连数据库、在线状态与业务数据分离且心跳不逐次持久化 | ADR-001 / 002 / 003；P0-03 依赖守卫、P0-11 ViewModel、P0-12 插件骨架、P0-13 内存注册表 |
| Q2 | 托管仓库 `git@github.com:jeeinn/motd.git`，module path `github.com/jeeinn/motd` | ADR-008；`go.mod` 与全部 import |
| Q3 | P0 的 Device Flow **进程内直接调用 application 用例**；HTTP 端点（`/auth/device/start`、`/auth/device/poll`）留到 P1 浏览器接入时再开 | P0-15 / P0-16 / P0-19；P0 不引入 Web 会话与 CSRF 面 |
| Q4 | SSH 默认监听 `2222`，避免 root / `CAP_NET_BIND_SERVICE`；对外 22 端口由 `iptables` 或 `setcap` 转发 | P0-21、P0-28 部署脚本、D1 验收命令 |
| Q5 | 状态栏「等级」按 karma 分档常量：`0 / 10 / 50 / 200 → L1–L4`，放在 `domain/user` | P0-24 / P0-25 |
| Q6 | 发帖限流：每人 60 秒 1 帖、每小时 10 帖；命中时返回剩余冷却时间 | P0-10、P0-20 |
| Q7 | GitHub OAuth App 在**第二波（M1 结束）前**注册并启用 Device Flow，`client_id` 只放服务端环境变量 | P0-15 前置条件；联调用 `httptest` 假端点，不依赖真实凭据 |

- 影响：Q1 与 Q3 是结构性决策，若后续要改，M0 的依赖守卫与 M2 的认证链路需要重做，改动前应先出 diff 评审。

## ADR-010 写入队列：单 worker + 私有写门面

- 状态：已实现（P0-06，2026-11-18）
- 结论：
  - `chan WriteTask`（容量 256）由唯一 worker 串行消费；队列满立即返回 `ErrQueueFull`，不阻塞；
  - `Submit(ctx, TxFunc)` 是 application 层唯一写入入口。`Pools` 的 `*sql.DB` 字段改为私有，对外只暴露 `QueryRowContext` / `QueryContext` 只读门面与 `Ping`；
  - 每次写入受 `context.WithTimeout(task.ctx, 3s)` 约束；一个业务用例一个事务，**`Commit` 成功后才返回 nil**；
  - 可重试错误限 `SQLITE_BUSY` / `SQLITE_LOCKED`（优先用驱动 `Error.Code()`，包装错误退回字符串匹配），最多重试 3 次、指数退避；其余错误立即返回；
  - 停机时 `stopCh` 优先于队列，未开始的任务由 `drain()` 逐一回写 `ErrShuttingDown`，`Submit` 不会永久等待。
- 理由：§2.6 要求「队列保留但必须补边界」；ADR-005 的「用户名校验不可绕过」也需要一个执行点——只要 application 还能拿到裸 `*sql.DB`，任何约束都能被绕过。私有化写门面把约束从约定变成结构性保证。
- 代价：写入必须写成 `TxFunc` 闭包，比直接 `db.Exec` 略啰嗦；迁移、备份、测试因同包可直接访问私有字段，不受影响。
- 验证：12 条测试覆盖提交后才确认、失败回滚、队列满、超时回滚、重试分类与上限、200 并发、未启动 / 停机拒绝、停机排空；`/healthz` 输出 `queue=<depth>/<cap>` 便于观察背压。
- 已知限制：本机无 C 编译器（gcc），`go test -race` 未能执行（报 `cgo: C compiler "gcc" not found`）。并发正确性目前由 200 并发测试与代码审查保证；P3 压测阶段需在带 gcc 的环境补跑竞态检测。

## ADR-011 备份恢复：能恢复才算成功，且不自动删除

- 状态：已实现（P0-07，2026-11-18）+ 已完成首次实际演练
- 结论：
  - `VACUUM INTO` 产出一致性快照（WAL 下不停服、不阻塞读），每个备份带 `.sha256` sidecar；
  - `VerifyBackup` 做三件事：sha256 校验、`PRAGMA integrity_check`、关键表行数（`users/nodes/threads/posts/schema_migrations`）；任一失败即中止恢复；
  - `Restore` 先验后恢复，**目标已存在时拒绝覆盖**；恢复后再次以 `sqlite.Open` 自检（含 PRAGMA 基线）；
  - 运维工具独立成 `cmd/motdctl`（备份/校验/恢复），不依赖 `sqlite3` CLI —— 部署形态只有 Go 二进制 + SQLite 文件；
  - 备份脚本**不做任何自动删除**：定时脚本一次路径变量写错就可能清掉唯一回退点，保留清理只给人工核对命令。
- 理由：评审稿 §7 明确「备份的成功标准是能恢复，不是生成了文件」。把校验和恢复验收写进代码，而不是靠运维人员记得做。
- 已知限制：`VACUUM INTO` 会整库重写，大库 IO 峰值明显，备份期间写队列可能触发 `ErrQueueFull`。切换到 Online Backup API 的三个量化条件写在 `docs/OPS.md §7`，待 P0-27 压测给出基线后评估。
- 演练结论：2026-11-18 11:27 备份 → 校验 → 恢复到空目录 → 以恢复库启动服务，`migrations_applied=0`、探针正常，D7 达成。当前是空库，P0 产生真实数据后需重做带数据的演练。
- 附带修复：`flag` 包遇到首个位置参数即停止解析，`motdctl restore <file> --target <path>` 原会吞掉 `--target`；已做参数归一化并加回归测试。

## ADR-012 仓储端口定义在使用方，写入只暴露受限 `WriteTx`

- 状态：已实现（P0-08，2026-11-18）
- 结论：
  - `application/forum/ports.go` 声明 `NodeReader` / `ThreadReader` / `PostReader` / `UnitOfWork` / `WriteTx`，`infrastructure/sqlite/repo_forum.go` 实现；`infrastructure → application` 是合法方向，反向被禁止表拦住。
  - 端口只返回领域结构，不返回 `*sql.Rows`；`sql.ErrNoRows` 由仓储映射成 `forum.ErrNotFound`，领域层无需 import `database/sql`。
  - `UnitOfWork.Write(ctx, fn)` 是唯一写路径，`fn` 只拿到 `WriteTx`（受限方法集），拿不到 `*sql.Tx` / `*sql.DB`。一个用例对应一次 `Writer.Submit`。
  - 权限与限流查询也放进 `WriteTx`，保证「检查 + 写入」在同一事务快照内，不留 TOCTOU 窗口。
  - `archtest` 增加守卫：application 分组出现 `database/sql` 或 `modernc.org/sqlite` import 即测试失败（实测注入后退出码 1）。
- 理由：ADR-005 的「用户名校验不可绕过」和 ADR-010 的私有写门面，需要延伸到论坛域——只要 application 还能拿到裸连接，任何约束都能被绕过。把「不碰 SQL」变成会失败的测试，比写在 `doc.go` 里可靠。
- 代价：`Repos` 聚合三个端口时，Go 不允许同名不同签名的方法共存，故统一命名为 `ListNodes` / `ListThreads` / `ListPosts` / `GetThread`（比 `List` 更明确，也避免调用点靠参数猜语义）。
- 验证：9 条仓储回归（分页 / 双排序 / 软删除 / 事务原子性 / 首帖同事务 / 限流统计）+ 1 条真实链路集成测试（`forum.Service` 直驱 SQLite 仓储）。

## ADR-013 发帖限流按「动作」计数，`score` 与 `last_reply_at` 同条 UPDATE 维护

- 状态：已实现（P0-10，2026-11-18）
- 结论：
  - 发帖限流（Q6：60 秒 1 帖、1 小时 10 帖）**在写事务内查 DB 判定**，不走内存计数。P0-20 的内存计数只用于 Device Flow 发起频率、绑定尝试等认证类限流；发帖配额涉及「已提交内容」的正确性，进程重启不该丢失。
  - `CreateThread` 同事务写入 `threads` + 首帖 `posts`（§5.2 定义 Post 含首帖），两者共享同一个 `created_at`。`RecentWriteStats` 用 `COUNT(DISTINCT created_at)` 按**发帖动作**计数，否则一次发帖会消耗两份配额。60 秒冷却保证同一用户不可能在同一秒产生两次独立发帖，去重等价于按动作计数。
  - `0003_rate_limit_indexes.sql` 建 `(user_id, created_at)` 覆盖索引；`EXPLAIN QUERY PLAN` 实测 `SEARCH ... USING COVERING INDEX`，避免每次写入全表扫描。
  - `TouchThread` 用一条 UPDATE 同时维护 `last_reply_at` 与 `score = score + ?`。P0 没有投票来源，`scoreDelta` 传 0；P1 的投票可直接复用这条事务路径，**不臆造 P0 的计分语义**。
  - 列表分页用 keyset（排序键 + id）而非 OFFSET：深翻页代价恒定，且不会因翻页期间有新帖而重复或跳行；游标经 base64 编码为不透明字符串，解码时严格校验版本、排序一致性与锚点 ID。
- 已知边界：DDL 时间存 Unix 秒，**同一秒内的回复不会推进 `last_reply_at`**；排序用 `id` 作 tie-breaker，不影响正确性。测试用可控时钟跨秒验证「回复后 last_reply_at 必须变化」，真实运行同秒回复属预期行为。
- 另一处 schema 事实：`threads` 表**没有 `status` 列**（设计稿 §5.2 亦如此），因此 P0 不做主题删除；P1 启用主题软删除前必须补字段与迁移（记为 `P0-PLAN §7 R9`）。
- 验证：15 组用例层测试（校验 / 权限 / 事务 / 限流 / 游标）+ 真实链路集成测试 + `TestRateLimitIndexes` 执行计划断言。

## ADR-014 统一 ViewModel 独立成包，并用三层守护栏住「不含渲染概念」

- 状态：已实现（P0-11，2026-11-18）
- 结论：
  - **路径偏离计划**：类型定义放在 `internal/application/viewmodel`，`application/forum/viewmodel.go` 只负责「领域结果 → 语义状态」映射。计划原写 `application/forum/viewmodel.go`，但登录屏、帮助屏不属于论坛用例；若类型定义在 `forum` 包，M2 的 `auth` 与 M3 的页面要反向 import `forum` 才能构造屏幕，语义倒置。application 组内互引方向合法，不破坏 §2.2。
  - `Screen` 按设计稿 §2.3 的图（当前用户 / 列表 / 焦点 / 选中项 / 弹窗）补全 `Selection`、`Pagination`、`Detail`；`Row` 补 `Body`。计划只列 4 + 5 个字段，但详情屏既要主题正文又要回复列表，原字段集无法表达。
  - 所有字段都是值类型（整数 / 字符串 / 布尔 / 枚举 / 结构体 / 切片），**不含函数、指针、接口、通道与 `time.Time`**；时间统一存 Unix 秒，`RowMeta` 用结构而非拼好的字符串，格式由 Renderer 决定。`Notice.Code` 给机器读，`Notice.RetryAfterSec` 承接 Q6 限流冷却。
  - Q5 等级 `user.Level(karma)` 放在 `domain/user`（0/10/50/200 → L1–L4），**只用于显示，不参与权限判定**；冻结账号高 karma 仍由 `Status.CanPost()` 拒绝。
  - 映射入口：`ScreenNodes` / `ScreenThreads` / `ScreenThreadDetail` + `NoticeFromError` + `WithNotice`。`ViewContext` 由 transport 从 presence 与 Principal 组装传入，application 不 import presence / auth。
- 三重守卫（P0-11 验收项「出现任何样式/终端概念即不合格」）：
  1. `archtest` 静态扫描 `application/viewmodel` 的 AST：禁渲染库 import、渲染相关标识符、终端控制序列字面量；另扫整个 application 目录禁渲染库 import；
  2. 反射断言 `Screen` 递归字段只由允许类型组成，并用注入 `func` / 指针 / `any` / `chan` / `map` / 外部 `time.Time` 证明守卫会失败；
  3. 实测注入 `Color` / `Width` / `\x1b[31m` 后守卫退出码 1，移除后通过。
- 工程取舍：禁用词按**驼峰词段精确相等**匹配，而不是整名子串。子串匹配会把 `EmptyRows` 里的 `pty`、`Description` 里的 `esc` 误判成渲染概念；`NextCursor` 是分页游标（数据语义），不因含 `cursor` 被拦。长组合词（如 `CursorPos`）另用整名子串匹配。
- 已知边界：`RowMeta.AuthorID` 目前只保留语义 ID，仓储查询未 join `users`，`Author` 为空。P0-23 终端页面需要展示作者名时补仓储投影，字段已预留。
- 验证：16 条测试（状态模型 5 / 映射 5 / 等级 2 / 守卫 4），全量 `go build` / `go vet` / `go test ./...` 通过。

## ADR-015 领域事件与插件骨架：端口在使用方、成就不写库、两条通道分离

- 状态：已实现（P0-12，2026-11-18）
- 依赖方向解法（本轮最关键的结构决策）：
  - `application` 不能 import `plugin`（§2.2 禁止表）。因此 `application/forum/events.go` 声明 `DomainEventSink`，参数类型取自 `domain/event`；`plugin/events.DomainBus.Publish` 靠**结构化接口匹配**实现它，两边互不 import。
  - `plugin` 不能 import `infrastructure`。受限能力由 `cmd/server/wiring.go` 装配时注入 `registry.Context`（只读查询 + 成就申请门），cmd 是唯一能同时看到三层的分组。
  - `archtest` 新增两条守卫：扫描 `internal/plugin` 禁 `database/sql` / `modernc.org/sqlite` / `internal/infrastructure`；扫描 `internal/application` 禁 `internal/plugin`。实测注入后守卫退出码 1。
- 事件发布时机：`CreateThread` / `CreatePost` 在 `repos.Write` **成功返回之后**才发布 `EventPostCreated`（§2.4 流程图：提交 → 发布 → 订阅）。写入失败绝不发事件。事务已提交，投递失败**不回滚、不改变用例返回值**，只增 `Service.PublishFailures()` 并通过 `WithPublishObserver` 上报，避免调用方误判为发帖失败而重试出重复帖。
- 两条通道不共用总线/类型/语义：
  - `DomainBus`：同步派发，校验事件自洽性，收集全部订阅者错误（不短路），`Published` / `Failures` 可观测；
  - `InteractionBus`：带缓冲 channel + 非阻塞 `TryPublish`，满即丢弃并计 `Dropped`；`Close` 先与 send 互斥再排空在途事件，使 `Handled()` 为确定值。心跳、按键、连接类信号绝不写 SQLite（§2.6）。
- 插件能力边界：`registry.Context` 只有 `ReadOnlyQuery`（当前仅 `ThreadTitle`）与 `AchievementGate`。`init()` 编译期注册、无动态加载；`All()` 返回防御性副本，重名 / 空名 / 缺 handler / 非法订阅 / 无名命令一律拒绝。
- 成就链路：`demo` 插件订阅 `PostCreated` → 取主题标题作 evidence → `application/user.AchievementService.Apply` 四道校验（代码白名单、用户 ID、账号 `active`、未解锁）→ `AchievementStore.Grant`。重复申请返回 `Already=true` 而非错误。
- **P0 不写 SQLite**：成就系统属 P1 范围（P0-PLAN §1.1），`MemoryAchievementStore` 只是可观测占位；`achievements` 表已建但本阶段不写。P1 换成读写该表的实现即可，`application/user` 校验逻辑不变。
- 已知边界：
  1. 领域事件的「可靠」在 P0 仅限**单进程内同步投递**；进程崩溃会丢未处理事件。P1 需补 `domain_event_outbox` 表，与业务写入同事务落库后再投递（记为 P0-PLAN §7 R12）。
  2. 成就状态在内存，重启即失；这是 P0 未启用成就系统的预期行为，不影响 D1–D7。
- 验证：新增 34 条测试（事件模型 2 / 两条通道 8 / 注册表 4 / demo 4 / 成就校验 5 / 用例发布 6 / 守卫 3 / 端到端装配 2）；全量 `go build` / `go vet` / `go test ./...` 通过；从零启动实测 `nodes=5 plugins=1 post_created_subscribers=1`、`/healthz → ok dev queue=0/256`；端到端测试用真实 SQLite + 真实队列 + 真实插件链路验证发帖后成就解锁且只解锁一次。

## ADR-016 在线状态纯内存：静态守卫 + 真实写库计数双重验收

- 状态：已实现（P0-13，2026-11-18），M1 关死
- 结论：
  - `application/presence.Registry` 维护在线连接数、每会话状态（认证中 / 浏览 / 编辑 / 空闲）、20 秒心跳超时、历史峰值与快照副本；时钟与超时可注入，测试不靠真等 20 秒。`Reap()` 返回排序后的超时 ID，由 `cmd/server` 每 10 秒调用一次（随 `ctx` 取消退出）。
  - `Active` 与 `Online` **独立统计**：Active 只认「心跳窗口内且处于 Browsing / Composing」的会话；认证中与 Idle 不计入。否则两者恒等，§2.7 要求的「同时活跃独立统计」就失去意义。
  - **不写 SQLite 用两层证明**：
    1. 静态守卫 `archtest` 扫描 `internal/application/presence`，禁 `database/sql` / `modernc.org/sqlite` / `internal/infrastructure` import（实测注入后退出码 1）——编译期就不可能拿到写句柄；
    2. 运行时集成测试给 `sqlite.Writer` 加 `Commits()`（只统计 `Commit` 成功的事务），先做一次真实写入让计数变为 1，再驱动 100 个连接上下线 + 心跳 + 状态切换 + `Reap`，断言提交数**增量严格为 0**。计数器本身另有回归：失败事务与唯一约束冲突都不计数，避免「恒为 0」造成假绿。
- `/healthz` 输出扩展为 `ok <version> queue=<depth>/<cap> online=<n> active=<n> commits=<n>`。ADR-006 的「纯运维探针、不承载业务」语义不变，只是增加可观测字段。
- 已知边界：在线状态是内存态，进程重启归零——这正是设计稿 §2.6 的意图（在线状态不是业务数据）。M3 的 SSH 入口才会真正往注册表登记连接；当前阶段为空表。
- 验证：新增 11 条测试（注册表 7 / 守卫 2 / 写库计数集成 2）；全量 `go build` / `go vet` / `go test ./...` 通过；从零启动实测 `/healthz → ok dev queue=0/256 online=0 active=0 commits=0`。

## ADR-017 SSH 入口前置于 M2：身份解析用 application 端口解耦

- 状态：已实现（P0-21，2026-11-18），第三波 M3 起始
- 结论：
  - **依赖最小集**：只用 `golang.org/x/crypto/ssh`（v0.59.0），不引 `gliderlabs/ssh` 等封装层；host key 用标准库 `ed25519` + PKCS#8 PEM 落盘，不增加第二个依赖。
  - **波次冲突的解法**：§5 把第三波定为 M3 的 21→23，但依赖图里 `P0-14 Principal → P0-21 SSH`。为使 SSH 入口不等 M2，把 `Principal` 与 `Resolver` 端口**前置定义**在 `application/auth/principal.go`（P0-14 的接口部分），实现仍归 P0-14。`transport/ssh` 只依赖 `auth.Resolver`，M2 换实现不改协议层。
  - `cmd/server` 注入 `guestResolver` 占位：账号体系未落地前，凡通过公钥挑战（服务端已验证对方持有私钥）的连接一律按访客放行。这与 D1「以访客身份浏览」一致，也不违反 P0-18——指纹由 `PublicKeyCallback` 从服务端持有的 `ssh.PublicKey` 算出并放进 `ssh.Permissions.Extensions`，不接受客户端提交文本。
  - 仅公钥认证：`PasswordCallback` / `KeyboardInteractiveCallback` 为 nil，`NoClientAuth=false`，`root` 用户名在回调中显式拒绝；`MaxAuthTries=3`。
  - 容量门禁：`chan struct{}` 作连接槽（默认 100），accept 前先判满，满员在握手前写明文提示并关闭；`BannerCallback` 与 `PublicKeyCallback` 再做两层兜底，覆盖 accept 与占槽之间的竞态窗口。
  - 会话收口用**连接级** `connDone`（`chans` 关闭即连接断开），不用「channel 读到 EOF」：客户端可以只 `CloseWrite` 输入方向，此时服务端仍应继续输出，EOF 不等于断线。空闲超时（默认 10m）与 ctx 取消是另外两条收口路径，三条都汇到幂等的 `Session.Close()`。
  - 每次输入经 `Session.Read` 刷新空闲计时器与 `presence.Touch`，**不写 SQLite**（ADR-016）。PTY `pty-req` / `window-change` 解析后存进 `Session`，供 P0-24 页面排版使用。
- 理由：Q4 定 2222 端口、Q3 定 P0 不开 HTTP 端点；SSH 是唯一业务入口，必须尽早用真实终端验证渲染边界（§5 第三波原意）。把身份解析做成端口而非具体实现，是让两条波次互不阻塞的唯一干净做法。
- 已知边界：
  1. 满员拒绝在握手前写明文，OpenSSH 客户端会报 `Bad remote protocol version identification`，提示文本可见但不属标准 SSH 错误码；这是「不进握手、快速失败」的代价，客户端可读性由服务端日志 + banner 两层兜底。
  2. `guestResolver` 不查 `ssh_keys` 表，认证三状态中的「已吊销拒绝」要等 P0-14；当前通过公钥挑战即放行。
  3. `greetingHandler` 只输出服务端固定文本，不回显任何用户字节——回显会绕过 P0-23 的安全渲染。P0-22 / P0-24 替换该 handler。
  4. host key 权限 0600 只在非 Windows 显式 `chmod`；部署目标为 Linux/systemd，本机 Windows 开发不受影响。
- 验证：11 条测试（host key 稳定 / 权限 / 损坏拒绝、配置校验、仅公钥认证、匿名拒绝、root / 吊销 / 普通解析失败分类、Permissions 还原、端到端 presence 上下线 + PTY + window-change、空闲超时、满员拒绝）；全量 `go build` / `go vet` / `go test ./...` 通过。
