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

## ADR-018 输入归一化：命令表唯一入口，用户文本零回显

- 状态：已实现（P0-22，2026-11-18）
- 结论：
  - **类型分层**：`application/input.KeyMsg`（语义按键）定义在 application（使用方），`transport/ssh.KeyDecoder` 只负责字节解码；`application/command.Registry` 是命令与键位的唯一事实来源。原因：application 不得 import transport（§2.2 禁止表），把事件类型放 application 才能让命令表与解码器共用一套类型。
  - `Help()` 由 `All()` + `Bindings()` 生成，页面不得手写第二份命令清单（§3.2 验收项）。默认 8 条 P0 命令 + 10 条键位说明，`?` 明确映射到 `/help`；`New()` 对重名 / 空名 / 非法字符 / 别名冲突 / 参数区间矛盾一律拒绝。
  - 解码器处理跨 Read 分块：`pending` 保留未决的 UTF-8 多字节与 CSI/OSC/SS3 序列。孤立 ESC 用 **25ms 决策窗口**（`Poll()`）+ 下次 `Feed` 开头合并消歧，既不会把 `\x1b[A` 拆成 Esc + `[` + `A`，也不会让用户单按 Esc 迟迟无响应。
  - 所有控制序列（清屏 `\x1b[2J`、颜色 `\x1b[31m`、OSC 标题 / OSC 52 剪贴板）一律降级为 `KeyUnknown`，**不产生任何语义动作**；方向键是唯一被提升为语义按键的 CSI。粘贴块按 bracketed paste 还原为纯文本（含其中的 ESC 字符），限长 8 KiB。
  - **用户文本零回显**：`KeyMsg.Raw` / `Text` / `Invocation.Args` 都不写回终端；`Registry.Parse` 的错误信息也不携带用户提交的原文。`formatResult` 只输出服务端生成的固定文本或注册表白名单里的命令名。这是 P0-23 安全渲染落地前不留旁路的关键。
  - `applyKey` 抽成纯状态机（不碰 IO），命令行的收集 / 退格 / 取消 / 提交无需 SSH 会话即可回归。
- 理由：§3.2 要求命令表唯一入口；§3.5 要求用户内容不得驱动终端控制序列。P0-22 先做到「用户文本不进入输出流」，P0-23 再引入「净化后才允许进入」——顺序反了就会先开一个渲染旁路再补。
- 已知边界：
  1. 行编辑器只做收集与提交，不回显输入缓冲；P0-24 接入编辑器时必须经过 P0-23 的白名单渲染，不能直接 `Write(m.Raw)`。
  2. ESC 决策窗口 25ms 是经验值；极端网络抖动下跨包箭头序列若间隔超过 25ms 会被判成独立 Esc。P1 可用终端能力协商（如 kitty keyboard protocol）消除该歧义。
  3. `application/command` 只描述元数据，业务动作由 P0-24 页面控制器调用 application 用例；M2 的 `/login`、P0-18 的 `/keys` 子命令届时补齐。
- 验证：新增 18 条测试（解码器 9：单键 / 方向键 / ESC 三种歧义 / 跨 chunk UTF-8 / 控制序列 / 粘贴完整与分片与限长；注册表 6：命令集 / 帮助由注册表生成 / 解析与别名 / 错误信息不回显 / 非法定义 / 防御性副本；会话层 3：行编辑状态机 / 取消与退出 / `formatResult` 恶意输入）；全量 `go build` / `go vet` / `go test ./...` 通过。

## ADR-019 终端安全渲染：三段分离、唯一 ANSI 常量表、两条路径

- 状态：已实现（P0-23，2026-11-18），M3 第三波收尾
- 结论：
  - **三段严格分离**：`SanitizeText`（净化）→ `ParseInline`（白名单 Markdown）→ `renderInline`（受控 ANSI），任何一段都不能被跳过。
  - 净化规则：ESC / C0 / DEL 转成 U+2400 系列可见控制图示（其后 CSI/OSC 参数字节退化为普通文本）；C1（含 `\u009b` 这种无需 ESC 的 CSI 起始）与双向文本 / 不可见格式字符一律剥离；CR / CRLF 统一成 LF（杜绝 `\r` 覆盖同行内容）；Tab 按 8 列展开。
  - 白名单 Markdown 只识别 `**粗体**` / `*斜体*` / `` `代码` `` / `[文字](链接)`；链接协议白名单 `http` / `https` / `ssh`（大小写不敏感），`file:` / `javascript:` / `data:` / 协议相对一律降级为纯文本，URL 不进 `Href`、不生成 OSC 8 超链接。
  - **受控 ANSI**：`ansi.go` 是输出中唯一允许的 ESC 常量表，且只允许纯 SGR（`\x1b[...m`）。archtest 静态扫描本包其余非测试文件，出现任何 `\x1b` 字面量即失败；常量本身也必须通过纯 SGR 校验；负向注入实测。输出侧 `assertOnlySGR` 逐段扫描，非 SGR 的 CSI（清屏 `2J`、光标 `H/K`、备用屏 `?1049h`、改尺寸 `t`）与全部 OSC 一律拦下。
  - **两条路径不共用 sanitize**：`RenderUser` 净化用户内容；`RenderSystem` 对服务端可信文本做更严格校验，含控制字符直接返回 `ErrUnsafeSystemText` 而不是替调用方擦除。archtest 用 AST 证明 `RenderSystem` 函数体内没有 `SanitizeText` 调用。
  - 折行而非截断：`WrapText` 按显示宽度折行（`DisplayWidth` 覆盖常见 CJK / emoji / 组合字符），内容一个 rune 不丢。
- 理由：§3.5 要求从「过滤」升级为「分层安全渲染」，并明确「服务端生成的 ANSI 与用户提交的 ANSI 走不同函数」。把唯一 ANSI 常量表做成会失败的静态守卫，比靠 code review 可靠；恶意样本集固化成测试，避免只靠人工看终端。
- 实测发现并修复的真实缺陷：`[2J␛]0;pwned **粗体** [x](...)` 这类以 `[` 开头但并非链接的文本，原实现会把第一个 `[` 到后面某个 `](` 之间的整段吞成纯文本，连带吞掉加粗。已加「链接标签不得含 `[]` / 换行 / 超长」约束并固化 `TestLinkParserDoesNotSwallowUnrelatedBrackets`。
- 已知边界：
  1. `RenderUser` 输出 LF（用户 `\r` 已归一），P0-24 页面写入 PTY 前统一做 LF→CRLF；该转换属于页面层，不放进净化层，避免把用户 CR 与控制序列混为一谈。
  2. 宽字符宽度表覆盖常见 CJK / emoji 区间，未覆盖字符按 1 列；P0 不引 `x/text/width`（依赖最小集），P1 可换标准实现。
  3. `Width > 0` 时先折行再解析，跨折行边界的 Markdown 标记按纯文本处理，不产生错误渲染。
  4. P0-23 是渲染库，P0-24 页面才接入链路；接入时用户内容必须走 `RenderUser`、状态栏走 `RenderSystem`，禁止直接 `Write(m.Raw)`。
- 验证：22 条恶意样本 + 净化 / 折行 / 宽度 / Markdown / 渲染 / 静态守卫共 20 余条测试；全量 `go build` / `go vet` / `go test ./...` 通过。

## ADR-020 身份解析三状态：端口在 application，查库在 infrastructure，失败不降级

- 状态：已实现（P0-14，2026-11-18），第四波 M2 首项
- 结论：
  - `application/auth.KeyLookup` 声明「指纹 → `KeyStatus{Found, Revoked, UserID}`」只读端口；`infrastructure/sqlite.SSHKeyLookup` 参数化查 `ssh_keys`、只走读池；`auth.NewResolver` 完成三状态映射。`transport/ssh` 只依赖 P0-21 已有的 `Resolver` 接口，**协议层零改动**（ADR-017 兑现）。
  - 三状态严格按 §4.6：未命中 → `Guest(fp)` 只读；命中未吊销 → `KindUser + UserID`；命中已吊销 → `ErrKeyRevoked`，由 `publicKeyCallback` 转成拒绝认证。
  - **失败关闭**：查库错误一律返回错误并拒绝认证，绝不降级成访客放行；空指纹与格式非法在查库前拦下；命中但 `UserID <= 0` 视为脏数据报错。理由：一次 DB 抖动不该让已绑定用户「变成」访客，更不能让实现缺陷把吊销态漏成放行。
  - **指纹来源约束**：指纹由认证阶段服务端持有的 `ssh.PublicKey` 经 `gossh.FingerprintSHA256` 算出，格式 `SHA256:` + 43 字符 RawStdEncoding。`application/auth` 只用标准库校验格式、不 import SSH 库；archtest 静态守卫禁止该包出现 SSH 库 import 或 `ParseAuthorizedKey` / `MarshalAuthorizedKey` / `FingerprintSHA256` 标识符（负向注入实测）。`transport/ssh/auth_contract_test.go` 用真实 ed25519 key 验证两侧格式一致，防止规则写错把全部连接拒掉。
  - `forum.ActorFromPrincipal(p, status)` 把认证快照映射成用例层 Actor；账号状态由调用方从 `UserStatusReader` 取得，写用例仍会在事务内二次复核（P0-10），认证层不缓存。验收项「访客调用写用例返回明确权限错误而非 panic」由 `checkWrite()` 的 `ErrReadOnly` 落实。
- 理由：`ssh_keys` 表在 `0001_init.sql` 已存在，P0-14 不需要新迁移；把「查库」压在一个只读端口上，可以让 P0-18 的绑定 / 解绑写入继续走写入队列，不出现第二条写路径。端口前置还让第三波先做 SSH 入口时不必等 M2。
- 已知边界：
  1. 账号 `status` 不参与本项判定，由 P0-10 写用例在事务内二次复核，避免认证快照过期造成越权。
  2. 本项只提供只读端口；公钥绑定、解绑、一钥一号的唯一约束 enforcement 属于 P0-18。
  3. 已吊销公钥的**已有连接**按设计稿「按策略处理」留给 P1 会话撤销，本项只拦新认证。
- 实测发现并修复的真实缺陷：首轮全量回归中 `repo_auth_test.go` 手写 43 字符指纹常量时 `authFPRevoked` 少了一个字符，被 `ValidFingerprint` 正确拦下，`TestResolverWithRealSQLite/已吊销→拒绝` 报「指纹格式非法」而非预期的 `ErrKeyRevoked`。已改为 `strings.Repeat("B", 43)` 程序化生成，不再手数长度；校验逻辑本身无需改动，反而证明它在真实链路上确实生效。
- 验证：`application/auth` 三状态 / 非法指纹 / 查库失败 / 脏 UserID / nil 端口共 7 组；`application/forum` Actor 映射 2 组；`infrastructure/sqlite` 查库三状态 / 注入串 / Resolver 端到端 3 组；`internal/archtest` 静态守卫 + 负向注入 2 组；`transport/ssh` 指纹格式契约 1 组。全量 `go build` / `go vet` / `go test ./...` 通过。

## ADR-021 GitHub Device Flow 客户端：端口在 application，协议在 infrastructure，零新依赖

- 状态：已实现（P0-15，2026-11-18），第四波 M2 第二项
- 结论：
  - **分层**：`application/auth` 定义 `DeviceFlowClient` 端口、`DeviceCode` / `AccessToken` / `GitHubUser` / `GitHubEmail` 值对象、`PollStatus` 决策分类与 `ClassifyDeviceError` / `NextInterval` 两个纯函数；`infrastructure/github.Client` 实现 4 个端点。依赖方向仍是 infrastructure → application，与 ADR-020 的 `KeyLookup` 同一口径。`cmd/server` 只依赖端口，HTTP 细节不进装配主文件（抽到 `github_wiring.go`）。
  - **四个端点**：`POST /login/device/code`、`POST /login/oauth/access_token`、`GET /user`、`GET /user/emails`。端点地址抽成可注入的 `Endpoints`，生产指向 `github.com` / `api.github.com`，测试全部指向 `httptest.Server`。Q3 兑现：P0 全程进程内调用，不开任何业务 HTTP 端点，不绑定 transport。
  - **零新增第三方依赖**：只用 `net/http` / `net/url` / `encoding/json` 等标准库，`go.mod` 一行未改（第三波引入 `x/crypto/ssh` 后继续按最小集）。
  - **换 token 不需要 `client_secret`**（§7 R1）：两个 POST 的请求体只有 `client_id` / `scope`、`client_id` / `device_code` / `grant_type` 三组参数；`grant_type` 固定为 `urn:ietf:params:oauth:grant-type:device_code`，`scope` 固定为 `read:user user:email`（接口不暴露这两个参数，调用方无法放大权限）。archtest 静态守卫禁止 `infrastructure/github` 出现 `client_secret` 标识符或字符串字面量。
  - **8 个错误码显式分类**：`authorization_pending` → 继续；`slow_down` → 延长间隔；`expired_token` / `token_expired` → 重新申请；`access_denied` / `device_flow_disabled` / `incorrect_client_credentials` / `incorrect_device_code` / `unsupported_grant_type` → 终止；未识别码 → `PollUnknown` 终止且不吞。`PollError.Error()` 只返回净化后的中文提示，不带响应体 / token / client_id。设计稿只列 4 个，§7 R3 补 3 个，本轮对照官方文档又补 `unsupported_grant_type`，并兼容官方表格与正文对过期码的两个名字。
  - **退避语义**（§7 R2）：`slow_down` 优先取响应携带的新 `interval`；未带时在上次基础上加 5 秒；其余情况不缩小。`NextInterval` 返回值恒 `>= max(last, 5)`，客户端本身不 sleep，轮询节奏由 P0-16 状态机驱动；`DeviceCode.PollInterval()` 让调用方只能拿到合法间隔，验收项「轮询间隔不得小于 interval」有可测落点。
  - **两套 `Accept` 头**（§7 R14）：Device Flow 两个端点默认返回 form-urlencoded，必须显式 `Accept: application/json`；`/user` 与 `/user/emails` 用官方推荐的 `application/vnd.github+json` + `Authorization: Bearer`。混用会把表单文本当成成功响应，已用负向测试锁死。
  - **失败关闭与不泄漏**：`client_id` 缺失时客户端仍可构造（本机 / `httptest` 无需真实凭据），但两个 POST 返回 `auth.ErrClientIDMissing` 且不发出请求，启动不 panic；每次请求带 10s 硬超时（不依赖调用方 `http.Client` 是否设 Timeout）；响应体限 1 MiB；错误消息不携带响应体原文；`/user` 缺少合法数字 `id` 直接报错（§4.3 身份主键）。
  - **装配**：`cmd/server` 注入 `newDeviceFlowClient(cfg)` 到 `inputHandler`（`withDeviceFlow` 保持 `newInputHandler()` 无参签名、现有会话层测试零改动）；启动日志只报 `client_id_configured` 布尔值与 scope，**不打印凭据值**（OPS §3）。P0-16 状态机 / P0-19 `/login` 直接消费该端口。
- 理由：设计稿 §4.4 明确要求「实际编码前针对 GitHub 当前官方文档独立复核」，本轮复核得到 R13 / R14 两处计划外偏差，必须落成实现与测试而不是留在评审稿里。把错误码分类与退避做成 application 层纯函数，P0-16 状态机无需理解 HTTP 即可被测试驱动；把 `client_secret` 做成会失败的静态守卫，避免后续有人把 Web 流程的凭据习惯带进 Device Flow。
- 已知边界：
  1. P0-15 是客户端库，P0-16 状态机才接入轮询循环并持久化 `device_auth_states`；本项不建表、不写库、不 sleep。
  2. `/user/emails` 用 `per_page=100` 取单页上限，P0 不实现分页；用户邮箱超过 100 条时 P1 补分页。
  3. 未实现 `refresh_token` / `expires_in`（OAuth App 未启用过期令牌）、未固定 `X-GitHub-Api-Version`、未校验 `token_type`；P0 拿到 `access_token` 后立即查用户并丢弃，不持久化令牌。
  4. `error_description` 被解析后刻意不采用：它是 GitHub 的英文原文，不应直接展示或进入日志；中文提示由 `ClassifyDeviceError` 统一生成。
- 实测发现并修复的真实缺陷：
  1. 首轮 `go vet` 报 `device_test.go` 把 `auth.DeviceCode` 误写成本包 `DeviceCode`，改为包限定后通过。
  2. 首轮全量回归 `TestClassifyDeviceErrorUnknownIsSanitized` 误报：`strings.Contains(msg, "\x9b")` 是**字节级**匹配，会命中中文「回」的 UTF-8 次字节 `0x9B`。改为 `strings.ContainsRune`（按 rune 比较）——与 P0-23 里 C1 CSI 与 UTF-8 continuation byte 混淆是同一类陷阱，只是这次踩在测试自身而非实现上。净化逻辑本身正确，无需改动。
- 验证：新增 28 个测试函数（`application/auth` 7：9 个错误码 / 未知码净化 / `safeCode` / interval 回落 / token 有效性 / `NextInterval` 10 组 / 序列不缩小；`infrastructure/github` 16：两个端点的请求形状与请求体精确断言、9 个错误码、第一步错误、非 2xx 不泄漏、`/user` 与 `/user/emails` 头与字段、缺 token / 缺 client_id、响应体限长、非 JSON 拒绝、context 取消、端点默认值、`flexInt` 5 组、轮询序列；`internal/archtest` 4：两条静态守卫 + 各自负向注入；`internal/config` 1：`MOTD_GITHUB_CLIENT_ID` 读取与去空白）。全量 `go build` / `go vet` / `go test ./...` 通过（30 个包）；从零启动实测两种分支均出现 `GitHub Device Flow 客户端就绪 client_id_configured=false|true scope="read:user user:email"`，无 panic，日志不回显凭据值。

## ADR-022 Device Flow 状态机：状态持久化、CAS 防迟到覆盖、网络不占写事务、节奏交给终端

- 状态：已实现（P0-16，2026-11-18），第四波 M2 第三项
- 结论：
  - **表已存在，无新迁移**：`device_auth_states` 在 `0001_init.sql` 已建好（`device_code` UNIQUE、`status` CHECK 四态、`session_ref`、`polling_interval`、`expires_at` + 过期索引）。与 P0-14 的 `ssh_keys` 同理，P0-16 只补读写与用例，`migrations_applied` 仍为 `[1 2 3]`。设计稿 §4.4.1 要求「不复用 `oauth_states`」，独立表天然满足。
  - **三层分离**：`application/auth/state.go` 放四态枚举、`CanTransition` 迁移表、`EvaluatePoll` 轮询门禁（纯函数）；`state_port.go` 定义 `DeviceAuthStore = Reader + UnitOfWork`（与 forum.Repos 同构，application 不碰 SQL）；`state_machine.go` 的 `DeviceFlowService` 编排 `Start` / `Poll` / `Cancel`。实现落 `infrastructure/sqlite.DeviceAuthRepos`，cmd 装配（`device_wiring.go`）。
  - **合法迁移只有 3 条**：`pending → authorized | denied | expired`。终态出发、`pending → pending`、未知状态一律 `ErrInvalidTransition`（4×4 矩阵穷举测试）。`slow_down` 改间隔不算状态迁移，走独立的 `UpdateInterval`。
  - **同会话重复发起以最后一次为准**：`Start` 在**同一事务**内先 `SupersedeSessionPending` 把该 `session_ref` 下所有 pending 批量迁为 `expired`，再插入新 pending；任何一步失败整体回滚，不留「旧的已作废、新的没写进」的中间态（真实 SQLite + UNIQUE 冲突实测）。
  - **过期码不再轮询**：`EvaluatePoll` 是纯门禁，只有 `pending && now < expires_at` 才 `GateReady`；`GateTerminal`（终态）直接返回且**不写库**，`GateExpired` 先 CAS 迁 `expired` 再返回，两种路径**都不发网络请求**。`now == expires_at` 视为过期，不留边界窗口。
  - **网络调用一律在写事务之外**（§2.6 / ADR-010）：`RequestDeviceCode` / `PollAccessToken` 单次最长 10s，绝不能占唯一写连接。顺序固定为「只读查状态（读池）→ 门禁 → 网络 → CAS 写回」。fake client 在测试里记录网络调用时是否正处于写事务中，检出即失败。
  - **CAS 防迟到覆盖**：因为「读状态」与「写回」跨了一次网络调用，期间用户可能按 Esc 取消或重复发起。`Transition` / `UpdateInterval` 的 `UPDATE ... WHERE id = ? AND status = ?` 用 `RowsAffected == 0` 判冲突，再复查区分 `ErrDeviceAuthNotFound` 与 `ErrStateConflict`。**取消优先**：迟到的成功令牌被 CAS 拒绝后立即作废，绝不把 `denied` 覆盖回 `authorized`（内存 fake 与真实 SQLite 双重实测）。
  - **令牌绝不落库**：`DeviceAuthState` / `NewDeviceAuthState` 结构上没有 token 字段，`access_token` 只在 `PollResult.Token` 里进程内交给 P0-17。archtest 守卫扫描 `0001_init.sql` 的 `device_auth_states` 段落，禁止出现任何 token 列（负向注入实测）；端到端测试直接查 DB 全部文本列确认不含令牌值。
  - **状态机不持有节奏**：不 `time.Sleep`、不起 ticker / timer。轮询间隔以 `PollResult.NextInterval` 交给 P0-19 的 SSH 会话循环；`slow_down` 的 `NextInterval` 结果持久化到 `polling_interval`（跨请求存活，P0-15 的纯函数复用）。archtest 用 AST 扫描 `application/auth` 非测试源码，禁止 `time.Sleep` / `NewTicker` / `NewTimer` / `Tick` / `After` / `AfterFunc`（负向注入实测）。理由：状态机必须能在无真实等待下穷举回归，且不能拖住 SSH 会话 goroutine。
  - **四态枚举与 DDL 不漂移**：archtest 解析 `0001_init.sql` 里 `device_auth_states` 段落的 `CHECK (status IN (...))`，与 `state.go` 的四个 `StatusXxx` 常量逐字比对。刻意只取该表段落，避免误命中 `users.status` 的 CHECK（`active/pending/deleting/deleted`）。
  - **失败关闭细节**：空 `session_ref` 会被去空白后拒绝（否则批量作废会误伤所有空 ref 记录）；空 / 不完整的设备码响应、空令牌都返回明确哨兵；非 `*PollError` 的网络错误（5xx / 超时）**不改状态**、原样返回让上层重试，绝不把一次网络抖动误判成用户拒绝；`expires_in <= 0` 回落 `DefaultExpiresIn=900`、`interval <= 0` 回落 `DefaultPollInterval=5`。`Cancel` 对终态幂等，重复按 Esc 不报错也不再写库。
- 理由：状态跨请求存活是 §4.4.1 的硬要求，必须落库；但轮询是长耗时外部调用，若把网络塞进写事务会直接违反 §2.6 的单写连接模型。用「事务外读 + 网络 + 事务内 CAS」既满足两者，又顺带解决了迟到结果与取消/重复发起的竞态。节奏归终端而非状态机，是为了让 P0-19 能在 `Esc` 时立即取消，而不是等一个 sleep 醒来。
- 已知边界：
  1. P0-16 只做状态机与持久化，`/login` 终端呈现、轮询循环、会话升级属 P0-19；账号创建/登录事务属 P0-17。状态机已装配到 `inputHandler.deviceAuth`，但尚未接线到命令分发。
  2. `GetUser` / `GetEmails` 在 P0-16 不调用（stub 直接报错），令牌交 P0-17 使用；P0 不持久化令牌、不实现 refresh。
  3. 没有定时清理过期 `device_auth_states` 行的任务；`idx_device_auth_states_expiry` 已存在，P1 可加清理 job。P0 数据量小，过期行只影响表大小不影响正确性（门禁保证不再轮询）。
  4. 「同会话重复发起」按 `session_ref` 字符串匹配，P0 由 SSH 会话 ID 提供；多实例部署时该语义仍在单库内成立，无需分布式锁。
- 实测发现并修复的真实缺陷：首轮 `go build` 报 `PollTerminal redeclared` —— P0-15 的 `PollStatus`（GitHub 错误码分类）已占用 `PollTerminal`。门禁枚举改用 `GateReady` / `GateTerminal` / `GateExpired` 前缀，语义上把「错误码分类」与「轮询门禁」彻底分开，避免后续读代码时混淆两个不同的决策域。
- 验证：新增 **51 个测试函数**（`application/auth` 领域层 8：枚举合法性 / 4×4 迁移矩阵穷举 / 门禁 7 组 / 门禁全集闭合 / 间隔回落 / 会话标识 / 终态提示 / Outcome 字符串；状态机 22：Start 落库与字段、网络不在写事务、重复发起作废、空会话 / 网络失败 / 响应不完整 / expires 回落 / 事务回滚；Poll 过期不轮询、终态不轮询不写库、pending 继续、slow_down 持久化与序列不缩小、成功 authorized 且令牌不落库、空令牌、8 个终态错误码（子测试）、传输错误不改状态、CAS 拒绝迟到结果、空码 / 不存在；Cancel 迁移 / 幂等 / 不存在；nil 依赖 panic）。`infrastructure/sqlite` 15：往返读写、空 session_ref 落 NULL、DDL 拒绝脏状态、UNIQUE、批量作废只命中同会话 pending、作废目标非法、CAS 成功/冲突/缺行/非法路径、间隔 CAS、事务回滚，以及 4 条端到端（真实 SQLite + 真实 Writer + stub client：完整轮询序列、过期码不轮询、重复码事务回滚、迟到结果不能覆盖取消）。`internal/archtest` 6：状态机不阻塞时间 / 枚举与 DDL 一致 / 状态表无 token 列 + 各自负向注入。全量 `go build` / `go vet` / `go test ./...` 通过（30 个包）；从零启动实测出现 `设备码状态机就绪 store=device_auth_states modes=pending|authorized|denied|expired`、`migrations_applied=[1 2 3]`（无新迁移），无 panic。

## ADR-023 登录事务：身份主键是数字 id、邮箱只提示不合并、判定与创建同一事务

- 状态：已实现（P0-17，2026-11-18），第四波 M2 第四项
- 结论：
  - **表已存在，无新迁移**：`users` / `identities` 在 `0001_init.sql` 已建好（`username` COLLATE NOCASE UNIQUE、`identities` 有 `UNIQUE (provider, provider_uid)`）。与 P0-14 的 `ssh_keys`、P0-16 的 `device_auth_states` 同理，P0-17 只补用例与仓储，`migrations_applied` 仍为 `[1 2 3]`。
  - **身份主键是 (provider, provider_uid)，且 provider_uid 取 GitHub 数字 id 的字符串**：`strconv.FormatInt(g.ID, 10)`，绝不用 login / email（两者都可被用户改写，会把账号交给改名者）。archtest 静态守卫要求源码里存在 `FormatInt(<x>.ID, 10)`，负向注入实测。
  - **§4.3 三步判定的落法**：① `(provider, provider_uid)` 命中 → 登录（账号 `status` 必须是 `active`，否则 `ErrAccountInactive` 失败关闭；注销流程中的 pending/deleting 账号不得重新登录）；② 未命中 → 若邮箱 primary+verified，查 `users.primary_email` 是否已有归属，命中则只生成 `Notice`；③ 都未命中 → 同一事务内 `InsertUser` + `InsertIdentity`。
  - **§7 R16 口径消解（重要）**：设计稿 §4.3 步骤 2 写「邮箱命中已有账号 → 不自动合并，提示先登录原账号再 /link」，而 P0-17 验收写「邮箱相同但 UID 不同必须建成两个账号」，两句字面相冲。R7 已确认 `users.primary_email` **无唯一约束是有意设计**（正是「禁止按邮箱自动合并」的结构保证），因此唯一自洽解是：**邮箱命中不阻断、不复用旧账号，仍为新 UID 建独立账号，只把「先登录原账号再 /link」作为 `LoginResult.Notice` 返回**。这样「不自动合并」「提示 /link」「同邮箱两账号」三条同时成立，也守住 §8 的账号接管风险。Notice 是**固定文案、不含邮箱/login 原文**，避免把外部数据拼进终端提示。
  - **网络调用一律在写事务之外**（ADR-022 同一条边界）：`GetUser` / `GetEmails` 在 `store.Write` 之前完成，单次最长 10s，绝不占 §2.6 的唯一写连接。fake client 在测试里记录「网络调用时是否正处于写事务」，检出即失败。
  - **三步判定与两张表的插入必须在同一个写事务内**：若把「查身份」放到事务外，并发同 UID 首登会出现 TOCTOU——两个事务都判定「未命中」，然后都插入。`LoginWriteTx` 因此**不设独立读端口**，所有查询与插入绑定同一个事务快照（与 forum 把权限/限流查询放进 `WriteTx` 同一理由）。
  - **唯一约束是最后一道防线，冲突整体重试**：`identities.UNIQUE(provider, provider_uid)` 被并发撞上时，仓储把驱动错误映射成 `auth.ErrIdentityConflict`；`users.username` 的 NOCASE UNIQUE 撞上映射成 `auth.ErrUsernameConflict`。用例层捕获这两类哨兵后**整体重试**（最多 3 次，每次都是新事务，首轮已回滚无半成品）。身份冲突重试时 `FindIdentityByUID` 会命中赢家账号，于是转为「登录」而不是重复建号——这正是「并发同 UID 首登只产生一个用户」的实现方式（真实 SQLite 10 goroutine 实测）。
  - **用户名派生必须过 domain 校验**：`DeriveUsername` 清洗非法字符 → 截断到 20 码点 → 不足 3 字符时**优先补下划线**（两字中文 login「张三」→「张三_」，保留可读性）、清洗后为空才回退 `gh<uid>` → 保留名走 `domainuser.ResolveUsername` 追加 `_0` → 最终 `ValidateUsername`。冲突时 `AllocateUsername` 生成 `base, base2, base3...`，加后缀前按 20 码点上界截断基准名（中文按码点计数，与 SQLite `length(TEXT)` 及 domain 正则一致）。仓储 `InsertUser` 再调一次 `ValidateUsername` 兜底——ADR-005 要求任何写 `users.username` 的路径都必须先过它，DDL 的 CHECK 只管长度与空白。
  - **邮箱只收 primary && verified**：`SelectPrimaryVerifiedEmail` 跳过非 primary、未验证、格式非法（含空白/控制字符）的项，normalize 为 trim+小写。**没有验证邮箱不阻断登录**（邮箱是恢复通道不是身份主键），此时 `primary_email` 落 NULL，步骤 2 的邮箱查询直接跳过。
  - **令牌绝不落库**：`IdentityProfile` / `LoginResult` / `NewAccount` / `NewIdentity` 结构上没有 token 字段，`access_token` 只在 `Login(ctx, token)` 的参数与两次 GET 之间进程内流转。archtest 守卫扫描 `users`/`identities` 表段落禁 token 列、扫描登录结构体禁 token 字段（均含负向注入）；端到端测试直接扫两张表的全部文本列确认不含 `gho_` 前缀。
  - **R7 结构性守卫**：archtest 解析 `0001_init.sql` 的 `users` 表段落，要求 `primary_email` 列定义**不含 UNIQUE**。若有人「顺手」给它加唯一约束，「同邮箱两账号」的验收会立刻失败——这条约束因此从口头约定变成会失败的测试。同时守卫 `identities` 必须保留 `UNIQUE (provider, provider_uid)`。
  - **禁止按邮箱复用账号的静态守卫**：archtest 用 AST 检查 `FindAccountByEmail` 的所有赋值语句，左侧第一个返回值**必须是 `_`**。一旦有人写成 `hit, err := tx.FindAccountByEmail(...)` 并拿 `hit.UserID` 去登录，守卫立即失败——把「邮箱命中只提示不合并」从注释变成结构约束。
- 理由：账号创建是唯一一个「查完就写」且「外部网络结果决定写入内容」的用例，三重风险（并发首登重复建号、按邮箱误合并导致接管、token 顺带落库）都必须靠结构而不是纪律来防。所以判定与创建同事务、唯一约束做兜底、邮箱结果被静态丢弃、token 连字段都不给。
- 已知边界：
  1. `LoginService` 只做账号事务，`/login` 终端呈现、轮询循环与 `guest → user` 会话升级属 P0-19；`inputHandler.login` 已装配但尚未接线到命令分发。
  2. 命中已有身份时**不更新** `last_seen` / 邮箱 / display_name（P0 保持幂等零写入）；需要「登录即刷新资料」时属 P1。
  3. `profile_json` 恒写 `'{}'`：GitHub login 只用于 `display_name` 与用户名派生，不做 JSON 落库，避免把外部文本塞进未净化的字段。
  4. P0 只有 `github` 身份源；`provider='email'` 的 DDL 值域与 `local_credentials` 表为 P1 邮箱注册预留，本项不写入。
  5. 邮箱归属只查 `users.primary_email`，不查 `identities.email`：前者是账号主邮箱，后者是该身份源返回的历史快照，用它判定会把「同一 UID 换过邮箱」误判成「邮箱属于别人」。
- 实测发现并修复的真实缺陷：首轮回归 `TestDeriveUsername/中文保留` 报 `得到 "gh1"，期望 "张三"` —— 「张三」只有 2 个码点，被「不足 3 字符」分支直接整名丢弃、回退成 `gh<uid>`，中文可读性丢失。改为**先补下划线到 3 字符**（`张三_`，仍合法），只有清洗后完全为空才回退 `gh<uid>`。测试期望与实现同时修正，`TestDeriveUsernameAlwaysProducesLegalName` 增加 9 组极端输入（空串、纯标点、ESC 序列、30 个汉字、纯下划线、纯中划线、保留名）全部产出合法用户名。
- 验证：新增 **63 个测试函数**。`application/auth` 纯函数 19：数字 id 必填 / provider_uid 格式 / 只选 primary+verified / 邮箱 normalize / 非法邮箱 / 控制字符 / 用户名派生 9 组表驱动 / 极端输入恒合法 / 码点截断 / 分配器（首个可用、数字后缀、后缀不越界、耗尽、错误透传、默认次数）/ 候选生成 / Notice 无外部数据 / 哨兵互异 / Created 语义。`application/auth` 用例 20：空 token 不触网不开事务、网络不在写事务内、GetUser/GetEmails 失败不落库、非法资料开事务前拒绝、命中身份不写库、三种非 active 状态拒绝、原子创建字段校验、InsertIdentity 失败整体回滚、无验证邮箱跳过步骤 2、邮箱命中不合并且不复用旧 ID、同邮箱不同 UID 建两账号、Notice 不回显外部数据、用户名冲突加后缀、用户名额冲突整体重试、身份冲突重试后转为登录、重试耗尽、8 goroutine 并发同 UID 只建 1 个号、不调 Device/Poll 端点、nil 依赖 panic。`infrastructure/sqlite` 12：真实 SQLite 端到端建号字段、重复登录幂等、同邮箱两账号（含 primary_email 相同）、NOCASE 大小写冲突加后缀、仓储拒绝非法用户名、仓储拒绝非 active、UNIQUE 冲突整体回滚（users 不留孤儿）、10 goroutine 并发只建 1 个号、令牌不落库（扫全部文本列）、身份/邮箱未命中哨兵、NOCASE 探测、pending 账号拒绝登录。`internal/archtest` 12：两张表无 token 列 / primary_email 无 UNIQUE / identities 有 UNIQUE(provider,provider_uid) / 登录结构体无 token 字段 / FindAccountByEmail 返回值被丢弃 / provider_uid 由 FormatInt(ID,10) 生成，六条均含负向注入。全量 `go build` / `go vet` / `go test ./...` 通过（30 个包）；从零启动实测出现 `账号登录事务就绪 store=users+identities provider=github`、`migrations_applied=[1 2 3]`（无新迁移），无 panic。

## ADR-024 公钥绑定：只认服务端持有的公钥、软吊销、会话撤销在事务之后

- 状态：已实现（P0-18，2026-11-18），第四波 M2 第五项
- 结论：
  - **表已存在，无新迁移**：`ssh_keys` 在 `0001_init.sql` 已建好（`fingerprint TEXT NOT NULL UNIQUE`、`public_key TEXT NOT NULL`、`revoked_at INTEGER` 可空、`idx_ssh_keys_user` 覆盖 `(user_id, revoked_at)`）。与 P0-14/16/17 同一模式：只补用例、仓储、认证产物传递与命令分发，`migrations_applied` 仍为 `[1 2 3]`。
  - **指纹与公钥文本的唯一来源是认证阶段服务端持有的 `ssh.PublicKey`**：`publicKeyCallback` 在客户端通过公钥挑战后，把 `gossh.FingerprintSHA256(key)` 与 `gossh.MarshalAuthorizedKey(key)` 一并放进 `Permissions.Extensions`；`Session.Fingerprint()` / `Session.PublicKey()` 只读暴露。application 层既不解析公钥（P0-14 的 `collectAuthPublicKeyParsing` 守卫继续生效），也不从命令参数反推指纹。文本先过 `ValidatePublicKeyText`（非空 / 单行 / 无控制字符与 NUL / ≤8192 字符）才落库，P0 不解析、不回显。
  - **「伪造提交别人的公钥字符串」由三道门挡住**：① 命令表 `/key` 的 `MaxArgs=2` 粗筛；② `auth.ParseKeyCommand` 要求 `add` **恰好 1 个参数**、`rm` 恰好 2 个且第二个必须是 `ValidFingerprint` 格式的**指纹标识符**（不是公钥文本）；③ archtest AST 守卫要求 `BindRequest.Fingerprint` / `.PublicKey` 的值只能是 `sess.Fingerprint()` / `sess.PublicKey()`，并限制 `sess.PublicKey()` 在 handler 里只出现一次（不得写回终端）。三条均含负向注入或行为回归。
  - **四态判定与插入绑定同一个写事务**：未命中 → `Insert`；命中已吊销 → `ErrKeyRevokedBinding`；命中他人 → `ErrKeyBoundToOtherUser`；命中本人 → 幂等零写入。`KeyBindingWriteTx` 不设独立读端口（同 ADR-023 的 TOCTOU 理由）。`fingerprint UNIQUE` 被并发撞上 → `ErrKeyConflict` → 整体重试（≤3 次，首轮已回滚无半成品），下一轮重新查命中后转为幂等或拒绝。真实 SQLite 6 goroutine 不同账号同指纹实测恰好 1 个创建、其余全部拒绝；10 goroutine 同账号同指纹实测最终 1 行。
  - **§7 R17：解绑是软吊销，不是物理删除**。`UPDATE ssh_keys SET revoked_at=? WHERE fingerprint=? AND user_id=? AND revoked_at IS NULL`。`user_id` 钉住归属（不得解绑他人钥匙），`revoked_at IS NULL` 保证幂等，行永久保留。理由：P0-14 的三状态靠 `revoked_at` 区分「拒绝认证」；若 `DELETE` 行，该指纹会退化成「未命中 → 访客放行」，等于把吊销动作反过来**放宽**权限。archtest 静态禁止 `DELETE FROM ssh_keys`；运行时回归确认解绑后 `SSHKeyLookup.Lookup` 返回 `Revoked=true`、`COUNT(*)` 仍为 1，`/keys` 列表排除该行。
  - **解绑后撤销相关会话，且只在事务提交之后**：`SessionRevoker` 端口定义在 `application/auth`，实现落在 `transport/ssh.Server`（`sessions map[string]*Session` + 独立锁，`RevokeSessions` 按指纹关闭所有匹配 channel）。这是纯内存动作、不写库、不可回滚，因此绝不进写事务闭包（失败时不能提前踢人）；archtest 用 AST 比较 `Write` 的 `End()` 与 `RevokeSessions` 的 `Pos()` 钉死顺序。真实 SSH 端到端实测：2 个同指纹会话被撤销、第 3 个不同指纹会话不受影响。
  - **用例刻意不持有 SSH Session**：`KeyBindingService` 不改任何会话状态。绑定失败时 DB 事务整体回滚、会话保持原状态、不落半成品（§4.6 验收）；P0-19 的 `guest → user` 升级必须等 `Bind` 成功才发生。当前 `Session.principal` 只读且无 setter，P0-19 需补一个受限的升级方法。
  - **零回显不变**：`/keys` 只输出 DB 里读出且再次过 `ValidFingerprint` 白名单的指纹；`/key` 的成功 / 失败文案全是服务端固定文本（`bindResultText` / `unbindResultText`），不输出 `inv.Args`、指纹、公钥文本或 `err.Error()` 原文。`formatResult` 的既有约束与 P0-22 / P0-23 的分层渲染边界保持。
  - **装配循环用 setter 后填化解**：`sshd.Server` 依赖 `Handler`，`Handler` 要用 `KeyBindingService`，而 `KeyBindingService` 又依赖 `Server`（作为 `SessionRevoker`）。`main.go` 先建 handler → 再 `sshd.New` → 最后 `newKeyBindingService(pools, writer, sshSrv)` + `handler.withKeyBinding(...)`，沿用 P0-15/16/17 的无参构造 + setter 模式，会话层测试零改动。
- 理由：公钥是凭证不是身份，绑定一旦允许「客户端提交一段文本」，任何连接都能把自己的公钥挂到别人账号上。所以约束不能靠纪律，必须靠结构：来源只能是认证阶段的服务端对象、参数只能是格式受限的指纹标识符、判定与写入同一事务、撤销动作与事务成功绑定、结果文案不含任何外部数据。
- 已知边界：
  1. `/key rm <fingerprint>` 的参数确实来自用户文本，但它是**指纹标识符**且过 `ValidFingerprint`，不是公钥文本；与 §4.6 禁止的「提交公钥字符串」是两回事。
  2. `public_key` 列 P0 只落库不读取、不展示；`label` 恒为空，`last_used_at` 不更新（P1 的 `/keys` 详情页与登录时刷新 `last_used_at` 再补）。
  3. **已吊销钥匙不可重新绑定**：吊销通常是钥匙泄露后的安全动作，重新激活会恢复其认证能力。P0 不提供重新激活；确需时属 P1 的显式流程（且必须重新走私钥证明）。
  4. 会话撤销按指纹精确匹配、同指纹多会话一起关闭，不区分会话当时是访客还是登录态（认证快照绑定的是连接而非账号）。
  5. P0-19 的自动绑定（D2：登录成功后当前连接公钥自动绑定到新账号）直接复用 `KeyBindingService.Bind`，不经命令表；本项不实现 `/login` 交互与轮询循环。
  6. `auth_events` 未写（P0-20 安全日志与限流）；本项不引入任何限流计数与 HTTP 端点（Q3）。
- 实测踩坑：
  1. 新测试里的 `testFingerprint` 与既有 `resolver_test.go` 同包重名 → 改名 `kbTestFingerprint`；全量 `go vet` 先于 `go test` 拦下。
  2. 装配循环（见上）若直接把 `KeyBindingService` 注入 `sshd.New` 会形成鸡生蛋，改用 setter 后填。
- 验证：新增 **58 个测试函数**（`application/auth` 纯函数 6 + 用例 22；`infrastructure/sqlite` 真实 SQLite 9 含两组并发；`transport/ssh` 3 含真实客户端双会话撤销；`cmd/server` 命令分发 6；`internal/archtest` 12 含 6 条负向注入）。全量 `go build ./...` / `go vet ./...` / `go test -count=1 ./...` 通过（30 个包、共 280 个测试函数）；从零启动实测日志出现 `公钥绑定就绪 store=ssh_keys revoker=ssh_sessions modes="keys|key add|key rm"`、`migrations_applied=[1 2 3]`（无新迁移），无 panic。

---

## ADR-025 登录交互：按 Q3 用最小交互层接 P0-19，偏差集中登记

- 状态：已实现（P0-19，2026-11-18），第四波 M2 第六项；**R18 / R19 / R20** 见 §7 与 `docs/DEVIATIONS.md`
- 结论：
  - **波次冲突由用户拍板消解**：`P0-PLAN.md §4` 依赖图写 `A24 → A19`（P0-19 依赖 P0-24 终端页面），但 `§5` 把 P0-24 排在第五波、P0-19 排在第四波。V 先生 2026-11-18 明确选推荐方案：**按 Q3 用最小交互层接 P0-19**（复用 P0-22 命令表 + P0-23 `RenderSystem` / `RenderUser` 输出固定文本 + 进程内轮询），**P0-24 落地时再替换呈现层**，与 P0-22 既定的「页面控制器替换 handle 分发」模式一致，**不改协议层**。同时要求「同步更新设计稿或新建文档记录不一致之处」——评审稿按既有纪律**不回改**，改为新建 `docs/DEVIATIONS.md` 作为偏差单一事实源（R1~R20 总表 + 边界/替换清单 + 逐条核对表）。
  - **不变 / 替换的边界写死在文档里**：`transport/ssh` 解码器与会话协议（仅补 `Session.UpgradeToUser`）、`application/command` 注册表、`application/auth` 四个既有用例（P0-15/16/17/18）**全部零改动**；替换点只有 `cmd/server/login_view.go`（`loginPresenter`）、`login_flow.go` 的 `out io.Writer` 注入与 `Serve` 循环里的 `pollTimer` / `pollCh` 分支。P0-24 接入 `ViewModel` 双 Renderer 时无需重写业务链路。
  - **表已存在，无新迁移**：`device_auth_states` / `users` / `identities` / `ssh_keys` 均在 `0001_init.sql`，`migrations_applied` 仍为 `[1 2 3]`。
  - **呈现分层不合并**（§3.5 / ADR-019）：`loginPresenter.line` 只吃编译期固定文案 → `safe.RenderSystem`；`user_code` / `verification_uri` / `username` 属外部数据，先过 `auth.ProjectLoginView` 格式白名单（只收 `[A-Z0-9-]` 的 user_code、`https://` 且 URI 安全字符集的 verification_uri；不合法则**失败关闭**并作废设备码），再走 `safe.RenderUser` 净化路径。`LF → CRLF` 在呈现层完成。两类文本**绝不拼进同一次渲染调用**。`LoginResult.Notice` 只有等于白名单常量 `auth.EmailOccupiedNotice` 才允许进 `RenderSystem`。
  - **轮询节奏归会话循环，网络调用异步化**：`DeviceFlowService` 仍不 sleep / 不起 ticker（ADR-022），`auth.LoginDirective` / `ClassifyPoll` 把结果映射成 `Continue` / `Authorized` / `Terminal` / `Retry`；间隔统一过 `ClampInterval`（`[5, 60]` 秒）。`Serve` 用 `time.Timer` 持有节奏，`Poll` 放进独立 goroutine 经 channel 回传——单次网络最长 10s，若同步执行会阻塞 Esc；取消后迟到的结果由 `flow.active` 判定丢弃，ADR-022 的 CAS 保证「取消优先」。连续网络错误按 `Retry` 处理（**不误判为用户拒绝**），上限 `LoginMaxNetworkRetries = 3` 后失败关闭。
  - **`device_code` 与 `access_token` 都不进呈现结构**：`LoginView` / `LoginDirective` 刻意不含这两类字段；`device_code` 只保存在 `loginFlow` 进程内字段用于 `Poll`，`access_token` 只在 `finish` 的栈上进程内交 `LoginService`。archtest 静态扫描 `cmd/server/login_*.go` 的所有写终端调用，禁止参数里出现 `.Token` / `.DeviceCode` / `deviceCode` / `.Message`（响应原文），并含负向注入。
  - **`guest → user` 唯一升级入口**：`presence.Registry.AttachUser` 只允许 `UserID == 0 → > 0` 一次；`ssh.Session.UpgradeToUser` 三道门（`userID > 0`、当前必须访客、presence 绑定成功）全过才写 `principal`，且 `principal` 读写都走 `s.mu`（消除数据竞争）。archtest 钉死 `session.go` 里 `s.principal =` 只有一处。
  - **R19：账号事务与自动绑定不是同一原子单元**。授权后顺序 `Login → Bind → UpgradeToUser`，**三步全成功才升级**。`Bind` 直接复用 `KeyBindingService.Bind`、**不经命令表**（ADR-024 第 5 条），`BindRequest` 的指纹 / 公钥只来自 `loginConn` 端口（即认证阶段服务端持有的 `ssh.PublicKey`）。`Bind` 或 `Upgrade` 失败时账号已建但**会话保持访客态**、返回固定文案，不落「已登录却无绑定」的半成品；此时设备码是 `authorized` 终态而非 `denied`（后者只由 Esc / 用户拒绝产生）。
  - **P0-18 的 archtest 守卫按端口语义扩展，强度不降**：`TestBindRequestSourcesAreServerHeld` 原先把来源钉死为 `sess.*`，P0-19 的 `conn.*`（`loginConn`）被误报。改为白名单 `sess.* | conn.*`（二者都是服务端会话门面，**不是客户端可提交的文本**），并新增两条负向注入：`conn.*` 应被接受、`other.*` 等未知 receiver 必须被拒绝。守卫仍要求方法名精确匹配 `Fingerprint()` / `PublicKey()`，`inv.Args[...]` 依旧拦死。
  - **Q3 不变**：`cmd/server/main.go` 的 `probeMux` 仍只有 `/healthz`；交互层静态守卫禁止 `net/http` import 与 `time.Sleep` / `NewTicker`。启动日志 `登录交互就绪 mode=minimal_in_process presentation=fixed_text replace_at=P0-24` 明确标注临时形态。
- 理由：把 P0-19 押后到 P0-24 会让 D2 在第四波无法验收，把 P0-24 提前则要一次性引入 ViewModel 页面框架、超出 M2 范围；最小交互层是唯一既守住 Q3「进程内调用」、又不让临时代码伪装成最终设计的做法。偏差不写进文档，事后最容易发生的错误就是「照着 P0-19 的固定文本继续加页面逻辑」，把呈现职责永久焊死在 handler 里。
- 已知边界：
  1. **呈现形态是临时降级**：剩余时间只在发起屏给固定文本，轮询期间输出固定「等待授权中」行，无全屏重绘 / 光标控制。P0-24 落地时按 `DEVIATIONS.md` 第 3 节清单替换，业务链路零改动。
  2. **R19 的诚实边界**：`Bind` 失败时 `users` / `identities` 已提交（账号真实存在），当前连接只是没升级；用户需换一把公钥重连后重新 `/login`，届时 UID 命中即登录并重新尝试绑定。P0 不做补偿事务（会违反 §2.6 单写连接模型）。
  3. `Esc` 取消只在 `flow.active` 时生效；`Cancel` 在连接退出时用独立 2s 上下文执行，避免 `ctx` 已取消导致写不回 `denied`。
  4. D2 的「手工跑通」仍待真实 `MOTD_GITHUB_CLIENT_ID` 在测试 VPS 上联调；本轮 D2 由真实 SQLite + 假 GitHub 端点（进程内 fake `DeviceFlowClient`）覆盖，不依赖外网。
  5. `auth_events` 未写（P0-20），本项不引入任何限流计数；`-race` 全量检测因本机无 gcc / CGO 未启用，按既定安排后置到 D7 之后。
- 实测踩坑：
  1. P0-18 的 `BindRequest` AST 守卫把 `conn.Fingerprint()` 判为违规（见上「守卫扩展」）——这是**守卫假设与端口抽象不匹配**，不是实现缺陷；修法是把白名单从「变量名」提升到「会话门面语义」，同时补未知 receiver 的负向注入，确保没有放宽成任意来源。
  2. `ProjectLoginView` 的 `strings.TrimSpace` 会吃掉尾部换行，测试用例里 `"ABCD-EFGH\n"` 意外通过；改为内部控制字符（`ABCD\nEFGH`）才真正验证「失败关闭」。
  3. `normalizeVerificationURI` 的字符白名单不含 `\t` / `\n` / 尖括号，URI 中间夹控制字符会被拒绝，符合预期。
- 验证：新增 **49 个测试函数**（`application/auth` 领域 11；`presence` 3；`transport/ssh` 会话升级 6 含 8 goroutine 并发只成功 1 次；`cmd/server` 呈现 5 + 交互 / D2 集成 11；`internal/archtest` 13 含 6 条负向注入与文档一致性守卫）。全量 `go build ./...` / `go vet ./...` / `go test -count=1 ./...` 通过（30 个包；核心 6 包共 336 个测试函数）；从零启动实测日志出现 `登录交互就绪 mode=minimal_in_process presentation=fixed_text provider=github replace_at=P0-24`、`migrations_applied=[1 2 3]`，无 panic。`docs/DEVIATIONS.md` 由 archtest 守卫校验存在性与 R18~R20 关键段落。

---

## ADR-026 回显按字段分级：修订 ADR-018 的「全局零回显」

- 状态：已采纳（2026-11-18，V 先生本机实测后拍板）；**修订 ADR-018 第 2 条与「代价」第 1 条**，ADR-018 原文保留供追溯
- 结论：
  1. 正常操作（斜杠命令、发帖标题 / 正文、回复正文）**必须有回显**；
  2. 只有 Secret 字段（P1 邮箱登录密码、验证码等）隐藏原文，只回显 `*`；
  3. 回显的用户字符一律先经 `safe.SanitizeText` 净化；`KeyMsg.Raw` / `Text` 原始字节永不直接进输出流；
  4. 编辑控制（`\b \b` 退格擦除、`\r\n` 换行）是**服务端生成的固定常量**，与用户字节分属不同信任域；
  5. 不回显清单保持不变：命令参数 `inv.Args`、指纹 / 公钥文本、`err.Error()`、凭据值、Device Flow 的 `device_code` / `access_token`。
- 理由：
  - **体验**：全局零回显让用户输入命令 / 标题时完全看不到自己打了什么，本机实测被判为「输入没反应」，属可用性缺陷而非安全必需。
  - **设计依据**：评审稿 §2.3 与 `viewmodel.Field.Secret` 注释已写明「Secret / Multiline 描述语义约束，**具体回显字符由 Renderer 决定**」——回显与否本就是字段级语义，不是全局开关。
  - **安全**：§3.5 禁止的是「用户提交的控制序列」影响终端，不是「服务端生成的编辑控制」。逐字符净化后回显 + 服务端常量控制，恰好是 §3.5 要求的两条信任路径；恶意 ESC 变成可见图示 `␛`，不会成为控制序列。
  - **最小改动**：不引入依赖，不动 `safe/ansi.go` 的 SGR 白名单与既有 archtest 守卫（FF09）。
- 代价 / 边界：
  - 逐字符回显 + `\b \b` 擦除依赖终端 VT 兼容；宽字符按 `safe.DisplayWidth` 计算列数。粘贴含换行的多行文本后退格不能跨行回到上一行（已知限制，P1 多行编辑器解决）。
  - `lineEcho` 每会话一份，与 `pageController` 同生命周期；`applyKey` 仍是纯状态机（不碰 IO），回显在调用侧按前后长度推导是否真删除了字符。
  - 当前 P0 无密码字段，`startComposer` 显式 `setSecret(false)` 声明所有编辑字段非敏感；P1 邮箱密码字段改 `setSecret(true)`，对应 `viewmodel.Field.Secret`。
- 验证：`cmd/server/line_echo_test.go`（5 个：普通 / 宽字符退格、恶意字节净化、Secret 掩码、粘贴与换行 reset、预置 `/` 顺序）+ 真实 SSH E2E 断言 `//login` 回显与标题 / 正文回显；全量 `go build` / `go vet` / `go test -count=1 ./...` 通过。
- 关联：`DEVIATIONS.md` R21、`docs/MVP-DELIVERY.md §10`。

---

## ADR-027 终端 TUI 原地重绘：服务端白名单光标控制（MVP 收官）

- 状态：已采纳（2026-11-18，V 先生在 `MVP-DELIVERY.md §10.3` 的 A/B 方案中选 **A「一次到位」**）
- 结论：
  1. 连续导航（`j/k/Enter/h/q/n/p`）**原地重绘**，不再每次追加整屏；
  2. `safe/ansi.go` 白名单从「纯 SGR」扩为「SGR + EL `\x1b[K` + CUU `\x1b[nA` 模板」，两者**只由服务端按上一屏行数生成**，绝不接收用户输入；
  3. 仍明确禁止 ED 清屏 `2J`、CUP `H/f`、CUD `B`、DECSET 备用屏 `?1049h`、OSC 52 剪贴板、OSC 窗口标题、带参数 EL；用户内容路径（`RenderUser` / `SanitizeText`）不触达任何光标控制；
  4. `presentation/terminal.RenderFrame` 是光标控制的**唯一消费点**（纯函数）；`pageController` 只负责记住上一屏行数；
  5. 屏幕连续性：任何非屏幕输出（命令提示、逐字符回显、编辑器、登录文本、错误提示）都 `invalidateScreen()`，下次退回追加模式，避免 `CursorUp` 覆盖错位置；
  6. 极窄终端发生自动折行时自动降级为追加（宁可多输出也不错位）。
- 理由：
  - **体验**：追加式整屏重绘让用户每按一次 `j` 就滚一屏，实测判定不可用；原地重绘是标准 TUI 形态。
  - **带宽**：§1.2 的 1 Mbps 约束下先解决视觉；当前仍逐行重发整屏内容，**不提前引入 §3.3 的 Frame 增量协议**（那是 P1 浏览器端设计，FF09 避免过度设计）。
  - **安全**：§3.5 禁止的是「用户提交的控制序列影响终端」，不是「服务端生成的编辑控制」。白名单只放开两条，并配负向注入守卫，红线不破。
  - **最小改动**：`application/forum`、`application/viewmodel`、`transport/ssh` 协议层、`go.mod` 全部未改。
- 代价 / 边界：
  - 依赖终端 VT 兼容（Windows Terminal / 现代 conhost / xterm）。旧版 conhost 若未启用 VT 支持，会退化为字面追加式输出，**功能不受影响**。
  - 每行 `\r` + EL + 内容会重发整屏字节，差异行优化留 P1。
  - `screenLines` 是每会话内存状态，重启归零（原则二：持久化数据才是事实）。
- 验证：
  - 新增 `presentation/terminal/frame_test.go` 5 项（追加 / 重绘 / 短屏清空 / 窄屏降级 / 白名单序列）；`archtest/safe_test.go` 扩 `allowedANSI` + 3 条新守卫（负向注入、用户路径无光标控制、唯一消费点）。
  - `cmd/server` 补 `TestNavigationRedrawsInPlace` 与真实 SSH `TestMVPNavigationRedrawsInPlace`；`cmdOnlySGR` 升为 `cmdOnlyWhitelistedANSI`。
  - **真实 Windows OpenSSH 客户端实测**：连按 `jjj` → 3 次 `\x1b[8A`、24 次 `\x1b[K`（3×8 行）、清屏/OSC/备用屏 0 次；`go build` / `go vet` / `go test -count=1 ./...` 0 FAIL（479 测试函数）。
- 关联：`DEVIATIONS.md` R22、`docs/MVP-DELIVERY.md §11`。

## ADR-028 领域事件事务性发件箱（outbox）：写内落库、提交后投递

- 状态：已采纳（2026-11-19，P1-M1）
- 结论：
  1. 新增 `domain_event_outbox` 表（迁移 `0006`）；`WriteTx.AppendOutbox` 把事件与业务写**在同一事务内**落库，`event_id` UNIQUE 做入库去重；
  2. 投递在 `repos.Write` **提交成功后**由 `application/forum/outbox.go` 调度器执行：`status` CAS 领取（pending→dispatching）→ 调 `DomainEventSink.Publish` → delivered / failed；**写事务内绝不调 plugin handler**；
  3. 语义为 at-least-once：崩溃残留 `pending` 由启动 `RecoverPending` 重扫（并退回 dispatching），订阅者侧 `events.DomainBus` 按 `DomainEvent.ID` 有界去重（cap 4096）达到至多一次；
  4. `failed` 为终态、不自动重试（attempts 仍递增）；要"失败定时重投"属另开需求。
- 理由：
  - P0 事件仅进程内同步派发、崩溃丢事件（ADR-015）；outbox 让事件与业务写原子化，是"可靠投递"的最小实现。
  - 网络调用（plugin handler）必须在写事务外：单写连接恒为 1，3s 超时，事务内调外部会拖垮队列。
  - DB 侧 UNIQUE + 总线侧去重双防线，覆盖"投递后崩溃、重启重投"的重复风险。
- 代价 / 边界：
  - 每条事件额外 2 次 writer.Submit（CAS 领取 + 状态回写），单条发帖约 +2 次事务提交；MVP 量级可接受，压测时关注（P3）。
  - `seen` map 有界，触顶清空最坏情况是一次旧事件重投，订阅者自身仍需幂等。
  - outbox payload 正常由 `json.Marshal` 生成；若被外部篡改致反序列化失败，该行停 pending 并让投递报错（不可达路径）。
- 验证：
  - `archtest/outbox_test.go` AST 守卫 + 3 负向注入（AppendOutbox 在闭包外 / Publish 在闭包内 / 调用缺失）；`outbox_integration_test.go` 覆盖同事务写与回滚丢弃、event_id 去重、CAS 状态流转、崩溃恢复、端到端投递；`plugin/events` 去重单测。
  - 隔离库迁移演练：v3（schema=3）→ v6 成功，`domain_event_outbox` 表建成，启动 `RecoverPending` requeued=0；从迁移前备份回滚精确回到 v3。
- 关联：`docs/PHASE1-PLAN.md` R12、`P0-PLAN.md §7 R12`、`ADR-015`。

## ADR-029 P1 数据完整性前置：threads.status 与回复父子同主题约束

- 状态：已采纳（2026-11-19，P1-M1）
- 结论：
  1. R9：迁移 `0004` 给 `threads` 加 `status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','deleted','hidden'))`，常量默认自动回填历史行；`ListThreads`/`GetThread` 只认 `active`，主题软删不物理删（与 `posts.status` 口径一致）；
  2. R8：迁移 `0005` 建两条 `BEFORE` 触发器（INSERT / UPDATE 各一，SQLite 不支持 `INSERT, UPDATE` 连写），当 `parent_post_id` 非空时校验父帖 `thread_id = NEW.thread_id`，不等或父帖不存在即 `RAISE(ABORT)`；应用层 `InsertPost` 在同事务先查父 `thread_id` 再插（双保险，错误更可读）；
  3. 只加触发器与列，不重建 `threads`/`posts` 表，历史数据零改写。
- 理由：
  - 主题软删是 P1 "有帖节点只归档不物理删" 与投票/审核状态的共同前置；`threads` 此前无 `status` 列，删除无处落地（`P0-PLAN §7 R9`）。
  - 投票 score 要记到正确主题，必须先保证回复的父帖与子帖同属一个 thread（R8）；否则跨主题回复会让分数归属错乱。
  - 触发器兜住裸 SQL/UPDATE 路径，应用层守卫兜住业务路径，两层都不依赖调用方自觉。
- 代价 / 边界：
  - `status='hidden'` 暂与 `deleted` 同样从公开视图过滤，二者区分留 P1 审核场景。
  - 触发器对 `parent_post_id IS NULL` 的扁平回帖不生效（符合现状）。
  - 迁移只向前，回退靠迁移前备份恢复（M0 已验证恢复能力）。
- 验证：
  - `migrate_test.go` 历史行回填与 fresh 版本=6；`repo_forum_parent_test.go` 覆盖跨主题写入被拒（应用层 + 裸 SQL 两条路径）；`archtest/parent_thread_test.go` 3 负向注入（缺校验调用 / 缺比较 / 触发器缺 RAISE）。
  - 隔离库实测：2 条历史主题迁移后 `status=active=2`，触发器 `posts_parent_same_thread_insert/update` 均建成，回滚后列与触发器均消失、数据完整。
- 关联：`docs/PHASE1-PLAN.md` R8/R9、`P0-PLAN.md §7 R8/R9`、`ADR-011`。

## ADR-030 认证审计与限流的纯内存边界：入队判定与落库分离

- 状态：已采纳（2026-11-19，P1-M2）
- 结论：
  1. `application/auth` 只做两件事——审计事件**入队**（`audit.go` 有界环形缓冲 cap 512）、限流**判定**（`ratelimit.go` 纯内存滑动窗口）；二者都不 import `database/sql` 或 `infrastructure`，由 `archtest/auth_security_test.go` 守卫 + 负向注入强制；
  2. 落库走 `AuditSink → infrastructure/sqlite/repo_auth_events.go → Writer.Submit` 批量 INSERT，复用 0001 已有 `auth_events` 表与 `idx_auth_events_time`，**无新迁移**；
  3. 定时节奏由 `cmd/server` 持有（1s flush），application 层不起 ticker（沿用 ADR-022「状态机不持有轮询节奏」口径）；
  4. 缓冲溢出 **fail-closed**（D-a 拍板）：`Record` 返回 `ErrAuditQueueFull`，安全门拒绝认证动作并递增 `dropped`；审计缺失期间不放行认证；
  5. 限流键 = 场景前缀 + 已哈希标识（`hash(ip)` / userID / `hash(email)`）；application 层不接触明文 IP/邮箱，明文只在 SSH Session 内存里，不入库/终端/日志；M2 无邮箱流程，emailHash 留接口待 M5；
  6. 默认参数 5 次 / 5m 窗口 / 锁定 15m，四类场景（login_failure / device_start / key_binding / verify）共用配置；计数进程重启清零、多实例不共享（已接受限制）。
- 理由：
  - 单写连接恒为 1、队列 256：把认证失败计数或审计逐条写进 SQLite，攻击者可用暴力尝试直接堵死发帖/投票；内存判定 + 批量落库是唯一能守住写通道的做法。
  - fail-closed 而非 fail-open：审计是安全事件的可追溯性底线，缺审计时继续放行认证等于放弃追责能力。
  - 与 presence.Registry 同构（纯内存 + archtest 禁 SQL import），模式一致、心智负担低。
- 代价 / 边界：
  - 进程重启计数清零，锁定状态丢失（已知限制，写进 OPS）。
  - `audit_dropped>0` 意味着认证动作正在被拒绝，必须告警（OPS §3 已记）。
  - key_binding 与登录共用 5 次额度：同一用户 5 分钟内第 6 次登录或 key 操作会被锁；是否单独放宽留待真实数据支撑。
- 验证：
  - 550 个测试函数全绿；新增审计有界溢出 fail-closed/dropped、批量 flush 顺序与失败保留、限流三类键独立/锁定恢复/RetryAfter 递减/容量淘汰、archtest 6 守卫（含 2 负向注入）。
  - `scripts/repro_auth_limit.sh` 实测 `accepted=3 rejected=2 retry_after=2m0s auth_events=5 dropped=1`，隔离临时库、生产 data 未动。
- 关联：`docs/PHASE1-PLAN.md` §2.5、`ADR-022`。

## ADR-031 注销边界：按 userID 撤销会话，撤销不可回滚故先于审计

- 状态：已采纳（2026-11-19，P1-M2）
- 结论：
  1. SSH 层新增 `RevokeUserSessions(ctx, userID) int`（照 P0-18 `RevokeSessions(ctx, fingerprint)` 模式），application 定义 `UserSessionRevoker` 端口；`/logout` 命令撤销该账号**全部**在线会话，含当前连接；访客拒绝；
  2. `LogoutService` 顺序为**先撤销、后审计**；审计入队失败时撤销**不回滚**，只上报「注销已生效，但审计记录失败」；
  3. 已吊销公钥的已有连接由 P0-18 的 `RevokeSessions` 覆盖，确认无遗漏、不重复实现；
  4. 密码变更复用同一 `UserSessionRevoker`，M5 邮箱包接入时接线，本阶段只预留接口、不臆造密码流程。
- 理由：
  - 会话撤销是内存动作、不可回滚；把已踢下线的会话恢复以凑一条审计，比少一条审计危险得多——安全取舍明确。
  - 「先撤销后审计」保证即使审计队列满（fail-closed 拒绝入队），注销本身仍然生效，用户不会被困在已决定退出的会话里。
- 代价 / 边界：
  - 审计失败时用户会看到部分成功提示，需明确文案（已实现）。
  - 撤销只覆盖在线会话；已签发的外部凭据（如未来的 CLI token）不在本 ADR 范围（P2）。
- 验证：`logout_test.go` 覆盖先撤销后审计、审计失败不回滚、访客拒绝；`revoke_user_test.go` 覆盖同账号 2 会话全撤、异账号/访客不误伤；archtest `TestLogoutRevokesBeforeAudit` 守卫。
- 关联：`docs/PHASE1-PLAN.md` §2.5、`P0-PLAN.md §4.6`、`ADR-024`。

## ADR-032 投票分数与主题活跃度正交：AdjustScore 与 TouchThread 分离

- 状态：已采纳（2026-11-19，P1-M3）
- 结论：
  1. `TouchThread`（`UPDATE threads SET last_reply_at=?, score=score+?`）**仅供发帖/回复**，推进活跃度以维持 `SortLatest` 语义；
  2. 投票走独立的 `AdjustScore`（`UPDATE threads SET score=score+? WHERE id=?`），**绝不触碰 last_reply_at**——评价行为不能把旧主题顶到"最新"；
  3. `WriteTx` 新增 `GetVote` / `UpsertVote` / `DeleteVote` / `AdjustScore` / `PostThreadID`；`DeleteVote` 显式存在是因为 `votes.value CHECK(value IN (-1,1))` 不允许用 0 表示撤销，把删除塞进 UpsertVote 会造成沉默逻辑错误；
  4. 投票幂等靠 `UNIQUE(user_id,target_type,target_id)` + `delta = 新值 - 旧值`（同值 delta=0 直接返回）；事件 `EventVoteCast` 与业务写同事务入 outbox，幂等键 `vote:<uid>:<type>:<id>`。
- 理由：分数（`score`）与活跃度（`last_reply_at`）是两个正交语义：前者是评价累计、后者是时间排序。P0 的 `TouchThread` 把两者塞进一条 UPDATE 是因为当时只有回复会改分数；P1 引入投票后必须拆分，否则投票会污染排序。
- 代价 / 边界：
  - 多一个端口方法与一条 SQL，换来排序语义不被评价行为污染（值得）。
  - 事件幂等键固定 → 改票/撤销的后续事件被 outbox 的 `event_id UNIQUE` 静默去重，插件只感知**首次投票**。若插件需感知每次票变，需换含序号/值的键（待拍板，当前按首次投票口径）。
  - 终端 `/vote` 只对当前打开的 thread；Service 层已完整支持 post 目标（取所属 thread 记分、不碰 last_reply_at），命令行暂不暴露。
- 验证：`archtest/vote_test.go` + `vote_negative_test.go` 正向守卫 + 负向注入（vote.go 出现 TouchThread / last_reply_at / 缺 ON CONFLICT 均失败）；`vote_integration_test.go` 覆盖幂等、改票 delta（-1→1=+2）、撤销、访客拒绝、目标不存在、分数事务一致、事件入 outbox。隔离库实测投票路径零 TouchThread 调用。
- 关联：`docs/PHASE1-PLAN.md` §2.4、`ADR-015`。

## ADR-033 FTS5 外部内容表的索引一致性：posts_fts_indexed 跟踪表

- 状态：已采纳（2026-11-19，P1-M3）
- 结论：
  1. 迁移 `0007` 建 FTS5 外部内容表 `posts_fts`（`content='posts'`, `content_rowid='id'`, `tokenize='trigram'`）+ 三条同步触发器（INSERT/UPDATE/DELETE）+ 存量回填；
  2. 新增 `posts_fts_indexed(post_id)` 小表作为"哪些行真正在索引里"的**唯一事实来源**：FTS5 外部内容表对未索引行发 `'delete'` 会报 `disk image is malformed`（实测复现），而 `SELECT rowid FROM posts_fts` 会从外部表合成行、无法判断是否已索引；
  3. 触发器只在确认 `posts_fts_indexed` 有该行时才发 `'delete'`，顺序为先删倒排项再删标记；status 转 `deleted`/`hidden` 时撤索引，与 `PostReader` 只暴露 `status='normal'` 的口径一致；
  4. 恢复备份后**不自动重建**，需显式 `motdctl fts-rebuild --db <path>`（用 FTS5 `delete-all` + 仅回填 normal 行，不用 `DELETE FROM posts_fts`——同样踩 malformed 陷阱）。
- 理由：外部内容表没有"是否索引"的内省手段，靠推测会损坏索引；一张小跟踪表换来确定的同步语义。trigram 分词让中文短语可检索，代价是 <3 码点查询无结果，应用层回退 LIKE 并转义 `%`/`_`/`\`。
- 代价 / 边界：
  - 多一张小表，未来换索引实现时仍需维护它。
  - MATCH 串整体加引号、内部引号翻倍、仅作绑定参数，用户输入 `*`/`"`/`%`/`_` 均为字面量（archtest 负向注入守拼接）。
  - 页长锁 20、摘要 ≤240 字节，适配 1 Mbps 约束。
- 验证：隔离库实测 v6→v7 迁移在 4 条历史帖上回填 `posts_fts=4` / `posts_fts_indexed=4`、三触发器建成；回滚到备份后 schema=6、FTS 表消失、数据完整。`archtest/search_test.go` 负向注入（拼接 MATCH / 无 trigram / LIKE 无 ESCAPE 均失败）。
- 关联：`docs/PHASE1-PLAN.md` §2.6、`OPS.md` FTS 恢复说明。

## ADR-034 多行正文提交语义：Enter 换行，哨兵 / Ctrl-D 提交

- 状态：已采纳（2026-11-19，P1-M4，二轮讨论拍板）
- 结论：
  1. 正文编辑态：`KeyEnter` **只插入换行**、绝不提交；单独一行恰好为 `.`（`SubmitSentinel`）后按 Enter 提交，哨兵行不进正文；`KeyCtrlD`（0x04）立即提交；
  2. `KeyPaste` 整块进正文，内部换行**不拆成 KeyEnter**——粘贴进来的 `.` 行不会被误判为哨兵（`decoder.go` 保证一次 paste 只产一个 `KeyPaste`）；
  3. 标题字段仍是单行、Enter 确认（两套 Enter 语义在 UI 明示）；
  4. **禁用 Ctrl+Enter 作为提交键**：通用 VT/xterm/conhost 下 Enter / Shift+Enter / Ctrl+Enter 都发 `0x0D`、字节级不可区分；且"Ctrl+Enter 提交"与"Enter 换行"物理冲突（都要解释 CR）。Ctrl+Enter 增强（kitty CSI-u / modifyOtherKeys 探测）整体挪 P2；
  5. UI 硬约束：服务端固定文案只出现哨兵与 Ctrl-D，archtest + 单测双重禁止 "Ctrl+Enter" 字样进入 `cmd/server` / `application/input` 生产源码。
- 理由：候选 B（Ctrl+Enter 提交）语义直观但跨 SSH 客户端不可靠，且需终端能力探测（P1 对外接口冻结期不引入）；候选 A（Enter 提交）正文无法换行。选 C 是零协议依赖、与 here-doc 习惯一致，代价是用户记一个新约定。
- 代价 / 边界：编辑器退格不跨行回到上一行（已知限制，逐字符 `\b` 擦除的边界）；行数与字节数双上限防滥用。
- 验证：`input/composer_test.go` 8 项（换行/哨兵/Ctrl-D/多行粘贴/退格/双上限/多字节计数/副本）；`archtest/composer_test.go` 3 守卫 + 负向注入（上层 import / paste re-split / 粘贴分支 KeyEnter / Ctrl+Enter 文案）；`TestNoCtrlEnterInServerFixedText` 扫源码零命中；真实 SSH e2e 正文提交改 Ctrl-D、标题仍 Enter。
- 关联：`docs/PHASE1-PLAN.md` §2.7、`ADR-019`、`ADR-027`。

## ADR-035 过期设备码清理策略：只清自身过期行，保护在途登录

- 状态：已采纳（2026-11-19，P1-M4）
- 结论：
  1. `device_auth_states` 清理在进程启动时立即执行一次、之后每小时一次，走唯一写入队列的单事务参数化 DELETE；
  2. **只删自身 `expires_at` 已过的行**，四态（pending/authorized/denied/expired）共用该条件；未过期的 `pending` 代表在途设备码登录，绝不删除（OPS 多次记过：手工 DELETE 会让在途会话因 `ErrDeviceAuthNotFound` 中断）；
  3. 可重复执行，第二次零删除（幂等）；停机时先等清理循环退出再关写队列（defer 顺序固定）。
- 理由：表只增不删会让历史行无限累积（`idx_device_auth_states_expiry` 已存在但无 job）；按 `expires_at` 而非按 status 过滤，是因为 pending 之外的终态也可能尚未过期，过早删会破坏审计追溯。
- 代价 / 边界：pending 严格按自身 `expires_at` 清理；若客户端恰在过期瞬间仍在轮询，下一次会拿到"码不存在"——既定口径，grace period 需另立 ADR。
- 验证：`maintenance_test.go` 覆盖终态过期被清 / pending 未过期保留 / 在途仍可迁移 / 幂等；进程级 ticker 测试覆盖启动即跑 + 周期跑 + ctx 取消退出。
- 关联：`OPS.md` Device Flow 状态表小节、`DECISIONS.md` ADR-022。

## ADR-036 公钥使用痕迹写入策略：5 分钟节流 + 异步写

- 状态：已采纳（2026-11-19，P1-M4）
- 结论：
  1. SSH 公钥认证成功（且指纹命中已绑定用户）后，经 `UsageRecordingResolver` 装饰器记录 `ssh_keys.last_used_at`；访客、失败认证不记录；
  2. 同一指纹 **≥5min 最多排一次写**，写动作在独立 goroutine 经写入队列完成，失败只记日志、**不影响认证结果**；
  3. 采用「先标记窗口、后异步写」：写失败时该钥匙要等下一个 5min 窗口才重试——详情页最近使用时间**可能滞后一个窗口**（已接受）。
- 理由：每次连接都写库会放大唯一写通道的压力（单写连接恒为 1、队列 256）；`last_used_at` 是展示性字段，不值得为它阻塞认证或挤占业务写入。
- 代价 / 边界：至多一个窗口的展示滞后；停机时 `Wait()` 等在途写完成后再停写队列。
- 验证：`key_usage_test.go` 覆盖同钥节流 / 跨钥独立 / 写失败不阻塞 / 默认窗口 ≥5min；装饰器测试覆盖绑定用户记录、访客与失败不记录、空依赖 panic。
- 关联：`docs/PHASE1-PLAN.md` §2.8、`OPS.md` 认证安全小节。

## ADR-037 `/keys <fingerprint>` 详情边界

- 状态：已采纳（2026-11-19，P1-M4）
- 结论：
  1. `KeyBindingReader` 新增 `GetByFingerprint(ctx, userID, fingerprint)`，**userID 必须进查询 WHERE**；跨用户与不存在返回**同一错误**，不区分以避免指纹枚举；
  2. 详情含 label / created_at / last_used_at；`label` 当前无输入口、恒为空，输出仍经净化；
  3. 只覆盖未吊销钥匙（与 `/keys` 列表同口径），已吊销历史不展示。
- 理由：指纹是半公开标识（认证成功才可见），若详情接口跨用户可查，会变成账号关联枚举面；统一错误是最小泄露口径。
- 代价 / 边界：已吊销钥匙历史查不到（P2 可加管理端 `/keys --all`）；label 设置入口未开放（P2 可加 `/key label`）。
- 验证：`key_detail_test.go` 覆盖正常读取 / 不存在 / 跨用户 / 已吊销；`TestRunKeysDetailAndOwnership` 覆盖详情字段、跨用户不泄露、恶意参数不回显、无参仍列表。
- 关联：`ADR-024`、`OPS.md` 认证安全小节。

## ADR-038 软删主题的跨读路径可见性

- 状态：已采纳（2026-11-19，v0.2.1）
- 结论：
  1. 作者删除主题时**保留下属回复行**（不级联物理删，护住引用完整性），仅置 `threads.status='deleted'`；回复行的 `posts.status` 不变；
  2. 详情、列表、全文检索三条读路径都把"所属主题 `status<>'active'`"视为不存在；检索在 SQL 层 `JOIN threads t ON t.id = p.thread_id AND t.status='active'` 落地（FTS MATCH 与 LIKE 回退两条分支都加），由 archtest 静态守卫 + 负向注入锁定；
  3. 所有权判定集中在 application 层唯一的 `canModify(userID, ownerID)`；非作者、访客、目标不存在或已删统一回「仅作者可删除」固定文案，不泄露状态、不回显事务内部错误；
  4. 无公开恢复入口；`hidden` 预留给将来治理角色，当前无入口；冻结账号仍可删自己内容（统一提示口径优先）。
- 理由：仅按 `posts.status` 过滤会让已删主题下的回复仍被 `/search` 命中并泄漏，违背"随主题一并不可见"的产品决策。级联硬删会破坏引用完整性；在 FTS 同步层过滤易与 `RebuildFTS` 漂移——SQL 读路径过滤是最小且确定的位置。
- 代价 / 边界：
  - 已删内容留在库里，只能从 DB 层找回（非目标）。
  - FTS 索引行仍在（删除主题不动 posts 行），但被查询层过滤，属预期行为，无需 `fts-rebuild` 清理。
  - 将来引入版主时只在 `canModify` 一处加 `author OR moderator` 分支，`hidden` 作治理位复用。
- 验证：`delete_test.go`（应用层 6）/ `repo_delete_test.go`（真实 SQLite 3）/ `repo_search.go` 两条 JOIN 过滤 / `archtest/delete_test.go` 4 守卫 + 3 负向注入 / `cmd/server/delete_command_test.go` 端到端 5 用例。三条验收（作者删后回复保留但不可见、非作者零变更、已删内容返回不存在）全过。
- 关联：`docs/PHASE2-PLAN.md §1.2`、`ADR-013`（软删口径）、`ADR-033`（FTS 索引一致性）。

## ADR-039 跨行退格合并的回显协议（方案 B 收窄版）

- 状态：已采纳（2026-11-19，v0.3.0）
- 结论：
  1. 正文编辑态在空行行首按退格：composer 删除该空行、`ComposerOp` 带回 `MergedLines` / `MergedText`，回显层用 `safe.CursorUp(1)` + `\r` + `safe.ClearToEOL()` 整行重绘合并后的文本，光标停在两行拼接处可续输；首行空退格无动作；
  2. **ADR-027 白名单零改动**：全程只用 SGR + EL + CUU，绝不内联 ESC、绝不放宽 CUP(H/f)/CUD(B)/ED/DECSET；用户文本仍经 `SanitizeText`，与编辑控制分属两条写入路径；
  3. 标题阶段维持单行退格语义，不做跨行；行内左右移动本版不做；
  4. 超宽自动折行与 kitty keyboard protocol 留待后续（与既有宽字符退格限制同源）。
- 理由：用户真实痛点是"跨行退格不回上一行"，方向键只是设想的手段。输入侧方向键早已可用（decoder 提升 CSI/SS3 为 KeyArrow*），真正限制是输出侧禁 CUP。方案 B 用逻辑光标 + 当前行整行重绘在白名单内达成目标，不牺牲安全边界。
- 代价 / 边界：
  - 单逻辑行超终端宽度自动折行时 CUU 只上移一个物理行（本版不处理）。
  - 回显整行重写有少量额外字节（1 Mbps 下可忽略）。
- 验证：`composer_test.go` 5 项（空行并入 / 首行无动作 / 连续空行逐次合并 / 合并后续输 / 宽字符行）；`line_echo_test.go` 3 项（输出只含白名单、零 CUP/CUD/ED/DECSET、合并文本 ESC 被净化）；端到端合入→续输→Ctrl-D 提交落库；`archtest/safe_test.go` 2 守卫 + 负向注入（CUP/CUD/ED 必失败）；`ansi.go` git diff 为空。
- 关联：`docs/PHASE2-PLAN.md §2.2`、`ADR-027`、`ADR-034`、`ADR-038`。

## ADR-040 邮箱验证码两段式投递与单事务原子消费

- 状态：已采纳（2026-11-19，v0.3.0-email）
- 结论：
  1. 配置显式 resolve：`MOTD_EMAIL_ENABLED` 默认 false，开启时逐项校验 `MOTD_SMTP_HOST` / `PORT` / `USER` / `PASS` / `FROM`，缺哪项报哪项、不静默兜底；凭据只从 `.env`（0600）读，不进仓库、不进 systemd unit、不进日志（启动日志只打 enabled/host/port）；
  2. **两段式发信**：先在 `Writer.Submit` 单事务内写 `email_tokens`（只存 `code_hash`），**提交成功后才在事务外发信**（465 隐式 TLS + PLAIN AUTH，有界并发 4、10s 超时）；发信失败保留 token 自然过期，不补偿删除；`/resend` 另写新 token 并把旧 token 置 `consumed_at`；
  3. `/verify <code>` 单事务原子消费：`UPDATE email_tokens SET consumed_at=? WHERE id=? AND consumed_at IS NULL` + `RowsAffected==1` 抢消费（并发下仅一者成功）→ 校验 purpose / expires_at / attempts<5 → 同事务建 `users` / `identities` / `local_credentials`；错码在事务内 `attempts++`；
  4. 密码在 `/verify` 之后以独立隐藏字段设置（设计稿 §4.5），不经命令行参数——避免密码回显与会话暂存；argon2id 19MiB / t=2 / p=1。
- 理由：3s 写超时扛不住 SMTP 网络调用，网络调用绝不进唯一写连接事务；验证码只存 HMAC 散列，库泄漏也无法离线爆破 6 位码；密码独立隐藏字段是 `setSecret()` 的既定语义。
- 代价 / 边界：
  - 发信失败时 token 留库自然过期（TTL 10min），无补偿删除。
  - HMAC pepper 为进程随机：**服务重启会使在途验证码失效**，用户可 `/resend` 重发；换得"库泄漏无法离线爆破"（已接受，写进 OPS）。
  - 无新迁移：`email_tokens` / `users` / `identities` / `local_credentials` 均已在 0001 建好，迁移仍止于 0007。
- 验证：705 测试全绿；`archtest/email_test.go` 4 守卫（发信不在写事务 / code 只存 hash + 两个负向注入）；真 SQLite 并发抢消费测试；配置缺项逐个点名；`.env` gitignore + 工作区/HEAD 凭据零命中；163 SMTP 465+AUTH 实测通过（凭据不落代码）。
- 关联：`docs/PHASE1-PLAN.md §2.9`、`ADR-015`、`ADR-033`。

## ADR-041 邮箱身份归属：email 作 provider_uid，不自动合并（D-c / R7）

- 状态：已采纳（2026-11-19，v0.3.0-email）
- 结论：
  1. 邮箱身份的 `provider='email'`，`provider_uid` 取 email 本身（GitHub 身份取数字 UID，两者不混淆）；`primary_email` **仍不加 UNIQUE**——这是"禁止按邮箱自动合并"的结构保证（沿用 ADR-023）；
  2. 邮箱被占用时回固定提示，**绝不自动合并账号**；当前无 `/unlink` 命令，提示不指向不存在的操作（改为"如需变更归属请联系管理员处理"）；
  3. `/recover` 响应统一"若邮箱已注册则已发送"，防账号存在性枚举；找回成功后撤销该用户全部 SSH 会话（复用 M2 `UserSessionRevoker` 提交后调用模式）。
- 理由：同一邮箱可能属不同 GitHub UID（ADR-023 已定），自动合并会造成账号串号；找回响应统一文案避免把邮箱目录暴露给未认证调用方。
- 代价 / 边界：变更邮箱归属当前只能从数据层处理（`/unlink` / `/link` 属后续阶段）；用户看到"联系管理员"但暂无自助入口——优于指向死命令。
- 验证：`email_service_test.go` 占用场景（seedIdentity 后 RequestCode 报 ErrEmailOccupied）、找回未知邮箱统一响应、recover 会话撤销；固定提示断言不指向"解绑"命令。
- 关联：`ADR-023`、`docs/PHASE1-PLAN.md §2.9 R7`。

## ADR-042 邮箱通道参数与限流口径

- 状态：已采纳（2026-11-19，v0.3.0-email）
- 结论：
  1. SMTP 走 465 隐式 TLS + PLAIN AUTH（163 实测可用），发信错误归一、不回显 AUTH 串；单一 `email.Sender` 实现，不预留第二 Provider（避免孤儿抽象）；IMAP 993 本轮不接（找回走发码 + SSH 输入，不读邮箱）；
  2. 验证码 TTL 10min；限流复用 M2 `application/auth/ratelimit.go`，键 `hash(email)`：60s 重发冷却、每小时 5 封、5 次尝试；叠加 IP 维度防滥号；错码 5 次锁 15min；
  3. 命令面只做终端 `/register` / `/verify` / `/resend` / `/recover` / `/login email`，**不做任何 HTTP 端点**（对外接口继续冻结）。
- 理由：163 授权码非登录密码，只放服务端 `.env`；滥用入口（公开注册）必须双维限流；HTTP 端点会焊死对外契约（P3 未验证前不做）。
- 代价 / 边界：进程重启清空限流计数（M2 已知限制）；163 单账号发信量受服务商限制，5 封/小时是保守值。
- 验证：`infrastructure/email/sender_test.go`、`config/email_test.go`（缺项报错）、限流用例；命令白名单测试。
- 关联：`ADR-030`、`docs/PHASE1-PLAN.md §2.9`。

## ADR-043 邮箱交互实测修复：掩码复位、密码二次确认、SSH 登录引导

- 状态：已采纳（2026-11-19，v0.3.0 实测后修复）
- 背景：用户真实测试 v0.3.0-email 时报告三点——① `/verify` 输入密码后，下一条命令的 `/` 被 `*` 掩码覆盖，之后进 composer 才恢复；② 注册设密码建议二次确认；③ root / 密码登录被拒时只见通用 `Permission denied (publickey)`，不知道正确登录姿势。
- 结论：
  1. **掩码复位收口**：`emailFlow.finish()` 统一 `setSecret(false)`，`step()` 用 `defer` 保证 done / cancelled 立即复位；新增 `clearEmailFlow()` 在"新命令取代未结束的旧流程"三类出口（register/recover 新建、verify 新建、login email 新建）也先复位。全仓 `setSecret(true)` 生产调用点仅 email_flow.go 一处，已穷尽。
  2. **密码二次确认**：`/verify` 密码阶段（注册/找回）输入两遍，一致才提交 service（此时才消费验证码）；不一致回退首次输入，**不消费验证码、不计尝试次数**，两遍均掩码。`/login email` 保持单次——登录失败可重试，确认徒增步骤。
  3. **SSH banner 始终下发引导**：`bannerCallback` 由"仅满员提示"改为无条件下发两行固定文本（非 root 用户名 + 公钥登录、未绑定则访客可 `/register`、禁 root 禁密码），满员时叠加满员提示（抽出 `fullLine()` 与 rejectFull 共用）。banner 是服务端固定文案（ADR-019），不含用户数据或凭据；wire-level 测试用真实 gossh 客户端以 root 连接，确认被拒同时收到引导。
- 理由：三点都是真实用户卡点——掩码泄漏是可用性 bug，密码确认防手抖输错，banner 解决"被拒却无提示"的入口死路。
- 代价 / 边界：二次确认多一次输入（仅注册/找回，一次操作）；banner 增加两行固定文本（1 Mbps 下可忽略）；弱密码仍由 service 校验（两次一致后返回 ErrWeakPassword，结束流程提示重新 `/verify`）。
- 验证：717 测试全绿（705 → +8 邮箱确认 → +4 banner）；`banner_test.go` 4 测试含 root 被拒 wire-level 验证、banner 无凭据负向断言；`email_flow_confirm_test.go` 8 测试覆盖三种掩码复位出口、两次一致/不一致、不一致不消费验证码、login 无确认、Esc 取消。
- 关联：`ADR-019`、`ADR-026`、`ADR-042`。

## ADR-044 验证码只读预检与爆破 oracle 边界

- 状态：已采纳（2026-11-19，v0.3.1）
- 结论：
  1. `/verify <码>` 在收密码**之前**调用 `PreCheckCode()` 做只读预检（格式 + token 存在/未消费/未过期/attempts<5），命中失败直接回固定文案、不进密码交互；预检走读池、零写库、不消费、不计尝试；
  2. 预检本身是"码是否有效"的 oracle，用**独立限流实例**（不复用 Verify 的 attempts——否则一次正常 verify 预检+提交会把 5 次挤成 2 次）：邮箱哈希 + IP 双作用域、带 `precheck:` 前缀、20 次/小时；邮箱作用域跨 IP 共享，轮换 IP 绕不过；错码与有效码都计数，格式非法不计（无 oracle 价值）；
  3. **TOCTOU 正确性**：预检只是体验优化、不是信任决策；正式 `Verify()` 仍重做全部校验并抢消费。预检通过后码失效（并发消费/超时）会让用户白输密码——这是可接受的残留；
  4. token 的 `attempts>=5` 仍在预检中校验，但正常 cmd 路径的错码已被预检拦下且不计数，故 `token.attempts` 退化成"绕过 cmd 层直接调 Verify"的纵深兜底，oracle 前线由预检限流承担。
- 理由：v0.3.0 实测中无效码也要用户输两遍 19MiB argon2 密码才报错；查 token 原本排在哈希之后。把查码整体移出写事务会破坏原子消费语义，故选只读预检 + 独立限流。
- 代价 / 边界：多一次只读查询；预检通过后仍有 TOCTOU 窗口；限流阈值 `DefaultEmailPrecheck`（20/h、1h 窗口、15min 锁定）集中一处可运维调整。
- 验证：737 测试全绿；archtest「预检不写库」AST 守卫 + 负向注入 + 只读实现零误报；无效/过期/已消费/限流四路不进密码交互；预检通过仍由 Verify 独立拒绝。
- 关联：`ADR-040`、`ADR-042`、`docs/PHASE2-PLAN.md §5`。

## ADR-045 `.env` 自动加载与环境变量优先级

- 状态：已采纳（2026-11-19，v0.3.1）
- 结论：
  1. 程序启动自动加载 `.env`：查找顺序 **cwd → 可执行文件目录**（软链先 `EvalSymlinks`），命中第一个即停、不合并；两处都没有则静默走纯 env，不报错；
  2. **进程已有环境变量优先**，`.env` 只填空缺（判据 `os.Getenv(key)==""`，空串等同未设置，与既有 config 约定一致）——systemd `EnvironmentFile=` 注入的凭据不会被旁路文件静默覆盖；
  3. 开关 `MOTD_DOTENV_DISABLED=true`（大小写不敏感、允许首尾空白）完全关闭加载，默认 false（开启）；建议由 `Environment=` 外部注入（写进被加载的 `.env` 要下次启动才生效）；
  4. 加载副作用留在 `cmd/server/main.go`（`LoadDotEnv()` 在 `config.Load()` 之前），**`config.Load()` 保持零文件读取**——否则 config 单测会读到项目根含真实授权码的 `.env`，踩安全红线；
  5. 手写 parser（约 60 行、纯标准库、零新依赖）：整行注释/空行忽略、可选 `export` 前缀、成对引号去包裹、未加引号的 `#` 是普通字符（授权码可能含它）、重复键最后一行为准、非法行静默忽略、单行上限 1MiB；启动日志只打 `配置来源 file=<路径>` / `env_only`，绝不打键值。
- 理由：用户实测嫌手动 `source .env` 麻烦，要求默认自动加载；但 `.env` 覆盖进程环境会让 systemd / 容器注入的凭据来源变模糊，是部署隐患，故按 dotenv 标准惯例选"环境优先"。加载放入口而非 config 是为了测试不碰真实凭据。
- 代价 / 边界：多一个启动副作用点；非法 `.env` 行静默忽略（不阻塞启动，但也不会提示写错）；`MOTD_DOTENV_DISABLED` 写进被加载文件要到下次启动才生效。
- 验证：737 测试全绿；`dotenv_test.go` 6 用例（进程优先 / 查找顺序含不合并 / 格式容错 / 开关 / 错误不泄漏 / 配置字段），全部用 t.TempDir() 自建占位 `.env`，零命中真实凭据路径；go.mod/go.sum 零变更。
- 关联：`ADR-021`、`docs/PHASE2-PLAN.md §5`。

## ADR-046 展示时区可配置（MOTD_DISPLAY_TIMEZONE）

- 状态：已采纳（2026-11-19，v0.3.1.1 patch）
- 背景：用户实测反馈帖子/回复时间显示为 UTC。存储层 `created_at` 是 INTEGER（Unix 秒，时区无关），application/domain 的 `.UTC()` 属存储归一化；缺陷面只在展示层硬编码 UTC（`view.go:303` 的 `formatTime` 与 `ssh_handler.go` 的 `/keys` 详情）。
- 结论：
  1. 新增 `MOTD_DISPLAY_TIMEZONE`（默认 `Asia/Shanghai`），支持 IANA 名 / UTC / Local；配置在 `config.Load()` 解析一次得到 `DisplayLocation *time.Location`，非法值**启动期直接失败**、绝不静默回落 UTC；
  2. main 启动期一次性 `terminal.SetDisplayLocation()`，论坛 `formatTime` 与 `/keys` 的 `formatKeyTime` 共用 `terminal.DisplayLocation()` 单一来源；包级 `atomic.Pointer[time.Location]` 保证多会话并发无锁读取；
  3. 论坛格式串 `MM-DD HH:MM` 不变、`sec<=0` 空串行为不变；`/keys` 后缀取**时间戳自身所在时刻**的时区缩写（CST/UTC/EST…），空缩写回退时区名——比取 `time.Now()` 更准，跨 DST 历史时间正确；不再硬编码 " UTC"；
  4. 二进制内嵌 IANA 时区库（`_ "time/tzdata"`），distroless/scratch 无需挂载 zoneinfo；std lib、无第三方依赖。
- 备选与否决理由：
  - **(a) 给 `RenderScreen` 加显式 location 参数**：更纯，但需改 17 处调用点（含 15 处测试）与两路下发（pageController / loginPresenter），且 cmd/server 测试里约 10 处直接构造 `&pageController{}` 会产生 nil location、需额外防御——对 patch 扩散面过大，否决。
  - **(b) 包级 set-once location**：采用。`RenderScreen` 的纯函数承诺澄清而非破坏——它仍是 (Screen, width) + 一份不可变进程配置的确定性函数，不读时钟、不碰 DB/SSH、不持有会话状态；测试用 `SetDisplayLocation` + `t.Cleanup` 复位隔离。
- 代价 / 边界：二进制因内嵌 tzdata 略增数百 KB；时区是进程级策略、不随会话变化（若将来要多用户各自时区，需重设为 session 级并回到方案 a）；`-race` 因本机无 gcc 未跑，但并发读测试 `go test -count=20 -run TestDisplayLocationConcurrentRead` 稳定通过。
- 验证：749 测试全绿（737 → +12）；config 5 用例（默认 / IANA / UTC / Local / 非法值 fail-fast）+ terminal 5 用例（不同 location 输出不同 / 格式串不变 / sec<=0 / 并发读）+ cmd/server 2 用例（/keys 时间按配置、后缀非硬编码）；E14「时间未带时区」/ E18「环境变量静默兜底」门禁 PASS；业务层 UTC 归一化 git diff 0 行。
- 关联：`ADR-045`（.env 自动加载优先级）、`docs/PHASE2-PLAN.md`。

## ADR-047 列表符号体系裁决与作者名投影（R11 翻转）

- 状态：已采纳（2026-11-20，v0.3.3）
- 背景：用户提出把分数 `[0]` 改 `▲0`、帖子 ID `id=xxx` 改 `[xxx]`、`#` 留作标签、作者 `#ID` 改 `@ID`，理由是"符合通用论坛标志、降低心智负担"。PM 与设计师独立论证后收敛。
- 结论：
  1. **否决分数 `▲` 化**：▲/▼ 在 Reddit/HN 是**可点击投票控件**，不是净分标签；只读行里用属假可供性。score 可为负（投票 value∈{-1,1}、`AdjustScore(delta)`），`▲-3` 语义自相矛盾。且 U+25B2 是 East Asian Ambiguous，CJK 终端占 2 列、en_US 占 1 列，会让 80 列定宽表漂移。**分数保持 `[N]`**；
  2. **否决帖子 ID 改 `[109]`**：`id=` 的等号是"这是可复制命令参数"的功能提示，且 v0.3.2 刚建立该肌肉记忆；`[` 在本项目已是 UI chrome 通用符（`[%s]` 动作键、`[提示]`、markdown 链接），再挤进 ID 只会更糊。**保持 `id=109`**；
  3. **`#` 不预留给标签**：无 tags 表/规划/排期；符号不是稀缺资源，标签真上线那天再挑 `#`。不写进 UI 与帮助，仅在本文留档；
  4. **作者栏 `#7` → `@username`**（本轮唯一改动）：`#7`/`@7` 都是把 `users.id` 主键漏进 UI，换壳不解决"用户看不出是谁"。username（3–20 字符、NOCASE UNIQUE，ADR-005）才是稳定可读句柄——**翻转 R11「作者名投影留存价值未验证、0.x 不固化」**：当初是"无证据"，本次符号讨论构成第二个触发器（作者标识从锦上添花变功能必需）。
- 实现边界：
  - username 不进 domain（Thread/Post 无此字段）；新增只读端口 `UsernameReader.UsernamesByIDs`，sqlite 参数化批量查 `users`，一屏一次（非 N+1）、不按 status 过滤（历史帖作者仍显示）；
  - application 组装 Screen 时填预留字段 `RowMeta.Author`（ADR-014 早已预留）；查不到留空；
  - **信任域分离**：username 是用户数据，作者段在渲染循环里走 `ext()`→`RenderUser`；`metaID`/`metaTail` 仍为纯本地数值走 `sys()`→`RenderSystem`；删掉原来同时混拼的 `metaFixed`。新增 AST 守卫静态断言作者段不得进 `sys()`，并有 ANSI/双向控制符负向注入测试；
  - CJK 截断：按显示宽度（汉字/emoji 2 列）在 rune 边界切，上限 10 列、ASCII `~` 收尾（禁 U+2026 同为 Ambiguous），先截断后净化；空 username 回退 `#ID`。
- 代价 / 边界：标题可用列 46→38（名称化成本）；详情页顶部作者行本版不新增（遵循"其余符号不动"）；**回复数 `rN` 格式已改但生产路径 `RowMeta.Replies` 从未赋值、实际不显示**——用户拍板"暂搁"，不加计数投影，待后续单独实现。
- 验证：768 测试全绿（755 → +13）；批量查询 spy 断言一次；ANSI 注入 `Author="\x1b[2J\r\x07\u202e"` 净化后无原始控制符；中文截断后 `utf8.ValidString` 且宽度≤10。
- 关联：`ADR-005`（username 约束）、`ADR-014`（信任域与预留字段）、`ADR-019`（终端白名单）、`docs/P2-PLAN.md §10`（@提及推 P3）。

## ADR-048 节点三不变量：slug 规则 / 归档不物理删 / seed 不可变

- 状态：已采纳（2026-11-20，P2-M0，补 P1 欠账）
- 背景：`PHASE1-PLAN.md §2.10` 与决策台账 #6 承诺 P1 固化节点三不变量，但实际未落地（原计划占用 ADR-030，该编号实为「认证审计与限流的纯内存边界」；`nodes` 表亦无 `status` 列）。P2-M1 要做节点 CRUD，若欠账不补，物理删会撞 `threads.node_id` 的 FK，且没有归档语义。本 ADR 接当前最大 ADR-047 编号，不复用 030。
- 结论：
  1. **slug 规则**：形如 `/dev/ai`、`/etc/rant`、`/bin/help`。字符白名单仅 **ASCII 小写字母 `[a-z]`、数字 `[0-9]`、斜杠 `/`**——大写、连字符、下划线、空格、Unicode、emoji 一律拒绝；结构为绝对路径式：必须以 `/` 开头、**1–2 段**（`/help` 或 `/dev/ai`），段间单个 `/`，禁止结尾 `/` 与连续 `//`；每段 1–32 字符、整体 ≤64 字符。`slug` 在 `0001_init.sql` 已 UNIQUE，故大小写敏感问题因白名单无大写而不存在。**M0 只固化规则、不写 `ValidateSlug` 函数**——无调用者的校验函数是孤儿能力，M1 落地 CRUD 写路径时再实现并由写路径强制；
  2. **归档不物理删**：`threads.node_id INTEGER NOT NULL REFERENCES nodes(id)`，有帖节点物理删会被 FK 拒绝。节点只取 `status IN ('active','archived')`，`0008_nodes_status.sql` 用 `DEFAULT 'active'` 常量自动回填历史行。归档是可逆 `UPDATE`，与 threads/posts 软删同构（`ADR-038`）：公开列表 `ListNodes` 带 `WHERE status = 'active'`，归档节点从列表消失、数据不丢；**物理删一律禁止**，由 archtest 静态守卫扫 `DELETE FROM nodes`。`GetNodeBySlug` 刻意不过滤 archived——历史帖详情页仍需显示所属版块名；
  3. **seed 不可变**：`0002_seed_nodes.sql` 的 5 个 seed（`/dev/ai`、`/dev/rust`、`/dev/jobs`、`/etc/rant`、`/bin/help`）不可删、不可改名。`INSERT OR IGNORE` + slug UNIQUE 保证迁移重放或版本登记丢失时不会插入重复版块、也不会覆盖用户改名；M1 的 CRUD 命令需另加代码层守卫拦截对这 5 个 slug 的删/改名（M0 无 CRUD 命令面，故不在本轮实现），archtest 守卫 5 个 slug 原样存在。
- 实现边界：
  - M0 仅落地：本 ADR + `0008` + `ListNodes` 一行过滤 + archtest 守卫 + 隔离演练证据。**不做**节点 CRUD 命令、写端口、权限模型、`/node` 命令面（M1 范围）；
  - `0008` 不加 `merged_into_id`——节点移动/合并已推 P3（`docs/P2-PLAN.md §4.6`），archtest 同步禁止该列提前进场；
  - 不改 `selectNodes` 常量（与 `GetNodeBySlug` 共用），只在 `ListNodes` 的 SQL 追加过滤，避免影响按 slug 的历史读取路径。
- 代价 / 边界：多一列与一个 CHECK；5 个 seed 当前全 active，`ListNodes` 加过滤对现有行为**零影响**（返回值完全一致）；归档节点仍占 slug UNIQUE 名额，未来若要复用 slug 需显式清理，属 M1+ 课题。`GetNodeBySlug` 不过滤 archived 是有意选择，代价是归档 slug 仍可按名直取，不做"彻底隐身"。
- 验证：777 测试全绿（768 → +9：迁移回填/回滚 2、行为过滤 1、archtest 守卫与负向注入 6）；`go build ./... && go vet ./... && go test -count=1 ./...` 全绿；P0 工程门禁对 M0 改动文件零新增命中（与 HEAD 基线同为 54 处既有命中）；`v7→v8` 迁移、回滚精确回 v7、带数据 kill-9 + WAL 恢复三项隔离演练见 `docs/OPS.md §5` 2026-11-20 条。生产 `data/motd.db` 全程未触碰。
- 关联：`PHASE1-PLAN.md §2.10`、`docs/P2-PLAN.md §1`（M0 欠账）/ `§4.6`（节点合并推 P3）、`ADR-038`（软删口径）、`ADR-030`（编号澄清：实为认证限流，非节点）。

## ADR-049 管理入口与授权模型：指纹白名单

- 状态：已采纳（2026-11-20，P2-M1 已落地，P2-M1b 补写决策记录；`hide` 治理同样复用本模型）
- 背景：公开邮箱注册开放后，任何 active 账号都能发帖；但版块治理（create/rename/archive）与内容治理必须有管理入口。M1 实现时面临三种候选授权模型，需要把已落地的取舍显式固化，避免后续新增治理命令时另造第二套判定。
- 结论：
  1. **唯一凭证是服务端 SSH 会话公钥指纹白名单**（启动配置注入 `map[string]struct{}`，由 `forum.WithAdminWhitelist` 复制进 Service）。判定只有一个谓词：`admin.CanAdmin(fingerprint, whitelist)`；所有治理用例的唯一入口是 `Service.requireAdmin(actor)`，cmd 层只传 `Principal.KeyFingerprint`，不写任何权限表达式；
  2. **判定先于账号状态**：`requireAdmin` 不查 `users.status`、不调 `actor.checkWrite()`。白名单里的访客（`UserID=0`）就是 bootstrap 管理员，账号冻结与否不影响治理权——这是为了解决「没有管理员账号就无法建第一个版块」的鸡生蛋；
  3. **审计主体用指纹**：治理事件 `EventAdminAction` 的 `ActorID` 对访客管理员为 0，`AdminFingerprint` 承担「谁」的语义，`DomainEvent.Validate` 对此有专门分支，不因 `UserID=0` 丢事件；
  4. **统一失败口径**：空白名单、名单外指纹、空指纹、格式非法一律 `ErrAdminDenied`「仅管理员可执行」，不区分原因、不回显输入；
  5. `/node list|create|rename|archive` 与 P2-M1b 的 `/hide` 共用这条路径，没有第二套管理员判定，也没有「内容管理员」之类的并行角色。
- 理由（三方论证）：
  - **否决 `users.role` + RBAC**：需要新迁移、提权接口和「谁能授予角色」的再归零问题；账号冻结与治理权耦合后，冻结管理员会连带失去处理事故内容的能力，与 bootstrap 需求冲突。
  - **否决 Web 管理后台**：要新增 session/cookie/CSRF 与第二条传输面，与本项目 SSH-first 的信任边界（ADR-019/ADR-024）不一致，且不能复用 SSH 认证阶段算出的指纹。
  - **否决独立 CLI 在线入口**：需要另外分发凭据与通道，把攻击面从「已认证的 SSH 会话」扩大到「网络可达的管理端口」；收益（远程运维）当前没有需求。
  - **采用白名单指纹**：指纹由认证阶段服务端持有，永远不来自命令参数，用户无法通过提供文本自我提权；配置即授权，变更走部署流程，审计可直接落指纹，零新表、零新依赖。
- 代价 / 边界：新增或撤销管理员必须改配置并重启，无在线增删；换钥匙即失去治理权；无角色分级（全有或全无）；审计详情记指纹而非账号名，需要配置维护映射。`requireAdmin` 刻意不复用 `checkWrite`，意味着冻结账号仍可治理——这是有意的取舍，不是遗漏。
- 验证：M1 的节点治理用例与 cmd 测试覆盖越权固定文案、名单外拒绝、访客 bootstrap、冻结账号可治理；M1b 复用同一谓词并加 AST 守卫：用例入口必须调 `requireAdmin`，`requireAdmin` 必须调 `CanAdmin` 且不得调 `checkWrite`，cmd 层不得出现 `CanAdmin(`，全项目 `CanAdmin` 定义数必须为 1，每条守卫配负向注入。812 测试基线全绿。
- 关联：`ADR-019`（终端信任域）、`ADR-024`（命令单一事实来源）、`ADR-048`（节点不变量）、`ADR-050`（本模型在内容治理上的复用）。

## ADR-050 内容治理：管理员隐藏而非删除

- 状态：已采纳（2026-11-20，P2-M1b）
- 背景：公开注册后，垃圾主题 / 回复有真实治理需求，但 `Delete` 用例的 `canModify` 只让作者删自己的内容，白名单管理员对他人内容无能为力。数据库早已预留 `posts.status CHECK (... 'hidden')`（0001）与 `threads.status CHECK (... 'hidden')`（0004），但没有任何写入路径，也没有命令面。
- 结论：
  1. **命令口径选 (a)**：`/hide` 无参隐藏当前打开的主题，`/hide <id>` 隐藏该主题下 ID 为 `<id>` 的回复，与 `/delete` 的心智模型对称。管理员要能处理整条垃圾主题，而不只是单条回复；作者自删仍由 `/delete` 独占，不把治理语义塞进它；
  2. **hidden 与 deleted 是两种语义**：`deleted` 是作者自删（现有 `Delete` 用例不写治理审计），`hidden` 是管理员治理，固定命令名为 `post hide` / `thread hide`。两者都是软删（不物理删、不级联），审计因此能明确区分动作来源。公开可见性完全由存储层既有过滤保证：`ListPosts` 固定 `status='normal'`、`GetThread`/`ListThreads` 固定 `status='active'`、检索 JOIN 也带 `t.status='active'`，因此隐藏后自动从所有公开视图消失，无需新增过滤分支；
  3. **零迁移**：复用现成 CHECK，写端口新增 `HidePost` / `HideThread`，SQL 只 `UPDATE ... SET status='hidden'`，带 `status IN ('normal','hidden')` / `IN ('active','hidden')` 幂等条件：重复隐藏不报错，作者已删或不存在的目标收敛为固定文案「对象不存在」，不回显 ID、不区分状态差异；
  4. **权限与审计都复用现成单点**：用例入口只经 `requireAdmin`（ADR-049），管理员隐藏自己的内容也允许；审计事件复用 `adminActionEvent("post hide"|"thread hide", ...)` → outbox → `AuditBridge` → `auth_events`，目标以 `post:<id>` / `thread:<id>` 记在 `target` 字段，访客管理员的 `ActorID=0`、指纹进 `fingerprint`；
  5. **明确不做**：审核流、批量隐藏、Web 管理端、用户举报入口、恢复命令。首版只解决「管理员能立刻处理一条垃圾内容」。
- 理由：物理删会丢失证据与上下文，也无法与作者自删在审计上区分；新增 status 值或状态机需要迁移与回归面，而 hidden 已在 CHECK 里；把可见性交给既有查询过滤，比在用例层散落 `if hidden` 更不容易漏；权限若另写一套「内容管理员」判定，迟早与节点治理分叉，因此坚持单一谓词；审计链路已由 M1 打通，复用它零新依赖、零新表。
- 代价 / 边界：隐藏内容仍占数据库行（FTS 影子表由 0007 触发器在同一事务里摘除），无 `unhide` 命令，恢复需数据层操作；`/hide <id>` 与 `/delete <id>` 一样按 ID 定位，命令层只要求处于主题详情页以保证导航确定，不额外校验该回复是否属于当前主题（治理权本身就覆盖全站内容）；审计 detail 的 `node_id` 为 0，因为隐藏用例不从命令参数解析版块；事件 ID 含纳秒时间戳，固定时钟的单测需让时钟前进，否则 outbox 幂等键会合并两次调用。
- 验证：835 测试全绿（812 → +23：应用用例 8、真实 SQLite 集成 3、cmd 接线 6、archtest 守卫与负向注入 6），`go build ./... && go vet ./... && go test -count=1 ./...` 全绿。集成证据：隐藏后 DB `status='hidden'`（不是 deleted）、`ListPosts` 与检索均不再返回、回复行仍在且为 normal；`auth_events` 落一条 `command="post hide"` / `"thread hide"` 且 `target` 含目标 ID；白名单访客隐藏他人回复成功，`user_id` 为空而 `fingerprint` 落地。无新迁移、无新依赖、生产 `data/` 未触碰、未 commit。
- 关联：`ADR-049`（授权模型单点复用）、`ADR-038`（软删口径）、`ADR-015`（outbox 时序）、`ADR-048`（节点不变量）。

## ADR-051 编辑功能：已编辑标记而非版本历史

- 状态：已采纳（2026-11-20，P2-M2）
- 背景：`PHASE2-PLAN.md §3` 早已拍板编辑策略为「已编辑标记」，但此前只落地了删除（v0.2.1）与内容治理（P2-M1b），编辑一直欠账。编辑触及 ViewModel 显示契约（需按 ADR-014 三层守卫评估），且「谁能改」必须与治理权（ADR-049/050）分清。本 ADR 从当前最大编号 050 顺延，不复用旧号。
- 结论：
  1. **保留原投票与全部回复，只改正文并加「已编辑」标记**：接受"投票对象可能是改前内容"的语义代价；首版不做富文本、版本历史 / diff、审核流、就地行编辑。用户拍板 `/edit <id>` 重新走 composer 输入**完整新正文**提交覆盖，不做行内编辑。
  2. **权限是作者权，复用 `canModify`，不复用 `requireAdmin`**：入口 `canModify(actor.UserID, owner)`（`delete.go` 的唯一所有权判定）原样复用；管理员不能编辑他人内容——这与 M1b 的管理员隐藏是两条不同权限。非作者 / 访客 / 目标不存在统一 `ErrNotEditor`「仅作者可编辑」，不区分原因、不回显 ID。进入 composer 前 `CanEditPost`（只读端口 `PostAuthor`）做体验前置判权，提交时 `EditPost` 在写事务内用 `PostOwner` 再判一次（防 TOCTOU），两处都走同一个 `canModify`。
  3. **`edited_at` 落在 posts 表，不落 threads**：正文存在首帖（首帖也是 posts 一行），主题正文的标记读首帖，回复的标记读回复行，统一 `0009_edited_at.sql` 加列（`INTEGER`、`NULL` = 从未编辑、无 DEFAULT，历史行天然 NULL 无需回填）。领域 `post.Post.EditedAt`（Unix 秒，0=未编辑）只负责把列带出仓储，`selectPosts` 用 `COALESCE(edited_at,0)` 归一。
  4. **只 UPDATE `body_md, edited_at`，绝不碰 `status / created_at / last_reply_at`**：编辑不改变可见性、不伪造新帖时间、**不推进 `last_reply_at`**（否则 SortLatest 把旧主题顶到最新，同投票 `AdjustScore` 的教训 ADR-029 / ADR-032）。作者编辑自己的内容是普通写，**不写审计事件**（与 ADR-050 的管理员 hidden 治理动作区分）。0007 的 `posts_fts_update` 触发器随本次 UPDATE 重建索引，检索即时可见新正文。
  5. **展示走 RenderSystem，列表行不加**：`viewmodel.Detail.EditedAt`（Unix 秒）由 `ScreenThreadDetail` 从首帖填；`view.go` 在正文 `ext()` 之后、`── 回复 ──` 之前加固定行 `已编辑 <formatTime>`，`sys()`→`RenderSystem`、StyleDim，时间复用 `MOTD_DISPLAY_TIMEZONE` 的 `formatTime`（MM-DD HH:MM，与列表一致），不进用户内容净化链（ADR-014 三层守卫）。列表行不加任何标记（80 列已紧，用户拍板）。
  6. **命令口径与 `/delete` 对称**：`/edit` 无参编辑当前主题正文（首帖）、`/edit <id>` 编辑指定回复；都要求处于详情页。注册表 `MinArgs 0 MaxArgs 1`；id 只按十进制正整数解析，不回显原文。无参必须重新输入完整正文（空缓冲起），不预填原文。
- 理由：选「已编辑标记」而非版本历史，是因为保留投票 / 回复脉络的实现相对简单，且用户已接受语义代价；version diff / 审核流会引入表、迁移、状态机与回归面，首版收益不足。权限坚持单一 `canModify`，是因为作者编辑与管理员治理若各写一套判定，迟早分叉并出现越权。`edited_at` 放 posts 而非 threads，是因为正文本就存首帖，放 threads 会引入"两份真相"。不推进 `last_reply_at` 是为了不破坏 SortLatest 的语义。固定文案走 `RenderSystem` 而非 `RenderUser`，是因为时间由服务端本地数值格式化，属可信文本，进用户净化链既无谓又可能误伤。
- 代价 / 边界：`/edit <id>` 与 `/delete <id>` 一样按 ID 定位，命令层只要求详情页，不额外校验该 post 是否属于当前主题（作者权只认内容作者，跨主题同作者内容也可改）；编辑后的投票对象语义可能滞后（已接受的代价），不做回滚入口；`edited_at` 只记最近一次，无历史；`CanEditPost` 与 `EditPost` 之间存在 TOCTOU 窗口，前置判权仅为体验，信任决策以写事务内复核为准；冻结账号编辑与删除同口径——`canModify` 只看 `UserID`，不查账号状态（作者权不是发帖准入）。
- 验证：全量测试全绿（835 → 860，+25：应用用例 6、迁移 3、真实 SQLite 集成 2、view 3、cmd 接线 7、archtest 守卫与负向注入 4），`go build ./... && go vet ./... && go test -count=1 ./...` 通过；E 类工程门禁对新增 `posts` UPDATE 零命中。集成证据：真实 SQLite 编辑后 `body_md` 更新、`edited_at` 非 NULL、`GetThread` 与新 FTS 命中均见新正文、`status/created_at/last_reply_at` 三者不变、列表排序不变、`ScreenThreadDetail.Detail.EditedAt` 取首帖。无新依赖、生产 `data/` 未触碰、未 commit。
- 关联：`ADR-014`（ViewModel 三层守卫与信任域）、`ADR-019`（终端安全渲染两路径）、`ADR-029`/`ADR-032`（`AdjustScore` 不推进 `last_reply_at` 的教训）、`ADR-046`（`MOTD_DISPLAY_TIMEZONE`）、`ADR-049`/`ADR-050`（治理权单点，与作者权区分）、`PHASE2-PLAN.md §3`（编辑策略已拍板）。

## ADR-052 回复数计数投影：读时聚合而非写时维护

- 状态：已采纳（2026-11-20，P2-M3）
- 背景：v0.3.3 已把列表行的回复数格式从 ` N回复` 改成紧凑 ` rN`（`view.go` 的 `metaTail`，`if ScreenThreads && Meta.Replies > 0`），但核实发现 `RowMeta.Replies` 在生产路径**从未被赋值**：`threadRow` 只填作者 / 时间，测试里的 rN 全靠手工 set。用户在 ADR-051 拍板「暂搁」后，2026-11-20 明确把它加入 P2 收官（M3）。本 ADR 从当前最大 051 顺延，不复用 `docs/P2-PLAN.md §11` 预留的旧号。
- 结论：
  1. **走读时聚合（P2-PLAN §11 方案 B），零迁移、零写路径改动**：不新增 `threads.reply_count` 列。项目对写事务的态度是「稀缺且只在必要时」（ADR-010/032），为纯展示计数引入发帖 / 软删 / 治理多处维护点会放大一致性面；当前生产 posts=21 / threads=7，聚合代价可忽略。若将来规模增长到计划失效，另起 ADR 切写时维护，不在本版预埋半成品列；
  2. **选批量查询而非列表 SQL 内标量子查询（方案 A2）**：新增只读端口 `ReplyCountReader.ReplyCountsByThreadIDs(ctx, ids)`，`ScreenThreads` 组装完 rows 后收集 thread ID、一次查 `GROUP BY` 填 `RowMeta.Replies`。与 ADR-047 的 `UsernamesByIDs` 同构；不改 `selectThreads` 常量（与 `GetThread` 共用）、不改 `scanThread`、不污染 `thread.Thread` 领域对象。一屏一次查询，spy 断言调用次数；空 ids 直接返回空 map 不查库；查询失败上抛，绝不静默显示 r0；
  3. **口径 = `COUNT(status='normal') - 1`，下限 0（不含首帖）**：详情页把首帖当正文展示、Rows 只放 `Posts[1:]`（ADR-051 同款口径），用户看到的「回复列表」条数就是 `normal 数 - 1`。直接 COUNT 会让「只有正文」的主题显示 r1，与直觉冲突。`deleted` / `hidden` 回复不计；首帖若被软删 / 隐藏，可见列表的第一条仍被当正文，减 1 依旧与 Rows 对齐。扣减在仓储层完成，端口直接返回「回复数」语义值；
  4. **`rN` 格式与显示时机完全沿袭 v0.3.3**：`metaTail` 只在 `ScreenThreads` 且 `N>0` 时输出 ` r%d`，不碰其他符号。详情页 Rows 与搜索结果屏不投影回复数（前者是回复列表本身、后者用户未要求），由「只在 `ScreenThreads` 调用投影」结构性保证；
  5. **SQL 显式 `INDEXED BY idx_posts_thread`，收益是计划稳定性而非性能**：同一条 `WHERE thread_id IN (...) AND status='normal' GROUP BY thread_id`，不锁索引时 SQLite 在小表（含生产 21 行）选 `SCAN posts`、在 100k posts 大表选 `SEARCH`——**SCAN 是小表上的正确代价选择，不是缺陷，也没有 measurable 的提速空间**。锁定索引的真正理由是消除计划随数据规模、SQLite 版本、数据分布漂移的不确定性：读路径的执行计划在任何规模下都是同一条，EXPLAIN 断言才稳定、行为才可预测。索引本身确实覆盖该查询：`idx_posts_thread(thread_id, created_at)` 的领头列正是 `WHERE thread_id IN (...)` 的过滤列，不存在「强制走坏索引」。查询构造器 `replyCountsQuery` 由仓储与 EXPLAIN 测试共用，去掉 `INDEXED BY` 会让测试变红。
- 理由：读时 vs 写时——展示字段若为正确性依赖写路径多处维护，任何新增写路径（编辑、治理、将来的节点合并）都可能漏改，而读时聚合的真相只有 posts 表一处。A2 vs A1——标量子查询虽天然走索引，但要复制 `selectThreads` 或引入 projection map，改动共用的扫描契约；批量查询隔离在独立端口，口径集中、可单独测、与既有作者名投影同构。减首帖——与「用户看到的回复行数」一致，避免 r1 表示「零回复」；详情页总把可见 Posts 的第一条当 `detail.Body`，因此首帖被隐藏后 `Rows` 少一条，减 1 仍与真实回复行数对齐。显式索引提示——三条独立理由：① 计划可预测性（SCAN 在小表上是优化器的正确选择，锁索引是为了不让它随规模 / 版本 / 分布漂移，属稳定性而非性能收益）；② 索引领头列就是过滤列，覆盖性无疑问，不是「强制走坏索引」；③ 小表上 SEARCH 与 SCAN 性能接近，实测无提速，代价如实记在下面。
- 代价 / 边界：每次主题列表多一次 DB 往返（1 次，非 N+1），分页 20 行时聚合 20 个 thread_id；索引名硬绑定，若将来改名 / 删除该索引，查询会直接报错而非静默退化，需同步改 `replyCountsQuery`。**技术债**：若将来 posts 规模显著增长（远超当前 21 行），`INDEXED BY` 会阻止优化器按新数据分布选择更优计划，届时须重新评估是否移除该提示，不要把它当作永久正确。小表上 SEARCH 与 SCAN 性能接近，本版未取得可测提速。计数是读时快照，与 posts 写入之间无事务一致性要求（展示值，非业务判定）；不进领域模型、不进写事务（archtest AST 守卫负向注入验证）。不显示 0 回复的 r（沿用 v0.3.3 的 N>0 条件），搜索屏与详情页不显示。
- 验证：全量测试全绿（860 → 871，+11：application 4、sqlite 仓储 / 首帖隐藏边界 / EXPLAIN / 真实 SQLite 端到端 5、archtest 守卫与负向注入 2），`go build ./... && go vet ./... && go test -count=1 ./...` 通过。EXPLAIN 证据（`TestReplyCountsQueryUsesThreadIndex -v` 可复现：灌 15 行 + ANALYZE 生成 sqlite_stat1）——对照·不锁索引：`SCAN posts` + `USE TEMP B-TREE FOR GROUP BY`；生产·`INDEXED BY idx_posts_thread`：`SEARCH posts USING INDEX idx_posts_thread (thread_id=?)`。另有独立探针实测：100k posts 大表上不锁索引也会自动转 `SEARCH`，印证 SCAN 是优化器按数据规模的正常选择。端到端证据：真实 SQLite 经 `Service.ScreenThreads`——建主题后 Replies=0（不显示 r）→ 回复一条后 Replies=1（r1）→ 作者软删该回复后回到 0（r 消失）；三条主题计数互不串，B 加两条回复只影响 B。首帖隐藏边界：首帖置 hidden 后投影回复数 = 1 = 详情页可见 Rows 数，且正文取可见第一条。P0 工程门禁对本次改动文件零新增命中（全库 270 处为既有命中，含 `data/` 演练副本）；无迁移、无新依赖、生产 `data/` 未触碰、未 commit。
- 关联：`ADR-047`（作者名批量投影同构）、`ADR-014`（展示投影不进领域模型）、`ADR-010`/`ADR-032`（写事务稀缺）、`ADR-051`（详情页首帖口径）、`docs/P2-PLAN.md §11`（方案 B 与 DoD）。
- **patch（v0.7.3，用户 2026-11-23 反馈）**：主题列表行元信息顺序调整——回复数 `rN` 由时间之后提到时间之前，行尾形态由 `id=<id> @<username> MM-DD HH:MM r<N>` 变为 `id=<id> @<username> r<N> MM-DD HH:MM`。改动只在 `metaTail` 的 `parts` 拼接顺序：`ScreenThreads && Replies > 0` 的 ` r%d` 段先于 `formatTime` 段 append；`rN` 格式、显示时机（仅 `ScreenThreads`、`N>0`）、分隔符与其余符号一律不动，详情页回复行与搜索结果屏本就不投影回复数，零变化。信任域边界不变：两段固定文本（id= / 回复数 / 时间）仍走 `sys()`→`RenderSystem`，作者段仍走 `ext()`→`RenderUser`（ADR-014/019）。新增 `TestMetaTailRepliesBeforeTime` 锁定「r3 在 11-14 22:13 之前」的组合顺序；全量四门禁绿，测试 990 → 991，无迁移、无新依赖。

## ADR-053 隐藏可逆：unhide 而非隐藏内容列表

- 状态：已采纳（2026-11-20，用户实测反馈后的 P1 修复；同日追加 `/unthread` 补丁，见结论 6）
- 背景：ADR-050 落地 `/hide` 后，隐藏是终态——误隐藏一条正常帖无法在命令面恢复，只能数据层操作。用户明确表达「担心误隐藏无法恢复」，并拍板「全做 + B」：三项修复一并做，其中隐藏可逆选最小口径的 `/unhide`，**不做隐藏内容列表页**。本 ADR 从当前最大 052 顺延。
- 结论：
  1. **提供与 `/hide` 严格对称的 `/unhide [id]`**：无参恢复当前主题（`threads.status` hidden→active），带 id 恢复当前主题下的回复（`posts.status` hidden→normal）。注册表 `MinArgs 0 MaxArgs 1`，只在主题详情页可用；id 只按十进制正整数解析，不回显原文。命名空间刻意不复用 `/delete`——后者是作者自删，审计命令名也不同；
  2. **权限不扩散：复用 `requireAdmin` 单点**（ADR-049/050），不新增判定、不新增 `Service.IsAdmin` 查询端口、不把白名单泄漏到 cmd 层。`Help()` 仍无用户上下文；帮助里的管理员分组标记是纯展示元数据（见下文 B 方案），不参与任何鉴权。白名单访客（UserID=0）合法，与节点治理 / hide 同口径；
  3. **审计复用同一条链路**：`adminActionEvent("post unhide" | "thread unhide", ...)` → outbox → AuditBridge → `auth_events`，固定命令名、目标记 `post:<id>` / `thread:<id>`，不回显用户正文。业务写与 `AppendOutbox` 在同一 `WriteTx` 闭包内，提交前失败两者一起回滚；
  4. **写端口只加状态迁移**：`WriteTx` 增 `UnhidePost` / `UnhideThread`，SQL 仅 `UPDATE ... SET status = 'normal'|'active'`，带幂等条件 `status IN ('hidden','normal')` / `IN ('hidden','active')`：未隐藏的目标再恢复是合法 no-op，作者已删除（deleted）或不存在收敛为固定文案「对象不存在」。不物理删、不级联、不改任何其它列。0001/0004 现成 CHECK 与 0007 触发器已覆盖，`ALTER` 与迁移一律不需要——恢复后 FTS 索引由触发器在同一事务里重建，检索立即可见；
  5. **明确不做隐藏内容列表页**：列表页会引出筛选、批量操作、跨主题导航与「谁在看隐藏内容」的审计面，本质上是审核流的第一步；本版只解决「误隐藏能立刻撤销」这一件事。管理员若需处理多条，仍逐条 `/unhide`；不做批量、不做 Web 管理端、不做举报入口；
  6. **补丁：无参 `/unhide` 对已隐藏主题不可达，补独立命令 `/unthread <thread-id>`**（用户实测后由主理人裁决）：`GetThread` / `ListThreads` 固定只返回 `status='active'`，主题一旦隐藏就从列表消失、打不开详情，而 `/unhide` 无参要求详情页，等于主题救不回来——这是 unhide 的核心价值缺口。新增独立命令从**任意屏幕**按 ID 恢复主题，`MinArgs 1 MaxArgs 1`，落 `GroupAdmin` 分组。**不重载 `/unhide <id>`**：主题 ID 与回复 ID 都是全局自增、可能撞号，重载无法可靠判别类型；独立命令名无歧义，也不改现有 `/unhide` 的参数语义（零回归）。审计命令名用 `"thread unthread"`，与无参 `/unhide` 的 `"thread unhide"` 分开——`adminActionEvent` 的 event ID 含命令名，两个入口互不撞幂等键，审计也能区分来源；用例层复用 `UnhideThread` 的写端口（幂等、只 UPDATE status），不新增迁移。
- 理由：对称性优先——`/hide` 的操作者已经在详情页，恢复走同一入口、同一权限、同一审计链路，认知负担和回归面都最小。拒绝新增权限判定是因为任何第二套「谁能恢复」的谓词迟早与 `requireAdmin` 分叉（ADR-049 的教训）；拒绝列表页是因为它把「可逆」问题扩大成「治理工作台」问题，而用户本次只要求恢复单条误操作。零迁移则是因为 hidden/normal/active 本来就在 CHECK 里，为状态回滚加列或加表只会制造新的真相来源。
- 代价 / 边界：恢复不区分「谁隐藏的」，白名单管理员可恢复任何隐藏内容（与隐藏权同宽）；`deleted` 是作者自删，刻意不可通过 unhide 复活，避免把作者权与治理权混为一谈。`/unhide` 与 `/hide` 一样只在主题详情页可用：回复按 id 定位不校验是否属于当前主题（治理权覆盖全站内容），主题无参恢复依赖当前详情页——已隐藏的主题不在公开视图里，因此从列表无法再打开它；**这个缺口由 `/unthread <thread-id>` 从任意屏恢复主题来兜住**，它没有把隐藏主题变成可浏览对象，不触碰公开可见性边界。`/unthread` 成功后直接落到该主题详情（此时已可打开），不让用户停在旧屏幕。事件 ID 含纳秒时间戳与命令名，固定时钟单测需让时钟前进，否则 outbox 幂等键会合并两次调用。
- 验证：全量测试全绿（871 → 903，+32：Esc 静默 3、帮助归组 2、应用用例 9、真实 SQLite 集成 5、cmd 接线 10、archtest 守卫与负向注入 3），`go build ./... && go vet ./... && go test -count=1 ./...` 通过。端到端证据：真实 SQLite 中隐藏回复 → `status='hidden'` 且检索 0 命中 → 恢复 → `status='normal'`、`ListPosts` 两条可见、检索重新命中该条；主题隐藏 → 恢复后 `GetThread` / `ListThreads` 重回可见、回复行未变；`auth_events` 落 `command="post unhide"` / `"thread unhide"` / `"thread unthread"`，访客恢复 `user_id` 为空而 `fingerprint` 落地。**`/unthread` 往返证据**：隐藏主题后回到 `/top` 版块列表屏执行 `/unthread <id>` → 主题 `status='active'`、屏幕直接落到该主题详情（可打开）、固定文案「已恢复主题」；重复执行幂等成功；非管理员登录 / 访客均得「仅管理员可执行」且数据零变更；审计落 `command="thread unthread"`、`target="thread:<id>"`。越权零变更、幂等不报错、已删目标 `ErrNotFound` 均有断言。P0 工程门禁对新增改动零新增命中。无新依赖、无新迁移、生产 `data/` 未触碰、未 commit。
- 关联：`ADR-050`（隐藏语义与固定文案）、`ADR-049`（授权模型单点复用）、`ADR-015`（outbox 时序）、`ADR-010`/`ADR-032`（写事务稀缺）、`ADR-014`/`ADR-019`（帮助文本固定文案走可信渲染路径）。

## ADR-054 会话保活：空闲语义改为「连接不存活」+ 绝对上限兜底

- 状态：已采纳，v0.4.1 落地。
- 背景：用户实测反馈「停止浏览论坛一会儿就显示 SSH connect closed」。核实 `Session` 的空闲计时器由 `time.AfterFunc(idle, Close)` 实现（默认 10m），但只在 `Session.Read` 读到**实际按键**时 `resetIdle()`——用户不操作就被服务端主动断开。连接级心跳（10s）虽在跑，却只刷新 presence 注册表、不重置空闲计时器，因此救不了这条连接。客户端看到裸 `Connection closed`、退出码 255，无从判断原因。

- 决策：
  1. **空闲语义从「无按键」改为「连接不存活」**：`heartbeat()` 同时调 `resetIdle()`。连接级心跳持续运行即证明 TCP/SSH 连接仍活，此时用户长时间不操作也不断开。这是对的说法——空闲的本意是「连接已死」，不是「用户没打字」。
  2. **新增绝对存活上限 6 小时**：心跳是进程内 ticker，无法感知「客户端静默消失」（断电 / 网络中断 / SSH 层未感知断开）；这类连接心跳照跑、idle 被持续重置，会永久占用 `MaxConns` 槽位。增加一个**不被心跳重置**的 `sessionMaxLifetime`（6h，硬编码不开放配置）兜底收口。两个计时器职责正交：idle 管「连接无心跳」，lifetime 管「最长在线时长」。
  3. **收口统一为单一幂等序列**：`closeWithReason(notice)` 用 `sync.Once` 包裹「固定提示文案 → `sendExitStatus(0)` → `ch.Close()` → `close(done)`」，三条触发路径（handler 结束 / 空闲超时 / 硬上限）都走它。停计时器与 `close(done)` 在同一临界区原子完成，`resetIdle()` 在临界区内检测 `done` 已关则不再 `Reset`，杜绝已停 timer 被心跳复活。
  4. **超时下发固定文案**：空闲超时与硬上限分别下发两行服务端固定文本（ADR-019 可信路径，无用户数据 / 无 ANSI），退出码 0；正常退出传空文案、不下发提示。原实现超时直接 `Close()` 且与 `runHandler` 的 exit-status 竞争，后到的写因 channel 已关而失败，用户才只见裸断连。

- 理由：不把 `MOTD_SSH_IDLE_TIMEOUT` 简单调大（如 1h）——那只是推迟问题，且用户仍在被动等待断连；也不引入客户端 keepalive 要求（额外配置负担、且 SSH 子通道 keepalive 不重置服务端的 `Session.Read` 计时器，治不了本）。「连接存活即保活」符合用户对「空闲」的直觉，硬上限单独兜住真正的资源泄漏面。

- 代价 / 边界：`sessionMaxLifetime` 固定 6 小时不可配置，超长会话需求需后续版本评估；提示文案固定英文前缀 `motd:`，与登录 banner 同风格。`var sessionMaxLifetime` 用变量而非常量仅为测试注入短时长，生产代码不改。测试中的 `recordingChannel` 模拟「不按键但不 EOF」的连接，验证心跳保活与两条超时路径。

- 验证：`go build ./... && go vet ./... && go test -count=1 ./...` 全绿，测试 903 → **907**（新增 4 个：心跳保活不被 idle 断、空闲超时提示与 exit-status、硬上限在心跳持续时仍收口、正常退出无文案且 exit-status 幂等）。archtest 全绿，transport 层未因固定文案引入 presentation 依赖。无新迁移、无新依赖、无新配置项。

- 关联：`ADR-016`（presence 内存心跳与 Reap）、`ADR-019`（服务端固定文本可信路径）、`ADR-014`（展示与领域分层，本次未触碰 ViewModel）。

## ADR-055 移除 idle timer 与 MOTD_SSH_IDLE_TIMEOUT（ADR-054 的后续收敛）

- 状态：已采纳，待发版（目标 v0.4.2）。
- 背景：ADR-054（v0.4.1）把空闲计时器改为由连接级心跳每 10s 重置，语义从「无按键」变为「连接不存活」。落地后复查发现：心跳是进程内 ticker，只要会话未收口就一直运行，idle timer（默认 10m）因此**被持续重置、实际永不触发**——无论用户停多久都不会因它断开。真正兜住「客户端静默消失」的是不被心跳重置的 `sessionMaxLifetime`（6h）。于是 `MOTD_SSH_IDLE_TIMEOUT` 成了一个"看起来能配、改了没效果"的死配置：只有在被误设成小于心跳周期（10s）时才会偶尔触发，即「配小了才产生 bug」。

- 决策：
  1. **彻底移除 idle timer 机制**：删 `Session.idle`、`Session.timer`、`resetIdle()`、`heartbeat()` 与 `Read()` 里的 `resetIdle()` 调用、`idleCloseNotice` 常量；`newSession` 去掉 idle 参数。会话生存期只剩 `sessionMaxLifetime` 一条约束。
  2. **从对外契约移除 `MOTD_SSH_IDLE_TIMEOUT`**：删 `config.Config.SSHIdleTimeout` 与 `DurationEnv` 读取、`sshd.Config.IdleTimeout` 与 `DefaultIdleTimeout`、`main.go` 传参、`.env.sample` 与 README 配置表条目。`DurationEnv` 函数保留（认证限流/审计仍用）。
  3. **收口路径从三条减为两条**：handler 结束 / 硬上限，仍统一走 ADR-054 的幂等 `closeWithReason`（固定文案 → exit-status → close channel），该机制不变。
  4. **测试跟进**：删 `TestIdleTimeoutClosesSession` / `TestIdleTimeoutSendsNoticeAndExitStatus` / `TestHeartbeatKeepsIdleSessionAlive`；新增 `TestNoIdlePathWithoutInputOrHeartbeat`（无输入且无心跳也不关闭，证明 idle 路径确实消失）；保留并适配硬上限与正常退出测试。测试 907 → 905。
  5. **升级兼容**：`.env` 里残留的 `MOTD_SSH_IDLE_TIMEOUT` 被静默忽略（不报错、不影响启动），OPS 文档提示手动删除；视为配置项移除而非行为破坏。

- 理由：死配置比没有配置更有害——它让用户以为能控制断连时机，实际不能，还埋着「误设小值导致意外断连」的坑。保留一个硬编码 idle 常量同样无意义（同样被心跳架空）。移除后语义单一清晰：会话只看 6 小时绝对上限，之外的"保活"由连接级心跳负责 presence 注册表的准确性（ADR-016），两者正交。

- 代价 / 边界：这是对外配置契约的移除（0.x 阶段允许），已在 CHANGELOG 标注；不提供"自定义最长在线时长"配置——6h 固定，若未来有长会话需求再单独评估开放配置项，不与本次移除混。ADR-054 原文保留不回改，本条作为后续收敛记录。

- 验证：`go build ./... && go vet ./... && go test -count=1 ./...` 全绿，测试 903 → 905（相对 v0.4.0），archtest 全绿。无新迁移、无新依赖、生产 `data/` 未触碰。CI 门禁待发版前确认。

- 关联：`ADR-054`（会话保活——本条收敛其 idle 部分，保留硬上限部分）、`ADR-016`（presence 内存心跳与 Reap）、`ADR-019`（服务端固定文本可信路径）。

## ADR-056 成就落 SQLite：替换内存占位 Store

- 状态：已采纳，v0.5.0 / P3-M1 落地。
- 背景：P0 为让插件链路端到端可验证，注入 `MemoryAchievementStore`（`memory_store.go:24`，`wiring.go:41` 注释"P0 占位：不写 SQLite"）。结果是插件经事件总线申请成就成功、进程重启后全部丢失——用户永远看不到持久化的解锁记录。表 `achievements` 自 0001 就存在（`id/user_id/code/unlocked_at`，`UNIQUE(user_id, code)`），但无写入实现。
- 决策：
  1. 新增 `internal/infrastructure/sqlite/repo_achievement.go` 实现 `appuser.AchievementStore`（端口仍定义在 application，ADR-015）：`Has` 走读池；`Grant` 经 `Writer.Submit` 走唯一写连接，`INSERT ... ON CONFLICT(user_id, code) DO NOTHING` 保证幂等（与内存实现同口径）；`GrantedCount` 读池计数。
  2. `wiring.go` 换 SQLite 实现；`forumRuntime.Store` 从具体类型 `*appuser.MemoryAchievementStore` 改为 `*sqlite.AchievementStore`，并保留 `Entries()` 观测方法使现有端到端测试（`wiring_test.go:85`）零改动通过。
  3. `Grant` 在插件事件回调中调用，是独立事务——不与发帖/投票的写事务嵌套，无需处理重入。
  4. **无新迁移**：0001 的表结构、外键与 UNIQUE 约束已经够用。
- 理由：成就"解锁过"是用户可见的长期事实，属于持久化数据（设计稿 §1.3 原则二），内存 Store 只是 P0 为验证链路的临时妥协。落库不改变 `AchievementService.Apply` 的四道校验（`achievement.go:69-94`），风险面仅限实现替换。
- 代价 / 边界：`Entries()` 为测试观测保留，不是端口方法；`GrantedCount` 用 `context.Background()` 且错误返 0（纯观测，不参与业务判定）。成就落库失败不影响发帖结果——事件投递本就在 outbox 提交之后。内存实现文件保留但不再装配（避免删除带来的测试连带改动）。
- 验证：`repo_achievement_test.go` 两个专项测试——幂等/计数/Entries 快照、**关闭重开同一库路径后成就仍在**（直击内存实现做不到的差异）；全量 `go build ./... && go vet ./... && go test -count=1 ./...` 全绿，0 FAIL；格式门禁零输出。测试 905 → 907。
- 关联：`ADR-015`（outbox 与插件边界）、`ADR-013`（不臆造计分语义——本条只做持久化，不涉及规则）、`P3-PLAN.md §2`。

## ADR-057 karma 计分接线：规则与事务落点

- 状态：已采纳，v0.5.0 / P3-M2 落地。规则由用户 2026-11-20 确认（P3-PLAN §3.2）。
- 背景：`users.karma` 自 0001 存在、`Level()`（`domain/user/level.go`）阈值齐备、状态栏渲染（`terminal/view.go:226`）也通，但**全代码无计分写入点**，`page_controller.viewContext()` 也不读该列——真实用户恒 0 / L1，是一个"看得到但永远没值"的占位缺口。ADR-013 明确"不臆造 P0 计分语义"，因此先由用户拍板规则再实现。
- 计分规则（用户确认）：
  | 动作 | karma |
  | --- | --- |
  | 发主题 | +2 |
  | 发回复 | +1 |
  | 被 upvote | +1 / 票 |
  | 被 downvote | 不扣作者 |
  | 改票 / 撤票 | 按 upvote 状态差精确同步 |
  | 删除 / 隐藏内容 | 不回退 |
  | 给自己投票 | 不给 karma（防自刷声望） |
  | 等级 | 仅显示（L2=10 / L3=50 / L4=200），**绝不做权限谓词**（ADR-014） |
- 决策：
  1. `WriteTx` 增 `AdjustKarma(ctx, userID, delta)`；SQLite 实现用原子 `UPDATE users SET karma=karma+?`（`repo_karma.go`），不读后写，避免并发丢更新。
  2. 写用例挂接：`CreateThread` 插入后同事务 +2；`CreatePost` 同事务 +1；`applyVote` 在 `delta≠0` 分支按 `voteKarma(to)-voteKarma(current)` 给**目标作者**加减（只认 Up，被踩不扣；用 `ThreadOwner/PostOwner` 在事务快照内取作者）。
  3. 显示侧新增独立端口 `appuser.KarmaReader`，`sqlite.UserStatusReader` 顺带实现；`pageController.viewContext()` 用**可选接口断言**读取，未实现的 fake 自动回落 karma=0/L1——不扩 `StatusReader`，避免 6 处调用方与所有测试 fake 被迫实现无关方法。
  4. 无新迁移（列已存在，DEFAULT 0，存量行无需回填）。
- 理由：karma 是"用户声望"这一长期事实（设计稿 §1.3 原则二），必须落 SQLite 且与业务写同事务提交/回滚；与 `threads.score`（主题分数）正交，分别由 `AdjustKarma` / `AdjustScore` 维护。被踩不扣作者是防恶意刷踩的刻意选择；自投票不给分是防自刷声望。
- 代价 / 边界：karma 只驱动显示，任何权限判断不得读它（重申 ADR-014）；`/edit` 不额外加分；删/隐藏内容不回退 karma（简单、不制造负状态）；改票按 upvote 状态差而非 `threads.score` 的 delta——两者语义不同，不可复用。
- 验证：`karma_test.go` 5 个专项（发帖 +2/+1、up/down/改票/撤票精确同步、被踩不扣、自投票不给、规则常量锁定）+ `karma_display_test.go` 2 个（DB→viewContext→L2 派生、访客恒 0）；全量 `go build/vet/test` 全绿，0 FAIL；格式门禁零输出。测试 907 → 914。
- 关联：`ADR-013`（不臆造计分语义——本条由用户拍板）、`ADR-014`（等级仅显示）、`ADR-031`（投票幂等/插件只感知首次）、`P3-PLAN.md §3`。

## ADR-058 CI 加竞态检测 job 与 -race 因果澄清

- 状态：已采纳，v0.5.0 / P3-M3 落地。
- 背景：项目是多连接 SSH 服务，presence 注册表、写队列、outbox、插件事件、`atomic.Pointer` 时区均涉及并发。现有 19 个并发测试只验证功能正确性，不检测数据竞争。2026-11-20 前一轮口头说"本机无 gcc，-race 不可用"，并引用 `PHASE1-PLAN.md:75`"纯 Go（无 CGO，故 -race 不可用）"——**该因果不成立**，需澄清。
- 事实澄清：`go.mod` 仅 `modernc.org/sqlite`（纯 Go）与 `golang.org/x/crypto`，无 CGO 依赖。Go 官方要求 `-race` 需 `CGO_ENABLED=1` + C 编译器（Windows 还需 mingw-w64）。纯 Go 依赖**恰恰意味着** Linux 上开 CGO 无额外安装成本；本机跑不了的真因是 Windows + 无 gcc（`DECISIONS.md:99` 的 `cgo: C compiler "gcc" not found`），不是"纯 Go 所以不行"。`PHASE1-PLAN.md` 原文不回改（历史快照），本条为更正。
- 决策：
  1. `.github/workflows/ci.yml` **追加独立 `race` job**（不改现有 `check`）：`CGO_ENABLED=1`、`GORACE=halt_on_error=1`、`go test -race -count=1 -timeout 15m ./...`。
  2. 初期**不设为 required check**，观察 2–3 轮稳定后再提为门禁。
  3. 预计首次编译 4–8 分钟、缓存命中 2–5 分钟；内存开销 5–10×，4C/16GB runner 足够。
  4. 若暴露 data race，优先排查 19 个并发测试覆盖路径；若时间敏感断言在 race 下超时（`write_test.go:136` 的 60ms、`server_test.go:151` 的 3s、healthz 2s Ping），**调测试容差而非跳过 race**。
- 理由：竞态 bug 平时不报错、偶发崩溃或数据错乱，常规测试抓不到；CI 是唯一具备 C 编译器的可用环境。独立 job 让常规门禁时长不受影响，渐进式提为 required 避免一次性引入不稳定。
- 代价 / 边界：CI 多一个 job 的 runner 分钟消耗；本机（Windows）仍无法本地复现 race 结果，只能靠 CI。race 不是压测——它查并发正确性，不衡量容量（见 ADR-059 与 P3-PLAN §5 的诚实口径）。
- 验证：YAML 结构与现有 `check` job 同构、人工核对缩进；本机实测 `go test -race` 报 `requires cgo`（符合预期，非缺陷）；全量 `go build/vet/test` 仍全绿、0 FAIL。race job 本身待 push 后在 CI 验证。
- 关联：`ADR-016`（presence 内存状态与并发）、`ADR-010`（单写队列）、`PHASE1-PLAN.md:75`（被本条更正的因果）、`P3-PLAN.md §4`。
- **实施后续（2026-11-20~21，实况修正原决策）**：
  1. **抓到 4 处 data race**（首次跑即失败）：3 处测试假对象（`loginClient` 计数器无锁、`loginStore.inWriteTxn()` 无锁读、`sessionMaxLifetime` 裸全局），1 处**生产代码真 bug**（`session.go` 的 `s.lifetime`/`s.hb` 赋值无锁，与 timer 回调的锁内读竞争）。全部修复后 CI 全绿；`sessionMaxLifetime` 改 `atomic.Int64`，赋值纳入 `s.mu`。
  2. **`halt_on_error=1` 已移除**：它让 detector 在每个包第一个 race 处停止，导致"修一个冒一个"（经历两轮）。去掉后一轮报全，这是灰度期经验修正。
  3. **R3 真实发生**：race 下 `TestMVPLoginCommandHintOnRealSSH` 首屏等待偶发超过 8s（CI 实测 8.18s），非 data race、非挂起，是渲染变慢。解法用构建标签分离超时：正常构建 `e2e_wait_default.go` 仍 8s 保持挂起检测灵敏度，race 构建 `e2e_wait_race.go` 放宽到 30s；**没有跳过 race、没有全局放水**。修复后 CI 连续两轮全绿。
  4. required 状态：仍非 required，观察项见 `P3-PLAN.md §11`。

## ADR-059 最小压测集合：核心场景基线而非容量验收

- 状态：已采纳，v0.5.0 / P3-M4 落地。
- 背景：设计稿 §6 要求逐步压测 100/300/500/1000 连接并验证 1 Mbps 约束，但用户 2026-11-20 明确"压测用最小集合、对核心场景稳定测试即可"。仓库此前 benchmark 为 0；`/healthz` 与 `Writer.Depth/Cap/Commits`、`presence.Registry` 已暴露观测钩子（`write.go:160-165`），但无可执行压测。
- 决策：
  1. 新增 4 个同包 `*_bench_test.go`（复用私有 test helper，零新增导出 API）：场景1 SSH 连接建立/拆除（8/50 并发）、场景2 浏览列表读多（16/64）、场景3 写队列背压（`WithQueueSize(8)`，8/32 并发，`ErrQueueFull` 视为预期背压）、场景4 并发读+渲染+presence（100 会话）。
  2. 用 Go `testing.B`，`-benchtime=3s -count=3` 取中位数；`scripts/bench.sh` 封装参数。**不进 CI 默认 job**（共享 runner 延迟噪声大、避免时长失控），用 `workflow_dispatch` 或发版前本地跑。
  3. 判定从绝对阈值过渡到"相对基线劣化 ≤20%"，避免机器噪声误判；首次实测数据转为回归基线。
  4. **诚实口径**：本集合给的是核心闭环在回环环境下的功能正确性、并发安全性与相对延迟基线，**不构成** 1 Mbps / 1000 在线验收证据。1000 在线与 1 Mbps 仍是设计目标（设计稿 §1.2/§6、MVP-DELIVERY A3），需后续在目标机器、真实网络、提升后的 `MaxConns` 配置下分档实测。未验证项：1 Mbps 真实上下行、扇出 6/60 MB/s、公网 RTT/丢包、小时级长稳、压测下 kill-9/WAL 恢复。
- 理由：用户已真实部署并开放 SSH 体验，最缺的是"核心闭环没有性能回归"的保证，不是 1000 在线的容量证书。最小集合能用现成钩子快速落地（总时长 1–2 分钟），且把不确定的容量口径诚实留在目标侧，不制造虚假验收。
- 代价 / 边界：内存 fake 下的场景2/4 不覆盖真实 SQLite 读延迟与网络传输；场景3 的背压率接近 100% 是小队列刻意设计（验证快速失败而非吞吐）；SSH 场景上限 50 < 默认 `MaxConns=100`，不触及满员拒绝路径。
- 验证：`go test -run '^$' -bench ...` 四场景实跑通过（本地 200ms benchtime 冒烟：SSH ~3ms/握手、ListThreads ~220ns/op、写队列背压 ~99.8% 符合预期、渲染 ~1.1µs/op）；全量 `go build/vet/test` 全绿、0 FAIL；格式门禁零输出。
- 关联：`ADR-058`（-race 与压测是不同维度）、设计稿 §1.2/§6、`P3-PLAN.md §5`。

## ADR-060 自动备份调度模板与恢复流程补齐 FTS 步骤

- 状态：已采纳，v0.5.0 / P3-M5 落地。
- 背景：`sqlite.Backup/VerifyBackup/Restore`（`backup.go:62/99/157`）与 `scripts/backup.sh`/`restore.sh` 早已可用，演练记录完整（OPS §5），但仓库**无任何 cron/systemd/timer 文件**——OPS §9.5 明确"首次部署默认不建立每日 cron/timer，由用户拍板"。另发现 OPS §4 恢复流程六步**漏了 `fts-rebuild`**，而 §3 与 ADR-033 要求恢复后必须重建 FTS 索引，否则 `/search` 返回空。
- 决策：
  1. 新增 `deploy/motd-backup.service`（oneshot，`EnvironmentFile=` 注入 `MOTD_DB_PATH/MOTD_BACKUP_DIR/MOTDCTL_BIN`）与 `deploy/motd-backup.timer`（`OnCalendar=*-*-* 03:00:00`、`Persistent=true`）。仅为**模板**，需部署方复制到 `/etc/systemd/system/` 并按实际路径替换 `<ENV_FILE>` 后启用——不擅自替用户开启调度。
  2. 增强 `scripts/backup.sh`：解析 `motdctl backup` 输出的 `backup=<path>`，紧接着执行 `motdctl verify <backup-file>`；任一失败非零退出，便于 cron/timer 的 `systemctl --failed` 与告警发现。备份的成功标准是"能恢复"（OPS §2），至少要能 verify。
  3. OPS §4 恢复流程在"恢复"与"启动"之间插入**必须**的 `motdctl fts-rebuild --db data/motd.db`（输出 `indexed=N`），并在抽查步补做一次 `/search` 确认索引恢复；演练记录引用从 §4 改为 §5（原写作 §4 自身，是笔误）。
  4. 保留策略**不自动删除**（OPS §6），需人工清理；RPO≤24h / RTO≤15min 口径不变，是否收紧到小时级仍由用户拍板。
- 理由：备份工具链完整但缺调度与验收闭环——"生成了文件"不等于"备份成功"。timer 模板让部署方有据可依，脚本内 verify 让失败可见；补 fts-rebuild 消除"恢复了但搜索空"的隐性故障。
- 代价 / 边界：`deploy/` 下是模板不是生效配置，`<ENV_FILE>` 为占位符需替换；VACUUM INTO 整库重写的 IO 峰值与 `ErrQueueFull` 风险仍在（OPS §7，切换 Online Backup API 条件未满足）；异地备份未纳入本版本。
- 验证：`bash -n scripts/backup.sh` 语法通过；用隔离数据源实跑 `backup.sh`，先输出 `backup=`/`sha256=`/`size=`，随后 verify 输出 `integrity=ok` 与五张表计数，退出码 0，证明验收闭环成立。全量 `go build/vet/test` 全绿、0 FAIL；格式门禁零输出。
- 关联：`ADR-033`（fts-rebuild 必要性）、`ADR-010`（写队列边界）、OPS §2/§4/§6/§9.5、`P3-PLAN.md §6`。

## ADR-061 v0.5.1 彩蛋系统接入与命令合并

- 状态：已采纳，v0.5.1 / P3-M6 落地。
- 背景：P0 为插件链路可验证，demo 插件在 `init()` 登记了 `/fortune`（`demo.go:25`）并订阅 `PostCreated` 申请 `first_post` 成就，但两套注册表彼此独立——插件命令名带 `/`、`command.validateCommand` 拒绝 `/`，且 `Plugin.Commands` 从未合并进 `command.Default()`，所以 `/fortune` 在解析阶段即 `ErrUnknownCommand`。同时 `night_owl` / `first_vote` 两个成就码自 0001 就在白名单里，却无任何事件钩子触发。用户 2026-11-20 拍板：v0.5.1 做推荐组合 9 项（命令 6 + 状态 2 + 退出彩蛋 1），`/xkcd` 走本地清单，状态类纳入并接已有成就码。
- 决策：
  1. **命令合并**：`command.WithCommands(extra)` 在内置表上合并额外命令；转换在 cmd 装配层 `eggRegistry()` 完成（剥前导 `/`、`Description→Summary`），并以 `command.EggCommands()` 兜底防止插件元数据与命令清单漂移——`command` 不 import `plugin`、`plugin` 不 import `application/command`（ADR-015），只有 cmd 同时看到两者。`inputHandler.withCommands()` 注入，`newInputHandler()` 无参构造仍用默认表，既有会话层测试零改动。
  2. **彩蛋内容**：`internal/application/command/eggs.go` 全部为本地常量语料——`/fortune`（20 条箴言）、`/cowsay`（ASCII 牛）、`/xkcd`（12 条本地标题，**不联网**，避免失败态与新依赖）、`/blame`（固定甩锅文案，参数只做个数校验绝不回显）、`/sudo`（假提权失败，呼应白名单+审计）、`/karma`（复用 M2 `KarmaReader` 显示声望/等级，等级仅显示）。`/quit` 约 12% 概率多说一句告别语。
  3. **安全路径**：`writeEgg()` 先规范化 `\r`→`\n`，经 `safe.RenderSystem(text, StylePlain)`（ADR-019 可信路径）再 `toCRLF` 输出；RenderSystem 拒绝 `\r`，故不能裸传含 CRLF 的语料。渲染失败退化为不输出，绝不裸写原文。彩蛋不改库、不改计分。
  4. **状态类成就**：新建 `internal/plugin/social` 订阅 `PostCreated`（发帖时刻转到 `MOTD_DISPLAY_TIMEZONE` 后在 [2,5) → `night_owl`，见下方 patch）与 `VoteCast`（首次被投票 → `first_vote`）。`VoteCast.ActorID` 是**投票者**而非作者，故扩展插件 `ReadOnlyQuery` 加 `ThreadOwnerID/PostOwnerID`（只返回整数 ID、无 PII），`forum.PublicQuery` 用既有 `ThreadReader.GetThread` / `PostReader.PostAuthor` 实现；自投票不解锁；查不到作者（内容已软删）时静默跳过——成就不是关键路径，不能让插件读失败导致 outbox 事件投递失败重试。
  5. 无新迁移、无新成就码（复用 0001 白名单里的 `night_owl` / `first_vote`）、无网络依赖。
- 理由：彩蛋是"外围消费者"，必须通过受限端口接入，不因彩蛋把 SDK 或数据库句柄引入领域内核（设计稿 §1.3 原则三）。本地清单让 `/xkcd` 零网络失败态；把状态类彩蛋接到既有成就码，让"彩蛋→成就"闭环真正生效而不新增 schema。
- 代价 / 边界：动态数字雨 `/matrix`、连续签到 7 天（需迁移）、Konami 码解锁 `/dev/null`（需输入序列检测）三项高风险/需迁移项延后 v0.5.2；彩蛋文案是固定语料，重复触发内容不变。
- **patch（用户 2026-11-20 建议，同日落地）**：`night_owl` 时段从 UTC 改为按 `MOTD_DISPLAY_TIMEZONE` 计算——`OccurredAt.In(c.Location).Hour()`，与用户在终端看到的时间一致。实现：`registry.Context` 加可选 `Location *time.Location`（nil 回落 UTC 保持零值安全），cmd 装配层注入 `terminal.DisplayLocation()`（main 已在 `newForumRuntime` 之前 SetDisplayLocation），插件不反向依赖 presentation 层。测试用 `18:30Z` 在 Asia/Shanghai（本地 02:30）解锁、UTC（18 点）不解锁证明时区生效。
- 验证：`eggs_test.go` 3 项（合并/重名拒绝/语料无 ESC）、`social_test.go` 3 项（时段边界、投票给作者而非投票者、自投票不给）、`cmd/server/egg_test.go` 4 项（固定文案、安全渲染、注入、等级阈值）；全量 `go build/vet/test` 绿、0 FAIL，测试 914 → 924。
- 关联：`P3-PLAN.md §7`（M6）、`ADR-015`（插件权限）、`ADR-019`（信任域）、`ADR-026`（零回显）、`ADR-014`（等级仅显示）。
- **patch（v0.7.2，用户 2026-11-22 反馈）**：`/help` 里彩蛋命令被显示在「管理员命令（需白名单指纹授权）」标题之下，读者会误判权限。根因是排版边界而非分组误标：`eggRegistry` 没给彩蛋设 `Group`，`WithCommands` 把彩蛋 append 到内置表末尾（管理员组之后），而 `Help()` 只用一个 `inAdminGroup` bool、仅在首次遇到 `GroupAdmin` 时插说明行，之后再无分组边界。修法：新增 `command.GroupEgg` 与 `eggGroupHelpHeader`（「彩蛋命令（无需权限，纯娱乐）」），`Help()` 改用 `map[string]bool` 按分组首次出现插头，`eggRegistry` 两处构造（插件元数据 + `EggCommands()` 兜底）都补 `Group`。**未隐藏任何命令**：`?` 是彩蛋唯一发现入口（ADR-061 的「命令表唯一入口」），隐藏会让功能事实上不存在；`Group` 仍只是展示元数据，鉴权零变化。

## ADR-062 v0.6.0 彩蛋扩展：签到、合规数字雨与按用户解锁隐藏版块

- 状态：已采纳，v0.6.0 落地。用户 2026-11-21 拍板：做完剩余彩蛋 4/10/11，按方案 2（ViewContext 注入）实现 /dev/null 按用户解锁，版本定 0.6.0。
- 背景：v0.5.1 做了 6 条本地彩蛋命令 + 2 个状态成就，延后了三项高风险/需迁移项（动态 matrix、连续签到、Konami 隐藏版块）。本条收口这三项。
- 决策：
  1. **0010 迁移**：`daily_checkins(user_id, local_date)` UNIQUE 幂等、`user_node_unlocks(user_id, node_id)` UNIQUE、`/dev/null` 种子。**关键坑**：0008 给 nodes.status 加了 `CHECK(status IN ('active','archived'))`，`INSERT ... 'hidden'` 被 `INSERT OR IGNORE` 静默吞掉（迁移登记成功但种子没插）。解决：种子用 `status='archived'`（合法枚举、ListNodes 只查 active 故默认不可见），避免为彩蛋重建 nodes 表放宽 CHECK（ADR-048 三不变量）。隐藏节点识别按 **slug 精确匹配** `/dev/null`，不按 status——否则管理员归档的普通版块会被误当彩蛋节点。
  2. **签到**：`application/user.CheckInService` 幂等记录本地日期（按 `MOTD_DISPLAY_TIMEZONE`，复用 ADR-046）、算连续天数、达标经 `AchievementGate` 申请新成就码 `streak_7`。写事务幂等靠 SQLite UNIQUE。入口层在登录成功（GitHub `loginFlow.finish` / 邮箱 `emailFlow`）会话升级后由 `onLogin` 钩子调用，钩子失败只记日志不影响登录结果。
  3. **/matrix 合规雨帘**：只生成 SGR 青色 + 固定字符集（行 60 字符、稀疏分布），50ms/行、24 行、ctx 取消即停。**不使用** ADR-027 禁止的 ED 清屏(2J)/CUP 定位/备用屏/非数字参数 CSI——真全屏动画需扩白名单，风险高，留路线图。
  4. **Konami 按用户解锁**：`command.KonamiDetector` 纯状态机（↑↑↓↓←→←→BA，错键重置，完成后自动复位），在浏览态（命令输入/编辑态不拦截）喂入；访客不触发。解锁写 `user_node_unlocks`，slug 由服务端固定不接受用户参数。可见性走**方案 2**：`ViewContext.UnlockedHiddenNodes []node.Node` 由入口层注入，`forum.ScreenNodes` 按 ID 去重后追加到列表末尾——forum 层不做解锁判权、不依赖解锁存储，维持 ADR-015 边界。
- 理由：三项彩蛋都是"外围消费者"，必须走受限端口接入，不因彩蛋把存储或渲染控制面引入领域内核。签到复用既有成就 gate 与展示时区配置，零新增配置；隐藏节点用 ViewContext 注入避免污染 forum 通用查询链（比"列表注入行"稳健、比"改 /open 支持 slug"语义完整）。
- 代价 / 边界：签到跨时区旅行可能出现日期空洞（按字符串比对）；/dev/null 只有可见性、无内容与权限变化；Konami 状态机在浏览态会消费方向键导航（序列完成/错键重置前短暂改变焦点，属彩蛋代价）。真全屏数字雨、多色终端、更多隐藏版块写入 `docs/ROADMAP.md`，不在本版。
- 验证：格式零、build/vet OK、全量 0 FAIL，测试 924 → 935；专项覆盖 Konami 精确/错键、签到连续/断签/幂等、SQLite 解锁持久化、ViewContext 去重注入、matrix 帧无控制字符。迁移后节点数断言（5→6）与状态分布（5/5/0→6/5/1）同步更新。
- 关联：`ADR-027`（ANSI 白名单）、`ADR-046`（展示时区）、`ADR-048`（节点三不变量）、`ADR-015`（插件权限）、`P3-PLAN.md §7`、`docs/ROADMAP.md`（多色/节日主题评估）。

## ADR-063 `/latest` 跨版块最新列表：allow-list 可见性与强制索引

- 状态：已采纳，v0.7.1 / P4.5-M2 落地（分支 `feat/p4.5-ops-visibility`）。
- 背景：已部署实例（jeeinn.com）的真实反馈——浏览最新动态必须逐版块翻找。计划稿给出的做法是 `JOIN nodes n ON n.id = t.node_id AND n.status = 'active'`，实施期核对发现三个硬事实与之冲突：① `threads` 自 0004 起 8 列，计划里的 `SELECT t.*` 会让 `scanThread`（7 列）运行期报错；② 版块内列表靠「归档版块从 `ListNodes` 消失、导航不可达」隐藏，跨版块没有这层兜底，但简单 JOIN `status='active'` 又会挡掉彩蛋版块 `/dev/null`（`status='archived'`，靠 `user_node_unlocks` 按用户解锁）；③ 实测不加索引提示时，同一句 SQL 在不同数据分布下会给出不同计划（`idx_threads_node` + `USE TEMP B-TREE` 与 `idx_threads_last_reply` 之间摇摆），索引断言不可稳定复现。
- 决策：
  1. **语义**：`ThreadQuery.NodeID == 0` 表示跨版块；此时**必须**同时给出 `AllowedNodeIDs`，仓储只做 `node_id IN (...)` 过滤，白名单为空时 fail-closed 返回空页（绝不退化成全站可见）。`NodeID < 0` 返回 `ErrInvalidNodeID`（用例层单点校验，仓储保留同款防御）。`NodeID > 0` 的版块内分支 SQL **逐字未改**，既有行为零变化。
  2. **可见性口径**：「`/latest` 的可见性必须与版块列表完全一致：用户能点进去读的版块，其主题就必须出现在『全论坛最新』里」——否则命令名义上跨版块，实际悄悄丢掉用户有权看的那一块，这是可见性口径的分裂，不是彩蛋的私密性。因此白名单 = `ListNodes`（公开 active）∪ 该会话的 `ViewContext.UnlockedHiddenNodes`，由 `ScreenThreads` 一次算出；**SQL 里不得出现任何 `status` 放宽**（会泄漏归档普通版块），也不得收紧成 `status='active'`（会挡掉已解锁彩蛋版块）。
  3. **查询形态**：`INDEXED BY idx_threads_last_reply` 强制命中 + 行值游标 `(t.last_reply_at, t.id) < (?, ?)`。前者让计划确定（ANALYZE 前后逐字一致），后者把 `SCAN`（从索引头扫到游标，翻 k 页 O(k×N)）变成 `SEARCH`（从游标 seek，总量 O(N)）；首屏无差别，深翻页差三个数量级。列投影逐列书写，`SELECT t.*` 永久禁用。
  4. **展示**：跨版块行由 `ScreenThreads` 事后投影 `RowMeta.Node`（slug，走 `ext()` 净化链）；版块内屏不填（与状态栏重复）。页面层用固定串 `latestScope = "latest"` 作 nodeSlug——`ValidateSlug` 要求真实 slug 以 `/` 开头，故不可能冲突。不新增 `ScreenKind`、不动 ViewModel 契约。
  5. **同屏上下文三处修正**（实施期实测）：`showThreads` 用本次 slug 覆盖 `v.Context`（否则首帧状态栏显示上一屏）；`nodeID==0 && slug==""` 直接回版块列表（检索结果开详情后按 `q` 的残缺态）；`cursor==""` 时 `offset=0`（防检索屏的 offset 带进跨版块首屏）。
  6. 空态文案中性化为「（暂无主题，登录后可用 /new 发第一帖）」——同一个 `ScreenThreads` 也承载跨版块列表，说「该版块」是错指；不让 presentation 去比对 `latest` 字面量（会造成跨包字符串漂移）。
- 理由：白名单是唯一能同时满足「归档普通版块不可见」与「已解锁彩蛋版块可见」的口径；把可见性留在 application（可测、与呈现无关）、仓储只执行集合过滤，符合 ADR-001 的端口边界。强制索引把「静默退化」换成「缺失即报错」，与 `ReplyCountsByThreadIDs` 的既有取舍同源。
- 代价 / 边界：
  1. 索引名硬绑定：缺 `idx_threads_last_reply` 时错误在 **prepare 期**抛出，`/latest` 整体不可用；同理**不把 `/latest` 加入启动自检**——它不是 liveness 依赖，0011 用 `IF NOT EXISTS` 已保证正常部署不会缺。错误经 `p.fail` 呈现时，`NoticeFromError` 的 default 分支会外显 `err.Error()`（既有行为，无注入面，但会露出英文内部描述）。
  2. `/latest` 空态里引导 `/new` 会二次提示「请先进入一个版块再发新主题」——零主题实例才可能触发的两步绕行，换取零跨包字符串耦合。
  3. allow-list 越窄越亏：正常形态下白名单 ≈ 全部公开内容，仅在「大部分版块被归档」时出现期望外的扫描量（≈ 21 × 总行数/可见行数），个人论坛量级可接受，不做优化。
  4. `RowMeta.Node` 跨版块填、版块内不填，两处口径不同（版块内填会与状态栏重复），是刻意差异。
- 验证：`cross_node_test.go` 8 项（排序与软删、白名单为唯一判据、空白名单 fail-closed、`SortScore` 跨版块拒绝、负 ID 拒绝、同秒 5 帖翻页不重不漏、行值游标与 OR 形态结果全等、执行计划含 `idx_threads_last_reply` 且无 `TEMP B-TREE`、缺索引 fail-fast）；`latest_command_test.go` 6 项（跨版块列表与 slug、版块内无 slug、导航闭环、`/new` 拒绝、检索来源返回、空态中性）；命令表与 `/help` 断言同步。四门禁 + 两条闭环绿。
- 关联：`ADR-048`（节点三不变量）、`ADR-062`（按用户解锁隐藏版块）、`ADR-014/019`（作者与外部文本投影）、`ADR-046`（展示时区）、`P4.5-PLAN.md §3`。

## ADR-064 `/stats` 社区统计看板：诚实口径与峰值采样

- 状态：已采纳，v0.7.1 / P4.5-M1 落地。
- 背景：同一批运营反馈要求「一屏看到社区活跃度」。计划稿的取自方向正确（登录人数取 `daily_checkins`、峰值取新表、人次取新审计事件），但实施期暴露出四处「会写出错误数字」的形状：① `presence.Peak()` 是**进程**生命周期最大值，不按天重置；② 计划写的 `gate.Record(...)` 不存在，且 `record()` 固定 provider=`github`；③ `stats_daily.local_date TEXT PRIMARY KEY` 在 SQLite 允许 NULL 且允许多行 NULL；④ 每 30 秒无条件 UPSERT 会把写队列当计时器用。
- 决策：
  1. **口径表**（每项都能追到数据源，零回填零臆造）：今日/昨日登录人数 = `daily_checkins` 按 `local_date` 去重计数（v0.6.0 起有真实数据）；今日/昨日在线峰值 = `stats_daily.peak_online`；历史在线峰值 = `MAX(peak_online)`；当前在线/活跃 = `presence.Online()/Active()`（实时内存）；注册用户 = `users COUNT(status='active')`（pending/deleting/deleted 不计，防注销虚增）；累计登录人次 = `auth_events COUNT(event='login_success')`（自 v0.7.1 起累积）。各指标起点必须上屏声明。
  2. **`--` 判据（两条件，与实现字段解耦）**：`今日在线峰值` 显示 `--` 当且仅当「该日无 `stats_daily` 行 **且** 渲染时刻当前在线为 0」。实时在线数本身构成一次观测，因此「没有落库行」≠「没有观测」。`昨日在线峰值` 无实时源，判据是纯「有无该日行」；`历史在线峰值` = max(全时 MAX, 今日显示值)，故今日一有观测、历史必不为 `--`。图例 `-- 表示暂无在线观测数据` 只在屏上真出现 `--` 时插入一次，判据与三处 `--` **同源**（同一次钳制后的 observed/seen 标志）；升级首日因昨日位无行，必定出现一次。
  3. **展示层自洽（两处纯算术钳制）**：今日 = max(库值, 当前在线)；历史 = max(全时 MAX, 今日显示)。由此恒有「历史 ≥ 今日 ≥ 当前在线」。**不保证**同一天内多次渲染的今日峰值单调不降：`Online` 在两次 tick 之间会动、日内峰值只在 tick 时前推，要单调必须让渲染路径写回日内高水位——为 ≤30 秒的显示抖动引入读路径写入，收益与风险不成比例，故不做；ADR 已声明「显示值可领先持久化值一个 tick」，本现象是该句的直接推论。
  4. **`login_success` 语义**：一次「登录动作」成功 = GitHub Device Flow 授权完成登录/建号，或邮箱密码登录成功（两处 `onLogin` 调用点，均在 `UpgradeToUser` 成功后）。重连凭公钥恢复登录态**不计**——那是会话恢复不是登录动作。埋点走新增的 `SecurityGate.NoteLoginSuccess(userID, provider)`：直接 `audit.Record` 并显式传 provider（白名单 `github`/`email`），**不走** `g.record()`（它固定用 `g.provider`，会把邮箱登录记成 github）。
  5. **fail-open 边界（audit.go「队列满必须拒绝认证动作」的唯一显式例外）**：login_success 是登录**成功之后**的事后统计事件，写不进去不能回滚已完成的登录——`Record` 失败只 `logger.Warn`。装配层收敛为 `newOnLoginHook(checkIn, noteLoginSuccess, logger)`：审计先行（纯内存入队）、签到在后（写库），**两者互不短路**。v0.6.0 的「签到失败即 return」会让 login_success 跟着丢，人次被系统性低估且永不报警。
  6. **峰值采集机制**：cmd 层 30s ticker（application 不起 goroutine、presence 不 import SQL，两条守卫不破）。状态只有 `day / dayPeak / rowWritten / writtenPeak`：**当日首 tick 无条件写一次**（值可为 0——这是「该日存在观测窗口」的来源，也是 `--` 与 `0` 的唯一判据，写频优化不得省掉它），之后只在创新高时写；跨天 `dayPeak` 归零、**不读 `presence.Peak()`**（进程峰值会把昨日 50 写进今天并被 `MAX` 锁死一整天）；停机补采一次（独立 2s ctx）。写入是**单条幂等 MAX-upsert**：`Writer.Submit` 的失败含「commit 结果未知」（队列未入队 / 关闭中 / 重试耗尽 / commit 失败），只有幂等语句能安全重放；失败时状态不推进 → 下轮自然重试（`!rowWritten` 本身就是 pending 标志），日志当日首次 Warn、其后 Debug，恢复时 Info。
  7. **`stats_daily` 约束**：`local_date TEXT NOT NULL PRIMARY KEY` + 形状 CHECK（`GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'`），`peak_online >= 0`，`updated_at >= 0` 且只在 MAX 生效时推进。SQLite 的 `TEXT PRIMARY KEY` 允许 NULL 且允许多行 NULL，一次空日期即污染全历史 `MAX`；`length=10` 挡不住 `2026/11/22`。
  8. **文案**：6 行定稿（「今日在线峰值 / 历史在线峰值」而非「今日峰值在线 / 历史最高在线」——前者主语明确，后者「最高」是绝对化措辞而该值自 v0.7.1 起才累积）；40 列是排版**目标**不是结构不变量（数字涨三位数时正解是重排行）；末行「登录自 v0.6.0；峰值与累计登录自 v0.7.1」是免责尾注，不可删。
- 理由：这三条不是三个 bug，是同一条不变量——**屏幕上的数字必须等于我们真的观测到的**——被三个方向破开的样子：

  | 反例 | 触发 | 后果 | 防线 |
  | --- | --- | --- | --- |
  | 把昨天的观测写成今天 | 用进程生命周期 `Peak()` 而非当日自持峰值 | 昨日 50 写进今天，被 `MAX` 锁死一整天 | 基线隔离（`dayPeak` 跨天归零） |
  | 把没观测写成有 | 无行渲染 0，或用昨日行/历史值兜底 | 进程没跑的那天读成「今天没人」 | 类型层（`observed bool`） |
  | 把观测到 0 写成没观测 | 写频优化省掉当日首 tick 建行 | 真实 0 人显示成 `--` | 首写义务（首 tick 无条件写 + 失败补建） |

- 代价 / 边界：
  1. `peak_online` 是**当日 30 秒采样峰值**，可能低估瞬时并发；不以此为口径承诺、不据此做容量决策。将来要精确并发的做法是提高采样频率或改事件计数，不是改文案。
  2. 两次采样之间起落的瞬时尖峰可能不被任何持久化记录，后到的读者看不到该尖峰；属已接受的采样精度边界，不通过展示层或文案补救。**这正是「采样器内存日内峰值」曾存在的动机**——它为消除这类遗漏而引入，但因无法实现同日单调、且读接口缺日期守卫（午夜会把昨日峰值当今日）被移除。未来任何试图补回该能力的改动必须同时满足：①真正实现同日单调（而非只在 tick 时前推）；②带日期守卫；③不得在读/渲染路径写库。缺一不合入。
  3. 建行持续失败并跨过午夜 → 昨日行永久缺失 → 昨日显示 `--` + 图例解释；**不做**跨日 closing write（要第二个 pending 槽，且触发窗口要求 DB 恰在 00:00–00:30 不可用，成本大于收益）。
  4. 进程启动后首 tick 落库前的 ≤30 秒内，今日峰值可能翻转一次（有人在线显示数字 → 人走了且仍无行 → 翻回 `--`）；不为它加「今天见过就记住」的状态。
  5. 图例解释的是全屏所有 `--`，不含「登录 0」——`0` 是真值（观测到没人），不是缺失。
  6. Ambiguous 宽度字符（East Asian Width = A）进入输出词汇需一次性决策：已入词汇且经终端实测的（U+00B7 `·`，屏幕标题/页脚/`/karma` 共 8 处）可复用；新增的一律拦（U+2014、U+2026），除非同时补 `wideRanges` 与宽度口径说明。
  7. 昨日日期一律 `AddDate(0,0,-1)`，禁止 `now.Add(-24h)`：`MOTD_DISPLAY_TIMEZONE` 可配任意时区，跨 DST 切换日用 24 小时减会落到同一天或跳一天（症状：「昨日数字莫名等于前日」）；更根本的理由是「昨日登录」与「昨日在线峰值」现在同屏相邻，两行的「昨日」若指向不同日期，用户会读成我们算错。
- 验证：`repo_stats_test.go` 7 项（UPSERT 只升不降/写 0 不打回、`updated_at` 只在变高时推进、跨天隔离、非法日期与形状被拒、注册数只算 active、按日去重登录、只数 login_success、0011 对象存在）；`stats_test.go` 5 项（本地日历日期、展示时区归属、峰值自洽、`--` 两条件、读失败不兜 0）；`stats_command_test.go` 3 项（6 行文案逐字、`--` 变体 + 图例、行宽）；`stats_sampler_test.go` 5 项（首行必写、只写变高、跨天归零、失败重试、nil 安全）；`login_hook_test.go` 6 项（审计先行、失败不短路、nil 安全、`/stats` 固定文案与零回显）。测试函数 948 → 989。
- 关联：`ADR-046`（展示时区）、`ADR-016`（presence 纯内存）、`ADR-019/026`（信任域与零回显）、`ADR-010`（写队列）、`P4.5-PLAN.md §4`。

## ADR-065 `/whoami` 与 `/who`：本人资料命令、在线名单，`/karma` 入口删除

- 状态：已采纳（2026-11-23，`feat/whoami-who` 分支；设计稿 `docs/DESIGN-whoami-who.md` §11 决策记录）。
- 背景：用户要求「查看已登录状态/我的信息（账号、注册时间、等级）」与「列出当前在线人员（类 Linux `who`，人多可分页）」。Principal 只带 `UserID`，现有端口无个人资料读口；`/karma` 只显示声望等级，与新命令重叠。
- 决策：
  1. **命令**：`/whoami`（别名 `/me`）查看本人资料；`/who [页]` 列在线人员。`/info` 不启用（语义过宽，留给未来实例信息）。`/who` 参数只接受正整数页码，非数字 / `@xxx` / 0 / 负数给固定用法提示，不查库、不回显原文。
  2. **`/whoami` 输出**：账号 `@username（ID）`、注册时间（`formatKeyTime` 时区口径）、声望与等级（复用 `karmaLevel`/`progressDesc`，阈值不另造）。访客给固定提示；端口缺失 / DB 失败 / 记录不存在统一固定文案。
  3. **新只读端口 `ProfileReader.ProfilesByIDs(ctx, []int64)`**：批量 IN 查询零 N+1，落 `UserStatusReader`；返回 `ProfileView{Username, UserID, Karma, Status, CreatedAt}`，不含凭证、身份源、邮箱。`/whoami` 传单元素、`/who` 传名单共用。
  4. **`/who` 形态**：内联块 + 参数分页（页长 15），不做专用屏、不接 n/p 键（候选 B 留待在线规模真正增长后评估）。单次 `Snapshot()` 派生在线/活跃/访客计数与名单，避免多次快照漂移；活跃态（浏览/编辑）在前，其余按 `LastSeenAt` 倒序，稳定排序；访客连接（UserID=0，含认证中）不列名，只计入汇总。用户名经批量投影，查不到回退 `#ID`。
  5. **`/karma` 命令入口直接删除**（不做别名过渡）：删彩蛋条目、`case "karma"` 分发与 `runKarma`；**能力层保留**——`karmaLevel`/`progressDesc` 由 `/whoami` 复用，`KarmaReader` 端口与 repo 实现保留（状态栏持续消费），等级阈值测试与 `karma_display_test.go` 不动。
  6. **信任域（评审 P0 修正）**：固定标签与本地数字走 `RenderSystem`；`@username` 是 DB 用户数据，单独走 `RenderUser` 净化链，两段绝不拼进同一次渲染（初版「净化后嵌入整串交 writeSystem」违反 ADR-019，已改为分段 `segSystem`/`segUser`）。
  7. **可见性**：两条命令所有人（含访客）可用，不加 `requireAdmin`——只读公开信息，在线数本就在状态栏公开；ADR-049 白名单只卡治理写操作。
- 理由：`whoami`/`who` 是 Unix 成对语义，比 `/info` 精确；参数分页以零屏模型改动满足「分页查看」，专用屏成本与个位数并发不匹配；删 `/karma` 而非保留别名，避免两条命令长期重叠，同时能力层不删保证状态栏与未来复用不受影响。
- 代价 / 边界：
  1. `/who` 块打印后留在原屏，n/p 键不响应（与 `/stats` 同构）；翻页靠再输 `/who <页>`。
  2. 名单基于单次快照，翻页期间连接上下线会造成页间漂移；个人论坛规模下接受，不做快照冻结。
  3. v1 不展示 provider / 邮箱、发帖回复计数、成就列表；A 看 B（`/finger`）延迟立项，届时先定字段级隐私白名单。
  4. `karmaLevel`（cmd）与 `domain/user.Level` 是两套阈值实现，由 `TestKarmaLevelMatchesDomainLevel` 逐点对拍防漂移。
- 验证：`who_command_test.go` 15 项（计数/排序/投影回退/时长文案/状态词、访客提示、端口缺失、DB 失败、成功分段、注入无 ESC、非法参数、空快照、happy path、页界、16 人分页）；`repo_profile_test.go` 2 项（批量投影/空输入）；`TestKarmaLevelMatchesDomainLevel` 对拍；四门禁全绿 + 两条闭环专项。
- 关联：`ADR-016`（presence 纯内存）、`ADR-019/026`（信任域/零回显）、`ADR-057`（karma 计分，能力层保留）、`ADR-049`（授权边界）、`ADR-014`（展示投影不进领域模型）。

## ADR-066 `q` 返回列表按行 ID 回填焦点，不做数字直达

- 状态：已采纳（2026-11-24；设计稿 `docs/DESIGN-back-focus.md`）。
- 背景：从帖子详情按 `q` 返回版块列表后，`screenOf` 无条件把 `FocusedIndex` 重置为首行，用户得重新往下找刚读的帖。用户另提「按行号直达」，但那解决的是新列表远距离跳行，与续读痛点无关，本期不立项。
- 决策：
  1. **只做 P0-b（返回保持焦点）**：`back()` 重建屏后按锚点行 ID 回填 `FocusedIndex`/`TopRowIndex`；锚点详情→列表用 `p.threadID`，列表→节点用 `p.nodeID`（`showNodes` 会清零 `nodeID`，故先捕获）。
  2. **按 `Row.ID` 定位，不按显示序号**：`Index = RowOffset+i+1` 是页内位置号，列表按最新排序重查后同帖可能换位，按序号会落到另一篇；`Row.ID` 在节点行=node.ID、主题行=thread.ID、检索行=threadID。
  3. **未命中回退首行**：锚点 <=0 或新列表找不到该 ID（原帖软删/检索来源 `nodeID=0`）时，保留 `screenOf` 首行默认，不报错、不落空焦点。
  4. **不保留场景**：`n`/`p` 翻页、`refresh` 刷新、删帖重建、检索详情回节点列表均落首行；`restoreFocus` 只挂在 `back()`。
  5. **双渲染**：`show*` 内部已渲染首行焦点帧，回填后再 `render` 一次由 `RenderFrame`（ADR-027）原地覆盖；`back()` 低频，不为单次回填重构 `show*` 渲染职责。
  6. **P0-a 数字直达（1–9 直开、两位数缓冲）本期不立项**，留待本特性落地后单独评估。
- 理由：痛点直接由焦点重置引起，按 ID 回填是控制器层纯状态修补，不碰用例、仓储、渲染与键位；误删/检索来源等失配路径天然回退，无需额外状态机。
- 代价 / 边界：
  1. 返回时多一帧原地重绘；列表数据在打开详情期间变化时，定位的是「同一篇」，位置以新排序为准。
  2. 不记忆滚动位置（当前列表整屏容纳，无滚动模型）；`TopRowIndex` 仅回填语义，渲染不消费。
  3. 无 schema 迁移、无新环境变量、无键位/渲染改动。
- 验证：`back_focus_test.go` 3 项（详情返回定位原帖、原帖软删回退首行、节点返回定位原节点）；既有导航/重绘/检索返回测试不破；四门禁全绿 + 两条闭环专项。
- 关联：`ADR-027`（原地重绘帧连续性）、`ADR-063`（游标/offset 与残缺状态回退）、`ADR-014`（展示投影不进领域模型）。

## ADR-067 提交失败保留会话草稿：限流 A 原地重建 / 准入拒绝 B 回屏预填

- 状态：已采纳（2026-11-24；设计稿 `docs/DESIGN-composer-draft.md` v2，四方评审 + 用户拍板 D1~D5）。
- 背景：发帖/回复命中限流（60s 冷却 / 1h 10 帖）或被只读/冻结拒绝时，`submitEditor` 先 `p.editor = nil` 再 `fail()`，标题与正文随 GC 丢失，真实用户需逐字重打。SSH 架构下服务端写不到用户终端，「本地保存」不可行，等价落点是服务端会话内存草稿。
- 决策：
  1. **错误分流（§4.2）**：`retryableDraftError` 只收 `*RateLimitError` / `ErrReadOnly` / `ErrForbidden`；校验错误、`ErrNotFound`、存储故障不存草稿。
  2. **路径 A（限流）原地重建**：存草稿后 `Load(body)` 整块重建编辑器、保持 `StateComposing`，**不调 `pasteBlock`**（避免第二遍正文 + `echo.cols` 叠加致退格错乱，P0-2），只打印「约 X 秒后 Ctrl-D 重发」静态等待提示，不整屏重绘、不回 Browsing。
  3. **路径 B（ErrReadOnly / ErrForbidden）回屏预填**：摘编辑器、回 `StateBrowsing`，`fail()` 固定文案后追加「仅本次连接」草稿提示；重进同上下文 `/reply` / `/new` 或回原帖按 `r` 时 `Load` 预填并回显一次。
  4. **挂起回屏（D2）**：A 状态按 Esc = 挂起（保草稿、回屏、提示按 r 恢复），覆盖评审原「取消」语义；Ctrl-C = 取消并清草稿。无草稿时 Esc/Ctrl-C 维持原行为。
  5. **`actor()` 失败分支同样存草稿（P0-1）**；账号不存在（`ErrNotFound`）在 `actor()` 统一映射 `ErrForbidden`，与事务内 `checkWritePermission` 口径一致。
  6. **`/edit` 不检出草稿（§4.6）**：`startEditComposer` 从空缓冲开始，避免 EditPost 覆盖原文；回复/发帖草稿保持不动。
  7. **只做内存版（D4）**：草稿挂 `pageController`，不落库、不进日志；断连即失，文案统一「临时草稿（仅本次连接）」（D5）。P1 落库登记 backlog，本期不实现。
  8. **`Composer.Load(text)`**：整块构造 lines，绝不逐字符 Feed（防独立 `.` 行误判哨兵）；CRLF/CR→LF；超限不改状态。
- 理由：痛点是失败链路丢输入，控制器层纯状态修补即可闭环；A/B 共用一份会话草稿，恢复入口统一（按 r / 同上下文重进）；不碰用例、仓储、渲染与键位。
- 代价 / 边界：
  1. 草稿仅本次连接有效，断连/重连即失（内存版既定边界，D5 管理预期）。
  2. 倒计时不自动刷新（守 ADR-027），秒数为静态文案「约 X 秒、以服务端为准」。
  3. **基础设施层错误语义变更**：`repo_user.go` 的 `UserStatusReader.Status` 在 `sql.ErrNoRows` 时由普通 `fmt.Errorf("用户 %d 不存在")` 改为包装 `forum.ErrNotFound`，供 `actor()` 以 `errors.Is` 识别并收敛为 `ErrForbidden`（决策 5 的落地载体）。影响面超出编辑器本身：`Status()` 另两个调用点已核对无回归——`ownerActor`（err≠nil 时吞掉、沿用零值状态）与 `AchievementService.Apply`（`fmt.Errorf("读取账号状态: %w", err)` 包装上抛，错误文本变化但判定语义不变）。架构方向仍为 infrastructure→application（archtest 通过）。
  4. 无 schema 迁移、无新环境变量、无键位/渲染改动；P1 落库另立 ADR 评估 ADR-010 写队列背压。
- 验证：`composer_draft_test.go` TC-A01~A11 P0 断言集 + `composer.go` Load 纯单测；四门禁全绿 + 两条闭环专项 + archtest。
- 关联：`ADR-026`（回显净化）、`ADR-027`（原地重绘 / 不做倒计时刷新）、`ADR-034`（多行编辑）、`ADR-010`（写事务稀缺，约束 P1 落库）。
- **patch（v0.8.3，用户线上实测反馈）**：草稿恢复的回显光标与 `echo.cols` 错位，修两处——
  1. **B/挂起恢复**（`resumeDraft`）：原在 `pasteBlock(正文)` 后写恢复提示，`writeFixed` 自带 CRLF 使提示贴正文末字并把光标甩下一行，而 cols 仍描述正文末行 → 退格擦错位置、提示粘连。修法：提示前置到正文回显之前，末尾输出正文，光标自然停正文末字、cols 对齐。
  2. **A 限流原地重建**（`rebuildComposerFromDraft`）：Load 后 cols 被 `newline()`+`setSecret()` 双清为空，退格删缓冲却零擦屏。新增 `line_echo.resyncLastLine`：白名单 `CursorUp`+`CR`+`ClearToEOL` 回正文末行重写并重建 cols，只重写末行不重复正文；`up=2`（提交换行 1 行 + 提示尾部 CRLF 1 行）。**边界**：`up` 依赖限流提示不折行，极窄终端折行时可能偏差（常规宽度实测单行，成立）。
  - 新增 5 项回归测试（提示顺序、宽字符退格精度、单行/多行 cols 重建、正文不重复），适配原 A 路径断言；测试 1028 → 1032，全门禁绿。
- **已修订（ADR-069）**：决策 2 已由 ADR-069 修订（v0.9.0 方案 B：追加重建，改为 `pasteBlock` 整段回显）。本条原文（含"不调 `pasteBlock`"）与上方 v0.8.3 patch 段保留作历史记录，不删改。

## ADR-068 回复成功后焦点回填到自己的回复（ADR-066 锚点机制扩展）

- 状态：已采纳（2026-11-24；设计稿 `docs/DESIGN-hint-reply-focus.md` O2）。
- 背景：发表回复成功后 `showDetail` 经 `screenOf` 把 `FocusedIndex` 重置为首条回复，用户自己刚发的回复没有 `> ` 光标指示，需重新下滚寻找。
- 决策：
  1. **复用 ADR-066 `restoreFocus(anchor)`，不改其实现**：`submitEditor` 捕获 `CreatePost` 返回的新 postID，回复成功分支先判 `showDetail` 的 err（失败直接返回，不在失败屏上回填），成功后 `restoreFocus(newPostID)`，再二次 `render`（沿用 ADR-066 双渲染）。
  2. **仅回复分支**：编辑（`editing`）不推进 `last_reply_at`、不做焦点锚定；发帖（`creating=true`）回列表，焦点维持首行，均不动。
  3. **未命中回退首行**：新回复被软删，或主题已有 ≥200 条可见 Post（`MaxPostsPerThread`，新回复 ID 最大、不在 `ListPosts` 返回集合）时，`restoreFocus` 保留 `screenOf` 首行默认，不报错、不落空焦点，不劣化现状。
  4. **不改 ADR-066 原文**：其"`restoreFocus` 只挂 `back()`"口径保持；本条是同一锚点机制在"提交后落点"场景的扩展使用。
- 理由：`> ` 是键盘光标位置（`view.go` 仅比较 `r.Index == FocusedIndex`），无选中/已读业务语义；按自己 postID 精准命中，不受并发插入他人回复的排序影响。控制器层纯状态修补，不碰用例、仓储、viewmodel、渲染与键位。
- 代价 / 边界：
  1. 回复成功多一帧原地重绘；`<200` 条常规情形新回复必在 Rows，稳定命中。
  2. 超 200 条新回复不可见与焦点不命中同源，属详情屏内容分页缺陷（设计稿 O3，独立 P1 backlog），本期不修。
  3. 误删/误改不依赖光标：`/delete` 无参删主题、`/edit` 无参取首帖，移动光标不改变目标。
  4. 无 schema 迁移、无新环境变量、无键位/渲染改动。
- 验证：O2-T1~T5（正常回复命中自己、并发插入锁定自己、软删回退首行、超上限不 panic、发帖不误伤）；`back_focus_test.go` 3 项与 `composer_draft_test.go` 全套不破；四门禁全绿 + 两条闭环专项 + archtest。
- 关联：`ADR-066`（按 Row.ID 回填焦点、双渲染）、`ADR-027`（原地重绘帧连续性）、`ADR-014`（展示投影不进领域模型）。

## ADR-069 v0.9.0 方案 B：限流原地重建改为追加重建止损（修订 ADR-067 决策 2）

- 状态：已采纳（2026-11-24；设计稿 `docs/DESIGN-v090-display-i18n.md` §4.2 W4-W7，评审 B1/B3/B4 + 用户拍板 D2/D6）。
- 背景：v0.8.4 在大视窗终端完整，但用户窄屏连续回复时正文字符散落。根因不是"窄屏边缘"：限流提示实测显示宽度 **125 列**（含 `[提示] ` 前缀与 `1m0s`），任何 **< 126 列**终端都会物理折行——80×24（SSH 最常见默认）、100 列、120 列全部命中，约 150 列宽屏才"正常"，可达面是绝大多数用户。ADR-067 的 `resyncLastLine(up=2)` 硬编码"提示 = 1 个物理行"，提示折行后 `CursorUp(2)` 跳进提示中间；`EL`（`\x1b[K`）只清当前物理行，清不掉折行产生的 H 行碎片，动态算 `up` 也是假修复。优先级仍 **P1**，依据是"数据不丢、闭环未断、可重发/恢复"，不是"全屏可规避"。
- 决策：
  1. **删除 `resyncLastLine`**（`line_echo.go`），A 路径 `rebuildComposerFromDraft` 不再输出任何 `CUU`。
  2. **A 路径改为追加重建**：`writeHint` 按 locale 查表写等待提示（写 hint 的 i18n 化见 ADR-070），`setSecret(false)` 已清 `cols` 后不额外 reset，末尾直接 `p.echo.pasteBlock(e.body.Text())` 整段追加回显；光标自然停在正文末行末字，`cols` 由 `pasteBlock` 逐 rune 重建。
  3. **明确以正文多回显一遍换光标与退格正确**：不整屏重绘、不回 Browsing 态、不引入 CUP/CUD/ED/DECSET，倒计时保持静态文案（守 ADR-027）。
  4. **修订 ADR-067 决策 2**：其"不调 `pasteBlock`"被本条取代；原文与 v0.8.3 patch 段作历史记录保留，仅在 ADR-067 内加一行"已修订（ADR-069）"交叉引用，不删改。
- 理由（逐条回应 P0-2「退格错乱不复发」三条理由）：
  1. **光标落点由输出流末尾决定，不依赖硬编码 `up`**：`pasteBlock` 是最后一个写动作，光标自然停在正文末行末字；提示折成 1 行还是 4 行都不改变落点，从根本上移除"提示 = 1 物理行"的假设。
  2. **`cols` 由 `pasteBlock` 逐 rune 重建，退格精度保持**：正文每个 rune 经 `SanitizeText` 后计入末行 `cols`，与屏幕实际显示宽度逐字对应；中文等宽字符仍按 2 列擦除（`\b \b` 两组），v0.8.3 的退格精度不回退。
  3. **不用 CUP/CUD/ED/DECSET、不扩 ADR-027 白名单**：只用既有 `pasteBlock`、`writeHint` 与白名单内的 `\r\n`/SGR；信任域与零回显边界不变（服务端提示走 `RenderSystem`，用户正文仍走净化链）。
- 代价 / 边界：
  1. **正文多回显一遍（刷屏）**：同一段正文出现两遍（提交前一遍 + 重建追加一遍），长正文时刷屏明显——如实登记 D6 代价。
  2. **提示离光标变远（长正文时倒计时可能被顶出屏幕）**：提示打印在追加正文之前，光标停在正文末字，物理行距离随正文长度与折行数增长；倒计时不自动刷新。
  3. **不整屏重绘**（守 ADR-027），不回 Browsing，不改键位；"限流提示不折行"假设已破除，代码里不再有任何依赖它的光标回跳。
  4. 无 schema 迁移、无新依赖、无新环境变量。
- 验证：组 A 测试基建 W1（`fakePageSession` 可配宽度）/ W2（`cuuParams` 提取 CUU 参数）/ W3（虚拟终端网格 + pending wrap）；W4-W6 B 路径落地与三处断言语义适配，`len(cuuParams(out))==0` 取代 `Contains("\x1b[")` 防 SGR 假通过；虚拟网格 TC-01（24/40/125 列输出无 CUU、无碎片、正文紧随 hint）、TC-02（80×24 提示折 2 行仍无散落）绿；测试函数 1041 → 1056；四门禁 + 两闭环专项 + archtest 绿。
- 关联：`ADR-026`（回显净化）、`ADR-027`（原地重绘 / ANSI 白名单）、`ADR-034`（多行编辑）、`ADR-066`（同链路焦点回填机制，本条不改）、`ADR-067`（被修订的决策 2 与 v0.8.3 patch 段）、`ADR-068`（同链路后续焦点扩展）。

## ADR-070 v0.9.0 方案 C：物理行模型与 i18n 基建（统一折行口径 + 双语 hint）

- 状态：已采纳（2026-11-24；设计稿 `docs/DESIGN-v090-display-i18n.md` §4.3-§4.4 W8-W14，评审 D4 升级纳入 B7）。
- 背景：显示层缺物理行模型——`lineEcho` 只有 `out/secret/cols`，`cols` 记录逻辑列宽、不知道终端宽度；`RenderFrame` 虽有 `overWide` 降级，却只覆盖整屏渲染，编辑器回显层裸奔；`frame.go` 的 `overWide`/`visibleWidth` 与回显层形成双口径。B7 缺陷同源：`mergeBackspace` 的 `CursorUp(1)` 与 `backspace` 的 `\b \b`×逻辑宽都假设"一行 = 一个物理行"，超宽行按 Enter 后退格会错位残留。i18n 无骨架，hint/notice 前缀散落字面量，无法承载第二语言。
- 决策：
  1. **`safe/wrap.go` 纯函数**：`MeasureLine`/`MeasureWrap`/`OverWide`，基于 `DisplayWidth` 逐 rune 计算逻辑行的显示宽度、物理行数、折行点与 **pending wrap**（占满末列时光标留行末，下个字符到达才折——决定 `EL` 清哪行、`CUU` 落哪行）；宽字符整字折、SGR 计 0 列、零 IO、不输出 ANSI、不改白名单。
  2. **`RenderFrame` 共用 `safe.OverWide`** 替换原 `overWide`/`visibleWidth` 重复逻辑，消除整屏渲染与编辑器回显层的双口径。
  3. **`lineEcho` 注入 `width`**（0 = 未知，退化为旧行为）：`mergeBackspace` 按 `MeasureLine` 的物理行数输出 `CursorUp(n)`，修 B7 超宽行合并残留；`\b \b` 仍按逻辑宽逐 rune 擦除，退格精度不回退。
  4. **i18n 骨架**：`i18n/catalog.go` 10 个 hint key（`hintPrefix` + 4 引导 + 5 草稿状态）× 2 locale（zh 现状原文逐字保留、en 译文含 `%s` 槽位），另有 notice 前缀查表；`pageController.locale` 默认 zh + 测试注入点，`writeHint`/`hintPrefix`/`noticePrefix` 一律查表；`RenderScreenWithLocale` 显式承载 locale，原 `RenderScreen` 委托 zh 保持签名与逐字节行为不变。**locale 绝不进 `viewmodel.Screen`，禁止渲染后字节替换文案**——以守 ADR-019 信任域（服务端固定文案走 `RenderSystem`，用户文本走 `RenderUser`/`SanitizeText`）。
- 理由 / 代价 / 边界：
  - 理由：物理行数由单一纯函数口径计算，同时修掉"显示层缺模型"与"硬编码光标跳行"两个根因，并为多语言预留通道；i18n 只在服务端固定文案通道查表，不碰用例、仓储、viewmodel 契约与键位。
  - 代价 / 边界：
    1. `width <= 0`（宽度未知）退化为旧行为（不折行、`Rows=1`、`Pending=false`），调用方不得假定已有物理行模型。
    2. en 文案在极窄终端仍会物理折行——这是终端软折行行为（程序层不折行），与中文同构，不视为代码缺陷。
    3. **全量 UI 翻译不在本期范围**（D3）：仅 hint 与 notice 前缀双语化，命令输出等后续版本再做。
    4. B7 超宽行合并后，上方物理行的空格残留于光标下方但**无内容**；终端无删行序列，按发版红线不以填充空格绕行，故只消除"错位散落到正文"、不消除该无内容残留。
    5. 无 schema 迁移、无新依赖、无新环境变量、无键位改动；`TestHintSingleCRLF` 保持不参数化。
- 验证：W8 `wrap_test.go` 纯函数单测（中/英/emoji 行数、pending wrap、宽字符整字折）；W9 既有 `RenderFrame` 测试与 archtest 绿；W10 虚拟网格 TC-04（v0.8.3 退格精度不回退）/ TC-05（B7 超宽行合并后网格无上方物理行错位残留）；W11-W14 双语参数化（TC-03 40 vs 200 列正文段字节与文案长度无关、TC-06 前缀 zh `[提示]`/`[错误]` → en `[Info]`/`[Error]`，信任域白名单不变）；`TestHintSingleCRLF` 不参数化；两闭环专项 + archtest + 四门禁全绿；测试函数 1041 → 1067。
- 关联：`ADR-019`（信任域与零回显）、`ADR-026`（回显净化）、`ADR-027`（原地重绘 / ANSI 白名单）、`ADR-034`（多行编辑）、`ADR-067`（patch 段 cols 重建历史）、`ADR-068`、`ADR-069`（方案 B 止损）。

> 验证结果（2026-11-24）：四门禁 + 两条闭环专项 + archtest + 虚拟网格 TC-01~TC-05 + 双语参数化全部通过；测试函数 1041 → 1067（ADR-069 落地阶段 1041 → 1056，ADR-070 落地后累计 1067）。发版动作（commit / tag / Release / 部署）待用户单独授权，按 `AGENTS.md` §5 执行。

