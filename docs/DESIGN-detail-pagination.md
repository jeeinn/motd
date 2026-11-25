# R-06 详情屏内容分页 — 设计方案 v2（待拍板）

> 状态：**已实施（2026-11-25，分支 feat/r06-detail-pagination）**，待真实 SSH 实测验收。
> 实施记录：`a9460f4` 行高测量+装箱分页器 → `876521e` 查询链 → `90cf464` 页面接入 → `dc79c1f` 回复锚定。四门禁 + 两闭环全绿，测试 1085 → 1112。
> v2 修订原因：v1 的方案 A（视口自适应页长）在**正文本身超过一屏**时失效——页长被钳到下限后正文仍溢出，等于没解决问题。按用户 2026-11-25 的追问重做。
> 拍板结论见 §7：**方案 D（整块装箱，单键位）+ 每页最多 20 条**。
> 优先级：**P0**（用户 2026-11-24 拍板：>200 回复时新回复不可见属功能性缺口）。
> 关联：`ROADMAP.md` R-06、ADR-066 / ADR-068（焦点回填）、ADR-070（物理行模型）、ADR-072（整屏干净重绘）。

## 0. 结论先行

**推荐：正文与回复装进同一个"页序列"，`n`/`p` 只表示"往下/往上读一页"，不新增键位。**

- 正文按**物理行**切页（用 ADR-070 的 `MeasureWrap` 口径，中文宽字符不会被切成半个字）。
- 回复按**整条**装箱，**绝不跨页切断**（一条回复只会整个出现在某一页）。
- 一页容量 = `height - 3`（标题 + 状态栏 + footer），尽量铺满但不满溢。
- 因此"正文长到三屏"和"正文只有两行"是同一套逻辑，没有特殊分支。

## 1. 现场事实（代码取证）

| # | 事实 | 位置 |
| --- | --- | --- |
| F1 | 详情屏单次拉取上限 200，`HasMore` 已算出但无人消费 | `internal/application/forum/validate.go:18`（`MaxPostsPerThread = 200`）；`service.go:212-218` |
| F2 | 仓储已支持 keyset 分页 + hasMore | `infrastructure/sqlite/repo_forum.go:278-304` |
| F3 | 端口 `PostQuery.AfterID` 存在，但 `GetThread` 不透传 | `application/forum/ports.go:66-70`；`service.go:204` |
| F4 | `Posts[0]` 被当作"主题正文"，两处依赖此契约（**分页不能改它**） | `viewmodel.go:226-234`；`page_controller.go:663-670`（`/delete` 无参取首帖 ID） |
| F5 | 只要 `Pagination.HasMore` 为真就渲染「（还有更多，按 n 加载下一页）」，详情屏的 `HasMore` 早已赋值 | `presentation/terminal/view.go:167-172`；`viewmodel.go:240` |
| F6 | footer 对除登录屏外的所有屏统一写 `n 下一页` | `view.go:226-231` |
| F7 | `nextPage` / `firstPage` 不认 `ScreenThreadDetail`，落到 `return nil` | `page_controller.go:510-535` |
| F8 | 渲染层无窗口化：`for _, r := range s.Rows` 全量输出，`TopRowIndex` 被赋值但渲染从未读取 | `view.go:114`；`viewmodel.go:165` |
| F9 | 回复成功后按 `newPostID` 回填焦点（ADR-068），前提是该回复在当前屏；**代码注释已自认缺口** | `page_controller.go:1104-1110` |
| F10 | 分层约束：`application` 不依赖 `presentation`；`viewmodel` 禁用词表含 `width`、`height` | `archtest/viewmodel_test.go:17-40` |
| F11 | 已有物理行测量工具，中文宽字符整字折、SGR 计 0 列 | `presentation/terminal/safe/wrap.go:73`（`MeasureWrap`） |

**执行计划实测**（临时探针，已删除）：10 000 帖 / 50 主题交错分布下，现有查询计划为 `SEARCH posts USING INDEX idx_posts_thread (thread_id=?)` + `USE TEMP B-TREE FOR ORDER BY`；额外建 `(thread_id, id)` 索引后计划不变。
→ **本期不需要新迁移建索引**；单帖 N 条排序 O(N log N)，N=200~1000 可接受。

## 2. 问题定义

| # | 问题 | 后果 | 定性 |
| --- | --- | --- | --- |
| P1 | 第 201 条回复起永不可达 | 新回复"消失"，用户以为没发出去；长贴无法追更 | **P0 必修** |
| P2 | 详情屏显示两处"按 n"，按键无反应 | 界面主动误导 | **同批必修** |
| P3 | 200 行全量输出，标题/正文被顶出可视区 | 焦点可能落在屏外 | 既有缺陷，本期**单列**（用户 2026-11-25 拍板） |
| P4 | 正文超一屏时无应用内翻阅手段 | 只能靠终端 scrollback 回滚 | 本期由装箱方案顺带解决 |

## 3. 候选方案

### 方案 D：整块装箱分页（**推荐**）

把详情屏内容视为一个**内容流**：正文（L 个物理行）→ 回复块 1（h1 行）→ 回复块 2（h2 行）……

- **切页规则**：每页容量 `C = height - 3`；正文按行切（可跨页续），回复按整条装（放不下就换页，该页留白）。
- **`n` / `p` 语义**：往下 / 往上一页，不区分"正文页"还是"回复页"——用户感知就是"读下一页"。
- **加载**：当 `n` 走到已加载回复的末尾且 `HasMore` 为真时，先取下一批回复（keyset 20 条）再继续翻。
- 优点：正文超长天然可分页；回复不被切断；**无新键位**；页高可控，标题与正文不会被顶出；焦点行必然落在本页内。
- 代价：需要装箱算法 + 正文按物理行切片 + 逐条测回复行数。

### 方案 E：正文与回复双段分离

正文单独翻页（如 `Ctrl-U/Ctrl-D` 或 `[`/`]`），回复固定 20 条用 `n/p`。

- 优点：实现比 D 简单，回复翻页与列表页完全一致。
- 缺点：**两套键位要记**；同屏时正文与回复会互相挤占（仍需给正文分配行预算）；正文翻页时回复去哪了要额外解释。

### 方案 A'（v1 的视口自适应，已证伪）

固定"正文常驻 + 回复按页长自适应"，正文超过一屏就把页长钳到下限 3，**正文依然溢出**——用户追问的场景正是它的失效点，不再考虑。

### 对比

| 维度 | D 装箱 | E 双段 | A' 自适应 |
| --- | --- | --- | --- |
| 正文超一屏 | ✅ 分页续读 | ✅ 单独翻页 | ❌ 仍溢出 |
| 回复跨页切断 | ✅ 不会 | ✅ 不会 | ✅ 不会 |
| 键位数量 | 1 套（n/p） | 2 套 | 1 套 |
| 焦点行可见 | ✅ | ✅ | ⚠️ |
| 实现复杂度 | 中（装箱 + 切片） | 中低 | 低但不解决问题 |
| 估工 | M~L | M | — |

## 4. 详细设计（方案 D）

### 4.1 分页器与行高测量（cmd + presentation）

**实施时修正（2026-11-25）**：原计划要新增 `safe.SliceByPhysicalLines` 按物理行切字符串，实测**不需要**——`RenderUser` 内部先调 `WrapText` 插入硬换行（`safe/sanitize.go:94`），渲染结果里「一行就是一个物理行」。因此正文只需要按 `\n` 取行区间，不存在切半个宽字符的风险，也不必新增切片工具。

改为新增**行高测量**（`internal/presentation/terminal/measure.go`），复用渲染层的同一批辅助函数，保证「测量口径 == 渲染口径」：

```go
func ContentWidth(width int) int                      // 行内容可用列数（width - prefixCol，下限 minContentCol）
func BodyLines(body string, width int) int            // 详情屏正文占用的物理行数（正文按整宽渲染）
func RowsLines(rows []viewmodel.Row, kind, width) []int // 每行占用的物理行数（逐段复刻行渲染顺序）
```

分页器是纯函数，输入行高、输出页区间，不碰渲染也不碰 IO（`cmd/server/detail_paginate.go`）：

```go
type detailPagePos struct{ BodyLine, ReplyIndex int }   // 两个分量都是"已消费量"，不是页码

type detailPage struct {
    BodyFrom, BodyTo   int   // 正文行区间 [from, to)
    ReplyFrom, ReplyTo int   // 回复下标区间 [from, to)
    Used               int   // 本页占用行数（单条过高时允许溢出）
    Next               detailPagePos
}

func paginateDetail(bodyRows int, replyRows []int, maxReplies, capacity int, from detailPagePos) detailPage
```

- **前进保证**：还有内容时至少装一个单位；单条回复高过容量时宁可溢出也不返回空页（否则调用方会在空页上死循环）。
- **不切断回复**：`ReplyTo` 只落在整条边界。
- **顺序**：正文全部翻完之前不装回复。
- **容量**：`height - 固定开销`。详情屏固定开销为 5 行（标题、`──`、`── 回复 ──`、状态栏、footer），出现下一页提示时 6 行；`height<=0` 按 24 兜底。

### 4.2 状态与键位（焦点链）

```go
type pageController struct {
    // 详情屏分页状态
    detailBodyLine int   // 正文已翻过的物理行数（0 = 从正文顶部开始）
    detailAfterID  int64 // 已加载回复的末尾游标（keyset）
    detailPage     int   // 1-based 页码，仅供提示与 p 判定
}
```

- `n`：`paginate(...)` 取下一页；若 `NextAnchor` 需要新回复且 `HasMore`，先 `ListReplies(afterID)` 再重算本页。
- `p`：回退一页；若退到正文区（`detailBodyLine > 0`），只移动正文行偏移，不动回复游标。
- 焦点：每页焦点落**本页第一条回复**；本页全是正文时焦点为空（与"暂无回复"时一致）。
- **序号连续**：`Row.Index` 用 `v.RowOffset + i + 1`，`RowOffset` 累计已翻过的回复条数，翻页后序号不重排。

### 4.3 回复后的游标定位（用户直接问的问题）

新回复永远是**最后一条**，因此不需要算页号：

1. 回复成功拿到 `newPostID`；
2. 用新增的 `ListRepliesTail(ctx, threadID, pageSize)` 取末页（`ORDER BY id DESC LIMIT n` 再反转），得到末页回复集合与各自行数；
3. 装箱：末页只装回复（正文留在前面页），按容量从末条往前装，确保 `newPostID` 在页内；
4. `restoreFocus(newPostID)` 命中，完成 ADR-068 的"焦点锚定自己刚发的回复"。

并发删除导致末页不含新回复时，退化为第 1 页并在 notice 里提示"已发布"（不报错）。

### 4.4 渲染链

- `viewmodel.Pagination` 增 `Page int`（不触 `width`/`height` 禁用词）；详情屏 footer 改为专用文案，`n` 只在"确实还有下一页"时出现，`p` 只在 `page > 1` 时出现。
- 正文片段与回复子集在 cmd 层装好后传给 viewmodel；渲染层仍按现有 `Detail.Body` + `Rows` 渲染，**不改渲染循环**（P3 窗口化单列，不动）。
- 页脚增加页码提示：`第 N 页`。

### 4.5 边界

| 边界 | 处理 |
| --- | --- |
| 单条回复本身超过一屏 | 该条独占一页（仍可能溢出）——与现状一致，登记为已知边界 |
| 正文 0 行（空正文） | 不可能：首帖正文非空由写入校验保证 |
| 回复数为 0 | 正文照常分页，不显示 `n`/`p` |
| 翻页中途有新回复 | keyset 只增不重；末页由 Tail 查询兜住 |
| 翻页中途被删 | 本页条数可能少于预估，`HasMore` 以仓储返回为准，不臆造空行 |
| resize | 页容量在**每次渲染时实时计算**（与 A' 的"进入时固定"不同）——因为装箱是纯函数，实时算不会导致内容漂移，只影响分页边界 |
| `MaxPostsPerThread = 200` | 语义改为"单批加载上限"，注释同步；不再作为"可见上限" |

## 5. 代价、风险

**生产改动**：`service.go`（新增 `ListReplies` / `ListRepliesTail`）、`viewmodel.go`（详情组装接受分页参数）、`page_controller.go`（分页状态与键位）、`view.go`（footer 与页码）、**新增 `presentation/terminal/measure.go`**（行高测量）与 **`cmd/server/detail_paginate.go`**（装箱分页）——6 文件；测试 4~5 文件。
**无 schema 迁移**（实测不需新索引）、无新依赖、无新环境变量、无新键位。

**风险**：
1. 装箱是新增的纯逻辑，边界多（正文跨页、回复放不下、末页不足）→ 需要表驱动单测覆盖。
2. 详情屏组装从"一次查询"变"两次查询 + N 次测量"，需确认 20 条量级下渲染耗时无明显上升。
3. 与 ADR-072 整屏重绘的配合：逐页渲染，每页都走 `RenderCleanFrame`，页高 ≤ height 时不应触发滚屏——需虚拟网格验证"末行不带 CRLF"。

## 6. 验证计划

1. **分页器单测**（表驱动）：正文 0/1/3/30 行 × 回复 0/1/20/250 条 × 容量 10/21/37，断言「无回复被切断」「每页总行数 ≤ 容量」「页码与游标单调」。
2. **切片单测**：`SliceByPhysicalLines` 对中文宽字符、超长行、SGR 混排的边界（不得切出半个字）。
3. **控制器测**：`n`/`p` 状态迁移（正文区 ↔ 回复区边界）、`p` 从回复首页回到正文末页、`RowOffset` 连续性。
4. **回复定位**：250 条回复的帖子发一条新回复 → 断言末页包含它且焦点命中。
5. **虚拟网格**：24 行终端下每页行数 ≤ 24 且末行不带 CRLF（不触发滚屏）。
6. **真实 SSH e2e**：250 条回复 → 连续 `n` 读到底 → `p` 回顶 → 发回复 → 焦点在自己回复上。
7. **回归**：四门禁 + 两 SSH 闭环 + archtest 全绿；测试函数数只增不减。

## 7. 拍板结果（2026-11-25）

| # | 问题 | 结论 |
| --- | --- | --- |
| Q1 | 分页模型 | **方案 D：整块装箱**。正文按物理行切、回复按整条装，`n`/`p` 统一表示"往下/往上读一页"，不新增键位。方案 E（双段分离）与 v1 的 A'（视口自适应）不采用。 |
| Q2 | 回复每页条数上限 | **行数 + 最多 20 条**。一页可装 `height-3` 行，但条数不超过 20，避免短回复时一页挤进 30~40 条。 |
| Q3 | 单条回复超一屏 | **独占一页**（仍可能溢出）。回复内部再切行会把一条完整表达切碎，且需额外键位，不做。 |
| Q4 | 页码显示 | **`第 N 页`**。不查总页数（多一次 count 查询、收益不抵成本）。 |

**实施顺序**（每步独立跑门禁，两个耦合点各自单独提交便于回滚）：

1. **工具与分页器**：`safe.SliceByPhysicalLines` + `paginate` 纯函数 + 表驱动单测（不含任何接入，零行为变化）。
2. **查询链**：`ListReplies` / `ListRepliesTail` 用例 + 仓储实现 + `/delete` 无参改走显式取正文（去掉对 `Posts[0]` 的隐式依赖）。
3. **接入与键位**：`pageController` 三个分页状态 + `n`/`p` 分支 + 详情屏 footer/页码；`MaxPostsPerThread` 语义改为"单批加载上限"。
4. **回复定位**：回复成功走 Tail 末页 + `restoreFocus`，恢复 ADR-068 语义。

**遗留（不在本期）**：P3 行窗口化（用户拍板单列）、正文内嵌图片/超长单条回复的自然溢出。
