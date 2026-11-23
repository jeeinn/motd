# 提案：`/whoami`（我的信息）与 `/who`（在线人员列表）

> 状态：
> **设计已评审、决策已拍板（2026-11-23），尚未实施**
> 。§11 是最终决策，效力高于前文的"推荐 / 待拍板"表述；§10 保留为历史清单。
> 本次仅固化决策，**不改动任何代码**；实施另起切片，并按惯例补 ADR-065。
> 证据基线：当前 working tree（v0.7.3 已提交
> `dd621fb`
> ）。

## 1. 目标与范围

用户提出两条新命令：



1. **查看已登录状态 / 我的信息**：账号、注册时间、等级等（命令名 `/info` 还是 `/whoami` 未定）。

2. **查看所有在线人员**（类 Linux `who`），人多时可分页。

v1 范围（刻意收窄）：



* `/whoami`：用户名、用户 ID、注册时间、声望、等级（含距下一级进度）。

* `/who`：在线用户名单 + 汇总（在线 / 活跃 / 访客），参数分页。

* **不做**：身份源 provider / 邮箱展示、发帖计数、成就列表、点人看资料、`who` 的键盘翻页。这些列入二期 backlog（§9）。

## 2. 证据基线（已读当前 revision）



| 事实                                                                                                                                             | 位置                                                                        |
| ---------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| 内联命令（`/stats` `/karma` `/keys`）在 `ssh_handler.go:481` switch 分发，打印后留在原屏                                                                        | `cmd/server/ssh_handler.go:499-505`                                       |
| `/karma` 用可选接口断言取 `KarmaReader`，访客固定提示，DB 失败固定文案                                                                                               | `cmd/server/ssh_handler.go:634-648`                                       |
| 等级阈值 L2=10 / L3=50 / L4=200，`domain/user.Level(karma)` 派生 1–4                                                                                  | `internal/domain/user/level.go:5-24`                                      |
| `karmaLevel` / `progressDesc` 已在 cmd 层实现，`/karma` 复用                                                                                           | `cmd/server/ssh_handler.go:652-670`                                       |
| Principal 只含 `Kind/UserID/KeyFingerprint`，**不带 username/provider/ 注册时间**                                                                       | `internal/application/auth/principal.go:26-30`                            |
| 现有只读端口：`StatusReader.Status`、`KarmaReader.Karma`、`UsernameReader.UsernamesByIDs`、`StatsReader`；**无个人资料端口**                                     | `internal/application/user/achievement.go:49-57`、`forum/ports.go:170-175` |
| `users` 表：`username / display_name / primary_email / karma / status / created_at / updated_at`；身份源在 `identities`（provider/provider\_uid/email） | `migrations/0001_init.sql:21-52`                                          |
| `presence.Registry.Snapshot()` 返回按会话 ID 排序的副本；`Session{ID,Kind,UserID,State,ConnectedAt,LastSeenAt}`                                           | `internal/application/presence/registry.go:58-65,231-240`                 |
| State：Authenticating / Browsing / Composing / Idle；`Active()` 只认窗口内 Browsing/Composing                                                         | `presence/registry.go:40-54,201-212`                                      |
| 连接登记：重连凭公钥直接 `Connect(id, KindSSH, p.UserID)`；主动登录后 `AttachUser`                                                                               | `transport/ssh/server.go:418`、`transport/ssh/session.go:270-291`          |
| 时间格式化先例 `formatKeyTime`：DisplayLocation 时区 + `2006-01-02 15:04:05` + 区缩写                                                                       | `cmd/server/ssh_handler.go:707-715`                                       |
| 用户文本进内联块的先例：`runKeyDetail` 先 `safe.SanitizeText(label)` 再嵌入固定模板                                                                                | `cmd/server/ssh_handler.go:733-741`                                       |
| 命令登记在 `defaultCommands`，帮助由注册表生成；新命令须同步 `TestDefaultRegistryHasP0Commands` 清单                                                                  | `internal/application/command/registry.go:88-138`、`registry_test.go:9-32` |
| 治理写操作才走 `requireAdmin`；在线数已在状态栏对所有人公开                                                                                                          | `docs/DECISIONS.md` ADR-049、`presentation/terminal/view.go:234-246`       |

## 3. 命令命名裁决（推荐）

**采用&#x20;**`/whoami`**，别名&#x20;**`/me`**；**`/info`**&#x20;不启用。**



* `whoami` 在 Unix 语义里就是「我是谁」，精确对应本命令；与 `/who` 形成成对的语义（whoami = 自己，who = 所有人）。

* `/info` 语义过宽，未来实例 / 调试信息可能占用，避免过早定义。

* 别名 `/me` 走注册表 `Aliases`（既有先例 `/login`↔`/signin`、`/help`↔`?`），零额外解析成本。

## 4. `/whoami` 设计

### 4.1 数据流



```
Principal.UserID
  → 可选接口断言 ProfileReader.Profile(ctx, userID)   // 新只读端口
  → 组装固定模板（标签 RenderSystem，username 先 SanitizeText）
  → writeSystem 输出
```

### 4.2 新只读端口（application/user，使用方定义）



```
// internal/application/user/profile.go（新文件）
type ProfileView struct {
    Username  string
    UserID    int64
    CreatedAt int64 // unix 秒
    Status    domainuser.Status
}

type ProfileReader interface {
    Profile(ctx context.Context, userID int64) (ProfileView, error)
}
```



* 实现落在 `infrastructure/sqlite/repo_user.go` 的 `UserStatusReader` 上（同 struct 加方法，结构化接口匹配，与 `Karma` 同款）。

* SQL：`SELECT username, created_at, status FROM users WHERE id = ?`；不存在沿 `sql.ErrNoRows` 上抛，由调用方给固定文案。

* 装配：`inputHandler` 用 `h.users.(appuser.ProfileReader)` 可选断言（复刻 `runKarma` 的 `KarmaReader` 断言，`ssh_handler.go:638`），测试 fake 未实现时零影响。

### 4.3 输出形态（已登录）



```
我的信息
  账号：@alice（ID 7）
  注册：2026-11-15 06:13:20 CST
  声望 12 · 等级 L2 · 距 L3 还需 38 声望
```



* 注册时间复用 `formatKeyTime`（同口径时区）。

* 声望行直接复用 `karmaLevel` / `progressDesc`，不另造阈值逻辑。

* 访客：`"请先 /login github 登录后查看我的信息"`（与 `/karma` 访客提示同构）。

* DB 失败：`"个人信息查询失败，请稍后重试"`，不回显错误链（ADR-026 零回显）。

### 4.4 信任域



* 固定标签与数字走 `RenderSystem`；`@username` 先 `safe.SanitizeText` 再嵌入模板（`runKeyDetail` label 先例），控制字符 / 双向控制符不可能进入输出。

* 不新增 SGR 样式，不碰配色常量表。

## 5. `/who` 设计

### 5.1 形态：内联块 + 参数分页（候选 C，见 §6）



```
在线 3 · 活跃 1 · 访客 1
  @alice    浏览中 · 在线 12 分钟
  @carol    编辑中 · 在线 40 秒
  @bob      空闲 · 在线 5 分钟
—— 第 1/1 页（/who <页> 翻页）
```



* 汇总首行：`Online()` / `Active()` 复用；访客 = 快照中 `UserID == 0` 的连接数（含认证中），**不逐行列名**。

* 名单只列 `UserID > 0` 的会话。

### 5.2 组装规则（纯函数，可单测）



1. 取 `presence.Snapshot()`。

2. 用户行：收集 `UserID > 0` 的会话，批量 `UsernameReader.UsernamesByIDs` 投影；查不到的 ID 回退 `#ID`（与 `fillRowAuthors` 同口径，`forum/viewmodel.go:269-296`）。

3. 排序：Browsing/Composing（活跃态）在前，其余按 `LastSeenAt` 倒序；同键稳定。

4. 每行：`@username` + 固定状态词（`浏览中 / 编辑中 / 空闲`）+ 在线时长（`now-ConnectedAt`，秒→`N 分钟`/`<1 分钟`）。

5. 分页：页长 15；`/who [页]`，无参 = 第 1 页；超界给固定提示；块尾 `第 x/y 页`。

6. 空块（无在线用户）：`"当前没有在线人员"`。

### 5.3 可见性

所有人（含访客）可用，**不加&#x20;**`requireAdmin`：`/who` 是只读公开信息，在线数本就在状态栏公开；ADR-049 的白名单只卡治理写操作。

## 6. 候选对比（`who` 的承载方式）



| 候选                           | 机制                                                                                                                                               | 代价                                   | 结论        |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------ | --------- |
| A. 内联单块，不分页                  | `/stats` 同款一次打印                                                                                                                                  | 不满足「分页查看」诉求                          | 否决        |
| B. 专用屏 `ScreenUsers` + n/p 键 | 新增 ScreenKind：viewmodel.Valid、renderer `screenTitle`/`emptyHint`/ 行渲染、pageController `command`/`nextPage`/`firstPage`/`back`/`open` 五处分支；快照跨页易漂移 | 成本与个人论坛个位数并发不匹配；ADR-063 也曾为省新屏复用既有链路 | 否决（留二期评估） |
| C. 内联 + `/who [页]` 参数分页      | 现取快照、按页切片；零屏模型改动                                                                                                                                 | n/p 键不响应该块（与 `/stats` 一致：打印后留在原屏）    | **采用**    |

## 7. 实施切片（文件清单）



| 文件                                                 | 改动                                                                      |
| -------------------------------------------------- | ----------------------------------------------------------------------- |
| `internal/application/user/profile.go`             | 新端口 `ProfileReader` / `ProfileView`                                     |
| `internal/infrastructure/sqlite/repo_user.go`      | `Profile(ctx, userID)` 实现                                               |
| `internal/application/command/registry.go`         | `defaultCommands` 追加 `/whoami`（别名 `me`）、`/who [页]`，置于 `/stats` 之后       |
| `cmd/server/ssh_handler.go`                        | switch 加 `case "whoami"` / `case "who"`；新增 `runWhoami`、`runWho`（组装抽纯函数） |
| `internal/application/command/registry_test.go`    | 更新 `TestDefaultRegistryHasP0Commands` 清单与计数；新增 Parse / 别名断言             |
| `cmd/server/*_test.go`                             | `runWhoami` / `runWho` 纯函数与注入守卫测试                                       |
| `internal/infrastructure/sqlite/repo_user_test.go` | Profile 读取与不存在口径                                                        |

无迁移、无新依赖、无新环境变量、无写路径改动。

## 8. 测试矩阵



1. **ProfileReader**：返回 username/created\_at；不存在上抛；status 读取不误伤。

2. **runWhoami**：访客固定提示；字段完整；DB 失败固定文案；**负向注入**：username 含 ESC / `\r` / 双向控制符时被净化、不出现在输出。

3. **runWho 组装**：快照→用户行 / 访客计数；活跃优先 + LastSeenAt 倒序；页长切片、页界与超界提示；无在线时空块。

4. **registry**：`/whoami`、`/who` 可 Parse；`/me` 别名解析到 whoami；帮助自动出现。

5. 四门禁（gofmt /build/vet/ `go test -count=1 ./...`）+ 两条闭环专项。

## 9. 二期 backlog（本次不做）



* provider（github/email）与邮箱展示：需查 `identities` 表，新端口。

* 发帖 / 回复计数、成就列表、点用户名看他人资料。

* 若未来在线规模真的增长，再评估候选 B（专用屏 + n/p）。

## 10. 待拍板



1. 命令名：推荐 `/whoami`（别名 `/me`），`/info` 保留 —— 是否同意？

2. v1 是否需要 provider / 邮箱？默认不展示。

3. `who` 分页采用参数页（C）；如坚持要 n/p 键盘翻页，改走候选 B（成本另估）。

## 11. 决策记录（2026-11-23 拍板）

> 本节为最终决策，效力高于 §3 的"推荐"与 §10 的"待拍板"。本次**只固化决策，不改代码**；
> 实施另起切片，按 AGENTS.md 惯例追加 ADR-065（`docs/DECISIONS.md`），并在同批同步命令清单、
> CHANGELOG、`docs/eggs.md`。历史结论不删改，只在此标注已定。

### 11.1 决策总表

| 编号 | 决策项 | 结论 |
| --- | --- | --- |
| F-1 | 定 `/whoami` 后 `/karma` 的处置 | **直接删除 `/karma` 命令**（不做别名过渡）；能力层保留，边界见 §11.2 |
| F-2 | `/who` 是否支持参数查看指定用户（A 看 B） | **不支持**。`/who` 一期只接页码；A 看 B 另起 `/finger`，**本期不立项、延迟** |
| F-3 | 命令命名 / 参数 / 用法风格 | **接受务实拟态**：主名 `whoami`/`who` 地道；`/me` 别名、`/who [页]` 页码属可接受妥协。红线：不得把"看人资料"塞进 `/who` |
| §10-1 | 命令名 | 采纳推荐：`/whoami` 主名 + `/me` 别名；`/info` 不启用 |
| §10-2 | v1 是否展示 provider / 邮箱 | 不展示（维持收窄） |
| §10-3 | `/who` 承载方式 | 参数页（候选 C）；n/p 键盘翻页不做，候选 B 留待在线规模真正增长后再评估 |
| D-3 | `/who` 对访客可见粒度 | 维持提案 §5.3：逐人可见（`@username` + 状态 + 在线时长，含访客），不加 `requireAdmin`；聚合在线数本就状态栏公开 |
| D-4 | 声望 / 等级是否进 `/whoami` | 保留（去掉则失去快速查看入口，且与 F-1 重复消解无需靠删字段实现） |

### 11.2 `/karma` 直接删除的执行边界（随决策附带，防止误删）

"删除命令"仅移除命令入口与彩蛋元数据；**能力层不得连删**：

| 处置 | 对象 | 位置 |
| --- | --- | --- |
| 删 | `karma` 彩蛋条目 | `internal/application/command/eggs.go:24` |
| 删 | `case "karma"` 分发、`runKarma` | `cmd/server/ssh_handler.go:499-500`、`634-648` |
| **保留** | `karmaLevel` / `progressDesc` | `cmd/server/ssh_handler.go:652-670`，`/whoami` 复用 |
| **保留** | `KarmaReader` 端口 + repo 实现 | `achievement.go:53-57`、`repo_user.go`，状态栏持续消费 |
| **保留** | 状态栏等级渲染、投票计分写入 | `view.go:226-246`、ADR-057 |
| **保留** | 等级阈值测试、`karma_display_test.go` | 与命令解耦，勿随之删除 |
| 改 | 彩蛋清单 / 帮助断言 | `docs/eggs.md:28`、`egg_test.go` 中 karma 解析项 |

### 11.3 `/finger` 延迟说明

A 看 B 涉及他人注册时间、邮箱、provider 等隐私字段，当前个人论坛并发为个位数，无实际诉求。
**本期不立项**；仅当出现真实用户量增长、且有明确使用场景时再单独评估，立项前须先定字段级隐私白名单
（默认仅 `@username` + 状态 / 声望，注册时间 / 邮箱 / provider 仅本人或管理员可见）。

### 11.4 实施前须处理（评审识别，留待实施切片，本次不动代码）

| 级别 | 事项 | 口径 |
| --- | --- | --- |
| P0 | §4.4 把净化后 `@username` 拼进整串交 `writeSystem`→`RenderSystem`，触 ADR-019 信任域红线 | 改分段渲染：固定标签走 `RenderSystem`，用户数据段单独渲染（`runKeyDetail` 先例） |
| P1 | 端口形态：`UsernameReader` 不在 `h.users`（`UserStatusReader`）上，`forumRuntime` 未暴露投影 | 改批量端口 `ProfilesByIDs(ctx, []int64)` 含 Karma，落 `UserStatusReader`，零 N+1 |
| P1 | `ValidateUsername` 允许纯数字用户名（`user.go:58`），`who 123` 与页码二义 | `/who` 参数只接受正整数页码；非数字 / `@xxx` 给固定提示且不查库 |
| P1 | 等级阈值双实现（cmd `karmaLevel` vs domain `user.Level`） | 删 karma 时加对拍测试，防口径漂移与误伤状态栏 |
| P1 | 汇总与名单多次 `Snapshot()` 漂移；§8 测试矩阵约 19 条遗漏 | 单次快照派生计数；补边界 / 注入 / 隐私零回显用例 |

下一步：以上决策与实施口径已固化，待明确授权后再进入代码实现切片。