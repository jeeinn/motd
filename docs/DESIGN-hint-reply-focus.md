# 决策记录：引导提示 `[提示]` 前缀 + 回复后焦点指向自己的回复

> 状态：**决策已拍板（2026-11-24），待实施**。用户已同意推荐方案，但明确要求"先不进入编码，
> 先写执行决策文档方便评审追踪"。本文固化讨论结论与实施口径；代码切片与 ADR 待授权后另起。
> 关联：灰阶 hint 已实施并提交（`e515b09`，分支 `ui/hint-dim-prefix`，未 push/未发版）；
> 本决策的两个优化点在该基础上继续，预计合并为一个补丁切片（建议版本 v0.8.4，待确认）。
>
> 证据基线：`main` = `dd8eb95`（v0.8.3 已发版）；工作分支 `ui/hint-dim-prefix` = `e515b09`（灰阶 hint）。

## 1. 决策摘要

| # | 优化点 | 用户诉求 | 拍板结论 | 改动量 |
|---|---|---|---|---|
| O1 | 引导 hint 加 `[提示]` 前缀 | "和发布成功一样增加 `[提示]`，让用户感知明确" | **采纳方案 A**：`writeHint` 内部统一拼 `[提示] `，覆盖全部引导/草稿状态提示；错误提示不加 | 约 4 行 + 测试 |
| O2 | 回复后焦点指向自己的回复 | "发表回复后 `>` 默认指向第一条，建议指向自己的回复，但要考虑分页" | **采纳方案 A**：复用 ADR-066 的 `restoreFocus(postID)`，未命中回退首行 | 约 6 行 + 测试 + ADR-068 |
| O3 | 详情屏分页缺陷 | 讨论中发现，非用户直接诉求 | **登记 backlog，本期不修**：详情屏 n/p 无效 + 200 帖截断致新回复不可见 | 独立 P1 工单 |

## 2. 背景与用户证据

用户本地实测 v0.8.3（暂未部署 VPS），截图反馈两点：
- 引导 hint 改灰阶后观感正常（已落 `e515b09`），但相比"发布成功"的 `[提示] 已发布`，缺少方括号锚点、感知不够明确；
- 发帖成功后详情页 `> ` 指向第一条回复（`1. 完美，完美`），自己刚发的 `2. 不断刷新回复` 没有指示器，期望 `> ` 指向自己的回复。

## 3. O1 — 引导 hint 加 `[提示]` 前缀

### 3.1 现状与源证据

`[提示]` 与 hint 分属**两条独立输出通道**：

| 维度 | 通道①：Notice（发布成功） | 通道②：hint（引导行） |
|---|---|---|
| 承载 | `Screen.StatusBar.Notice` 字段，整屏渲染 | `writeHint` 独立输出行（非 Screen 字段） |
| 前缀 | `noticePrefix(kind)` → `[错误]`/`[提示]`（`view.go:275-284`） | 无 |
| 样式 | default 分支 `[提示]` = **`StyleDim` 灰阶**（`view.go:282`） | `StyleDim` 灰阶（`writeHint`，`page_controller.go:294`） |
| 渲染 | `RenderScreen`→`RenderFrame`，参与 ADR-027 连续 | 独立行 + `\r\n`，`invalidateScreen()` 打断连续 |

**关键事实**：发布成功的 `[提示]` 本身就是灰阶（`noticePrefix` default → StyleDim），与刚改的 hint **同色**。所以"发布成功更醒目"的感觉来自**方括号文字锚点**，不是颜色——加前缀可逐字节对齐观感，无需分段染色。

### 3.2 候选方案对比

| 方案 | 做法 | 评价 |
|---|---|---|
| **A（采纳）** | `writeHint` 内部统一拼 `[提示] ` 服务端常量，覆盖全部 9 处 | 单点、单一事实来源、零 ADR/白名单触碰、约 4 行 |
| B | 9 个 hint 常量各自手写前缀 | 重复 9 处、含带 `%s` 的限流提示、易漏、无额外收益 → 否决 |
| C | 新增 NoticeKind 让 hint 走 notice 通道 | 需给编辑器态塞 Screen 字段 + 整屏重绘，覆盖回显、破坏 ADR-026/027 → 方向性错误，否决 |
| D | 前缀染琥珀色、正文灰色 | 比"发布成功"还醒目，**偏离"一样"的诉求** → 仅作未来备选 |

### 3.3 实施口径（待编码）

文件：`cmd/server/page_controller.go`
1. hint 常量块新增 `const hintPrefix = "[提示] "`；
2. `writeHint` 改为 `safe.RenderSystem(hintPrefix + text, safe.StyleDim)`（1 行）；
3. 注释同步：说明前缀语义对齐 `noticePrefix` 的 Success 分支；顺带修正限流重建处注释里 `writeFixed 尾部 CRLF` → `writeHint 尾部 CRLF`（命名漂移）。

**一致性决策（明确边界，不模棱两可）**：

| 类别 | 加 `[提示]`？ | 理由 |
|---|---|---|
| 引导 hint（新建/回复/编辑/标题后正文） | **加** | 诉求本体，方案 A 自动覆盖 |
| 草稿状态提示（恢复/挂起/保留/丢弃/限流等待） | **加** | 同属告知类轻量反馈，一致性要求一起加 |
| 输入校验错误（"内容不能为空"、超限） | **不加** | 走 `writeFixed`/StylePlain 普通色即最高对比；"错误"挂"提示"标签是语义降级 |
| 业务错误（`fail()`→NoticeError） | **不加** | 已有 `[错误] ` 语义（`view.go:277`），须保持一致 |
| 取消/失效反馈（"（已取消编辑）"、"草稿已失效，请重新输入"） | **不加** | 走 `writeFixed` 普通色，与即时校验错误同一通道；既非告知类也非错误，保持无前缀 |
| `commandEntryHint`（第三条裸写通道） | **本期不动** | 超范围 |

**一句话规则**：前缀按"通道语义"分配，不按"醒目程度"分配——`[提示]` 归告知类（writeHint + 非错误 Notice），`[错误]` 归错误类，即时校验错误保持无前缀普通色。

### 3.4 风险与边界

- 极窄终端长 hint 物理折行：`RenderSystem` 不折行、程序层不加 `\n`，是终端软折行；加前缀仅 +7 显示列，扩大触发面但非新增缺陷。ADR-027 窄屏降级只保护整屏渲染、不保护独立行——记录为既有边界。
- `resyncLastLine` 的 `up=2` 不受影响：`up=2` = 提交 `echo.newline()` 1 行 + hint 尾部 CRLF 1 行，加前缀不增 `\n`。
- 信任域/白名单零改动：前缀是服务端固定串走 `RenderSystem`，用户文本仍走 `SanitizeText`；`\x1b[2m` 是已入白名单 SGR。

## 4. O2 — 回复后焦点指向自己的回复

### 4.1 现状与源证据

- `screenOf`（`internal/application/forum/viewmodel.go:325-340`）**无条件**设 `FocusedIndex = rows[0].Index`(`viewmodel.go:328`)——根因。
- 回复成功后走 `showDetail`（`page_controller.go` 成功分支），其 `v.RowOffset = 0`（`:379`）回第一页，焦点自然是第一条。
- `detailRows(d.Posts[1:], v.RowOffset)`（`viewmodel.go:233,368`）把首帖之后的 Post 作为回复行；`Row.ID = p.ID`。
- ADR-066 已有 `restoreFocus(anchor int64)`（按 `Row.ID` 在新屏定位，未命中回退首行）——可直接复用。

### 4.2 分页语义澄清（讨论中的关键发现）

| 问题 | 准确结论 | 依据 |
|---|---|---|
| 详情屏有 n/p 翻页吗 | **没有**。`/nextPage` 与 `firstPage` 只处理 ScreenThreads/SearchResults，详情屏直接 return nil | `page_controller.go:495-520`、`ssh_handler.go:881-884` |
| 上限多少 | `MaxPostsPerThread = 200`，注释明写"P1 再做内容分页" | `validate.go:18` |
| `HasMore` 来源 | `ListPosts` 用 `LIMIT limit+1` 多取一条，`hasMore=(len>limit)` 后截断；`AfterID=0`、`id ASC` 取**最小 ID 的前 200 条** | `repo_forum.go:278-303`、`service.go:204-218` |
| 新回复可能不在集合吗 | **会**。新回复 ID 最大，已有 ≥200 条可见 Post 时不在返回集合内，无从定位 | 同上 |
| 常规情形 | <200 条时回复全量返回，新回复必在 Rows，可稳定命中 | `detailRows(d.Posts[1:], 0)` |

### 4.3 候选方案对比

| 方案 | 做法 | 评价 |
|---|---|---|
| **A（采纳）** | `submitEditor` 捕获 `CreatePost` 返回的新 postID → 成功分支 `showDetail` → `restoreFocus(postID)` + 二次 `render` | 仅控制器层约 6 行，不动用例/仓储/viewmodel；机制与 ADR-066 一致；未命中回退首行 |
| B | `showDetail` 支持锚点并自动翻到锚点所在页 | 要重定义分页契约、引入详情 cursor、让 `nextPage` 支持详情——工作量大，独立 P1 立项，与"交互优化"体量不匹配 |
| C | 只在首屏命中时定位 | 与 A 行为几乎相同（A 未命中本就回退），仅作 A 的实现细节（命中才二次 render） |
| D | 不动焦点，改用"我的回复"标记 / `SelectedID` | `SelectedID` 全仓仅声明无消费；解决不了"看不到自己回复" → 不推荐 |

### 4.4 实施口径（待编码）

文件：`cmd/server/page_controller.go`（全部在控制器层）
1. `submitEditor` 增 `var newPostID int64`；`CreatePost` 返回值由丢弃改为 `newPostID, writeErr = ...`；
2. 回复成功分支：先判 `showDetail` 的 err（照 `back()` `:459-461` 模式，失败直接 `return err`，不在失败屏上回填焦点），成功后调 `p.restoreFocus(newPostID)`（复用现有函数，不改其实现），再 `return p.render(sess)`（沿用 ADR-066 双渲染）；形态：

   ```go
   if err := p.showDetail(ctx, sess, threadID); err != nil {
       return err
   }
   p.restoreFocus(newPostID)
   return p.render(sess)
   ```
3. 发帖（`creating=true`）分支不动；编辑（`editing`）分支本期**不做**焦点锚定；
4. 文档：新增 **ADR-068** 记录场景扩展（ADR-066 原文不改——其"只挂 back()"的口径保持，本次是扩展使用场景）。

**边界**：
- 并发插入他人回复：按 ID 精准命中自己的，不受排序/插入影响；
- 软删 / 超 200 条未命中：`restoreFocus` 自然回退首行，不劣化现状；
- 发帖成功（非回复）：焦点仍为列表首行，不受影响。

### 4.5 产品视角与风险核对

- `> ` 语义：仅 `view.go:101-105` 比较 `r.Index == FocusedIndex` 打印 `> `，是**键盘光标位置**，不是"选中/赞同/已读"；无额外业务语义绑定，指向自己回复无副作用。
- 误操作风险：已核对 `/delete`、`/edit` 不带参时指向主题/首帖（`:603/648-657`），详情屏 `open` 为 no-op（`:448`），移动光标不会让误删/误改指向自己回复。
- 与 `[提示] 已发布` 不冲突：notice 在 `showDetail` 时已写入 Screen，二次 render 消费同一 Screen，前缀与落点并存。

## 5. O3 — 讨论中发现的既有缺陷（登记 backlog，本期不修）

**缺陷**：详情屏 `HasMore=true` 时渲染"（还有更多，按 n 加载下一页，p 回第一页）"（`view.go:154-159`），但 `nextPage()`/`firstPage()` 对详情屏**直接 return nil**（`page_controller.go:495-520`）——提示与行为不一致；且超 200 帖时新回复不在返回集合内，与"发表后可见"的产品预期有 gap。

**处置**：登记为独立 P1 工单（详情屏内容分页），另立 ADR 评估用例/仓储/渲染层改动；**不塞进本次优化**。

## 6. 测试口径（待编码时落实）

| 编号 | 用例 | 断言要点 |
|---|---|---|
| O1-T1 | 引导 hint 前缀 | 9 处 hint 输出含 `[提示] ` 与 `\x1b[2m`；错误提示（内容为空/超限）不含前缀 |
| O1-T2 | 单一事实来源 | hint 常量字面量均不含 `[提示]`，前缀只出现在 `writeHint` 输出（防回退到手写） |
| O1-T3 | 窄屏回归 | 宽度 24 列下 writeHint 输出仅 1 个 `\r\n`（程序层不折行） |
| O2-T1 | 正常回复 | 首帖+1 回复，回复成功 → `FocusedIndex == indexOfRow(newPostID)`、notice `saved`、`> ` 在自己那行 |
| O2-T2 | 并发插入 | 插入他人回复后焦点仍锁定自己 postID |
| O2-T3 | 软删 | 新回复被软删 → 回退首行、Screen 校验通过 |
| O2-T4 | 超上限 | 预置 200 条后回复 → 焦点保持首行、不 panic |
| O2-T5 | 发帖不误伤 | `creating=true` 回列表后焦点仍首行，notice 不变 |
| 回归 | 既有测试 | `back_focus_test.go` 3 项、`composer_draft_test.go` 全套、两条闭环专项、archtest、四门禁 |

## 7. 版本与实施顺序

- 两个优化**合并为一个补丁切片**（用户已同意），建议版本 **v0.8.4**（最小 patch，待发版时确认）。
- 顺序：先 O1（4 行）→ 再 O2（6 行 + ADR-068）→ 跑全部门禁 → 留回退点 → commit → PR → CI 全绿 → tag → Release →（如需）部署。
- 本次灰阶 hint 已提交在 `ui/hint-dim-prefix`（`e515b09`），尚未 push/发版；两个优化在该分支或新分支上继续，由实施时决定。

## 8. 待授权 / 状态

- 用户已拍板 O1=方案 A、O2=方案 A、O3=登记不修、两者合并切片。
- **用户明确要求：先写决策文档，不进入编码**（本文即该文档）。
- 编码、commit、tag、Release、部署均**待后续明确授权**；按 AGENTS.md §5 执行。
