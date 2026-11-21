# P4 阶段计划（可靠性收尾 + 基础多色）

> 状态：**P4 进行中（2026-11-21）——T1/T2/T3/T4 均已完成（T1/T2 代码已实现并通过 QA，T3 已授权、T4 已在 VPS 回访闭环），待发版**。用户给出中期路线图：P4 = 可靠性收尾 + 基础多色；P5 = 浏览器端 TUI 帧传输；其余个性化与运营扩展留待未来用户反馈。
> 基线：v0.6.0（2026-11-21），测试 935，迁移止于 0010，ADR 止于 062，P3 全部收口。当前分支 `p4-implement`：测试 941（顶层函数口径），T1/T2 代码改动未 commit（发版需用户当次确认）。
> 依据：`docs/ROADMAP.md` R-01/R-02/R-03、`docs/P3-PLAN.md` §5/§8/§11、`docs/P2-PLAN.md`、`docs/DECISIONS.md` ADR-014/019/027/058/059、设计稿 §3.2/§3.3/§3.5、代码核实。
> 性质：本文只固化方向、范围与任务骨架；实施时逐项补 ADR（编号从 ADR-063 顺延）、自测与结构化汇报。**发版（commit/tag/push/Release）必须经用户当次明确确认**。

## 0. 结论先行

用户 2026-11-21 拍板三段式路线图：

| 阶段 | 主线 | 状态 |
| --- | --- | --- |
| **P4** | 可靠性收尾 + 基础多色 | **T1/T2/T3/T4 均已完成，待发版**（见 §1.3） |
| **P5** | 浏览器端 TUI 帧传输 | 方向已定，设计见 §6，开工前需新 ADR |
| 未来 | 个性化 / 运营细节扩展 | 按真实用户反馈再排期，见 §3 |

P4 主线精确对应 P4 需求池的 **P0 四项**——不含任何需重拍板的 P1/P2 项。四项技术低风险、零新依赖、零或极少迁移、不触对外契约。

### 1.3 里程碑状态（2026-11-21）

| 里程碑 | 状态 | 完成证据 |
| --- | --- | --- |
| **T1** 终端基础多色 | ✅ **已完成** | 3 Style（34/33/31）落 `ansi.go`；状态栏 L3/L4 与提示前缀分色、正文走 `RenderUser`；2 条 archtest 守卫 + 负向注入；QA 独立验证信任域未破防、karma 阈值正确。四门禁全绿。 |
| **T2** 带宽探针 | ✅ **已完成** | `Server.bw`（2×atomic.Uint64）+ `Session.bw`（atomic.Pointer）；`/healthz` 输出 `tx_bytes`/`rx_bytes`；QA 实测响应体含字段、commits 不变、8×100 并发无丢计。四门禁全绿。 |
| **T3** race 转 required | ✅ **已完成（授权）** | 用户已授权（2026-11-21）；实施时改 GitHub 仓库 branch protection 把 `race` 设为 required（非代码变更）。 |
| **T4** P3 里程碑回访 | ✅ **已完成（VPS 验证）** | nyaterm 连 Jeeinn VPS 只读验证：v0.4.2→v0.6.0 升级完成、host key 指纹不变、迁移 9→10；备份 timer 未启用→已部署并 `enable --now`，下次触发 2026-11-22 03:00 CST。 |

> 发版状态：T1/T2 代码改动在分支 `p4-implement`，commit 于 2026-11-21；**未 push / tag / Release**——后续发版仍须用户当次明确确认。

## 1. 范围与目标

### 1.1 P4 纳入（四项）

| 任务 | 内容 | 量级 | 迁移/依赖 | 依据 |
| --- | --- | --- | --- | --- |
| **P4-T1** | 终端基础多色（克制的 3 色） | S（0.5d） | 零 | ROADMAP R-01；用户"基础多色"诉求；配色方案 2026-11-21 已拍板 |
| **P4-T2** | 带宽 tx/rx 可观测探针 | S~M（1d） | 零 | P3-PLAN:135/220 明确留 P4 |
| **P4-T3** | race job 转 required 门禁 | S | 零代码（改仓库保护规则） | P3-PLAN:218；ADR-058 观察条件已满足 |
| **P4-T4** | P3 里程碑回访 | S | 零 | P3-PLAN §11:217 唯一待执行确认项 |

### 1.2 目标（可验收）

1. 状态栏等级分档 / 状态提示前缀可见颜色，**版块名不上色**，且**颜色不单独承载语义**（保留 L2/L3/L4 文字与 `[错误]`/`[提示]` 标签，避免纯红绿表意）；
2. `/healthz` 暴露 `tx_bytes`/`rx_bytes`，纯内存计数、不写库、不改领域模型；
3. race job 成为强制门禁并连续全绿，备份 timer 首执行 + `verify` 通过有记录；
4. 全量测试绿、archtest 绿、go build/vet 全绿（CI 四门禁口径）。

## 2. 任务分解骨架（来自架构师技术评估）

T1~T4 相互独立、可并行。每项实施时补 ADR / 自测 / 汇报。

### P4-T1 终端基础多色（配色已拍板 2026-11-21）

**色板：克制的 3 色（用户拍板取主档、收紧、不上版块名）**

| 角色 | SGR | 颜色 | 唯一用途（仅服务端固定文案） |
| --- | --- | --- | --- |
| Muted | `[2m` | 灰 | 元信息/时间/footer（已存在，不算新增） |
| Accent | `[34m` | 深蓝 | 状态栏 `L3`/`L4` 两字符 |
| Notice | `[33m` | 琥珀 | `[公告]`/`[置顶]` 前缀（未来运营项落地时用；不用亮黄 93） |
| Error | `[31m` | 暗红 | **仅** `[错误]` 4 字符，不染消息正文 |

- **不新增**：32 绿、35 品红、37 白、90-97 亮色、任何背景色（40-47/100-107）、256 色、真彩。现有 `[36m`（StyleCode 行内代码）原样保留、不复用为 UI 强调色。
- **面积硬指标**（可写进 ADR）：彩色只出现在固定标签/前缀，绝不覆盖用户文本主体（标题/正文/作者名/slug/提示正文）；单屏彩色 ≤ 约 15 格（24×80 屏约 0.8%）；同屏最多 2 种颜色；禁整行/背景着色、禁连续两行着色；每个着色处必有文字标签，去掉颜色信息不丢失。
- **坚决不上色**：一切 DB/用户文本（标题、正文、回复、`@作者`、slug、提示 Message）——全走 `ext()`→`RenderUser`；焦点标记 `> `、序号、赞数、`id=`、`rN`、时间、按键提示、分页/空态文案；**版块名（`n.Name`/`n.Slug`，用户已拍板不上色）**。
- 改：`internal/presentation/terminal/safe/ansi.go`（加 3 个 Style 角色 + 31/33/34 常量 + `openANSI`/`closeANSI` 分支）；`terminal/view.go:226-229` 把一整段 `sys(StyleDim)` 拆多段，`L1/L2` 保持 Dim、`L3/L4` 走 Accent；`view.go:241-248` 按 `Notice.Kind` 映射前缀色（Error→红、Warning→琥珀、Info/Success→Dim）；`render_test.go`/`samples_test.go` 加断言。
- **已核实**：`archtest/safe_test.go` 的 `sgrOnly()` 放行任意纯数字 SGR，新常量无需改守门逻辑；但新增颜色 Style 扩大 `styleFor()`（`safe/render.go:47`）可返回域，须补守卫——
  - `TestUserMarkdownCannotSelectPalette`：断言 Accent/Notice/Error 三个 Style 不出现在 `render.go`/`sanitize.go`/`markdown.go`，`styleFor()` AST 不得引用；
  - `TestColorStylesOnlyUsedBySystemPath`：三个颜色 Style 只允许在 `ansi.go`（定义）与 `terminal/view.go`（消费）出现。
- 验收：新 SGR 常量只在 `ansi.go`；archtest（含新守卫）通过；`RenderUser` 路径仍无法选色；不上 256 色/真彩；示例图见讨论记录（主题列表 + 详情/提示两态）。
- 基础配色本身**不做可关闭开关**（节日主题才需要，已推未来）。

### P4-T2 带宽可观测探针
- 改：`internal/transport/ssh/session.go` 的 Read/Write 处累加 atomic 计数；Server 进程级聚合；`cmd/server/main.go` `probeMux:/healthz` 增 `tx_bytes`/`rx_bytes`。
- 验收：计数单调递增、不写库（commits 不因字节累加增长）、不改领域模型、不在终端呈现暴露；加"计数不写库"守卫。
- 注：仅为可观测数据准备，**不构成** 1 Mbps / 1000 在线容量验收（ADR-059 诚实口径）。

### P4-T3 race job 转 required
- 改：`.github/workflows/ci.yml`（job 已存在，转门禁在仓库 branch protection 设置）。
- 验收：race job 连续多轮全绿并设为 required；无代码行为变更。
- 开工前置：**需用户授权改仓库保护规则**。

### P4-T4 P3 里程碑回访
- 无代码：核对 v0.6.0 上线后 race job 连续运行状态、备份 timer 首次执行 + `motdctl verify`，结论回填 `P3-PLAN.md` §11。
- 开工前置：部署侧执行权限；若目标 VPS 未启用备份 timer，先确认启用（ADR-060 模板仍是 `<ENV_FILE>` 占位）。

## 3. 明确不做（P4/P5 之外，留未来按反馈）

用户拍板：个性化与运营细节扩展留给未来用户使用及反馈。以下全部**不在 P4、也不在 P5**：

- **个性化**：节日主题配色、@提及与未读、更多隐藏版块、真全屏数字雨、签到 30/100 天扩展、banner 按登录态。
- **运营**：告警接入、异地备份、RPO 收紧至小时级、容量分档压测、Online Backup API 切换。
- **小功能**：`/vote` 扩 post 目标（Service 已就绪，待产品拍板）。
- **对外契约（P5 专项除外）**：RSS / 只读 API / JSON 输出 / AI 摘要 / 原生 CLI token / Passkey / 任何 Web 管理面。
- 冻结依据沿用 `P3-PLAN.md` §8 与 `PHASE1-PLAN.md` §1.2；设计稿 §6 分期是历史愿景稿，不作排期依据。

## 4. 风险与护栏

1. 依赖单向（transport→application→domain；presentation→application；端口定义在使用方），改完先跑 archtest。
2. 终端文本全走 `safe`：服务端固定文案 `RenderSystem`、DB/用户文本 `RenderUser`；ESC 仅来自 `ansi.go` 常量表；多色只加 SGR，不扩白名单类别（禁 ED 清屏/CUP/OSC/备用屏，ADR-027）。
3. 凭据不落库/不进日志/不进呈现；权限单点 `canModify`/`requireAdmin`；软删不硬删；迁移从 0010 顺延不改历史。
4. 会话生存期：无按键不断连、6h 绝对上限（ADR-055）；带宽计数不引入新的空闲断开逻辑。
5. **版本口径待校准**（P3 审计发现）：`RELEASE_NOTES.md:4` 仍写"v0.6.0（待打）"但 tag 已存在且指向 d1596b8，其后 4 个修复 commit 未被 tag 覆盖；P4 开工前后一并更正。

## 5. 待确认项

> 四项前置已全部闭环，详见 §1.3 里程碑状态。以下为历史记录，保留追溯。

1. ~~是否立即开工 P4~~ **已开工并完成 T1/T2/T3/T4（2026-11-21）**。
2. ~~T1 用色清单与落点~~ **已拍板（2026-11-21）**：取主档 3 色（31/33/34）、收紧落点、不上版块名，详见 §2 P4-T1。
3. ~~T3 授权~~ **已授权（2026-11-21）**：用户授权把 race job 设为 required。执行方式 = 在 GitHub 仓库 branch protection 把 `race` 加入 required status checks（需仓库 admin 权限），`ci.yml` 的 job 本身无需改。
4. ~~T4 目标 VPS~~ **已完成回访（2026-11-21）**：nyaterm 连 Jeeinn VPS 验证并升级 v0.6.0、启用备份 timer。

## 6. P5 预告：浏览器端 TUI 帧传输（方向已定，开工前需新 ADR）

用户 2026-11-21 拍板 P5 主线 = 浏览器端 TUI 帧传输。这是对既有冻结口径的**正式解禁**，须如实记录以下张力与前置：

| 维度 | 说明 | 依据 |
| --- | --- | --- |
| 方案 | 服务端渲染语义 Frame + 本地键盘渲染器，与终端共用 ViewModel、独立渲染；Frame 增量 ops（upsert/remove/move/overlay/focus）+ seq/base_seq 恢复；不用 xterm.js，JS ≤15 KB gzip | 设计稿 §3.2/§3.3 |
| 现状 | `internal/transport/websocket/doc.go`、`internal/presentation/browser/doc.go` 各 3 行纯占位——从零建，非续写 | 代码核实 |
| **决策翻转** | 此前 P3-PLAN §8 / PHASE1 §1.2 明确冻结浏览器面；重议触发条件为"1000 在线压测通过 + 契约进 1.0"，**当前一条不满足**。用户拍板属产品层强制解禁 | P3-PLAN §8、P2-PLAN:164-170 |
| **风险** | 0.x 对外契约一旦上线即焊死；容量（1 Mbps/1000 在线）未验收；须在 P5 开工前正面处理"无容量背书下开新公网入口" | ADR-059、ROADMAP R-03 |
| 安全边界 | 浏览器端用 `textContent`，与终端 `RenderSystem`/`RenderUser` 同等严格；WebSocket 校验 `Origin`，不依赖 `SameSite=Lax` | 设计稿 §3.5:390、§3.2:326 |
| 量级 | L，多模块（transport/websocket + presentation/browser + Frame 协议 + ViewModel 双 Renderer + 会话状态模型） | 架构师评估 |
| 开工前置 | 新 ADR（解禁决策 + Frame 协议 + Origin 校验 + 双端安全边界）；容量验收是否作为 P5 前置需用户再拍板 | 本文 §0 |

> 纪律：本阶段只固化 P5 方向，不做 P5 详细设计；具体方案在 P4 收口后单独组织架构评审。

## 7. 标题变色长期形态（已拍板方向，不进 P4）

用户 2026-11-21 拍板："用户选语义标记 → 服务端映射 → 只染 `◆ ` 前缀"。这是未来运营项的方向锁定，**不在 P4、不在 P5**，实施前需新 ADR（编号从 ADR-063 顺延，规划 ADR-065）。核心约束：

1. **用户选的是语义标记、不是颜色**：领域层 `TitleMark` 闭合枚举（初始 `none|blue`，用户可选色只有蓝 1 种；红/琥珀保留给错误与运营标记，用户不可选），精确字符串相等校验，非枚举值拒绝且不回显原文。
2. **服务端映射**：`terminal/view.go` 的 `titleMarkStyle(mark)` switch → `safe.StyleAccent`；`safe` 包无任何"按用户字符串生成 SGR"入口，`fmt.Sprintf("\x1b[%dm", n)` 永久禁止（加 archtest 守卫）。
3. **只染前缀**：给标题加固定前缀 `◆ `（蓝，走 `sys()`/`RenderSystem`），标题文字本身仍走 `ext()`/`RenderUser` 保持默认色——避免 Markdown 内联 reset 打断外层色，不给 `RenderUser` 加 Style 参数。
4. **karma 显式消费**：`WriteTx.SpendKarma` 条件原子 UPDATE（`karma=karma-? WHERE id=? AND karma>=?`）与 `SetTitleMark` 同事务，同值 no-op 不重复扣；写前 `canModify` 校验仅作者本人。**是显式消费，不是 `Level(karma)>=N`**（ADR-014 等级不做权限谓词的例外说明须写进 ADR-065）。
5. **防花花绿绿硬约束**：标记不参与排序、不加权；单屏最多 1–2 条用户着色标题，超出按时间先后退化为无标记；可设 7 天期限或 `/unmark` 自助撤销。
6. **ViewModel 命名**：新字段只能叫 `TitleMark`/`Pinned`，不得含 `Color`/`Style` 词段（会命中 `archtest/viewmodel_test.go` 禁用词段），不得放 SGR 数值；终端与浏览器各自把同一枚举映射到自己的样式角色（浏览器 `span.s` 的 role），兑现设计稿 §3.3:356。
7. **真钱付费**是另一回事（需订单域/回调验签/对账/凭据不进日志与呈现），本方向锁定**不承诺**，明确推更远的未来。

> 相关未来项：公告/置顶（迁移 0011 加 `threads.mark` + `requireAdmin` 命令 + keyset 游标改造，规划 ADR-064）；本标题标记方向复用其前缀渲染位。
