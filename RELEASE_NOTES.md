# Release Notes — v0.8.1

**发布日期**：2026-11-24
**Git tag**：`v0.8.1`
**Module**：`github.com/jeeinn/motd`

> 补丁版本：`q` 返回列表时按行 ID 回填焦点，从详情/子列表返回后光标停在刚才那一项，续读不再需要从首行重新找帖。无 schema 迁移、无新依赖、无新环境变量、无键位/渲染改动；测试函数 1011 → 1014。

## 本版变化

### Changed

- **`q` 返回列表保持焦点**。修复前：从帖子详情按 `q`（或 `h` / ←）返回版块列表，光标总落在第一行，用户要重新往下找刚读的那篇。修复后：
  - 详情 → 主题列表：焦点回到**原帖行**；
  - 主题列表 → 版块节点列表：焦点回到**原节点行**；
  - 按**行 ID**（`thread.ID` / `node.ID`）在新列表里重新定位，不按显示序号——列表按最新排序重查后同一帖可能换位，按序号会落到另一篇。

  **未命中自动回退首行**：原帖被软删、检索详情返回节点列表（`nodeID=0`）等场景保留原行为，不报错、不落空焦点。`n` / `p` 翻页、`refresh` 刷新、删帖重建仍落首行；`restoreFocus` 只挂在 `back()` 一条路径上。

## 设计取舍

- 这是控制器层的纯状态修补：`back()` 重建屏后按锚点 ID 回填 `FocusedIndex` / `TopRowIndex`，不碰用例、仓储、渲染与键位（ADR-066）。
- 锚点必须在重建前捕获：`showNodes` 会把 `nodeID` 清零，故列表 → 节点返回前先存 `p.nodeID`。
- 双渲染：`show*` 内部已渲染首行焦点帧，回填后再 `render` 一次，由 `RenderFrame`（ADR-027）原地覆盖；`back()` 是低频动作，多一帧重绘可接受，避免为单次回填重构渲染职责。
- **数字直达（P0-a，按 1–9 直接打开行）本期不立项**：它解决的是「新列表远距离跳行」，与本续读痛点无关，待本特性落地后单独评估（含两位数缓冲候选）。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖、无键位变化。替换二进制并重启即可，配置与数据零改动。这是纯交互行为改进，不改变任何命令、权限与数据契约。

## 已知问题与限制

- 返回时多一帧原地重绘（低频，视觉无感）。
- 列表数据在打开详情期间变化时，定位的是「同一篇」，但屏幕位置以返回时的新排序为准。
- 沿用 v0.8.0：`/stats` 的在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出（含未跟踪新增文件）、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **1014**。
- 新增 `back_focus_test.go` 3 项：详情返回定位原帖（`TestBackFromDetailRestoresThreadFocus`）、原帖软删回退首行（`TestBackFromDetailDeletedThreadFallsBackToFirst`）、节点返回定位原节点（`TestBackFromThreadListRestoresNodeFocus`）。
- 既有导航/重绘/检索返回测试全绿不破：`TestPageControllerNavigation`、`TestNavigationRedrawsInPlace`、`TestBackFromSearchDetailGoesToNodeList`。
- 两条闭环专项 `TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData` 绿；archtest 绿。

## 破坏性变更

无。命令集合、权限、数据契约、键位完全不变，仅改变 `q` 返回时的焦点落点。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.8.1]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-066，以及设计稿 [`docs/DESIGN-back-focus.md`](docs/DESIGN-back-focus.md)。
