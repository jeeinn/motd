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

## ADR-005 加强 `users.username` 的 CHECK 约束（偏离设计稿）

- 状态：已实现，**待 V 先生确认**
- 设计稿：`CHECK (length(username) BETWEEN 3 AND 20 AND username GLOB '[a-z0-9_-]*')`
- 实际实现：`CHECK (length(username) BETWEEN 3 AND 20 AND username NOT GLOB '*[^a-z0-9_-]*')`
- 理由：原写法里的 `*` 会吞掉任意后续字符，`GLOB '[a-z0-9_-]*'` 只校验**首字符**。`A_B`、`has space`、`中文`、`x!y` 都能通过，白名单形同虚设。改写后语义与设计意图（§4.5「用户名白名单」）一致。
- 代价：约束是建表时写入的，晚改要一次迁移；现在改几乎没有成本。回归测试 `TestUsernameConstraint` 锁定该行为。

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

## ADR-008 Go module path 暂用 `motd`

- 状态：临时决定，可低成本更改
- 结论：`go.mod` 中 `module motd`，内部 import 为 `motd/internal/...`。
- 理由：尚未确定代码托管地址；域名形式的 module path 一旦铺开，改名会动到每个文件。
- 代价：托管到 git.weixin.qq.com 或 GitHub 时必须全量替换 import；建议在 P0 结束前定下来（`P0-PLAN.md §6 Q2`）。
