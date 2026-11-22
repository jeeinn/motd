# Release Notes — v0.7.2

**发布日期**：2026-11-22
**Git tag**：`v0.7.2`
**Module**：`github.com/jeeinn/motd`

> 补丁版本：修 `/help` 把彩蛋命令显示在「管理员命令（需白名单指纹授权）」标题之下。无 schema 迁移、无新依赖、无新环境变量，鉴权零变化。测试 989 → 990。

## 本版变化

### Fixed

- **`/help` 的分组边界丢失，彩蛋命令被读成「需要管理员权限」**。修复前：

  ```
  管理员命令（需白名单指纹授权）：
    /hide [id] … /node archive <slug>                    ← 真管理员（7 条）
    /fortune /cowsay /xkcd /blame /sudo /karma /matrix   ← 彩蛋，被吞进同一标题下
  ```

  修复后：

  ```
  管理员命令（需白名单指纹授权）：
    /hide [id] … /node archive <slug>

  彩蛋命令（无需权限，纯娱乐）：
    /fortune /cowsay /xkcd /blame /sudo /karma /matrix
  ```

  **根因**（排版边界，不是分组误标）：`eggRegistry` 构造插件命令时没设 `Group`（属普通分组），`WithCommands` 把彩蛋 append 到内置命令表末尾，而内置表最后一段正是管理员组；`Help()` 只用一个 `inAdminGroup` 布尔、仅在**首次**遇到管理员命令时插一次说明行，之后再无分组边界，彩蛋就落在该标题之下。

  **修法**：新增 `command.GroupEgg` 与固定说明行「彩蛋命令（无需权限，纯娱乐）」；`Help()` 改用 `map` 按分组首次出现插头（对任意分组通用，不再只认管理员）；`eggRegistry` 两条构造路径（插件元数据 + `EggCommands()` 兜底）都补 `Group`；彩蛋清单在 `EggCommands()` 里带 `Group`，保持「命令表是唯一事实来源」。
- `CHANGELOG.md` 补记缺失的 `[0.7.0]` / `[0.7.1]` compare 链接，并同步 `[Unreleased]` 基准到 `v0.7.2`。

## 设计取舍（为什么不直接隐藏）

`?` / `/help` 是这套 TUI 里彩蛋唯一的发现入口（`/xkcd` 等还刻意做成纯本地清单、不联网）。把它们从帮助里隐藏会让功能事实上不存在；而「混在管理员区」比隐藏更糟——它同时误导权限认知，用户会怀疑自己权限不足，而不是认为排版有问题。因此选择「独立成段、明说不需权限」，既消除误导又保留可发现性。

`Group` 始终只是展示元数据：`Parse` 与鉴权完全不看它，白名单准入仍只在 `forum.requireAdmin` 单点判定。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖。替换二进制并重启即可，配置与数据零改动。

## 已知问题与限制

- 沿用 v0.7.1：`/stats` 的在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **990**。
- 新增/扩展断言：两个分组说明各恰好出现一次、顺序为「管理员 → 彩蛋 → 键位」、彩蛋命令不得归入管理员组、无彩蛋命令时不得出现彩蛋说明行（防止无条件插头）。
- 真实 SSH 会话实测 `?` 输出：彩蛋已独立成段，管理员区只剩 7 条真管理员命令。
- CI 双 job（Build/Vet/Test + Race detector）全绿。

## 破坏性变更

无。帮助文本排版变化，命令集合与权限完全不变。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.7.2]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-061 patch 段。
