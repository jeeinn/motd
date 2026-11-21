# Release Notes — v0.7.0

**发布日期**：2026-11-21
**Git tag**：`v0.7.0`
**Module**：`github.com/jeeinn/motd`

> P4「可靠性收尾 + 基础多色」版本。终端基础多色（克制 3 色）+ `/healthz` 带宽探针；race 检测正式成为 main 强制门禁。无 schema 迁移，测试 935 → 941。

## 本版变化

### Added

- **终端基础多色（克制 3 色）**：`safe/ansi.go` 新增 3 个 SGR 前景 Style——Accent（34 深蓝）、Notice（33 琥珀）、Error（31 暗红），仅服务端固定文案使用；行内代码原有的青色（36）保持不变。
  - 状态栏：等级 `L3`/`L4` 走深蓝，`L1`/`L2` 与数值区保持暗灰；等级文字标签始终保留，颜色只是辅助提示。
  - 提示行：`[错误]` 前缀暗红、`[提示]` 前缀琥珀，消息正文仍走用户文本安全路径，不染色。
  - 明确不新增绿 / 品红 / 白 / 亮色 / 背景色 / 256 色 / 真彩；版块名与所有用户生成内容不上色。
- **带宽可观测探针**：`/healthz` 新增 `tx_bytes` / `rx_bytes`，记录进程级收发字节。纯内存原子计数，不写库、不改领域模型、不在终端呈现暴露，仅为后续容量口径备数据（不构成 1 Mbps / 1000 在线验收）。
- **archtest 配色守卫**：新增 `TestUserMarkdownCannotSelectPalette` 与 `TestColorStylesOnlyUsedBySystemPath`（含负向注入），从结构上阻止用户 Markdown 选色或非授权文件引用配色 Style。

### Changed

- **race 检测转强制门禁**：GitHub `main` 分支建立保护规则，`Build / Vet / Test` 与 `Race detector` 两个 CI job 设为 required 且 strict。此后合入 main 的 PR 必须通过竞态检测。
- `docs/P4-PLAN.md`：P4 计划与里程碑状态（T1/T2/T3/T4 均已完成）。
- `docs/ROADMAP.md`：R-01 基础多色纳入 P4；新增 R-04 浏览器端 TUI 帧传输（P5 方向）。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖。从 v0.6.x 升级只需替换二进制并重启。本版不改变任何对外协议或终端显示契约，颜色仅作辅助信息、不单独承载语义。

## 已知问题与限制

- 沿用前版：会话最长 6 小时；最小压测不构成 1 Mbps / 1000 在线验收；签到跨时区按本地日期比对。
- 真全屏数字雨、节日主题、@提及、公告/置顶、标题颜色等运营与个性化扩展按中期路线图留待后续用户反馈。
- 浏览器端 TUI 帧传输为 P5 方向，尚未开工；容量验收未作为其前置拍板。

## 验证

- CI 双 job 全绿：`Build / Vet / Test` pass（27s）、`Race detector` pass（1m31s）。
- 四门禁：`gofmt` 零输出、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **941**（v0.6.0 为 935）。
- QA 独立验证：用户文本注入的 ESC 被转可见图示（信任域未破防）、karma 阈值着色正确、`/healthz` 实测含 `tx_bytes`/`rx_bytes` 且 `commits` 不变、8×100 并发计数无丢失。

## 破坏性变更

无。所有变化为增量，配置与 schema 向前兼容。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.7.0]` 段、[`docs/P4-PLAN.md`](docs/P4-PLAN.md)。
