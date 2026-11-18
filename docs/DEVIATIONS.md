# 执行偏差登记（对设计评审稿 v0.2）

> 定位：本文件是「**实际实现与 `docs/design/命令行论坛整体设计方案-评审稿v0.2.md` 不一致之处**」的**单一事实源**，供事后逐条核对。
> 纪律：评审稿是**快照，不回改**（沿用 `P0-PLAN.md §7` 开头约定）。任何偏差只在本文登记，并在 `P0-PLAN.md §7`、`DECISIONS.md` 的对应 ADR 里交叉引用，禁止直接改评审稿原文。
> 权威口径：`DECISIONS.md` 的 ADR 是最终技术结论；本文负责「哪一处、为什么偏、落到哪、怎么验」的索引。
> 创建：2026-11-18 ｜ 最近更新：2026-11-18（P0-19 追加 R18 / R19 / R20）

---

## 1. 为什么需要这份文档

P0 执行到第四波 M2 时出现了**两类**与评审稿不一致的情况：

1. **复核补漏型**：对照 GitHub 官方文档、或把一句话验收拆成可执行代码时，发现评审稿有空白 / 笔误 / 语义冲突（R1~R17）。这类偏差不改变架构，只收紧或补齐实现口径。
2. **执行顺序与呈现层型**：评审稿的**依赖图**与 `P0-PLAN.md §5` 的**建议波次**本身存在冲突，导致「按波次推进」必然要在 P0-24 未落地时先接 P0-19（R18）。这类偏差改变的是**交付形态**，需要单独记录「临时实现 → 后续替换」的边界，否则事后会把临时代码误认为最终设计。

第 2 类是本文件存在的主要理由：把「当时为什么这么做、哪些是不变的、哪些必须被替换」写清楚，避免 P0-24 落地时找不到回归点。

---

## 2. 偏差总表

| # | 类别 | 偏差点（评审稿 → 实际） | 决策 / ADR | 首落任务 |
| --- | --- | --- | --- | --- |
| R1 | 复核 | Device Flow 是否需要 `client_secret`：确认不需要 | ADR-021 | P0-15 |
| R2 | 复核 | `slow_down` 语义：由「增加间隔」收紧为「取响应新 `interval`，否则上次 +5 秒」 | ADR-021 | P0-15 |
| R3 | 复核 | 错误码清单只列 4 个 → 补齐 GitHub 官方 8 个 | ADR-021 | P0-15 |
| R4 | 复核 | PKCE 支持须独立复核 → 已复核：仅 `S256`，不支持 `plain`（P1 用） | ADR-021 | P0-15 |
| R5 | 复核 | `users.username` 的 GLOB 只约束首字符 ≈ 无校验 → 改为 domain 正则唯一事实源 + 放宽中文 | ADR-005 | M0 |
| R6 | 复核 | §4.5 / §5.1 用户名只允许 ASCII → 被 R5 覆盖 | ADR-005 | M0 |
| R7 | 复核 | `primary_email` 无 UNIQUE → 确认是「禁止按邮箱自动合并」的有意设计 | ADR-023 | P0-17 |
| R8 | 复核 | `posts.parent_post_id` 未约束「父子同 thread」→ 记为 P1 前置 | ADR-011 | M1 |
| R9 | 复核 | §5.2 未给 `threads` 建 `status` 列 → P0 不做主题删除，P1 补字段 + 迁移 | ADR-011 | M1 |
| R10 | 复核 | 发帖限流计数单位未定义 → 按「发帖动作」计数，不按行 | ADR-010 | P0-10 |
| R11 | 复核 | §2.3 ViewModel 字段集不足 → 补 `Selection` / `Pagination` / `Detail` / `Row.Body` | ADR-014 | P0-11 |
| R12 | 复核 | §2.4 领域事件「可靠投递」→ P0 只做单进程同步派发，outbox 列 P1 前置 | ADR-015 | P0-12 |
| R13 | 复核 | 错误码 7 个 → 实为 8 个；`expired_token` / `token_expired` 双兼容 | ADR-021 | P0-15 |
| R14 | 复核 | `Accept` 头统一 `application/json` → 按端点分两套（Device Flow / REST API） | ADR-021 | P0-15 |
| R15 | 复核 | 状态机一句话验收 → 补 4×4 迁移表、三态门禁、CAS、`UpdateInterval` 独立路径 | ADR-022 | P0-16 |
| R16 | 复核 | §4.3「邮箱命中提示 /link」与验收「同邮箱不同 UID 建两个账号」字面相冲 → 不阻断、不复用、仅 Notice | ADR-023 | P0-17 |
| R17 | 复核 | §4.6 未定义解绑是删行还是留行 → `SET revoked_at` 软吊销，`DELETE` 会反转吊销语义 | ADR-024 | P0-18 |
| **R18** | **顺序 / 呈现层** | **依赖图 `A24 → A19` 要求 P0-19 依赖 P0-24 终端页面；实际按 §5 波次在 P0-24 之前接入 P0-19，用最小交互层替代页面层** | **ADR-025** | **P0-19** |
| **R19** | **事务边界** | **评审稿 §4.4.1 流程把「创建/登录账号」与「绑定当前 SSH 公钥」画成一条连续流程；实际是两个独立写事务，不能原子提交** | **ADR-025** | **P0-19** |
| **R20** | **呈现形态** | **评审稿 §3.5 / §4.4.1 的「剩余时间 + 轮询期间界面状态」隐含增量页面刷新；P0-19 用固定文本行 + 服务端轮询替代，P0-24 再替换为 ViewModel 双 Renderer 页面** | **ADR-025** | **P0-19** |
| **R21** | **安全口径细化** | **§3.5「用户内容不得影响终端光标」被 P0-22 扩大解释成「用户文本全局零回显」；实际按 §2.3 / `viewmodel.Field.Secret`，回显与否是字段级语义 → 正常输入经净化后回显，仅 Secret 字段隐藏** | **ADR-026** | **MVP 实测后** |
| **R22** | **渲染控制白名单** | **§3.5 与 ADR-002 只列出 SGR 样式白名单，未定义「服务端生成的光标控制」；终端 TUI 长期主界面需要原地重绘 → 白名单扩为 SGR + EL `\x1b[K` + CUU `\x1b[nA`，其余控制序列仍一律禁止** | **ADR-027** | **MVP 收官** |

> R1~R17 的完整论证见 `P0-PLAN.md §7`；本文只做索引，避免同一结论双写漂移。
> R18 / R19 / R20 详见第 3 节；R21 详见第 6 节；R22 详见第 7 节。

---

## 3. 本轮（P0-19）三条偏差详解

### R18 —— 依赖图与波次冲突，P0-19 先于 P0-24 落地

**原始口径**

- `P0-PLAN.md §4` 依赖图：`A24 --> A19[P0-19 登录交互]`、`A23 --> A24`、`A22 --> A24`、`A10 --> A24`，即 **P0-19 必须等 P0-24 终端页面**。
- `P0-PLAN.md §5` 建议波次：`第三波 M3 的 21 → 23`、`第四波 M2 全部`、`第五波 24 → 25 → 26 → 27 → 28`。即 **P0-19 属于第四波，P0-24 属于第五波**。
- 评审稿 v0.2 `§4.4.1` 的流程图把 `/auth/device/start`、`/auth/device/poll` 画成 **HTTP 端点**；`§4.8` 命令表列 `/login github`；`§3.5` 的分层渲染与页面由 P0-24 承接。

**冲突**：按 §5 推进到第四波时，P0-24 尚未实现；按 §4 依赖图，P0-19 无法开工。二者只能取其一。

**V 先生 2026-11-18 拍板**：按推荐方案，**用最小交互层接 P0-19**——复用 P0-22 命令表 + P0-23 `RenderSystem` 输出固定文本 + 进程内轮询；P0-24 落地时再替换呈现层；与 P0-22 既定的「页面控制器替换 handle 分发」模式一致，**不改协议层**。

**具体边界（必须照此核对）**

| 层次 | 本轮（P0-19 最小交互层） | P0-24 落地后 | 是否变化 |
| --- | --- | --- | --- |
| `transport/ssh` 解码器 / 会话协议 | 保持原样（仅补 `Session.UpgradeToUser`） | 保持原样 | **不变** |
| `application/command` 命令注册表 | 复用 `/login github`，不新增命令 | 复用 | **不变** |
| `application/auth` 用例（P0-15/16/17/18） | 全部复用，零改动 | 全部复用 | **不变** |
| 轮询节奏持有者 | `cmd/server` 的 `Serve` 循环（timer + channel） | 页面控制器 | **替换** |
| 终端呈现 | `cmd/server/login_view.go`（`loginPresenter` 固定文本行） | `ViewModel` + 双 Renderer 页面 | **替换** |
| 呈现替换点 | `loginPresenter` 与 `loginFlow` 的 `out` 参数 | 新页面控制器接管 `handle` 分发 | **替换** |

**验收降级说明**：D2（`/login github` 走完 Device Flow、会话升级、当前连接公钥自动绑定）**不降级**，仍由集成测试（假 GitHub 端点）跑通；降级的只是「界面状态」的丰富度——由固定文本行代替增量刷新。P0-24 后补页面呈现，业务链路无需重写。

### R19 —— 账号事务与公钥绑定不是同一个原子单元

**原始口径**：评审稿 §4.4.1 流程图第 6 行 `创建/登录账号，绑定当前 SSH 公钥（需私钥持有证明）` 把两步画成一次连续动作；§4.6 又要求「只有 OAuth 完成且当前连接确实持有对应私钥的证明时，才能绑定公钥」。

**实际约束**：`LoginService.Login`（`users` + `identities`，ADR-023）与 `KeyBindingService.Bind`（`ssh_keys`，ADR-024）各自是**一个独立写事务**，中间还隔着 `GetUser` / `GetEmails` 两次网络调用（ADR-022 规定网络不得占写事务）。把它们塞进同一事务会直接违反 §2.6 单写连接模型。

**落地口径**：授权成功后顺序执行 `Login → Bind → Session.UpgradeToUser`；**只有三步全成功才升级会话**。若 `Bind` 失败（典型：当前连接公钥已属于其他账号），账号已创建但**当前会话保持访客态**，返回固定文案提示换钥，不落「已登录却无绑定」的半成品会话。`device_auth_states` 此时已是 `authorized` 终态（P0-16 语义），不是 `denied`——`denied` 只由 `Esc` 取消 / 用户拒绝产生，两者的验收路径分开测。

### R20 —— 固定文本行替代增量页面刷新

**原始口径**：评审稿 §3.5 的分层渲染 + §4.4.1「终端展示 `user_code` 与 `verification_uri`」+ `P0-PLAN.md` P0-19 产出「`user_code` + `verification_uri` + **剩余时间** + **取消键** + **轮询期间的界面状态**」，隐含 P0-24 的 ViewModel 页面。

**实际约束**：P0-24 未实现，`ViewModel` 双 Renderer 页面不可用；但 Q3 已拍板 P0 不开 HTTP 端点，整条链路是进程内 `SSH session → application/auth → infrastructure/github`。

**落地口径**：新增 `cmd/server/login_view.go` 作为**显式临时呈现层**：
- 服务端固定提示文本 → `safe.RenderSystem`（P0-23 的服务端可信路径）；
- 来自 GitHub 响应的 `user_code` / `verification_uri`、派生 `username` → 先过 `ProjectLoginView` 格式白名单，再走 `safe.RenderUser`（P0-23 的用户/外部数据净化路径）；
- `LF → CRLF` 在呈现层完成（ADR-019 已约定该转换不放进净化层）；
- 剩余时间只在**发起屏**以固定文本给出，轮询期间输出固定「等待授权中」行，不做全屏重绘。

**必须被 P0-24 替换的清单**：`loginPresenter`、`loginFlow` 的 `out io.Writer` 注入、`Serve` 循环里的 `pollTimer` / `pollCh` 分支。替换时 `application/auth` 与 `transport/ssh` 协议层零改动。

---

## 4. 一致性核对表（供事后逐条验）

| 核对项 | 评审稿位置 | 决策位置 | 代码位置 | 回归测试 | 状态 |
| --- | --- | --- | --- | --- | --- |
| Q3 进程内调用、不开 HTTP 端点 | §4.4.1 流程图 / §4.8 端点表 | `P0-PLAN §6 Q3`、`ADR-021` | `cmd/server/main.go` `probeMux` 仅 `/healthz` | `archtest` 既有 Q3 守卫 + `login_flow` 负向注入 | ✅ |
| `/login github` 命令 | §4.8 | `ADR-018` | `internal/application/command/registry.go` | `registry_test.go` / `login_flow` 分发测试 | ✅ |
| 轮询在服务端、终端不持有 token | §4.4.1 | `ADR-022`、`ADR-025` | `state_machine.go` / `cmd/server/login_flow.go` | `archtest` 令牌不落库 + 本轮「token/device_code 不写终端」守卫 | ✅ |
| `Esc` 取消 → `denied` 且不留未绑定半成品 | `P0-PLAN` P0-19 验收 | `ADR-025` | `loginFlow.Cancel` → `DeviceFlowService.Cancel` | `cmd/server/login_flow_test.go` + 集成测试查 `device_auth_states` | ✅ |
| 当前连接公钥自动绑定（D2） | §4.6 / D2 | `ADR-024` 第 5 条、`ADR-025` | `loginFlow.finish` → `KeyBindingService.Bind`（不经命令表） | D2 集成测试 + AST「复用 Bind」守卫 | ✅ |
| `guest → user` 会话升级唯一入口 | §4.6 三状态 | `ADR-024` 第 6 条、`ADR-025` | `ssh.Session.UpgradeToUser` + `presence.Registry.AttachUser` | `session_upgrade_test.go` + AST「唯一写入点」守卫 | ✅ |
| 分层渲染（服务端 / 外部数据两条路径） | §3.5 | `ADR-019`、`ADR-025` | `cmd/server/login_view.go` | `login_view_test.go` 恶意样本 + `archtest/safe_test.go` | ✅ |
| **R18 波次冲突** | `P0-PLAN §4` vs `§5` | **本文第 3 节 / `ADR-025`** | `cmd/server/login_flow.go` 顶部「P0-24 替换点」注释 | `archtest` 文档一致性守卫 | ✅ 已登记 |
| **R19 双事务边界** | §4.4.1 第 6 行 | `ADR-025` | `loginFlow.finish` 错误分支 | 集成测试「Bind 失败不升级会话」 | ✅ 已登记 |
| **R20 呈现形态降级** | §3.5 / §4.4.1 | `ADR-025` | `cmd/server/login_view.go` | `login_view_test.go` 固定文本 / CRLF / 外部字段净化 | ✅ 已登记 |
| P0-18 守卫按端口语义扩展（本轮连带改动） | §4.6 | `ADR-025` | `internal/archtest/key_binding_test.go` `isServerHeldCall` | `TestBindRequestAcceptsLoginConnPort` / `TestBindRequestRejectsUnknownReceiver` | ✅ 强度不降 |
| D2 全链路（账号 / 身份 / 公钥 / 会话升级） | §6 D2 | `ADR-025` | `cmd/server/login_flow.go` `finish` | `TestLoginFlowD2EndToEnd`（真实 SQLite + 假 GitHub 端点） | ✅ |
| Esc 取消落 `denied` 且无半成品 | P0-19 验收 | `ADR-025` | `loginFlow.Cancel` | `TestLoginFlowCancelWritesDeniedAndNoHalfBoundState` | ✅ |

### 本轮验证摘要（2026-11-18）

- 新增 **49 个测试函数**；全量 `go build ./...` / `go vet ./...` / `go test -count=1 ./...` 通过（30 个包；核心 6 包共 336 个测试函数）。
- 从零启动实测：`migrations_applied=[1 2 3]`（**无新迁移**），日志出现 `登录交互就绪 mode=minimal_in_process presentation=fixed_text provider=github replace_at=P0-24`，无 panic。
- `-race` 因本机无 gcc / CGO 未启用，按 `P0-PLAN.md §8` 既定安排后置到 D7 之后。
- 本文件由 `internal/archtest.TestDeviationsDocumentRecordsP019` 校验存在性与 R18~R20 关键段落。

---

## 5. 维护约定

1. 新增偏差**只往本文第 2 节追加 R 编号**，并在 `P0-PLAN.md §7` 与对应 ADR 交叉引用；不要另开第二份清单。
2. R 编号一经分配不复用、不重排；已推翻的旧结论标为「已归档」，不删除。
3. 每条偏差必须能在第 4 节找到「代码位置 + 回归测试」；只有结论、没有可核对落点的条目视为未闭环。
4. 本文件由 `internal/archtest` 的一条文档一致性守卫做最低限度校验（存在性 + R18 关键段落），防止文档被误删或整段重写后丢失。

---

## 6. R21 —— 回显口径细化（ADR-026）

**原始口径**

- 评审稿 §3.5：「用户内容不得影响终端光标、剪贴板（OSC 52）或窗口标题」「服务端生成的 ANSI 与用户提交的 ANSI 使用不同的信任规则」。
- `ADR-018`（P0-22）把这条落地成「`KeyMsg.Raw` / `Text` / `Invocation.Args` 都不写回终端」的**全局零回显**，理由是「P0-23 安全渲染落地前不留旁路」。
- `P0-PLAN.md §3` P0-22 验收也写死「用户文本零回显」。

**冲突**：P0-23 的分层安全渲染已落地（`safe.SanitizeText` + 白名单 Markdown + 受控 ANSI），「不留旁路」这个**临时理由不再成立**；但全局零回显被沿用到了命令输入、标题、正文编辑。本机 PowerShell + OpenSSH 实测中用户完全看不到自己输入的内容，判定为「输入没反应」，体验不可用。

**V 先生 2026-11-18 拍板**：回显按字段分级——正常输入回显，只有密码类 Secret 字段隐藏。依据是评审稿 §2.3 与 `viewmodel.Field.Secret` 早已写明「Secret 描述语义约束，**具体回显字符由 Renderer 决定**」。

**具体边界（必须照此核对）**

| 层次 | 修订后 | 是否变化 |
| --- | --- | --- |
| 用户输入的字符 | 先过 `safe.SanitizeText`（ESC/CSI/OSC→可见图示、CR→LF、bidi/C1 剥离）再回显 | **变**（原为完全不回显） |
| 编辑控制 `\b \b` / `\r\n` | 服务端固定常量生成，与用户字节分两条写入路径（§3.5） | 新增 |
| Secret 字段（P1 邮箱登录密码） | `setSecret(true)` → 只回显 `*` | 新增 |
| `inv.Args` / 指纹 / 公钥文本 / `err.Error()` / 凭据 / `device_code` / `access_token` | **仍不回显** | **不变** |
| `safe/ansi.go` SGR 白名单与全部 archtest 守卫 | 一字未改 | **不变** |

**验收**：`cmd/server/line_echo_test.go` 5 项 + 真实 SSH E2E 回显断言（`//login`、标题、正文）；恶意字节样本经回显后输出不含原始 ESC / CR / 双向控制字符。

> 本条只细化 §3.5 的**解释**，不推翻「用户提交的控制序列不得影响终端」这条红线；ADR-018 原文保留，由 ADR-026 明确标注修订范围。

---

## 7. R22 —— 服务端光标控制白名单（ADR-027）

**原始口径**：§3.5 的分层安全渲染与 `ADR-002`（统一 ViewModel + 两个独立 Renderer）只定义了「服务端生成的 ANSI 与用户提交的 ANSI 走不同信任规则」，并把可用序列收窄到 `safe/ansi.go` 的 **纯 SGR 常量表**；archtest 的 `sgrOnly()` 也据此判定。评审稿没有预留「服务端主动移动光标做原地重绘」的通道。

**冲突**：P0 终端页面因无光标控制，`move()` 只能每次追加整屏；`j/k` 连按即刷屏，实测判定为不可用。而终端 TUI 被确定为长期主界面（V 先生 2026-11-18 决策），必须支持原地重绘。

**落地口径**：白名单**只增两条**，且只由服务端按会话状态生成：

| 序列 | 用途 | 谁生成 |
| --- | --- | --- |
| `\x1b[K`（EL） | 重绘单行前清除旧内容 | `safe.ClearToEOL()`，由 `RenderFrame` 逐行调用 |
| `\x1b[nA`（CUU 模板 `\x1b[%dA`） | 回到上一屏顶部 | `safe.CursorUp(prevLines)`，`n` 来自上一屏行数 |

**保持禁止**：ED 清屏 `2J`、CUP `H/f`、CUD `B`、DECSET 备用屏 `?1049h`、OSC 52 剪贴板、OSC 窗口标题、带参数 EL `2K`。`safe.RenderUser` / `SanitizeText` 等用户内容路径不引用任何光标控制。

**守卫升级**（`internal/archtest/safe_test.go`）：`sgrOnly()` → `allowedANSI()`；新增三条断言——① 8 类禁止序列负向注入全部被拒；② 用户内容路径文件不含 `CursorUp(` / `ClearToEOL(`；③ `internal/` 下只有 `ansi.go`（定义）与 `frame.go`（唯一消费点）可出现这些调用。

**验收**：见 `docs/MVP-DELIVERY.md §11`，含真实 Windows OpenSSH 客户端字节流统计。

> 本条**不推翻** §3.5 红线：用户提交的 ESC 仍被净化成可见图示，服务端控制序列与用户字节始终分两条写入路径。
