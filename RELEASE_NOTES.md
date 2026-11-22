# Release Notes — v0.7.1

**发布日期**：2026-11-22
**Git tag**：`v0.7.1`
**Module**：`github.com/jeeinn/motd`

> P4.5「运营可见性」版本：跨版块最新动态 `/latest` + 社区统计看板 `/stats`。含一次 schema 迁移（0011），测试 948 → 989。零新依赖、零新环境变量、零新入口。

## 本版变化

### Added

- **`/latest` 跨版块最新主题**：任意屏一条命令进入全论坛最新列表，按最后回复时间倒序（`last_reply_at DESC, id DESC`），新主题与新回复自动置顶。
  - 导航全部复用既有主题列表链路：`j/k` 移动、`n/p` 翻页、Enter 打开主题、`q` 回列表（保持位置）、`/top` 回版块列表。不新增页面类型、不动 ViewModel 契约。
  - **可见性与版块列表一致**：公开 active 版块 ∪ 该用户已解锁的隐藏版块——Konami 解锁 `/dev/null` 的用户，其主题也会出现在「最新」里；归档的普通版块任何身份下都不出现。查询用显式白名单 `node_id IN (...)`，不靠 `status` 放宽或收紧。
  - 查询用强制索引 + 行值游标：执行计划稳定（无 `TEMP B-TREE`），深翻页从 O(k×N) 降到 O(N)。
- **`/stats` 社区统计看板**（所有人可见，含访客）：
  - 今日/昨日登录人数（`daily_checkins` 按展示时区去重）、今日/昨日在线峰值与历史在线峰值（新表 `stats_daily`）、当前在线与活跃（实时内存）、注册用户数（只算 `status='active'`）、累计登录人次（新审计事件 `login_success`）。
  - 新增审计事件 `login_success`：GitHub Device Flow 与邮箱密码登录各计一次；**重连凭公钥恢复登录态不计**（那是会话恢复，不是登录动作）。该事件是事后统计，写不进审计队列只记日志，绝不回滚已完成的登录。
  - 新增 cmd 层 30 秒峰值采样器：自持当日峰值（不读进程级计数，避免把昨日峰值写进今天）、当日首个采样无条件建行、之后只写变高、单条幂等 upsert、停机补采一次。
  - **诚实口径**：`--` 表示「没有观测」（该日无行且当前在线为 0），`0` 表示「观测到没人」；每项起点上屏声明（登录自 v0.6.0、峰值与累计登录自 v0.7.1）。

### Changed

- **迁移 0011**：新增 `idx_threads_last_reply`、`stats_daily`（`local_date TEXT NOT NULL PRIMARY KEY` + 形状 CHECK、`peak_online`/`updated_at` 非负 CHECK、`updated_at` 只在峰值变高时推进）、`idx_auth_events_event`。
- **登录钩子**：`onLogin` 增加 `provider` 参数（命名类型 `loginHook`），装配收敛为 `newOnLoginHook`——审计先行、签到在后、两者互不短路。此前签到失败会连带丢掉登录人次记录。
- **页面上下文三处修正**：进入版块/`/latest` 首帧状态栏显示正确位置；无版块上下文时（如检索结果打开的详情按 `q`）回版块列表而不是空主题列表；首屏分页 offset 归零。
- **空态文案**：主题列表空态改为「（暂无主题，登录后可用 /new 发第一帖）」——同一屏也承载跨版块列表，原「该版块」是错指。

## 升级说明

- **有 schema 迁移**：启动时自动应用到 0011，无需手工操作；`stats_daily` 与 `login_success` **不回填**，峰值与累计登录人次自本版上线起累积。
- **升级首日的 `/stats` 会显示一行图例** `-- 表示暂无在线观测数据`：昨日还没有观测行（表是新建的），这是预期行为而非故障；次日有观测行后图例自行消失。
- 无新环境变量、无新依赖、无新入口；SSH 仍是唯一业务入口，`/healthz` 口径不变（业务指标不进探针）。

## 已知问题与限制

- 在线峰值是**30 秒采样峰值**，可能低估两次采样之间起落的瞬时并发；不以此为口径承诺、不据此做容量决策。两次采样之间的瞬时尖峰可能不落库，属已接受的采样精度边界。
- 同一天内多次渲染的今日峰值**不保证单调不降**（实时在线值与采样值同屏，采样峰值只在采样时前推）；展示层保证的是「历史 ≥ 今日 ≥ 当前在线」。
- `/latest` 使用强制索引：若某库缺少 `idx_threads_last_reply`（迁移异常的旧库），该命令会直接报错而不是静默变慢；因为不是 liveness 依赖，**未加入启动自检**，部署后建议手动确认 0011 已应用。
- 沿用前版：会话最长 6 小时；最小压测不构成 1 Mbps / 1000 在线验收；签到跨时区按本地日期比对。
- 与本次改动无关的既有问题：`internal/infrastructure/email` 的 `TestSendBoundsConcurrency` 为并发计时敏感用例，全量跑偶发失败（单独跑稳定通过），建议单独修复。

## 验证

- 四门禁：`gofmt` 零输出、`go build ./...` / `go vet ./...` / `go test -count=1 ./...` 全绿；两条闭环（`TestMVPSSHClosedLoop` / `TestMVPRestartAndBackupWithRealData`）通过；测试函数 **989**（v0.7.0 为 948，新增 41）。
- **真实库原地升级实测**（v0.7.0 库 schema_version=10 → 0011，在副本上执行，原件未动）：
  - 启动日志 `migrations_applied=1 applied_versions=[11]`；`PRAGMA integrity_check` = ok；
  - 数据零损失：users 2 / threads 12 / posts 31 / nodes 7 / ssh_keys 1 / auth_events 5，升级前后逐项一致；
  - `stats_daily` 由当日首个采样建行 `(2026-11-22, peak=0)`，`updated_at` 正常；两个新索引存在；跨版块查询计划为 `SCAN t USING INDEX idx_threads_last_reply`（无 TEMP B-TREE）；
  - 真实 SSH 会话实测：`/latest` 正确列出 9 条跨版块主题（含已解锁的 `/dev/null` 主题与自建版块 `/dev/sign`，行内带 slug，状态栏「位置: latest」）；`/stats` 输出与口径一致（升级首日出图例、今日峰值由实时值抬到 1、累计登录 0——公钥恢复不计人次）。
- `-race`：本地 Windows 环境不跑，由发版 CI 的 race job 覆盖（main 已设为 required）。

## 破坏性变更

无。迁移只新增表与索引，配置与对外协议不变。

## 变更明细

见 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.7.1]` 段、[`docs/P4.5-PLAN.md`](docs/P4.5-PLAN.md)（含实施期 15 条修订记录）、[`docs/DECISIONS.md`](docs/DECISIONS.md) 的 ADR-063 / ADR-064。
