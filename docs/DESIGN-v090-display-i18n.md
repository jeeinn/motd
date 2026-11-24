# 方案：v0.9.0 显示层物理行模型 + 国际化基建（含窄屏折行修复）

> 状态：**已实施、待发版（W1-W17 完成；W18 发版需单独授权）**。
> 历史状态（留痕，不删改）：决策已拍板（2026-11-24），方案定稿，待授权编码——现已由上方状态取代。
> 用户拍板：**一步到位 v0.9.0**——把原 v0.8.5 的 B 止损、方案 C（逻辑行→物理行映射）、
> i18n 基建一起做；理由：v0.8.4 在大视窗终端已完整，不急需过渡补丁，直接大版本从根源
> 解决 + 建测试夹具 + 为多语言打基础。要求"重新修订文档及文档名称，工作逐一登记列项"。
>
> 证据基线：`main` = `414b00d`（v0.8.4 已发版）。本方案文档取代 `DESIGN-narrow-reflow.md`
> （已删除，untracked）。本次纯方案，未改任何代码、未建分支、未做 git 写操作。
> 关联：`DESIGN-hint-reply-focus.md`（O3 详情屏分页 P1，本期不修）、ADR-067（草稿，其决策 2 将被 ADR-069 修订）。
>
> **实施状态回填（2026-11-24，W16）**：W1-W17 已全部完成（含 B7 纳入 W10），编码/测试/文档完成、四门禁 + 两闭环 + archtest + 虚拟网格全绿；新增 ADR-069（修订 ADR-067 决策 2）与 ADR-070，发版文档已定稿。**W18（commit → PR → CI → tag → Release）与部署待用户单独授权，尚未执行**。上方"本次纯方案、未改代码、未建分支、未做 git 写操作"为立项时历史留痕，不再代表现状；关联行所述 ADR-067 决策 2 已由 ADR-069 修订。

## 1. 决策摘要与范围

| # | 决策 | 结论 |
|---|---|---|
| D1 | 版本 | **v0.9.0 minor**（不是 patch）——含显示层模型重构 + i18n 基建 + 已报 bug 修复 |
| D2 | 修复路径 | 采纳 B（A 路径追加重建止损）+ C（逻辑行→物理行映射工具）一起做，B 是即时正确、C 是根治与 i18n 前置 |
| D3 | i18n 基建 | 本期建：文案表（key→locale）、会话 locale、`writeHint` 查表、中英双语 hint；**不做**全量 UI 翻译（notice/命令输出等后续版本） |
| D4 | B7 同源缺陷 | **升级纳入本期**（原评审建议 backlog）：既然做 C，`mergeBackspace` 的 CUU(1) 同源假设一并修，不留半拉子模型 |
| D5 | 测试夹具 | 建虚拟终端网格（含自动折行 + pending wrap）、可配宽度 fake、CUU 参数提取——这是本次的"测试基建"核心交付 |
| D6 | 代价 | 接受 B 的正文多回显一遍（刷屏）+ 提示离光标变远（长正文时倒计时被顶出屏幕），如实写入 RELEASE_NOTES |

**本期不做**：详情屏内容分页（`DESIGN-hint-reply-focus.md` O3，P1）、草稿持久化落库（ADR-067 backlog）、RTL 语言、全量 UI 文案翻译。

## 2. 背景与问题定性（修正评审 B4）

用户本地 v0.8.4 窄屏终端连续回复时字符散落（`。复`/`。`/`。|`/`恢复`），全屏正常。

**修正定性**（采纳评审 B4）：这不是"窄屏边缘"。限流提示实测显示宽度 **125 列**（含 `[提示] ` 前缀与 `1m0s`），因此**任何 <126 列的终端都折行**——80×24（SSH 最常见默认）、100 列、120 列全部命中；用户约 150 列宽屏才"正常"。故 bug 可达面是**绝大多数用户**，不是个例。优先级仍 **P1**（数据不丢、闭环未断、可重发/恢复），但依据改为"闭环未断"而非"全屏可规避"。

严格区分：物理折行（终端行为，可接受）vs 光标错位散落（代码缺陷，不可接受）。

## 3. 根因（三层）

1. **显示层缺物理行模型**：`lineEcho`（`line_echo.go:20-24`）只有 `out/secret/cols`，无终端宽度、`cols` 记录逻辑列宽；`RenderFrame`（`frame.go:26`）虽有 `overWide` 降级，但只覆盖整屏渲染，编辑器回显层裸奔。
2. **光标跳行硬编码**：`resyncLastLine(lastLine, up=2)`（`page_controller.go:939-946`）假设"提示=1 物理行"，折行后 `CursorUp(2)` 跳进提示中间；且 `EL`（`\x1b[K`）只清当前物理行，清不掉 H 行碎片——**动态算 up 也是假修复**。
3. **测试模型看不见物理行**：`fakePageSession.Window()` 固定 80 列（`page_controller_test.go:83`）、不执行 PTY 折行；`cmdOnlyWhitelistedANSI` 只认终结符类别丢弃 CUU 参数——`up=2`/`up=5` 在测试里等价。

## 4. 工作项登记（WBS）

> 每项含：编号 / 工作 / 文件 / 验收标准 / 依赖。优先级 P0=放行必须，P1=同期完成。

### 4.1 组 A：测试基建（先建夹具，W4-W6 依赖它）

| # | 工作 | 文件 | 验收标准 | 级别/依赖 | 状态 |
|---|---|---|---|---|---|
| W1 | `fakePageSession` 可配宽度：加 `width` 字段 + `newFakePageSessionWithWidth(w, uid)`，`Window()` 不再硬编码 80（`page_controller_test.go:83`） | `cmd/server/page_controller_test.go` | 同用例可在 24/40/80/125/200 列下跑；既有 80 列调用点默认行为不变 | P0 / 无 | ✅ 完成 |
| W2 | `cuuParams(out) []int`：正则提取 `\x1b\[(\d+)A` 参数序列 | `cmd/server/page_controller_test.go` | 能从字节流准确提取所有 CUU 数值；SGR/EL 不误报 | P0 / 无 | ✅ 完成 |
| W3 | **虚拟终端网格**：二维 `rows[][]rune` + 光标 `(row,col)`，逐字节执行 7 种 token（见 §5.1） | `cmd/server/terminal_grid_test.go`（新） | 能复现"提示折 4 行 + up=2 跳错"的散落；B 方案下无残留；单测自证（喂已知序列断言网格内容） | P0 / W1 W2 | ✅ 完成 |

### 4.2 组 B：止损（方案 B，即时正确、文案长度无关）

| # | 工作 | 文件 | 验收标准 | 级别/依赖 | 状态 |
|---|---|---|---|---|---|
| W4 | A 路径 `rebuildComposerFromDraft`（`page_controller.go:918-946`）：删 `resyncLastLine` 调用，改 `p.echo.pasteBlock(e.body.Text())` 整段追加；`setSecret(false)`@`:935` 已清 cols，**不额外 reset**（评审小项） | `cmd/server/page_controller.go` | 输出无 CUU；正文紧随 hint；cols 按末行重建；退格精度保持 | P0 / W3 | ✅ 完成 |
| W5 | 删 `resyncLastLine`（`line_echo.go:120-128`），全仓唯一调用已随 W4 移除；保留 `mergeBackspace` 的 CUU(1)（W8 再改） | `cmd/server/line_echo.go` | 编译通过；archtest 绿 | P0 / W4 | ✅ 完成 |
| W6 | 既有测试适配（**评审 B1**）：① `TestDraftRateLimitRebuildResyncCaret:540` 改写为"B 路径重建"，CUU 判据从 `Contains("\x1b[")` 改为 `len(cuuParams(out))==0`（SGR 含 `\x1b[` 会假通过），count>1 改 count==2；② `TestDraftRateLimitRetainsInPlace:163` 正文 1→2，**`:164` cols==4 保持不变**；③ `TestDraftRateLimitRebuildMultiLineResync:588` 改名 `...MultiLineCols`（"只重写末行"表述已不实） | `cmd/server/composer_draft_test.go` | 三条测试语义与 B 方案一致；无假通过断言 | P0 / W2 W4 | ✅ 完成 |
| W7 | B 代价写入文档：正文多回显一遍（刷屏）+ 提示离光标变远（长正文倒计时被顶出屏幕），RELEASE_NOTES 如实记 | `docs/RELEASE_NOTES.md` | 代价段含两条 | P1 / W4 | ✅ 完成 |

### 4.3 组 C：物理行模型（方案 C，根治 + i18n 前置）

| # | 工作 | 文件 | 验收标准 | 级别/依赖 | 状态 |
|---|---|---|---|---|---|
| W8 | 新增 `safe` 层"逻辑行→物理行"工具：基于 `DisplayWidth`/`WrapText`（`sanitize.go:94/124`）计算文本在给定宽度的物理行数与折行点；含 **pending wrap**（占满末列时延后到下个字符才折）语义 | `internal/presentation/terminal/safe/wrap.go`（新） | 中/英/emoji 各输入行数正确；纯函数可单测；不改 ANSI 白名单 | P0 / 无 | ✅ 完成 |
| W9 | `RenderFrame`（`frame.go:26`）与 W8 共用折行计算：替换 `overWide`/`visibleWidth` 的重复逻辑（`frame.go:55-83`），消除双口径 | `internal/presentation/terminal/frame.go`、`safe/wrap.go` | 既有 RenderFrame 测试全绿；archtest 绿；同一文本两处行数一致 | P0 / W8 | ✅ 完成 |
| W10 | 修 B7 同源缺陷：`mergeBackspace`（`line_echo.go:105-113`）的 `CursorUp(1)` 与 `backspace` 的 `\b \b`×逻辑宽，改为按 W8 物理行计算——超终端宽行→Enter→Backspace 不再残留 | `cmd/server/line_echo.go` | 虚拟网格断言：超宽行合并后无上方物理行残留；v0.8.3 退格精度测试不回退 | P1 / W3 W8 | ✅ 完成（B7 已纳入） |

### 4.4 组 D：i18n 基建（本期建骨架 + 中英双语 hint）

| # | 工作 | 文件 | 验收标准 | 级别/依赖 | 状态 |
|---|---|---|---|---|---|
| W11 | 文案表 `hintCatalog`：10 个 hint key（`hintPrefix` + 4 引导 + 5 草稿状态）→ `map[locale]string`，中文（现状原文）+ 英文译文；文案按"短句优先、关键信息前置、每句≤终端宽度"撰写 | `internal/presentation/terminal/i18n/catalog.go`（新） | 10 key × 2 locale 齐全；英文 hint 含槽位 `%s`；翻译产物仍过 `RenderSystem` 校验 | P1 / 无 | ✅ 完成 |
| W12 | 会话 locale：`pageController` 加 `locale` 字段（默认 zh），测试注入点；`writeHint(sess, key, args...)` 改为查表（签名变化，调用点 9 处同步） | `cmd/server/page_controller.go` | 9 处调用改 key 引用；无散落字面量；默认中文行为与 v0.8.4 逐字节一致 | P1 / W11 | ✅ 完成 |
| W13 | `noticePrefix` 前缀语义随 locale：`[提示]`/`[错误]` → en `[Info]`/`[Error]`（`view.go:275`）；`hintPrefix` 同理查表 | `internal/presentation/terminal/view.go`、`i18n/catalog.go` | 切换 en 后前缀正确；信任域不变（服务端常量） | P1 / W11 W12 | ✅ 完成 |
| W14 | i18n 参数化测试：核心光标/退格用例中英双语各跑一遍；正文段输出与文案长度无关（TC-A13 用"40 vs 200 列"+"与 resumeDraft 对照"两个无缝写法，采纳评审 B3） | `cmd/server/composer_draft_test.go`、`internal/presentation/terminal/i18n/*_test.go` | 同一光标断言中英均过；`TestHintSingleCRLF` 保持不参数化（架构红线） | P1 / W11 W12 W3 | ✅ 完成 |

### 4.5 组 E：文档与发版

| # | 工作 | 文件 | 验收标准 | 级别/依赖 | 状态 |
|---|---|---|---|---|---|
| W15 | ADR 合规（**评审 B5**）：ADR-067 决策 2 原文保留 + 补一行"已修订（ADR-069）"；新增 ADR-069（B：追加重建，逐条回答 P0-2 不复发三条理由）与 ADR-070（C+i18n 基建）；措辞统一为"新增 ADR + 标注已修订"，不用"patch" | `docs/DECISIONS.md` | 历史原文未删改；新 ADR 含代价/边界 | P0 / W4 W8 | ✅ 完成 |
| W16 | 决策文档同步：本方案 WBS 完成状态回填、O3/B7 状态更新 | `docs/DESIGN-v090-display-i18n.md`、`DESIGN-hint-reply-focus.md` | 与实现一致 | P1 / 全部 | ✅ 完成 |
| W17 | v0.9.0 发版文档：CHANGELOG `[0.9.0]`、README 版本、RELEASE_NOTES（含 D6 代价、125 列可达面、i18n 基建说明） | 三份文档 | 编码完成、门禁全绿后定稿 | P1 / 全部 | ✅ 完成（待发版口径） |
| W18 | commit → PR → CI（Build/Vet/Test + Race）全绿 → merge → tag `v0.9.0` → Release | git / gh | 按 AGENTS.md §5 留回退点、精确暂存、CI 全绿才 tag | P0 / W17 | ⛔ 待授权（未执行） |

## 5. 测试夹具规格（评审 B2 修正）

### 5.1 虚拟终端网格 token 表（7 种，补齐折行语义）

| token | 动作 |
|---|---|
| 普通 rune（经 `SanitizeText`） | `col += DisplayWidth(r)`；**若 `col > width` 则先 `row++, col=0` 再写（自动折行，宽字符整字折）** |
| **pending wrap** | 字符恰好占满末列时，光标留在行末**不立即换行**；下一个字符到达才 `row++, col=0`——这决定 `EL` 清哪行、`CUU(n)` 落哪行，**是本 bug 判定点** |
| `\r` | `col = 0` |
| `\n` / `\r\n` | `row += 1` |
| `\x1b[K`（EL） | 清空 `rows[row]` 当前物理行 |
| `\x1b[nA`（CUU） | `row = max(0, row - n)` |
| SGR（`\x1b[2m` 等） | 忽略（不改网格内容、不计列宽） |

**定位声明**：网格"宽度已知、只做物理层"，不追求终端级真值；与真实 PTY 窄屏 E2E 互补——前者自动化回归、后者人工终验。

### 5.2 用例清单

| 编号 | 用例 | 断言要点 | 级别 |
|---|---|---|---|
| TC-01 | 窄屏 24/40 列 + 125 列提示 | B 输出无 CUU（`len(cuuParams)==0`）；网格无碎片；正文紧随 hint | P0 |
| TC-02 | 标准 80×24（最常见） | 提示折 2 行仍无散落（B4 可达面） | P0 |
| TC-03 | 中/英双语 + 40 vs 200 列 | 光标落点正确、正文段字节与文案长度无关（评审 B3 无缝写法） | P0 |
| TC-04 | 退格精度（v0.8.3 不回退） | 宽字符 2 列退格 2 组 `\b \b`；末行空行 `mergeBackspace` 按 W10 物理行算、无残留 | P0 |
| TC-05 | B7 超宽行合并 | 输入超宽行→Enter→Backspace，网格无上方物理行残留 | P1 |
| TC-06 | i18n 前缀 | 中文 `[提示]`/`[错误]`、英文 `[Info]`/`[Error]`；信任域白名单不变 | P1 |
| 回归 | 既有守卫 | `TestHintSingleCRLF`、`TestHintConstantsCarryNoPrefix`（W12 后改查表断言）、`cmdOnlyWhitelistedANSI`、两条闭环专项、archtest、四门禁、CI Race | P0 |

**红线**：不得引入 CUP/CUD/ED/DECSET 或用填充空格绕行；ADR-027 白名单零改动；`TestHintSingleCRLF` 保持"程序层仅 1 CRLF"不参数化。

## 6. 实施顺序与授权

1. 组 A（W1-W3 测试基建）→ 组 B（W4-W7 止损）→ 组 C（W8-W10 物理行模型）→ 组 D（W11-W14 i18n）→ 组 E（W15-W18 文档发版）。
2. 门禁：每组结束跑四门禁；全部完成后跑四门禁 + 两闭环 + archtest + 虚拟网格 E2E + 真实 PTY 窄屏人工核对。
3. 发版：留回退点 → commit → PR → CI（含 Race）全绿 → tag v0.9.0 → Release；**部署另行确认**（当前 VPS 仍 v0.8.2，待决定一次性升级路径）。

## 7. 待授权 / 状态

- 决策 D1-D6 已拍板；评审 B1-B7 + 小项已逐条核实属实并登记进 WBS（B7 升级纳入 W10）。
- **本次仅出方案，未编码、未建分支、未改任何代码、未做 git 写操作**；旧 `DESIGN-narrow-reflow.md` 已删除（untracked）。
- 编码、commit、tag、Release、部署均待后续明确授权；按 AGENTS.md §5 执行。
- 遗留 backlog（本期不修）：详情屏内容分页（`DESIGN-hint-reply-focus.md` O3，P1）、草稿落库（ADR-067）、全量 UI 文案翻译、RTL。
- **实施现状回填（2026-11-24，W16）**：决策 D1-D6 已拍板且全部落地；B7 已纳入 W10 并完成；W1-W17 编码、测试、文档均完成，四门禁 + 两条闭环专项 + archtest + 虚拟网格 TC-01~TC-05 + 双语参数化全绿，测试函数 1041 → 1067；新增 ADR-069（修订 ADR-067 决策 2）/ ADR-070；CHANGELOG / README / RELEASE_NOTES 已按"待发版"口径定稿。**W18（commit → PR → CI 含 Race → tag v0.9.0 → Release）与部署待用户单独授权，尚未执行**，按 `AGENTS.md` §5 执行。上方"仅出方案、未编码"等句为立项时历史留痕，不删改。
