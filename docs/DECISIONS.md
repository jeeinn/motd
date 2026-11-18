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
