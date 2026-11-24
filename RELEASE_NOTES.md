# Release Notes — v0.8.3

**发布日期**：2026-11-24
**Git tag**：`v0.8.3`
**Module**：`github.com/jeeinn/motd`

> 补丁版本：修 v0.8.2 草稿恢复的回显光标与 `echo.cols` 错位——提示与正文粘连、恢复后退格无法精确擦除。无 schema 迁移、无新依赖、无新环境变量、无键位/渲染改动；测试函数 1028 → 1032。

## 本版变化

### Fixed

- **草稿恢复提示与正文粘连**：B 恢复与 A 挂起路径（`resumeDraft`）原在正文回显之后写"草稿已恢复"提示，而 `writeFixed` 自带 CRLF——提示先贴在正文末字后（"…试一试草稿已恢复：…"），再把光标甩到下一行。修法：提示前置到正文回显之前，最后输出的是正文，光标停在正文末字、`echo.cols` 与之一一对应。

- **草稿恢复后退格无法精确擦除**：
  - **B / 挂起恢复**：同上一处根因，提示的 CRLF 让光标所在行与 cols 描述的行错位，退格擦在错误位置；提示前置后一并修复。
  - **A 限流原地重建**：`Load` 重建缓冲后 `echo.cols` 被提交换行与 `setSecret(false)` 双清为空，退格会删掉缓冲里的字，但 `backspace` 因 cols 为空静默早退、屏幕零擦除——缓冲与屏幕永久分叉。新增 `line_echo.resyncLastLine`：仅用 ADR-027 白名单的 `CursorUp` + `CR` + `ClearToEOL` 回到正文末行重写一次并重建 cols（用户文本仍经 `SanitizeText`），只重写末行、不重复整段正文。

## 设计取舍

- 不扩展 ADR-027 的 ANSI 白名单（未新增 CUP/CUF/ED），`resyncLastLine` 复用已获准的 CUU/EL，与 `mergeBackspace` 同一信任域。
- `up=2` 由代码路径推算（提交换行 1 行 + 提示尾部 CRLF 1 行），不是用户输入；常规终端宽度下限流提示单行显示、成立。**边界**：极窄终端若提示折行会导致 `up` 偏差（本版不处理，已在 ADR-067 patch 标注）。
- 未改动 composer 核心逻辑、信任域与 ADR-034 提交语义；仅调整回显光标状态。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖、无键位变化。替换二进制并重启即可，配置与数据零改动。命令集合、权限、渲染契约不变。

## 已知问题与限制

- `up=2` 在极窄终端（提示文本折行）下可能偏差，导致退格回到错误行；常规宽度不受影响。
- 沿用 v0.8.2：草稿仅本次连接有效、断连即失；倒计时不自动刷新（守 ADR-027）。
- 沿用 v0.8.1：`/stats` 在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出（含未跟踪新增文件）、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **1032**。
- 新增 5 项回归测试：恢复提示顺序、宽字符退格精度（末字按 `DisplayWidth` 精确擦除）、A 路径单行/多行 cols 重建、正文不重复回显；适配原 A 路径断言（单次重写而非零回显）。
- 两条闭环专项 `TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData` 绿；archtest 绿。

## 破坏性变更

无。命令集合、权限、数据契约、键位、提交语义完全不变；仅修正草稿恢复的回显光标与退格精度。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.8.3]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-067 patch 段。
