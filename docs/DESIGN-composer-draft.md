# 提案：提交失败保留输入草稿（限流 / 只读 / 冻结不丢已写内容）

> 状态：**设计评审通过、决策已拍板（2026-11-24 v2），待实施**。未拍板项见 §10；
> 本文评审通过后再追加 ADR 与代码切片。
> 已确认口径：
> ① 交互走 **A 原地保留 + B 回屏兜底，共用同一份会话草稿**；
> ② `ErrReadOnly` / `ErrForbidden` 同样留草稿；
> ③ **只做内存版**，不落库；
> ④ **路径 A 支持「挂起回屏」**：限流草稿态按 Esc 挂起草稿回浏览屏，用户回到原帖按 `r` 即恢复草稿（用户 2026-11-24 拍板，覆盖评审原"取消"语义——见 §4.4/§4.7）；
> ⑤ 优先级 **P1 靠前**；触发频率前提经用户实测确认（回复限流可复现、提示频繁后内容丢失，痛点真实；发帖暂未触发）。
>
> 评审记录：四方（产品/技术/选品/测试）评审发现 4 个 P0 设计缺陷，已在 v2 修正（§4.2/§4.4/§4.5/§4.6）；
> 测试矩阵由 T1–T10 补强为 §8 的 P0 断言集。评审结论详见 §10.1。

## 1. 目标与范围

**痛点**：用户在编辑器里写完标题/正文，提交命中限流（60 秒 1 帖 / 1 小时 10 帖）
或只读/冻结拒绝，`submitEditor` 把编辑器整体摘除，已输入内容随缓冲一起丢弃，
用户必须等冷却结束后**逐字重打一遍**，体验很差。

**实测补充（用户 2026-11-24）**：回复场景的限流可稳定复现，提示频繁出现后回复内容丢失；
发帖场景暂未触发。痛感真实，但属"失败才发生的低频高痛感"——故定 P1 靠前，非 P0。

**本期目标**：提交被拒时不丢输入——内容存入会话草稿，冷却/问题解决后原地或
重进编辑器即自动恢复，一键重发。

**交互口径（已确认）**：

* **路径 A（原地保留）**：`RateLimitError` 短等待场景——失败后不退出编辑器，
  草稿原地重建并把正文回显出来，用户等冷却结束直接按 Ctrl-D / 单独一行 `.` 重发。
  **若等待期间想离开**：按 Esc **挂起**草稿回浏览屏（草稿保留），回到原帖按 `r` 恢复。
* **路径 B（回屏兜底）**：`ErrReadOnly` / `ErrForbidden`——失败后摘编辑器回浏览屏，
  草稿挂在控制器上；用户完成登录 / 等待解封后，重新 `/new` / `/reply` 进入同上下文
  时自动预填恢复。
* A、B 共用同一份 `pageController` 上的会话草稿，不做两套存储。

**明确不做**：

* **不做"本地客户端保存"**：SSH 架构下服务端写不到用户终端的输入缓冲或本地文件，
  本方案的等价物是服务端会话内存草稿。
* **不做断连/重登恢复**：草稿仅内存，会话结束即消失（持久化落库列入 P1，见 §9）。
* **不做倒计时自动刷新**：维持 ADR-027「屏幕只在事件时重绘」，不引入定时器刷屏。
* **不纳入 `/edit`**：编辑路径不限流、错误面为 `ErrNotEditor` / 校验错误 / 存储故障
  （`forum/edit.go:57-83`），与本痛点无关；且 `/edit` 的 `EditPost` 会覆盖原文，草稿误命中会造成数据损坏（§4.6）。
* 不改键位表、不改提交语义（ADR-034）、不改渲染、无 schema 迁移、无新环境变量。
  （"挂起回屏"复用既有 Esc 键在编辑态的语义，不新增键位；详见 §4.4。）

## 2. 证据基线（已读当前 revision）

| 事实 | 位置 |
| --- | --- |
| 提交入口：取标题 `e.runes`/`e.title`、正文 `e.body.Text()`，调 `CreateThread`/`CreatePost` | `cmd/server/page_controller.go:795-852` |
| **丢弃点**：`p.editor = nil` 在 `fail()` 之前执行，标题 runes + Composer lines 随 GC 消失 | `page_controller.go:856` |
| 失败时 `StateBrowsing` + `fail()` 只把 `Notice` 挂到旧浏览屏重绘 | `page_controller.go:857-860` |
| **`actor()` 失败（ErrReadOnly/ErrForbidden）走 `cancelEditorWithNotice(sess, err)`，函数签名无 `e/text`** | `page_controller.go:831-834`、`878-882`、`886-899` |
| 限流在写事务内、插入之前判定，失败回滚、内容本就不入库 | `forum/service.go:265-267`（发帖）、`346-348`（回复） |
| `checkRate`：先 60 秒冷却、再 1 小时配额，均带 `RetryAfter` | `forum/validate.go:65-92`；`forum/errors.go:54-64` |
| `Composer` 只有只读 `Lines()/Text()`；`Start()` 清空缓冲；`submit()` 后 `active=false`；**无文本恢复口** | `input/composer.go:106-125`、`139-160` |
| **独立 `.` 行提交由 `Feed(KeyEnter)` 的哨兵判定触发**——`Load` 若逐字符 Feed 会误触发，必须整块构造 lines | `input/composer.go:150-160` |
| `Notice.RetryAfterSec` 已携带，但终端渲染只取 `Message`，无倒计时 | `application/viewmodel/viewmodel.go:50`；`presentation/terminal/view.go:258-269` |
| `pageController` 每连接新建，会话内存天然隔离，断连即丢 | `cmd/server/ssh_handler.go:250` |
| **编辑态优先于命令表**：`pc.editor != nil` 时按键只进 `editorKey`，编辑器态不接受斜杠命令；Esc/Ctrl-C 统一走 `cancelEditor` | `ssh_handler.go:333-339`、`page_controller.go:686-700` |
| **浏览态 Esc = `actionNone`（静默、不失效屏幕）；Ctrl-C/Ctrl-D = `actionQuit`** | `ssh_handler.go:850-852`、`886-889` |
| 回显信任域：用户字节一律经 `SanitizeText`，粘贴块走 `pasteBlock`；`pasteBlock` 逐 rune 追加 `echo.cols` | `line_echo.go:47-83`；ADR-026 |
| `startComposer(creating=true)` 停在 `stageTitle`、`body=nil`；标题 Enter 确认后才 `body.Start()` 进 stageBody | `page_controller.go:647-657`、`812-820` |
| `/edit` 走 `startEditComposer`（空缓冲）→ `EditPost` 覆盖原文 | `page_controller.go:669-684`、`841-843` |

## 3. 根因

`submitEditor` 对所有写失败一视同仁：先 `p.editor = nil` 摘编辑器，再 `fail()`。
但失败分两类：**内容合法、仅时机/准入不对**（限流、只读、冻结）与**内容或上下文无效**
（空白/超长校验、目标不存在、存储故障）。前者应当保留输入，后者维持现状。
`Composer` 也没有从文本重建缓冲的入口，因此即使想保留也无处回填。

## 4. 方案

### 4.1 草稿模型（会话内存，挂在 `pageController`）

```go
// composerDraft 是提交失败后保留的输入草稿，仅存于当前 SSH 连接的控制器内存。
// 不落库、不进日志、不进 RenderSystem；回显走粘贴同一条净化路径（ADR-026）。
type composerDraft struct {
    creating   bool   // true=新主题（含已确认标题）；false=回复
    title      string // 发帖已确认的标题（回复为空）
    body       string // 正文全文，LF 连接
    nodeID     int64  // 发帖上下文
    threadID   int64  // 回复上下文
}
```

`pageController` 增加字段 `draft *composerDraft`，与 `editor` 分离——编辑器摘除后
草稿仍在（路径 B 的前提）。草稿读写**收敛到 `saveDraft` / `loadDraft` / `matches` 三个函数**，
与 A/B 交互解耦，为 P1 落库（换存储实现）降本，但本期不预留半成品接口。

### 4.2 错误分类（纯函数，可单测）

```go
// retryableDraftError 判定失败是否值得保留草稿：
// 时机/准入类（内容本身合法）保留；内容/上下文无效与存储故障不保留。
func retryableDraftError(err error) bool {
    var rl *forum.RateLimitError
    return errors.As(err, &rl) ||
        errors.Is(err, forum.ErrReadOnly) ||
        errors.Is(err, forum.ErrForbidden)
}
```

| 错误 | 路径 | 行为 |
| --- | --- | --- |
| `*RateLimitError`（60s 冷却 / 小时配额） | **A 原地保留**，可 Esc 挂起 | 存草稿 → 原地重建编辑器、回显正文、打印等待提示，保持 `StateComposing` |
| `ErrReadOnly`（访客/会话掉登录） | **B 回屏** | 存草稿 → 摘编辑器 → `fail()` 固定文案 + 追加草稿保留提示 |
| `ErrForbidden`（账号冻结/非 active） | **B 回屏** | 同 B；用户解封后重进预填 |
| `ErrNotFound`（版块/主题已不存在） | 现状 | 不存草稿，`fail()` |
| 空白 / 超长校验错误 | 现状 | 本就留在编辑态（`page_controller.go:804`），不经本分支 |
| 存储故障（`ErrInvalidNodeID` 等内部错误） | 现状 | 不存草稿，`fail()` |

> **口径统一（P1-3）**：`p.actor()` 取 `users.Status` 时若返回 `ErrNotFound`（账号被删），
> 与事务内 `checkWritePermission` 把"用户不存在"收敛为 `ErrForbidden` 的口径不一致。
> 在 `actor()` 处把账号不存在统一映射为 `ErrForbidden`（可留草稿），并补一条单测。

### 4.3 `Composer` 新增 `Load(text)`（纯应用逻辑）

```go
// Load 以全文重建缓冲并激活，供草稿恢复；替代 Start 的清空语义。
// CRLF/CR 归一化为 LF；空文本 = 单行空缓冲。超限（行数/字节）返回错误且状态不变。
func (c *Composer) Load(text string) error
```

契约（**锁死，不可逐字符 Feed**）：

* **整块构造 `lines`**：按 `\n` 切分后逐行 `[]rune`，不做 `Feed(KeyRune)`/`Feed(KeyEnter)`。
  否则正文里独立的 `.` 行会命中哨兵（`composer.go:150-160`）被误判为提交。
* CRLF/CR 归一化为 LF；复用现有 `maxLines` / `maxBytes` 与 `byteLen` 校验；
  **超限不改状态**（与 `Feed` 同款契约，初始 `active=false` 时保持 false）。
* `Load` 内部置 `active = true`，调用方不再 `Start()`（`Start` 会清空）。
* 不新增上限：草稿内容来自已通过 `clampEditor` / `validateBody` 的输入，`Load` 仅兜底。

### 4.4 路径 A：限流失败原地重建 + 挂起回屏

`submitEditor` 的写失败分支改造（替换 856-860 的统一摘除）：

```go
if writeErr != nil {
    if retryableDraftError(writeErr) {
        p.draft = p.saveDraft(e, text)          // 先存草稿
        if rl, ok := asRateLimit(writeErr); ok {
            return p.rebuildComposerFromDraft(sess, rl) // A：原地重建
        }
        // ErrReadOnly / ErrForbidden → B
        p.editor = nil
        _ = sess.SetState(presence.StateBrowsing)
        return p.failWithDraftHint(sess, writeErr)
    }
    // 不可重试：维持现状
    p.editor = nil
    _ = sess.SetState(presence.StateBrowsing)
    return p.fail(sess, writeErr)
}
```

`rebuildComposerFromDraft`：按草稿新建 `pageEditor`（`stageBody`、保留 `title`/
`nodeID`/`threadID`/`creating`），`NewComposer` + `Load(body)`，**不调 `echo.pasteBlock(body)`**，
只打印固定等待提示；**不调 `fail()` 整屏重绘、不回 `StateBrowsing`**。

> **P0-2 修正（去重复回显）**：原方案 `Load(body)` 后又 `pasteBlock(body)` 会打印第二遍正文，
> 且 `pasteBlock→rune()` 向 `echo.cols` 追加列宽导致退格错乱（`line_echo.go:75-83`）。
> 屏幕旧回显与 `Load` 后缓冲内容已一致，故**只保留 `Load`、去掉 `pasteBlock`**，
> 并在重建时 `p.echo.setSecret(false)` 复位。

**挂起回屏（用户拍板 D2，覆盖评审原"取消"语义）**：

编辑器态 Esc/Ctrl-C 统一走 `editorKey → cancelEditor`（`page_controller.go:693-695`）。
改造 `cancelEditor`：当 `p.draft != nil` 时，Esc 执行**挂起**——保留草稿、摘编辑器、
回 `StateBrowsing`、打印"草稿已保留，回到原帖按 r 恢复"；`Ctrl-C` 维持原取消语义（清草稿）。
这是**既有 Esc 键的语义扩展，不新增键位表条目**。

| 编辑态按键 | 无草稿时 | 有草稿时（A 限流后） |
| --- | --- | --- |
| `Esc` | 取消编辑（原行为） | **挂起**：保草稿、回屏、提示按 r 恢复 |
| `Ctrl-C` | 取消编辑（原行为） | 取消并清草稿（用户明确放弃） |
| `Ctrl-D` / 独立 `.` 行 | 提交 | 提交（成功即清草稿；仍在冷却则再次 A 保留） |

**回到原帖按 `r` 恢复**：`actionReply` → `startComposer(creating=false)` 检出 `p.draft`
且 `d.threadID == p.threadID` → 按 §4.5 预填。发帖草稿则 `/new` 同 `nodeID` 恢复（§4.5）。

固定文案常量（走 `RenderSystem`；剩余秒数为本地数值拼接，不拼用户文本）：

```go
// 限流原地保留：秒数由 RateLimitError.RetryAfter 本地算出后拼入。
composerDraftRetainedRateHint = "内容已保留为临时草稿（仅本次连接）：请约 %s 后按 Ctrl-D（或单独一行 .）重发；Esc 挂起草稿回浏览屏，回到原帖按 r 恢复"
// 挂起确认：
composerDraftSuspendedHint = "草稿已保留，回到原帖按 r 即可恢复"
// 只读/冻结回屏兜底（B）：
composerDraftRetainedHint = "（输入内容已保留为临时草稿，仅本次连接有效；处理完成后重新 /new 或 /reply 即可恢复）"
```

> **文案术语统一（R3/D3）**：A/B 一律用"**临时草稿（仅本次连接）**"，管理"断连即失"预期。
> B 文案**不含"Esc 放弃草稿"**——B 已回浏览屏，浏览态 Esc 静默（`ssh_handler.go:886-889`），
> 该提示是错的；放弃靠"重进不同上下文隐式覆盖"或 Ctrl-C（浏览态 Ctrl-C=退出会话，需提示）。

### 4.5 路径 B：回屏 + 重进预填

* 失败摘编辑器后，`p.draft` 保留；`failWithDraftHint` 在 `fail()` 的固定错误文案后
  追加一行草稿保留提示（含"仅本次连接"）。
* **`actor()` 失败分支必须存草稿**（P0-1）：`page_controller.go:831-834` 调用
  `cancelEditorWithNotice` 前先 `p.draft = p.saveDraft(e, text)`；或给该函数增加草稿入参。
  `cancelEditorWithNotice` 只负责"摘编辑器 + 固定文案"，不区分是否存草稿。
* `startComposer` 进入时检出草稿：

```go
// matches 按发帖/回复上下文判定草稿是否可沿用。
func (d *composerDraft) matches(creating bool, nodeID, threadID int64) bool {
    if creating {
        return d.creating && d.nodeID == nodeID
    }
    return !d.creating && d.threadID == threadID
}
```

  - **回复（creating=false）**：`stageBody`，`body.Load(d.body)` + `echo.pasteBlock(d.body)`
    （此处编辑器新建、屏幕无旧回显，故回显一次正确）+ 打印恢复提示。
  - **发帖（creating=true）**：草稿的标题在 `d.title`、正文在 `d.body`。
    恢复时**直接推进到 `stageBody`**（跳过 `stageTitle`，`e.title = d.title`），
    `body.Load(d.body)` + 回显标题行与正文 + 恢复提示（P0-4）。
  - **不匹配**：丢弃旧草稿，按原流程全新开始（`stageTitle` 或空 `stageBody`）。

### 4.6 `/edit` 排除与草稿生命周期

**`/edit` 排除（P0-4）**：`startEditComposer` **不检出草稿**（其 `editPostID > 0`，
且 `EditPost` 覆盖原文）。§4.5 的预填只在 `startComposer` 生效；`matches` 不涉及 `editPostID`。
用户在 `/edit` 期间按 Esc/Ctrl-C 也不影响回复/发帖草稿。

| 事件 | 草稿 |
| --- | --- |
| 限流失败（A）/ 只读、冻结失败（B，含 actor 分支） | 保存 |
| 限流草稿态按 `Esc` | **挂起（保留）**，回屏并提示按 r 恢复 |
| 限流草稿态按 `Ctrl-C` | 取消并清除（用户明确放弃） |
| 提交成功（A 重发 / B 恢复后 / 正常首发） | 清除 |
| 普通编辑态 `Esc` / `Ctrl-C`（无草稿） | 清除（若有）/ 原行为 |
| 导航切走（`back` / `open` / `n`/`p` / `refresh` / 斜杠命令换屏） | 保留（仅 B 与挂起后可导航；A 编辑态按键不分发命令） |
| 同上下文重进 `/new` / `/reply` 或回原帖按 `r` | 预填恢复（A 挂起后 / B） |
| 不同上下文新建 composer | 丢弃旧草稿，全新开始（输出一行固定提示，见 R4） |
| 会话结束（断连 / `quit`） | 内存自然消失 |

### 4.7 A/B 两路径的场景闭环（用户拍板后的完整链路）

| 场景 | 失败 → | 离开方式 | 恢复方式 |
| --- | --- | --- | --- |
| 60 秒冷却（短等） | A 原地重建，倒数静态提示 | 原地等 → Ctrl-D/`.` 重发；或 Esc 挂起 | 挂起后回原帖按 `r`（回复）/ 同版块 `/new`（发帖） |
| 小时配额（长等） | A 原地重建 | **Esc 挂起**回屏（本次新增出口） | 回原帖按 `r` / 同版块 `/new` |
| 掉登录 `ErrReadOnly` | B 回屏 | 已回屏 | `/login` 后回原帖按 `r` |
| 账号冻结 `ErrForbidden` | B 回屏 | 已回屏 | 解封后回原帖按 `r` |

> 这解决了评审 P0「A 无法导航、小时配额卡死」：Esc 挂起给出唯一出口，
> 且"回到帖子按 r"同时服务 A 挂起与 B 兜底，A/B 恢复入口统一。

## 5. 边界与不做项

| 场景 | 行为 |
| --- | --- |
| 草稿回显的安全边界 | 正文走 `echo.pasteBlock`（`SanitizeText` 净化），标题走 `echo.paste`；不写原始字节，ADR-026 零改动 |
| 草稿是否进日志/落库 | 否；纯 `pageController` 内存字段 |
| 60 秒内用户误触再提交 | 仍按限流判定，草稿原地保留，可反复重试；`echo.cols` 不叠加（去 pasteBlock） |
| 小时配额（等更久）期间挂起回屏 | 草稿保留，回原帖按 `r` 恢复（用户拍板 D2） |
| 断连 / 重连 | 草稿丢失（内存版既定边界），文案明确"仅本次连接" |
| `/edit` 失败 | 不纳入，维持现状；`/edit` 不检出草稿 |
| 目标被删（`ErrNotFound`）/ 存储故障 | 不存草稿，`fail()` 原行为 |
| 倒计时自动刷新、剩余秒数实时递减 | 不做（ADR-027）；秒数文案用"约 X 秒、以服务端为准" |

## 6. 需求价值与优先级（评审共识）

- **KANO**：期望型；砍掉它论坛闭环仍成立，有它则提交失败用户满意度显著提升。
- **优先级**：**P1 靠前**（用户确认触发频率前提成立——回复限流可复现、内容丢失真实；
  发帖暂未触发）。对比 ADR-066（每次返回都发生的高频）仍属低频，故不进 P0；
  改动小、确定性高，不进 P2。
- **投入产出**：约 2–4 人日，2 个源文件 + 文案常量 + 测试；零迁移、零新依赖、零审核。
- **与 ADR-010（写事务稀缺）的张力**：P1 落库（新 `drafts` 表 + TTL + 写队列）会在限流后
  新增写点，越频繁限流写队列压力越大，逻辑自相矛盾。**本期不预留、不实现**；
  抽象已解耦（`saveDraft`/`matches`/`Load`），P1 换存储实现即可，须另立 ADR 评估写队列背压。

## 7. 实施清单

1. `internal/application/input/composer.go`：新增 `Load(text)`（§4.3 契约），补纯单测 T1/T2 + 独立 `.` 行不误判。
2. `cmd/server/page_controller.go`：
   - `composerDraft` + `p.draft`；`retryableDraftError`；`saveDraft`/`loadDraft`/`matches`；
   - `submitEditor` 失败分支分流（§4.4）；
   - **`actor()` 失败分支补存草稿**（§4.5，P0-1）；账号不存在口径统一为 `ErrForbidden`（§4.2）；
   - `rebuildComposerFromDraft` 去 `pasteBlock` + `setSecret(false)`（P0-2）；
   - `cancelEditor` 有草稿时 Esc 挂起 / Ctrl-C 清除（§4.4）；
   - `startComposer` 匹配预填，发帖草稿直接进 stageBody（§4.5，P0-4）；
   - `startEditComposer` 不检出草稿（§4.6，P0-3）；
   - 成功路径清草稿；`failWithDraftHint` 在 `fail()` 后追加固定提示。
3. 固定文案常量（§4.4），走 `RenderSystem`；正文回显走 `pasteBlock`，不拼用户文本。
4. 测试（§8 P0 断言集），全量四门禁 + 两条闭环专项。
5. 文档：评审通过后追加 ADR（顺延 ADR-066 → ADR-067）；CHANGELOG `[Unreleased]` 加一条；
   `OPS.md` 编辑器小节补"临时草稿仅本次连接、Esc 挂起、回帖 r 恢复"行为。
6. 无 schema 迁移、无新环境变量、无键位/渲染改动；README 无需同步。

**P1 候选（本期不做）**：草稿持久化落库（新 `drafts` 表 + TTL 清理仿 ADR-035 + 唯一写队列），
解决断连/重登恢复；受 ADR-010 写事务稀缺约束，另立 ADR 评估，登记 ADR 号但不实现。

## 8. 测试矩阵（含评审补强的 P0 断言）

在原 T1–T10 基础上，补以下 P0 级可执行断言（未补不作为放行依据）：

| 编号 | 用例 | 断言 | 级别 |
| --- | --- | --- | --- |
| TC-A01 | 限流 A 回显后 `body.Text()` 恰等于原正文一次 | 输出无第二份正文，`echo.cols` 未叠加 | P0 |
| TC-A02 | advance 时钟 → Ctrl-D 提交 → 落库 1 条且正文完整 | `pc.draft == nil`，正文逐字节一致 | P0 |
| TC-A03 | `retryableDraftError` 纯函数正/反分流 | 含 nil、校验错误、`ErrNotFound` 反例 | P0 |
| TC-A04 | B 路径多行+CRLF 预填后逐字节完整 | `"行1\r\n行2"` → `"行1\n行2"` | P0 |
| TC-A05 | 草稿回显含 ESC/CSI/OSC52/U+202E/CR 的净化 | 仅白名单 ANSI（`cmdOnlyWhitelistedANSI`） | P0 安全 |
| TC-A07 | A 重发成功 / B 恢复后成功两条闭环均清草稿 | 两条路径各自 `pc.draft == nil` | P0 |
| TC-A08 | **A 挂起回屏 → 回原帖按 r 恢复**（用户拍板 D2） | Esc 后 `pc.editor==nil && pc.draft!=nil`、回屏；`r` 进编辑器后 `Text()` 逐字节一致 | P0 |
| TC-A09 | **actor() 失败（访客/冻结）存草稿**（P0-1） | `pc.draft!=nil`，B 文案含"仅本次连接" | P0 |
| TC-A10 | **发帖草稿恢复进 stageBody**（P0-4） | `e.title==d.title`、正文预填、无标题阶段 | P0 |
| TC-A11 | **/edit 不检出草稿**（P0-3） | `/edit` 进空缓冲，`pc.draft` 未被预填 | P0 |
| TC-B01–B13 | ErrReadOnly/ErrForbidden 独立、`matches` 四分支、导航方式全覆盖、异上下文丢弃、校验错误不进草稿、编辑器态命令表隔离、固定文案无用户数据 | 见成员评审 TC-B 清单 | P1 |
| TC-C01–C03 | SSH 客户端×TERM×按键字节流矩阵；长正文 Load 输出量基准；并发会话草稿内存上界 | 兼容/性能观察 | P1–P2 |

**放行门禁**：P0 bug = 0；核心链路（A 重发落库、B 恢复完整、挂起恢复、信任域净化）100%；
四门禁全绿 + 两条闭环专项 + archtest；异常流用例占比 ≥ 正常流一半。

## 9. 二期 backlog（本期不做）

- 草稿持久化落库（`drafts` 表 + TTL 清理 + 写队列），解决断连/重登恢复；另立 ADR 评估 ADR-010 背压。
- 倒计时自动刷新（若未来放宽 ADR-027）。
- `/edit` 草稿（如确有需求，独立评估 `EditPost` 覆盖语义）。

## 10. 评审与决策记录

### 10.1 四方评审共识（2026-11-24）

- **需求**：真痛点（低频高痛感、期望型），P1 靠前；与 ADR-066 同为"控制器层纯状态修补、减少重复劳动"主线。
- **架构**：基本合理，草稿挂 `pageController` 符合会话隔离；信任域红线（pasteBlock/SanitizeText/RenderSystem）守住。
- **发现 4 个 P0 设计缺陷**，v2 已全部修正：① actor 失败分支漏存草稿（§4.5）；② 路径 A 重复回显+列宽污染（§4.4）；③ B 文案"Esc 放弃草稿"与实际静默行为不符（§4.4）；④ `/edit` 预填冲突+覆盖原文风险（§4.6）。
- **测试**：T1–T10 覆盖约 6–7 成，缺 P0 断言；§8 已补 TC-A 系列。

### 10.2 用户拍板（2026-11-24）

| 决策 | 结论 |
| --- | --- |
| D1 B 场景"放弃草稿"入口 | 去掉"Esc 放弃"提示，靠"重进不同上下文隐式覆盖"放弃；Ctrl-C（浏览态=退出）需提示 |
| D2 A 的小时配额长等待 | **支持"挂起回屏"**：Esc 挂起草稿回屏，回原帖按 `r` 恢复（覆盖评审原"取消"语义） |
| D3 优先级 | P1 靠前；触发频率前提经实测确认（回复可复现、内容丢失；发帖暂未触发） |
| D4 P1 落库 | 登记 ADR 号但不实现，避免被当成承诺 |
| D5 文案术语 | 统一"临时草稿（仅本次连接）"，管理断连即失预期 |

## 11. 待授权

设计已评审通过、决策已拍板；待明确授权后进入代码切片（§7）。
commit/tag/push 按 AGENTS.md §5 另行确认。
