# Release Notes — v0.7.3

**发布日期**：2026-11-23
**Git tag**：`v0.7.3`
**Module**：`github.com/jeeinn/motd`

> 补丁版本：调整主题列表行元信息顺序，回复数 `rN` 移到时间之前。无 schema 迁移、无新依赖、无新环境变量，行为与鉴权零变化。测试 990 → 991。

## 本版变化

### Changed

- **主题列表行元信息顺序：回复数 `rN` 提到时间之前**。修复前：

  ```
  >  1. [3] 主题标题 id=1 @alice 11-23 09:15 r3
  ```

  修复后：

  ```
  >  1. [3] 主题标题 id=1 @alice r3 11-23 09:15
  ```

  **改动范围**：只在 `metaTail` 的 `parts` 拼接顺序——`ScreenThreads && Replies > 0` 的 ` r%d` 段先于时间段 append。`rN` 紧凑形态、显示时机（仅主题列表、`N>0` 才显示）、分隔符与其余符号一律不动（沿袭 v0.3.3 / ADR-052）。详情页回复行、搜索结果屏本就不投影回复数，零变化。信任域边界不变：`id=` / 回复数 / 时间走 `sys()`→`RenderSystem`，作者段走 `ext()`→`RenderUser`（ADR-014/019）。

## 设计取舍

这是纯展示层排版调整，不涉及计数口径（ADR-052 的 `COUNT(status='normal') - 1`）、不涉及投影链路、不涉及数据。顺序对调不改变任何数字含义，只让「回复数」这个与主题本身更相关的统计紧邻作者段、时间退为末位。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖。替换二进制并重启即可，配置与数据零改动。

## 已知问题与限制

- 沿用 v0.7.1：`/stats` 的在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **991**。
- 新增 `TestMetaTailRepliesBeforeTime`：锁定 `id=109 @alice r3 11-14 22:13` 的组合顺序，并反向断言 `r3` 不得出现在时间之后。
- 两条闭环专项 `TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData` 绿。

## 破坏性变更

无。仅列表行元信息排版顺序变化，命令集合、权限、数据契约完全不变。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.7.3]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-052 patch 段。
