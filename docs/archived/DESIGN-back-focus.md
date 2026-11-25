# 提案：`q` 返回列表时保持焦点（P0-b），不做数字直达

> 状态：**设计已评审、决策已拍板（2026-11-24），代码已实施、待发版**。
> ADR-066 已追加至 `docs/DECISIONS.md`，CHANGELOG `[Unreleased]` 已同步；
> 实施产物为 `page_controller.go` 的 `restoreFocus` + `back()` 两分支与 `back_focus_test.go` 三项。
> 证据基线：v0.8.0 已发布（`58d11cc`），本特性作为 v0.8.1 发布。

## 1. 目标与范围

**痛点**：从帖子详情按 `q`（或 `h` / ←）返回版块列表后，光标总是落在第一行，
用户得重新往下找刚才读的那篇。

**本期唯一目标（P0-b）**：返回列表后，焦点停在**刚才那一项**上，续读不再需要方向键。

v1 范围（刻意收窄）：

* 详情屏 → 版块主题列表：焦点回到原帖行。
* 主题列表 → 版块节点列表：焦点回到原节点行。
* 按**行 ID** 定位（不是按显示序号）；未命中（帖子被删 / 检索来源等）回退首行。

**明确不做**：

* **数字直达（P0-a，按 1–9 直接打开行）**：本期不立项。它解决的是「新列表远距离跳行」，
  与续读痛点无关；留待本特性落地后单独评估（含两位数缓冲候选）。
* 不做滚动记忆之外的任何新交互；不新增命令、不改键位、不改渲染。

## 2. 证据基线（已读当前 revision）

| 事实 | 位置 |
| --- | --- |
| `screenOf` 每次重建屏都把焦点重置为首行：`focus = rows[0].Index` | `internal/application/forum/viewmodel.go:325-340` |
| `back()` 两条分支直接调 `showThreads`/`showNodes` 重建屏，不带任何焦点记忆 | `cmd/server/page_controller.go:396-407` |
| `back()` 仅由 `key(actionBack)` 一处调用 | `cmd/server/page_controller.go:445` |
| 行 `Row.ID` 语义：节点行 = `node.ID`；主题行 = `thread.ID`；检索行 = `threadID`；详情回复行 = `post.ID` | `forum/viewmodel.go:342-352`、`354-366`、`253-259`、`368-382` |
| 打开主题后 `showDetail` 存 `p.threadID`；`showThreads` 存 `p.nodeID/nodeSlug/cursor/offset`；`showNodes` 把 `nodeID` 清零 | `page_controller.go:317-330`、`313`、`278` |
| 焦点校验只要求 `FocusedIndex ≤` 末行 Index；`focusedRow()` 找不到行返回 `false` | `internal/application/viewmodel/viewmodel.go:230-235`、`page_controller.go:370-377` |
| 渲染只消费 `FocusedIndex`（`>` 标记）；`TopRowIndex`、`SelectedID` 当前无渲染消费 | `internal/presentation/terminal/view.go:101-105`；全仓 `SelectedID` 仅声明 |
| 返回复用 `p.cursor/p.offset`，回到同一页 | `page_controller.go:399`；`forum/validate.go:16` 默认页长 20 |
| 测试 harness：`setupMVPRuntime`（内存 sqlite + 迁移）、`seedThread`、`pc.key(actionBack)` 驱动 | `cmd/server/page_controller_test.go:25-62`、`latest_command_test.go:23-40`、`latest_command_test.go:167-204` |

## 3. 根因

`back()` 调 `showThreads`/`showNodes` → 用例重建 `Screen` → `screenOf` 无条件把
`FocusedIndex` 设为首行。控制器上虽保留了 `p.threadID` / `p.nodeID` 锚点，
但重建后没有任何回填动作。

**不能按显示序号（`Index`）回填**：`Index = RowOffset+i+1` 是页内位置号，列表按最新
排序重查后同一帖可能换了位置，按序号回填会落到另一篇上。必须按 `Row.ID` 重新定位。

## 4. 方案

### 4.1 新增私有辅助 `restoreFocus`

```go
// restoreFocus 在重建屏后按锚点 ID 回填焦点；未命中或锚点 <=0 时保留 screenOf
// 的首行默认，不改动屏幕。
func (p *pageController) restoreFocus(anchor int64) {
    if anchor <= 0 {
        return
    }
    for _, r := range p.screen.Rows {
        if r.ID == anchor {
            p.screen.Selection.FocusedIndex = r.Index
            p.screen.Selection.TopRowIndex = r.Index
            return
        }
    }
}
```

* 只改 `p.screen.Selection`，不碰用例、仓储、渲染。
* `TopRowIndex` 一并回填保持语义完整；当前渲染不消费它，未来滚动记忆可直接用。

### 4.2 `back()` 两个分支的锚点捕获

```go
func (p *pageController) back(ctx context.Context, sess pageSession) error {
    switch p.screen.Kind {
    case viewmodel.ScreenThreadDetail:
        anchor := p.threadID // 必须在 showThreads 之前捕获（showThreads 不改 threadID）
        if err := p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset); err != nil {
            return err
        }
        p.restoreFocus(anchor)
        return p.render(sess) // showThreads 已渲染一次，回填后再渲染一次覆盖标记
    case viewmodel.ScreenThreads:
        anchor := p.nodeID // showNodes 会把 nodeID 清零，必须先捕获
        if err := p.showNodes(ctx, sess); err != nil {
            return err
        }
        p.restoreFocus(anchor)
        return p.render(sess)
    case viewmodel.ScreenSearchResults:
        return p.showNodes(ctx, sess) // anchor=0，无回填
    default:
        return nil
    }
}
```

**双渲染说明**：`showThreads`/`showNodes` 内部已 `render`（首行焦点帧）；回填后再
`render` 一次，`RenderFrame` 在屏幕连续时原地覆盖（ADR-027 机制不变）。`back()` 是
低频动作，多一帧重绘可接受，避免为单次回填重构 `show*` 的渲染职责。

### 4.3 锚点在各来源下的行为（自动正确，无需特判）

| 来源 | back 目标屏 | 锚点 | 结果 |
| --- | --- | --- | --- |
| 版块列表打开详情 → q | ScreenThreads（同页） | `p.threadID` | 命中原帖行 |
| /latest 打开详情 → q | ScreenThreads（latest 同页） | `p.threadID` | 命中原帖行 |
| 检索结果打开详情 → q | ScreenNodes（`nodeID=0` 重定向） | `p.threadID` | 节点行不匹配 → 首行（不破既有 `TestBackFromSearchDetailGoesToNodeList`） |
| 节点列表打开版块 → q | ScreenNodes | `p.nodeID` | 命中原节点行 |

## 5. 边界与不做项

| 场景 | 行为 |
| --- | --- |
| 原帖被删 / 被治理，ID 不在新列表 | 回退首行（`restoreFocus` 未命中，保留默认） |
| `n`/`p` 翻页、`refresh` 刷新、删帖后重建 | **不保留焦点**，`screenOf` 自然落首行（`restoreFocus` 只挂在 `back()`） |
| 检索详情回节点列表 | 首行（既有行为） |
| 详情屏回复列表本身 | 不参与回填 |
| 命令输出、编辑器中断后的重建 | 不参与 |
| P0-a 数字直达 / 两位数缓冲 | 本期不立项 |

## 6. 测试矩阵（`cmd/server` 既有 fake 链路）

| # | 用例 | 断言 |
| --- | --- | --- |
| T1 | 版块内两条主题（不同排序），打开第二条 → q 返回 | `FocusedIndex ==` 原帖行 `Index`，不是首行 |
| T2 | 打开后删掉原帖 → q 返回 | 未命中回退首行 `FocusedIndex == Rows[0].Index` |
| T3 | 节点列表打开第二个节点 → q 返回节点列表 | `FocusedIndex ==` 原节点行 `Index` |
| T4 | 检索结果打开详情 → q | 仍回 ScreenNodes 首行，`位置:` 不空（既有测试不破） |
| T5 | 既有 `TestPageControllerNavigation`、`TestNavigationRedrawsInPlace` 等导航/重绘测试 | 全绿，输出仍只含白名单 ANSI |

状态断言为主（`pc.screen.Selection.FocusedIndex`）；`>` 标记位置可抽查，不逐字节比对。

## 7. 实施清单

1. `page_controller.go`：新增 `restoreFocus`；改写 `back()` 两个分支（§4.2）。
2. 新增/扩充测试（§6），全量四门禁 + 两条闭环专项。
3. 文档：追加 **ADR-066**（`docs/DECISIONS.md`）；CHANGELOG `[Unreleased]` 加 Changed 一条。
4. 无 schema 迁移、无新环境变量、无键位/渲染改动；README、eggs.md 无需同步。

## 8. 待授权

决策已固化；待明确授权后进入代码切片。commit/tag/push 按 AGENTS.md §5 另行确认。
