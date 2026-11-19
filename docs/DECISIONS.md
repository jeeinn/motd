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
