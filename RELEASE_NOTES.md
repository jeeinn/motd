# Release Notes — v0.8.0

**发布日期**：2026-11-24
**Git tag**：`v0.8.0`
**Module**：`github.com/jeeinn/motd`

> 次要版本：新增 `/whoami`（本人资料）与 `/who`（在线名单）两条命令，退役 `/karma` 命令入口。
> 含此前未单独发布的 v0.7.3 主题列表样式调整。无 schema 迁移、无新依赖、无新环境变量；测试函数 991 → 1011。

## 本版变化

### Added

- **`/whoami`（别名 `/me`）——查看本人资料**。登录后输出：

  ```
  我的信息
    账号：@alice（ID 7）
    注册：2026-11-15 06:13:20 CST
    声望 12 · 等级 L2 · 距 L3 还需 38 声望
  ```

  账号、用户 ID、注册时间、声望与等级（含距下一级进度）。访客给固定提示；端口缺失 / DB 失败 / 记录不存在统一固定文案，不回显错误链。新增只读端口 `ProfileReader.ProfilesByIDs`（批量 IN 查询零 N+1），不含凭证、身份源、邮箱。

- **`/who [页]`——查看当前在线人员**。无参为第 1 页，页长 15；汇总行给出在线 / 活跃 / 访客计数，名单按「活跃态在前、其余 `LastSeenAt` 倒序」排列，访客连接只计入汇总不列名。所有人（含访客）可见。参数只接受正整数页码，非数字 / `0` / 负数 / `@xxx` 给固定用法提示，不查库、不回显原文。

### Changed

- **活跃口径统一**：presence 层提取单一谓词 `ActiveWithin(s, now, timeout)`（浏览/编辑态且心跳窗口内），状态栏、`/stats`、`/who` 三处共用同一判定，避免三个入口给出不同的「活跃 N」。
- **信任域隔离（评审 P0 修正）**：固定标签与本地数字走 `RenderSystem`，`@username` 作为 DB 用户数据单独走 `RenderUser` 净化链，两段绝不拼进同一次渲染（ADR-019）。

### Removed

- **`/karma` 命令入口直接删除**（不做别名过渡）：声望 / 等级展示并入 `/whoami`。能力层完整保留——`karmaLevel` / `progressDesc` 由 `/whoami` 复用，`KarmaReader` 端口与 repo 实现保留（状态栏持续消费），等级阈值测试与 `karma_display_test.go` 不动，投票计分逻辑不变。

## 设计取舍

- `whoami` / `who` 是 Unix 成对语义，比 `/info` 精确；`/info` 语义过宽，保留给未来实例 / 调试信息。
- `/who` 用内联块 + 参数分页而非专用屏：个人论坛并发为个位数，专用屏要改 viewmodel / renderer / pageController 五处，成本不匹配；n/p 键不响应该块，翻页靠再输 `/who <页>`。
- A 看 B（`/finger`）本期不立项：涉及他人注册时间 / 邮箱 / provider 等隐私字段，等产品有真实用户量与明确场景后再评估，立项前先定字段级隐私白名单。
- 单快照派生计数与名单，翻页期间连接上下线会造成页间漂移；个人论坛规模下接受，不做快照冻结。

## 升级说明

无 schema 迁移、无新环境变量、无新依赖。替换二进制并重启即可，配置与数据零改动。命令集合变化：`/karma` 下线，新增 `/whoami`（= `/me`）与 `/who`；习惯 `/karma` 的用户改用 `/whoami` 查看声望等级。

## 已知问题与限制

- 沿用 v0.7.1：`/stats` 的在线峰值为 30 秒采样值（可能低估瞬时并发）、同日多次渲染不保证单调、`/latest` 缺索引时 prepare 期报错（未加入启动自检）。
- 与本次无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出（含未跟踪新增文件）、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；测试函数 **1011**。
- 新增 `who_command_test.go`（17 项：分段渲染、ESC / 双向控制符注入、心跳窗口对齐、分页边界、别名与 happy path）、`repo_profile_test.go`（2 项批量投影）、`TestKarmaLevelMatchesDomainLevel`（cmd / domain 阈值逐点对拍）。
- 两条闭环专项 `TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData` 绿；archtest 绿。

## 破坏性变更

`/karma` 命令入口移除（改用 `/whoami`）；其余命令、权限、数据契约完全不变，声望 / 等级计分能力与状态栏显示不受影响。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.8.0]` 段、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-065，以及设计稿 [`docs/DESIGN-whoami-who.md`](docs/DESIGN-whoami-who.md) §11 决策记录。
