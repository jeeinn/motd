# MVP 交付与复盘记录

> 状态：**已交付**（2026-11-18 20:50，V 先生委托「审查 → 核对 → 最小优化 → 实现 → 验证 → 交付」）
> 依据：`docs/design/命令行论坛整体设计方案-评审稿v0.2.md`（**评审稿是快照，不回改**）、`docs/P0-PLAN.md` D1–D7
> 定位：本文件是本次交付的**单一复盘入口**，记录需求调整、技术取舍、重要决策、完成清单、验证结果、已知限制与变更记录。
> 与既有文档的分工：`DECISIONS.md` 存技术结论（ADR）、`DEVIATIONS.md` 存对评审稿的偏差、`OPS.md` 存运维手册。本文件不重复双写，只在必要处交叉引用。

---

## 0. 结论先行

| 项 | 结论 |
| --- | --- |
| 交付物 | 可运行的单进程 Go 二进制 `motd.exe`（本机）/ `bin/motd-linux-amd64`（交叉编译），启动即建库迁移并监听 SSH `:2222` 与 `/healthz` |
| 审查发现的关键阻塞 | **P0｜论坛用例与终端输出从未接线**：`rt.Service`、`Screen*`、`safe.RenderUser` 零业务调用点，`ssh_handler.go` 首屏只写「页面将在 P0-24 接入」，`/top /open /new /reply` 全返回占位文案。**这是「改前不能算可运行 MVP」的唯一根因** |
| 本次最小改动 | 新增终端 Renderer + 每会话页面控制器，把**已有零件**接进同一分发链；`go.mod` 一行未改、协议层与命令表结构未变 |
| 验收 | D1 / D3 / D4 / D5 / D7 **达成**；D6 **部分**（正常关闭重启已验，硬杀未演练）；**D2 必须两级口径，不得一句「已达成」概括**。2026-11-18 21:44–21:49 本机实测后已订正 D2 口径并修复一个 P0 缺陷，见 **§9** |
| 回归 | `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全绿（0 FAIL，全项目 456 个测试函数，本次新增 13 个） |
| 最重要口径风险 | 离线 `httptest` 假 GitHub 端点全绿 ≠ 真实 OAuth 可用。Q7「已拿到 client_id」只证明凭据就绪，不证明链路已验证 |

---

## 1. 审查结论（改动前）

### 1.1 已完成内容（代码核实，非文档自称）

- **M0 地基**（P0-01~07）：配置/日志/优雅停机、`/healthz`、archtest 依赖守卫、SQLite 双池 + PRAGMA 自检、版本化迁移（3 个迁移）、写入队列（容量 256 / 3s 超时 / 事务提交后才回执）、备份恢复与空库演练。
- **M1 业务内核**（P0-08~13）：仓储端口在使用方、5 个版块种子、论坛核心用例（限流/权限/校验/keyset 分页）、统一 ViewModel、领域事件与插件骨架、纯内存在线注册表。
- **M3 第三波**（P0-21~23）：`x/crypto/ssh` 入口（仅公钥、禁 root、2222、host key、PTY、100 连接门禁）、输入归一化与命令注册表、分层安全渲染（22 条恶意样本、唯一 SGR 常量表）。
- **M2 认证**（P0-14~19）：公钥三状态解析、GitHub Device Flow 客户端、设备码状态机、账号创建/登录事务、公钥绑定与软吊销、最小交互层登录与会话升级。
- git 状态：最新提交 `4f5102b`（P0-19），工作区干净。

### 1.2 关键阻塞

| 级别 | 阻塞 | 证据 |
| --- | --- | --- |
| P0 | 用例与页面未接线，D1/D4 不成立 | `cmd/server/ssh_handler.go` 首屏固定文本；`formatResult` 对 `/open /new /reply` 回「P0-24 接入业务动作」；`presentation/terminal/` 只有 `doc.go` 与 `safe/`，无页面文件 |
| P0 | 默认键位未实现 | `applyKey` 非编辑态只认 `?` `/` `Esc` `Ctrl-C/D` `Paste`；帮助表里写的 `j/k/h/l/q/r` 全是文案 |
| P0 | 闭环证据缺失 | 无 `test/e2e`、无 `deploy/`、无 `scripts/load.sh`；D1/D4 无法一条命令验证 |
| P1 | 运维收尾空缺 | `auth_events` 只建表无写入；100 连接压测、systemd unit、带数据恢复演练、`-race` 均未做 |

### 1.3 风险

- **R19 已知边界**：公钥已属他人时，账号已建但会话保持访客态，需换钥重连（设计取舍，非缺陷）。
- **零回显是正确安全选择**，但它意味着改前的 D5「提交→他人终端」链路根本不存在，无法端到端验证；接线后已补链路断言。
- `OPS.md:148` 判「D7 达成」与 `:152` 待办「不能只靠空库演练结案」自相矛盾，易误判结案。
- 文档滞后：`README.md` 当前状态停在 P0-13，落后 6 个提交；配置表只列 5 个变量，实际已实现 `MOTD_SSH_*` 与 `MOTD_GITHUB_CLIENT_ID`。

### 1.4 需求口径冲突（设计稿内部）

| 位置 | 冲突 |
| --- | --- |
| §3.1 / P0-25 vs `command/registry.go` Bindings | 前者 `q` = 返回上一层，后者写「q 退出（等价 /quit）」 |
| §4.9 / §7 vs `P0-PLAN` P0-20 | 安全日志与多维度限流在评审稿是 **P1**，计划却列入 P0 |
| §6 / §1.2 vs P0-27、P0-28 | 压测列 P3、容量「不作为 P0 验收标准」；部署属形态非 DoD，计划却把它们排在 M4 闭环里 |
| D2 判定栏 / P0-19 验收 / ADR-025 已知边界 4 | 一处写「假端点集成测试」、一处写「D2 手工跑通（已由集成测试覆盖）」、一处写「真实 client_id 联调待 VPS」，三处口径不一致 |

---

## 2. 需求范围核对与调整（用户授权的最小优化）

判定标准严格采用 `P0-PLAN.md §1.1` 唯一一条：**不做它，D1–D7 是否还能全部满足**。不能，才进 MVP。

| # | 调整 | 依据 | 影响 | 验证结果 |
| --- | --- | --- | --- | --- |
| A1 | **P0-20（auth_events 安全日志 / 认证类限流）移出 MVP，归 P1** | 评审稿 §4.9 把多维度限流、安全日志、`auth_events` 列 P1；§7「OAuth 重放→审计日志」亦为 P1；§1.1 唯一标准。§7 的 P0 红线「连接数与消息频率双限流」**已由 P0-21（100 连接门禁）+ P0-10（Q6 发帖限流）覆盖**，二者不是同一件事 | 不做 D1–D7 逐条仍满足；避免引入内存限流计数与审计写入等 P1 复杂度；代码零改动（该项未开工），`auth_events` 表保留不写 | 全量 `go test ./...` 保持绿；真实链路冒烟里 Q6 限流**确实生效**（见 §5.4） |
| A2 | **P0-24 由 7 页收敛为 4 页**（版块列表 / 主题列表 / 主题详情 / 发帖·回复编辑器） | `P0-PLAN §5` 自己写明「把 P0-24 页面数砍到 4 个（砍帮助浮层与搜索占位）」是合法压缩方向；`DEVIATIONS.md` R18/R20：登录引导已由 P0-19 最小交互层覆盖、只替换呈现层；§1.1 搜索属 P1 | 独立登录引导页、帮助浮层、搜索占位不做；帮助由 `Registry.Help()` 生成（仍是唯一事实来源），搜索明确列为 P1 | `TestPageControllerD1ShowsNodeList` / `Navigation` / `D4TwoUsersSeeEachOthersPosts` 与真实 SSH 冒烟全部通过 |
| A3 | **P0-27 容量压测降为非阻塞质量活动** | 评审稿 §6 把性能压测列 P3；§1.2「初期运行容量由压测确定，**不作为 P0 验收标准**」；§6「先验证核心闭环」 | 1000 在线是目标容量而非已验证能力；不写入任何验收承诺 | D1–D7 不依赖其结果；列为已知限制 |
| A4 | **P0-28 systemd 部署退出验收门禁，保留为交付附件** | 评审稿 §6 的 P0 清单无部署项、P3 才含「稳定运营」；`P0-PLAN §8` 第 14 条本就把逐步部署指引列为「P0 全部完成后**额外交付**」 | 不做 unit / cron / 22→2222 转发，D6/D7 仍可在本机闭环 | 本机从零启动实测通过（§5.2）；VPS 部署列为已知限制 |
| A5 | **D2 拆成两级口径，禁止合并表述** | D2 判定栏写「集成测试（假 GitHub 端点）」；`P0-PLAN §8` 第 8/10 条明确「M2 开发期一律用 `httptest` 假端点」「真实联调在 VPS」；`ADR-025` 已知边界 4 自认待真实联调 | 交付说明必须区分「离线链路通过」与「真实 client_id 联调通过」；避免把假端点全绿偷换成真实 OAuth 可用 | MVP 级：`TestLoginFlowD2EndToEnd`（真实 SQLite + 假端点）既有通过；上线前门禁：**未做**，见 §6 |
| A6 | **`q` 键位口径按 §3.1 裁定为「返回上一层」，并修正帮助文案** | 评审稿 §3.1「`q` 返回」+ P0-25「任意页面都能回退到版块列表」优先于帮助表文案；帮助表是唯一事实来源，行为与文案不符属缺陷 | `registry.go` 键位表 2 行文案改动；退出统一走 `/quit` 或 `Ctrl-C/D`；不改命令数量与解析逻辑 | `registry_test.go` 6 项全通过；`TestPageControllerNavigation` 验证 `q/h` 逐层回退 |
| A7 | **同步 `README.md` 当前状态与配置说明** | 审查发现文档滞后 6 个提交；`README` 是读者第一入口，滞后会让 M2/M3/MVP 被误判为未开工 | 纯文档同步，无功能影响 | 人工核对 `git log` 与 `config.Load` |
| A8 | **收口登录日志 `replace_at=P0-24`** | P0-24 页面已落地，继续把一个已完成编号写成「未来待办」会误导复盘；实际未替换的只是 `/login` 呈现层 | 日志改为 `pending=login_view_replacement`；`login_view.go` / `login_flow.go` 注释同步说明 | 从零启动实测日志已验证新字段（§5.2） |

> A1/A3/A4 是**把任务层相对评审稿 §6 的范围扩张退回**，D1–D7 本身是 §6 验收句的合理可判定拆解，不算范围漂移。

---

## 3. 技术取舍与重要决策

| # | 决策 | 理由 | 代价 / 边界 |
| --- | --- | --- | --- |
| M1 | **终端 Renderer 落 `internal/presentation/terminal/view.go`，页面状态机落 `cmd/server/page_controller.go`** | `presentation → application` 是 §2.2 合法方向，`presentation` 不得 import `transport`；把「ViewModel→ANSI」做成纯函数可独立回归，把「用例编排」放在唯一能同时看到三层的 `cmd` 装配层（沿用 `login_flow.go` 既有风格） | 两处新增文件；不新增包、不改 `go.mod` |
| M2 | **页面状态每会话独立** | `inputHandler` 是进程级单例，光标 / 当前节点 / 编辑缓冲绝不能放单例上（原则二：内存状态只是运行时状态） | `Serve` 里每连接 `newPageController()`；共享依赖只读 |
| M3 | **编辑态继续「零回显」**（**本条已被 §10 / ADR-026 修订，保留原文供追溯**） | P0-22 / ADR-018 原约束：用户文本未经净化不得进输出流；直接 `Write(m.Raw)` 会开渲染旁路 | 当时导致输入看不到自己打了什么；修订后改为「净化后回显、Secret 字段隐藏」 |
| M4 | **信任域按数据来源切分**：编译期常量与本地数值走 `safe.RenderSystem`；**一切 DB / 用户文本**（标题、正文、slug、Notice.Message）走 `safe.RenderUser` | §3.5「服务端生成的 ANSI 与用户提交的 ANSI 走不同函数」；DB 文本也属于外部数据，不进更严格但会拒绝控制字符的系统路径，而是净化后展示 | 两类文本绝不拼进同一次渲染调用；渲染器只产出纯 SGR |
| M5 | **Actor = `ActorFromPrincipal(Principal, DB status)`** | 权限最终判定仍由写用例在事务内二次复核（P0-10 / ADR-020），页面层不缓存账号状态、不做最终授权 | 多一次读池查询；失败关闭（`StatusReader` 不可用则拒绝发帖） |
| M6 | **不替换 P0-19 的 `/login` 呈现层** | R18/R20 的替换点是独立工作，MVP 只需 D2 业务链路；一并改会动到认证链路与 6 条 archtest 守卫，超出最小改动 | 登录屏仍是固定文本行，与四页论坛的呈现形态不一致，列为已知限制 |
| M7 | **零新增依赖** | 用户约束「非必要不引入新依赖」；`golang.org/x/crypto/ssh` 与 `modernc.org/sqlite` 已足够，`go.mod` 一行未改 | — |
| M8 | **E2E 用真实 `ssh_keys` 绑定 + 真实 `auth.NewResolver`，不用 fake Principal** | D3 要求「断开重连不再要求登录」，用 fake 身份解析只能测页面，测不到真实链路 | 测试需预置两把 ed25519 公钥；换来 D3 的真实证据 |
| M9 | **保留 Q6 限流，不为冒烟脚本开后门** | 评审稿已拍板；真实链路首次冒烟确实被限流拒了一次，改由第二个账号回复（同时更贴合 D4「在他人主题下回复」） | 记录为踩坑，见 §5.4 |

---

## 4. 完成清单

### 4.1 D1–D7 逐条判定

| # | 判定标准 | 状态 | 证据 |
| --- | --- | --- | --- |
| D1 | 陌生机 `ssh` 进 TUI，浏览版块与主题列表 | ✅ | `TestMVPSSHClosedLoop` 真实 `gossh.Dial` 首屏含 5 个版块；`TestPageControllerD1ShowsNodeList` |
| D2 | `/login github` 走完 Device Flow、会话升级、公钥自动绑定 | ⚠️ **待复测** | MVP 级：`TestLoginFlowD2EndToEnd`（真实 SQLite + 假 GitHub 端点）通过。**本机实测（§9）已跑通真实 Device Flow 授权并拿到 `user_code`、出现「授权成功，正在建立账号」，证明 `client_id` 与出站链路可用**；但当时会话升级因 Reap 缺陷失败（「登录已完成，但本会话未能升级为登录态」）。该缺陷已修，**完整闭环需在修复后由用户复测确认** |
| D3 | 断开重连免登录、直接具备写权限 | ✅ | E2E 预置真实 `ssh_keys` 绑定 → `auth.NewResolver(sqlite.NewSSHKeyLookup)` 解析指纹 → 重连状态栏「已登录」→ `/new` 直接进入编辑态 |
| D4 | 发主题、在他人主题下回复、另一账号可见 | ✅ | E2E：账号 1 发帖、账号 2 回复、账号 1 刷新详情可见；`TestPageControllerD4TwoUsersSeeEachOthersPosts` 双 fake 会话共享真实 Service |
| D5 | 恶意 `ESC/CSI/OSC 52/\r`/双向文本不影响他人终端 | ✅ | 既有 22 条样本（P0-23）+ 新增 Renderer 5 组（含 40 列窄屏、详情屏）+ `TestPageControllerHostilePostIsSafeForOtherSession`（DB 读出 → 另一会话渲染 → 只含纯 SGR） |
| D6 | 重启后帖子/账号/绑定仍在；`kill -9` 后 WAL 不丢已提交事务 | ⚠️ **部分** | `TestMVPRestartAndBackupWithRealData`：带真实数据关库 → 重开 → 迁移 0 版本、threads/posts/最新正文不变。**硬杀（`kill -9`）未演练**，见 §6 |
| D7 | 从备份恢复到空目录、行数内容一致、步骤有文档且实际执行过 | ✅ | 同一测试：`sqlite.Backup → VerifyBackup → Restore` 全链路；备份验收 `integrity=ok`、行数一致；恢复库 `schema_migrations=3`、最新正文一致。`OPS.md §4` 有步骤文档 |

### 4.2 任务状态

| 里程碑 | 任务 | 状态 |
| --- | --- | --- |
| M0 地基 | P0-01 ~ 07 | ✅ 既有完成 |
| M1 领域与用例 | P0-08 ~ 13 | ✅ 既有完成 |
| M2 认证 | P0-14 ~ 19 | ✅ 既有完成；P0-20 按 A1 移出 MVP |
| M3 终端 | P0-21 ~ 23 | ✅ 既有完成 |
| M3 终端（本次） | P0-24 四页 / P0-25 键位收尾 | ✅ 本次完成 |
| M4 闭环 | P0-26 E2E 冒烟 | ✅ 本次完成（真实 SSH 客户端） |
| M4 闭环 | P0-27 压测 / P0-28 部署 | ⬜ 按 A3/A4 退出验收门禁，列为已知限制 |

---

## 5. 验证结果

### 5.1 命令与结果

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test -count=1 ./...` | 全绿，**0 FAIL**；全项目 456 个测试函数 |
| 本次新增测试 | 13 个：`view_test.go` 5 / `page_controller_test.go` 6 / `mvp_e2e_test.go` 1 / `mvp_persistence_test.go` 1 |
| `TestMVPSSHClosedLoop` | PASS（0.18s，真实 SSH 客户端 + 真实 SQLite + 真实身份解析） |
| `TestMVPRestartAndBackupWithRealData` | PASS（D6 重启 + D7 带数据恢复） |
| archtest 全套守卫 | PASS（依赖方向、ViewModel 无渲染概念、唯一 SGR 常量表、令牌不落库等未因接线被破坏） |

### 5.2 从零启动实测（2026-11-18 20:48）

命令（空数据目录，环境变量覆盖端口）：

```bash
MOTD_DATA_DIR=./data/mvp-smoke \
MOTD_HTTP_ADDR=127.0.0.1:18080 \
MOTD_SSH_ADDR=127.0.0.1:22222 \
./motd.exe
```

- `GET /healthz` → `ok dev queue=0/256 online=0 active=0 commits=0`
- 启动日志要点：`migrations_applied=3 applied_versions="[1 2 3]"`、`论坛用例就绪 nodes=5 plugins=1 post_created_subscribers=1`、`身份解析就绪 mode=ssh_keys`、`SSH host key 就绪`、`终端页面就绪 mode=viewmodel_terminal_renderer screens=nodes|threads|detail|composer renderer=safe_sgr_only`、`登录交互就绪 ... pending=login_view_replacement`、`SSH 监听 addr=127.0.0.1:22222 max_conns=100`
- 空目录自动生成 `motd.db`（WAL/shm）、`ssh_host_ed25519_key`，无 panic。

### 5.3 交付产物

| 文件 | 用途 | sha256 |
| --- | --- | --- |
| `motd.exe` | 本机 Windows 可直接运行 | `553077c181c44d8742319d650d1aac6392d93835836e011a0ead6d8f35c4357e` |
| `bin/motd-linux-amd64` | 交叉编译 `GOOS=linux GOARCH=amd64`，供 VPS/systemd 部署（版本 `mvp-1.0.0`） | `13459d92b9862867a2518cc95adf0d517afe0345bab17879fb6a3cdce445ec48` |
| `bin/motdctl.exe` | 备份 / 校验 / 恢复工具 | `952ee37343a3325d09f15c06bd909b305f2d71c96a676fa5a10904fe97e0228a` |

> 校验和为 **§11 收官优化后重新编译**的版本（22:57）。从零启动实测：`data/mvp-final`，`migrations_applied=[1 2 3]`、`/healthz → ok dev queue=0/256 online=0 active=0 commits=0`、无 panic，并以系统 OpenSSH 客户端验证原地重绘字节流（§11.3）。

### 5.4 真实链路踩坑记录（有复盘价值）

首次 E2E 冒烟让**同一个账号**连续发主题 + `/reply`，被 Q6 限流拒绝，状态栏输出：
`写入事务失败（不可重试）: 发帖过于频繁（60 秒内只能发 1 帖），请 1m0s 后重试`。

- 判定：这是**设计稿规则正确生效**，不是缺陷，因此不改限流、不加测试后门。
- 修法：把 `/reply` 移到第二个账号（另一把已绑定公钥），既避开限流，又更贴合 D4「**在他人主题下**回复、另一个账号可见」。
- 附带验证：`NoticeFromError` → ViewModel → `RenderUser` 的错误提示链路在真实终端上安全可用（不含用户原文、只含纯 SGR）。

---

## 6. 已知限制

| 级别 | 限制 | 归属 / 说明 |
| --- | --- | --- |
| **P0（待复测）** | **完整 D2 闭环待用户复测**：本机实测已完成真实 Device Flow 授权（§9），但当时会话升级失败；修复后需重跑一次 `/login github` 并确认状态栏变「已登录」、可直接 `/new`。**VPS 上的 `client_id` 注入与出站放行仍未验证** | 不要再把「真实 GitHub 授权已跑通」写成「完整登录闭环已达成」；也不要再把「完全未联调」当作现状 |
| P1 | `auth_events` 无写入、按 IP/用户/邮箱的认证类限流未实现 | A1 裁定归 P1；§7 的 P0 带宽红线已由 P0-10 + P0-21 覆盖 |
| P1 | `/login` 屏仍是 P0-19 固定文本行，与四页论坛呈现形态不一致 | R18/R20 独立替换点；`loginPresenter` / `loginFlow.out` / `pollTimer` 待替换，业务链路零改动 |
| P1 | 编辑器仍是单行提交；多行正文需一次粘贴，且粘贴含换行后退格不能跨行回到上一行 | 回显已由 §10 / ADR-026 补齐；逐字符 `\b` 擦除的边界由 P1 多行编辑器解决 |
| P1 | 作者名未展示、`karma` 恒 0 / 等级恒 L1、无搜索、点赞、成就落库、浏览器 TUI / Frame 协议 | R11 未补仓储投影；Q5 分档已实现但 P0 无计分来源，刻意不臆造语义 |
| P1 | 原地重绘仍逐行重发整屏字节（视觉上只有一屏），未做差异行优化 | 刷屏已由 **§11 / ADR-027** 解决；差异行优化属 §3.3 Frame 增量（P1 浏览器端），按 FF09 不提前引入 |
| P2 | D6 的 `kill -9` 硬杀未演练；`-race` 因本机无 gcc/CGO 未启用 | WAL 提交语义由 `write.go`（提交后才回执）+ 带数据备份恢复演练覆盖；VPS 部署演练时补做 |
| P2 | 100 连接压测、目标 1000 在线未验证 | A3，评审稿 §1.2 明确不作 P0 验收 |
| P2 | systemd unit / cron / 22→2222 转发 / 异地备份 / RTO-RPO 定期演练 / Online Backup API | A4，交付附件或 P3；`OPS.md §9` 已有清单骨架 |
| P2 | 主题列表分页只做 `n` 下一页 / `p` 回第一页，无完整历史游标栈 | MVP 最小集 |

---

## 7. 变更记录

### 7.1 新增文件

| 文件 | 作用 |
| --- | --- |
| `internal/presentation/terminal/view.go` | 终端 Renderer：`viewmodel.Screen` → 受控 ANSI（LF 输出），信任域分离 |
| `internal/presentation/terminal/view_test.go` | 5 个测试：语义内容、非法 ViewModel 失败关闭、恶意样本、40 列折行不截断、详情屏 |
| `cmd/server/page_controller.go` | 每会话页面状态机：4 页、导航、编辑器、命令分发、Actor 构造 |
| `cmd/server/page_controller_test.go` | 6 个测试：D1、导航、D4 双账号、访客只读、`/open` 零回显、D5 跨会话链路 |
| `cmd/server/mvp_e2e_test.go` | 真实 SSH 客户端冒烟（D1/D3/D4），真实 `ssh_keys` 绑定与真实 Resolver |
| `cmd/server/mvp_persistence_test.go` | 带真实数据的 D6 重启持久化 + D7 备份恢复演练 |
| `docs/MVP-DELIVERY.md` | 本文件 |

### 7.2 修改文件

| 文件 | 变更摘要 |
| --- | --- |
| `cmd/server/ssh_handler.go` | 注入 `forum.Service` / `StatusReader` / `presence.Registry`；`Serve` 首屏改渲染版块列表、每会话建 `pageController`；`handle` 分发接业务命令与导航键；`applyKey` 补 `j/k/h/l/q/r/n/p`、方向键与 Enter；`formatResult` 去掉 P0-24 占位文案（**解析错误零回显行为不变**） |
| `cmd/server/main.go` | 装配 `withForum(...)`；新增「终端页面就绪」日志；登录日志口径由 `replace_at=P0-24` 改为 `pending=login_view_replacement`；顶部注释同步 |
| `cmd/server/login_view.go` / `login_flow.go` | 仅注释：说明页面控制器已落地、`/login` 呈现仍是独立替换点 |
| `internal/application/command/registry.go` | 键位表 2 行文案修正（`q` = 返回上一层、`Ctrl-C/D` = 退出）并补 `n/p` 说明；**命令集与解析逻辑零改动** |
| `README.md` | 「当前状态」与配置/快速开始同步到 MVP 事实（见 §2 A7） |
| `docs/P0-PLAN.md` | 头部状态追加 MVP 收尾与本文件交叉引用（滚动日志不回改） |

> **§9 的交付后反馈修复**另改动了 `session.go` / `registry.go` / `ssh_handler.go` / `auth/login_flow.go` / `cmd/server/login_flow.go`，详见 §9.4，不在此表重复。

> **未改动**：`go.mod` / `go.sum`、所有 migration SQL、`transport/ssh` 协议语义（仅新增连接级心跳）、`application/forum` 用例、`application/viewmodel` 类型、全部既有 archtest 守卫。

---

## 8. 复盘要点与下一步

1. **最值得保留的判断**：改前的「不能算可运行 MVP」不是缺功能，而是缺**一处接线**。已有 M0/M1/M2/M3 的业务内核、ViewModel、安全渲染器、命令表都是可用的；本次只把它们接起来，没有重写任何一层。以后遇到「进度看起来很满但产品跑不起来」，先找**零调用点的零件**。
2. **范围守门很有价值**：`P0-PLAN §1.1` 那条唯一标准，一次性把 P0-20 / P0-27 / P0-28 从门禁里摘掉，避免在交付末期引入限流计数、systemd 与压测脚本等新复杂度。
3. **诚实口径比漂亮结论重要**：D2 必须两级、D6 必须标「部分」。把假端点全绿写成「真实登录可用」会让整份 D1–D7 结论失去可信度。
4. **下一步（明确的复访钩子）**：VPS 注入 `MOTD_GITHUB_CLIENT_ID` 后回来做两件事 —— ① 手工跑一次真实 `/login github` 结清 D2 第二级；② 在带数据的库上做一次 `kill -9` + WAL 恢复演练，补 D6 剩余部分。之后按 `OPS.md §9` 完成 systemd 部署自检，再决定 P1 的登录呈现层替换与搜索/投票。

---

## 9. 本机实测反馈与修复（2026-11-18 21:44–21:55）

> 来源：V 先生在 PowerShell + OpenSSH 客户端的两轮实测输出。交付后的真实反馈闭环，按「现象 → 定性 → 根因 → 修改 → 验证」留档。

### 9.1 现象与定性

| # | 观察 | 定性 | 处置 |
| --- | --- | --- | --- |
| Q1 | 按 `j/k` 每次都在窗口追加输出一整屏 | **当时的实现预期**（**结论已被 §11 / ADR-027 修订，保留原文供追溯**） | 当时页面层是「追加输出」而非原地重绘；为守住 §3.5「只输出纯 SGR」未引入光标/清屏 ANSI，焦点只能靠整屏重绘表达。当时判断「改成原地重绘必须扩展白名单与守卫，属独立工作，本次不改」——后续由 §10.3 方案对比 → V 先生选 A → §11 实施 |
| Q2 | 按 `/` 后输入命令「没反应」 | **UX 缺陷 + 当时的安全硬约束**（**② 的结论已被 §10 / ADR-026 修订，保留原文供追溯**） | ① `cl.runes` 已预置 `/`，提示语却写「以 / 开头」，用户补打一个 `/` 得到 `//login` → 未知命令；② **P0-22 零回显**：用户文本在净化前一律不写回终端，所以看不到自己输入的字符。当时判断「零回显不改，但提示必须说清楚」 |
| Q3 | `/login` 不带参数只回「参数个数不符」 | **可发现性缺陷** | 原 `MinArgs=1`，Parse 直接 `ErrArgCount`，用户不知道要补 `github` |
| Q4 | 补全 `/login github` 后真实 Device Flow 走通 | **口径订正** | 输出出现 `user_code DC23-363F`、`https://github.com/login/device`、`授权成功，正在建立账号…`，证明本机 `MOTD_GITHUB_CLIENT_ID` 已生效、GitHub 出站可用。此前「真实 OAuth 完全未联调」的标注已按 §4.1 / §6 订正 |
| Q5 | `登录已完成，但本会话未能升级为登录态，请重新连接。` + `online=0 active=0` | **P0 真缺陷（本次修复）** | 见 9.2 |

### 9.2 Q5 根因（一个根因，两个症状）

```text
Device Flow 等待授权 > 20s 且期间用户无输入
  → main.go 每 10s 调 presence.Reap()，删掉 LastSeenAt 超 20s 的会话
  → 但 SSH 连接仍活着（Reap 只动内存注册表，不关连接）
  → 症状 A：状态栏与 /healthz 的 online/active 永久归 0，SetState 全部失败
  → 症状 B：finish() 的 UpgradeToUser → Registry.AttachUser 找不到会话
            → 账号已建、公钥已绑，但会话仍是访客
```

原先只靠 `Session.Read` 刷新心跳，等于把「在线」误绑在「用户有没有按键」上，对 OAuth 等待场景必然误判。

### 9.3 一次走错的路（留档）

最初尝试在 `UpgradeToUser` 里捕获 `ErrUnknownSession` 后自动补登记再升级。`go test ./...` 立即暴露回归：
`TestUpgradeToUserFailsClosedWhenPresenceUnknown` 失败 —— **ADR-024 第 6 条要求「presence 未登记时升级必须失败关闭」**，放宽它会打开账号接管面。
已回退，改为**治本**：让活连接自己维持心跳，安全门一动不动。这条回归是本次最有价值的保护。

### 9.4 实际改动（最小范围）

| 文件 | 改动 |
| --- | --- |
| `internal/transport/ssh/session.go` | 新增连接级心跳 `sessionHeartbeatInterval=10s`（取 20s 超时的一半，纯内存 `Touch`、不写 SQLite），`Close` 停止 ticker；`Read` 里对已被 Reap 清掉的活连接做一次防御性补登记（`reRegisterSession`）。**`UpgradeToUser` 的失败关闭逻辑保持原样** |
| `internal/application/command/registry.go` | `/login` 的 `MinArgs: 1 → 0`，让它进入 provider 白名单分支而非通用参数错误 |
| `cmd/server/ssh_handler.go` | `commandEntryHint` 明确「/ 已自动填入，无需再打一次」与「输入过程不回显（安全约束）」 |
| `internal/application/auth/login_flow.go`、`cmd/server/login_flow.go` | 新增 `LoginTextClientIDMissing` 专用文案与纯函数 `loginStartFailureText(err)`：未配 `MOTD_GITHUB_CLIENT_ID` 时给可操作提示，其它错误仍回通用文案，**绝不回显 `err.Error()`**（依据 OPS §3） |
| 新增测试 | `internal/transport/ssh/session_reap_test.go`（4 个）、`cmd/server/login_hint_test.go`（3 个）、`cmd/server/mvp_e2e_test.go` 追加 `TestMVPLoginCommandHintOnRealSSH` |

> 设计约束未动：`go.mod` / 迁移 SQL / `transport/ssh` 协议层 / `application/forum` 用例 / `application/viewmodel` / 全部既有 archtest 守卫。

### 9.5 验证结果

- `go build ./...` / `go vet ./...` / `go test -count=1 ./...` **全绿，0 FAIL**；`TestUpgradeToUserFailsClosedWhenPresenceUnknown` 仍通过（安全门未被放宽）。
- 关键回归：`TestHeartbeatKeepsLongOAuthWaitUpgradable`（注入时钟模拟 30s 无输入，Reap 不删活连接、升级成功）、`TestClosedSessionIsStillReaped`（断连僵尸仍被清理）、`TestMVPLoginCommandHintOnRealSSH`（真实 SSH 字节流：`/login` → 「仅支持 /login github」、`//login` → 未知命令）。
- 新二进制从零启动实测（21:54）：`migrations_applied=[1 2 3]`、`/healthz → ok dev queue=0/256 online=0 active=0 commits=0`、无 panic。
- 产物校验和已更新到 §5.3（linux 版本 `mvp-0.1.1`）。

### 9.6 仍待用户复测

修复在服务端，用户当时连的是旧进程。**重启 `motd.exe` 后**再跑一次 `/login github`，期望输出：
`授权成功，正在建立账号…` → `登录成功 / 账号已创建并登录` → 状态栏 `online=1 active=1 | 已登录`，随后可直接 `/new`。
若仍出现「未能升级为登录态」，把新的完整输出发回，按同一路径继续查。

---

## 10. 回显规则修订（2026-11-18 22:20）+ 刷屏方案待决

> 触发：V 先生本机实测反馈「正常操作必须有回显，零回显应只在输入登录密码时启用」「当前刷屏模式体验并不好」，并确认**发帖 / 回复已真实成功**。
> 顺序：按要求先提交留回退点，再改回显；刷屏只给方案对比，**本轮不擅自实施**。

### 10.1 先提交（已完成）

`3751fc0 feat(mvp): 接通终端页面闭环，交付可运行 MVP`（19 文件，+2410 / -86）。提交前核对 `git status` 与忽略项（`bin/` `data/` `motd.exe` `.workbuddy/` 均未入库），提交后工作区干净。

### 10.2 回显规则修订（已实施）

**规则变更**：全局零回显 → **按字段分级**。依据评审稿 §2.3 与 `viewmodel.Field.Secret`「具体回显字符由 Renderer 决定」；正式记录为 `DECISIONS.md ADR-026`（修订 ADR-018）与 `DEVIATIONS.md R21`（细化 §3.5 口径）。ADR-018 与 §9 的原文**保留供追溯**，不回改。

| 项 | 修订前 | 修订后 |
| --- | --- | --- |
| 斜杠命令 / 标题 / 正文输入 | 完全不回显 | 经 `safe.SanitizeText` 净化后逐字符回显 |
| 退格 | 无 | 服务端 `\b \b`，宽字符按 `safe.DisplayWidth` 擦 N 列 |
| 提交 / 取消 | 无 | 服务端 `\r\n` |
| 密码类 Secret 字段（P1 邮箱登录） | 无 | `setSecret(true)` → 只回显 `*` |
| `inv.Args` / 指纹 / 公钥 / `err.Error()` / 凭据 / `device_code` / `access_token` | 不回显 | **仍不回显** |
| `safe/ansi.go` SGR 白名单与全部 archtest 守卫 | — | **一字未改** |

**实现**（新增 `cmd/server/line_echo.go`，每会话一份，挂在 `pageController` 上）：
- `lineEcho` 维护与编辑器 `runes` 一一对应的列宽栈；`applyKey` 仍是纯状态机（不碰 IO），回显在调用侧按前后长度推导是否真删除。
- `echoCommandKey` 对 `actionCommandStart` 不直接回显——必须**先输出提示再 `preloadedSlash()`**，否则提示会把用户输入行截断（实测发现，已加回归）。
- 超长截断（`clampEditor`）时被丢弃的字符不回显，保证列宽栈与缓冲区严格对齐。
- 不触碰 `safe/ansi.go`：回显走 `SanitizeText` 纯文本路径，编辑控制是服务端常量，符合 §3.5 两条信任路径。

**验证**：新增 `line_echo_test.go` 5 项（普通 / 宽字符退格、恶意字节净化、Secret 掩码、粘贴与换行 reset、预置 `/` 顺序）；`mvp_e2e_test.go` 补 `//login`、标题、正文的**真实 SSH 回显断言**；`login_hint_test.go` 改为断言提示语**不再**宣称「不回显」。全量 `go build` / `go vet` / `go test -count=1 ./...` 通过（**0 FAIL，469 个测试函数**）。

**已知限制**：逐字符 `\b` 擦除依赖终端 VT 兼容；粘贴含换行的多行文本后退格不能跨行回到上一行（P1 多行编辑器解决）。

### 10.3 刷屏问题：方案对比（**V 先生已选 A，实施与验收见 §11**）

现状根因：`pageController.move()` 调 `render()`，而 `render()` 是「追加输出」；为守住 §3.5「只输出纯 SGR」没有光标 / 清屏序列，焦点只能靠重绘整屏表达。

| 维度 | **方案 A（推荐做长期方案）：受控光标 ANSI + 原地重绘** | **方案 B（保守）：导航只输出单行焦点提示** |
| --- | --- | --- |
| 做法 | 在 `safe/ansi.go` 白名单增加服务端常量 `\x1b[<n>A`（上移）、`\x1b[K`（清行），渲染后移回屏首重绘；archtest 守卫扩为「SGR + 白名单光标序列」 | `j/k` 不再重绘整屏，只输出一行「焦点 → N. 标题」；整屏只在进入新页面 / 提交后输出 |
| 体验 | **最好**：标准 TUI，焦点在原列表行上移动，`>` 标记实时更新 | 中等：不刷屏，但看不到列表里 `>` 的移动，需适应 |
| 带宽 | 每屏输出大幅减少（1 Mbps 约束下更优） | 最小（每次一行） |
| 改动面 | 较大：`ansi.go`、`sgrOnly()` 守卫、Renderer、回归集；属安全渲染层变更 | 极小：仅 `pageController.move()` + 一个提示行 |
| 风险 | 新增非 SGR 服务端控制序列，必须补负向注入证明清屏 / OSC 仍被拒 | 零新增 ANSI；但焦点表达弱化 |
| 与评审稿 | 贴合 §3.3 Frame 增量方向（那是 P1 浏览器端，提前到 SSH 端有 FF09 过度设计风险） | 不触及设计稿任何边界 |

**团长建议**：只求「立刻不刷屏」选 **B**（改动小、零安全面变化）；若打算把终端 TUI 做成长期主界面，选 **A** 一次到位。**两案都不动 `application/forum` 与 ViewModel 结构**。

> **决策结果（2026-11-18 22:48）**：V 先生明确「终端 TUI 是长期主界面，选 A 一次到位，作为 MVP 最后收官优化」。已追加 `ADR-027` / `DEVIATIONS.md R22`，实施与验收见 **§11**。

### 10.4 本次实测确认的完成项

- **D2 / D3 / D4 真实机闭环成立**：状态栏 `已登录`、`/new` 发主题「我的 agent 测试」、`/reply` 回复「nnnb!!!」；Q6 限流在真实链路上正确生效并给出剩余秒数（`请 20s 后重试`）。
- **§9 的连接级心跳修复生效**：`online=1 active=1` 全程稳定，未再归零；「登录已完成但本会话未能升级」不再出现。

---

## 11. MVP 收官：终端 TUI 原地重绘（2026-11-18 22:48–23:00）

> 决策：V 先生在 §10.3 的 A/B 方案中选 **A**——「终端 TUI 是长期主界面，一次到位」。
> 定位：MVP 最后一次功能优化；不新增依赖、不动 forum 用例 / ViewModel / transport 协议层。

### 11.1 实施内容

| 层 | 改动 |
| --- | --- |
| `safe/ansi.go` | 白名单从「纯 SGR」扩为 SGR + EL `\x1b[K` + CUU `\x1b[%dA`；新增 `ClearToEOL()` / `CursorUp(n)`，`n` 只由服务端行数计算 |
| `presentation/terminal/frame.go`（新） | `RenderFrame(text, prevLines, width)` 纯函数：CUU 回屏首 → 每行 `\r` + EL + 内容 → 新屏更短时补齐清空 → 返回本屏行数；超宽自动降级追加 |
| `cmd/server/page_controller.go` | 每会话 `screenLines`；`render` 按连续性自动选重绘 / 追加；`writeFixed` 与 `invalidateScreen()` 作废连续性 |
| `cmd/server/ssh_handler.go` | 纯导航键保持连续性；命令提示、逐字符回显、编辑器、登录轮询、提交结果一律 invalidate |
| `internal/archtest/safe_test.go` | `sgrOnly()` → `allowedANSI()`；新增 3 条守卫（负向注入 / 用户路径无光标控制 / 唯一消费点） |
| 测试 | `frame_test.go` 5 项、`TestNavigationRedrawsInPlace`、真实 SSH `TestMVPNavigationRedrawsInPlace`；`cmdOnlySGR` → `cmdOnlyWhitelistedANSI` |

**明确禁止（负向注入逐条验证）**：ED 清屏 `2J`、CUP `H/f`、CUD `B`、DECSET 备用屏 `?1049h`、OSC 52 剪贴板、OSC 窗口标题、带参数 EL `2K`。用户内容路径 `RenderUser` / `SanitizeText` 不引用任何光标控制。

### 11.2 一次验证踩坑（留档）

首次用系统 OpenSSH 客户端验证时输出仍是追加整屏，一度以为降级条件被触发。**真实原因是我只跑了 `go build ./...`（写入构建缓存），没有 `go build -o motd.exe`，客户端连的是旧二进制**；补上重编译后原地重绘立即生效。

> 教训：**任何「跑真机」验证前必须确认被测二进制是刚编译的**。已把该检查纳入本轮验证步骤，并写入项目记忆。

### 11.3 验证结果

- `go build ./...` / `go vet ./...` / `go test -count=1 ./...` **0 FAIL（479 个测试函数）**。
- **真实 Windows OpenSSH 客户端**（`ssh -tt -p 22228 localhost`，管道喂 `jjj` + `/quit`）：

| 指标 | 实测 |
| --- | --- |
| CUU `\x1b[8A` | **3 次**（每次导航回屏首） |
| EL `\x1b[K` | **24 次**（3 × 8 行） |
| 版块列表渲染 | 4 次（首屏 + 3 次重绘），光标每次回屏首，**视觉只保留一屏** |
| 清屏 / OSC / 备用屏 | **0 次** |
| `/healthz` | `ok dev queue=0/256 online=0 active=0 commits=0` |
| 启动日志 | `migrations_applied=[1 2 3]`、终端页面就绪、SSH 监听，无 panic、无调试残留 |

- 产物 `mvp-1.0.0` 校验和见 §5.3。

### 11.4 已知限制

- 依赖终端 VT 兼容；旧版 Windows conhost 未启用 VT 时会退化为字面追加输出，**功能不受影响**。
- 仍逐行重发整屏字节，差异行优化（§3.3 Frame 增量）留 P1；按 FF09 不提前引入。
- `screenLines` 是每会话内存状态，重启归零（原则二）。

### 11.5 MVP 收官状态

| 项 | 状态 |
| --- | --- |
| D1 / D3 / D4 / D5 / D7 | ✅ 达成（真实机验证） |
| D2 | ✅ 真实 GitHub Device Flow 授权 + 发帖 / 回复闭环已在本机跑通；仅 VPS 凭据注入 / 出站放行未验证 |
| D6 | ⚠️ 部分：正常关闭重启 + 带数据备份恢复已验，`kill -9` 硬杀与 `-race` 未演练 |
| 回显 / 刷屏 | ✅ ADR-026 按字段分级回显、ADR-027 原地重绘，均已实测 |
| 后续项 | VPS systemd 部署自检、`kill -9` 演练、100 连接压测、`/login` 呈现层替换（R18/R20）、搜索 / 投票 / 浏览器 TUI（P1） |

> **文档**：`DECISIONS.md ADR-027`、`DEVIATIONS.md R22`、`P0-PLAN` 头部、`README`、`OPS` 同步；§9 Q1 / §10.3 的旧结论只标注「已修订」不删，便于事后复盘。
