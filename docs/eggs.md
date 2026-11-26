# 彩蛋系统（eggs）

> 本文是 motd 彩蛋的**用户视角清单 + 实现口径**，面向使用者与后续维护者。
> 决策依据见 `DECISIONS.md` 的 ADR-061 / ADR-062 / ADR-027 / ADR-015；后续扩展见 `ROADMAP.md`。
> 当前版本：v0.10.1（2026-11-25）。测试 1167，迁移止于 0013（本分支新增 R-14 游戏，未发版）。

## 1. 设计原则

彩蛋是**外围消费者**，不是业务功能：

1. **不改数据、不改计分**：唯一写路径是通过受限的 `AchievementGate` 申请白名单成就；拿不到数据库句柄。
2. **零回显**：所有输出是服务端固定语料，参数只做合法性校验，绝不把用户原文写回终端（ADR-026）。
3. **安全渲染**：固定文案经 `safe.RenderSystem`（服务端可信路径）输出；用户内容走另一条 `RenderUser` 净化链，两条不混用（ADR-019）。
4. **本地优先**：彩蛋不联网、无新依赖、无新环境变量。
5. **分层边界**：`command` 包不 import `plugin`，plugin 不 import `application/command` 或 `infrastructure`；转换只在 cmd 装配层完成（ADR-015）。

## 2. 命令类彩蛋（显式触发）

在任意页面输入 `/` 进入命令模式。以下 3 条命令已并入命令注册表，`/help` 可见：

| 命令 | 参数 | 内容 | 实现要点 |
|---|---|---|---|
| `/cowsay` | 无 | ASCII 牛说一句话 | 5 条固定短句，纯本地生成 |
| `/sudo` | 无 | 假提权失败 | 固定文案，呼应公钥白名单与审计日志 |
| `/matrix` | 无 | 屏内滚动数字雨 | ASCII 字符池（`A-Za-z0-9_\|`）；每列一个雨头 + 拖尾；暗绿底色 + 亮绿雨头两档；12fps；默认 60s 兜底；**按任意键中断** |

另有隐性彩蛋：`/quit` 退出时约 **12%**（`rand.IntN(8)==0`）概率多输出一句固定告别语。

> ADR-075：`/fortune`、`/xkcd`、`/blame` 三个命令及其语料已删除，`/help` 中不再出现；删除范围与理由见 `DECISIONS.md`。

### 2.1 `/matrix` 的实现边界（v0.9.2 / ADR-076）

数字雨是**屏内覆盖**动画，不是逐行追加：

- **进入**：`ESC[H` + `ESC[2J` 清屏 → 第 1 行固定提示 → 雨区铺满第 2..H 行；
- **每帧**：`\r` + `ESC[{H-2}A` 回到雨区首行 → 逐行重写；**屏幕底行不写 CRLF**——写了会触发终端滚屏，把雨区顶出可视区，并让 scrollback 以每秒十几行增长（`ED 2` 只清可视屏、不清 scrollback）；
- **退出**：`ESC[H` + `ESC[2J` + `ESC[0m` 复位，随后重绘原页面。复位是必须的：`2J` 不重置 SGR，不补这一次，绿色会漏到重绘后的页面上。

**为什么是屏内覆盖而不是逐行追加**：追加式无法回改已打印行，做不出电影 Matrix 的雨头 + 拖尾；且不限时会把用户的 scrollback 历史冲掉。

**带宽口径**：80×24 终端实测单帧约 2.4 KB、12fps 稳态约 **29.7 KB/s**（占整站 1 Mbps 上行的 24%）。服务端另有**并发上限 2**，第三个会话会被明确拒绝——3~4 个并发即打满整站上行，届时全站论坛对所有人变慢。

**颜色**：只用基础 SGR 两档绿（`ESC[1;92m` 亮绿雨头 / `ESC[2;32m` 暗绿底色与拖尾），**不含 256 色与真彩**——后者是 `ROADMAP.md` §4 明文「不做」的冻结决策，tmux/screen 需用户自行配置才能显示。逐字符上色更不可行：单帧会从 2.4 KB 涨到 10 KB 以上，12fps 即超整站上行九成。

**字符集为什么是 ASCII**：半角片假名（`U+FF61..`）在 Windows Terminal 默认字体无字形，回落字体会被放大成全角，导致整行字符网格错位；ASCII 池每字 1 字节 1 列，跨终端零差异。

**退出口径**：任意按键中断（此前只有 Esc 生效，Ctrl-C 被静默丢弃，与 `/help` 文案不符）。

## 3. 状态类彩蛋（隐式触发 → 成就）

彩蛋通过事件钩子申请既有成就码，**只有申请成就一条写路径**，不改库、不改计分。插件订阅在 `internal/plugin/social`，事件类型与处理：

| 成就码 | 名称 | 触发条件 | 判定细节 |
|---|---|---|---|
| `first_post` | 首次发帖 | `PostCreated` 事件 | 由 `plugin/demo` 处理；取主题标题作 evidence |
| `night_owl` | 夜行者 | 02:00–05:00 发帖 | 发帖时刻转到 **`MOTD_DISPLAY_TIMEZONE`** 后取小时（默认 Asia/Shanghai），与终端显示时间一致；nil Location 回落 UTC |
| `first_vote` | 欢迎来到互联网 | 内容首次被投票 | `VoteCast.ActorID` 是**投票者**，作者要经只读端口 `ThreadOwnerID`/`PostOwnerID` 取（只返回整数 ID、无 PII）；**自投票不解锁**；内容已软删查不到作者时静默跳过，不让插件失败阻塞 outbox 重试 |
| `streak_7` | 七日之约 | 连续签到 7 天 | 见下节 |

插件拿到的只读接口只有 `ThreadTitle` / `ThreadOwnerID` / `PostOwnerID`，看不到邮箱、凭证、连接句柄。

## 4. 签到系统（v0.6.0）

- **触发**：登录成功后（`loginFlow.finish` / `emailFlow` 会话升级之后）由 `onLogin` 钩子调用 `CheckInService.CheckIn`。两条登录路径（GitHub Device Flow、邮箱密码）都已接；钩子失败只记日志，**不影响登录结果**。
- **日期口径**：按 `MOTD_DISPLAY_TIMEZONE`（ADR-046）的本地日期 `YYYY-MM-DD`，与终端展示时间一致。
- **幂等**：SQLite `daily_checkins(user_id, local_date)` 有 UNIQUE 约束，同一天多次登录只留一条记录。
- **连续天数**：从第 N 天往前逐日比对，缺一天即归零；上限回看 400 条防脏数据拖长查询。
- **达标**：连续 7 天经 `AchievementGate` 申请 `streak_7` 成就；底层 Gate 幂等，不会重复授予。
- **展示**：`CheckInService.ProgressBar(streak)` 生成 ASCII 进度条（`█` / `░`），如 `签到 ███░░░░ 3/7 天`。

> 限制：跨时区旅行或系统时区变更可能让日期字符串出现空洞，导致连续天数重置。

## 5. Konami 码解锁 `/dev/null`（v0.6.0）

- **序列**：浏览态输入 `↑ ↑ ↓ ↓ ← → ← → B A`（经典 Konami Code），错键立即重置，完成后状态机自动复位（可再次触发）。
- **拦截范围**：只在浏览态检测；命令输入态与编辑态不拦截，避免吞掉方向键原有语义；访客不触发（解锁需落用户记录）。
- **解锁语义**：**按用户**写 `user_node_unlocks(user_id, node_id)`，持久化，重连仍有效。解锁目标由服务端固定为 `/dev/null`，**不接受用户参数**。
- **可见性**：`/dev/null` 种子节点 `status='archived'`（0008 的 CHECK 只允许 active/archived），`ListNodes` 只查 active 故默认不可见。解锁后由入口层把节点通过 `ViewContext.UnlockedHiddenNodes` 注入，`ScreenNodes` 按 ID 去重后追加到列表末尾——forum 层不做解锁判权、不依赖解锁存储。
- **成功提示**：固定文案「隐藏版块已解锁：/dev/null」，经安全渲染输出。
- **注意**：`/dev/null` 无任何内容，解锁只影响版块列表可见性，不改变权限或计分。管理员归档的其它版块不会被误当彩蛋节点（按 slug 精确匹配识别）。

## 6. 代码地图

| 关注点 | 位置 |
|---|---|
| 彩蛋语料与纯函数 | `internal/application/command/eggs.go` |
| `/matrix` 帧生成 | `internal/application/command/matrix.go` |
| Konami 状态机 | `internal/application/command/konami.go` |
| 命令合并 `WithCommands` | `internal/application/command/registry.go` |
| cmd 装配层转换 `eggRegistry` | `cmd/server/wiring.go` |
| 命令分发 / 签到钩子 / Konami 拦截 | `cmd/server/ssh_handler.go` |
| 状态类成就插件 | `internal/plugin/social/` |
| 首次发帖成就插件 | `internal/plugin/demo/` |
| 签到 service | `internal/application/user/checkin.go` |
| 签到 / 解锁仓储 | `internal/infrastructure/sqlite/repo_checkin.go` |
| schema | `internal/infrastructure/sqlite/migrations/0010_eggs.sql` |

## 7. 测试与验证

- 专项测试：`eggs_test.go`（命令合并/重名拒绝/语料无 ESC）、`konami_test.go`（精确序列/错键重置/matrix 帧）、`checkin_test.go`（连续天数/断签/幂等/进度条）、`sqlite/checkin_test.go`（签到与解锁持久化）、`social_test.go`（时段边界/投票给作者/自投票）、`forum/viewmodel_test.go`（解锁节点注入去重）、`cmd/server/egg_test.go`（固定文案/安全渲染/等级阈值）。
- 全量门禁：`go build` / `go vet` 通过、`go test -count=1 ./...` 0 FAIL；CI check + race 两 job 全绿。
- archtest 守门：插件未越权 import infrastructure，ESC 序列只在 `safe/ansi.go` 白名单内。

## 8. 未做 / 后续

| 项 | 状态 | 说明 |
|---|---|---|
| 真全屏数字雨 | **已完成（v0.9.0）** | ADR-072 解禁无参 `ESC[H` + `ESC[2J`，`/matrix` 改真全屏动画，带 `[Esc 返回]`，退出后清屏重绘原页 |
| 30/100 天签到里程碑 | 未做 | 观察 7 天成就反馈再定（`ROADMAP.md` R-12） |
| 更多隐藏版块 | 未做 | 需产品定义"隐藏什么、怎么解锁"（`ROADMAP.md` R-13） |
| 终端多色 | **基础配色已完成（v0.7.0）** | `StyleBold/Dim` + `accent/notice/error` 三色，`archtest/color_test.go` 守门 |
| 节日主题 | 未做 | 固定日期窗口换配色，须可关闭（`MOTD_THEME=auto\|off`）；详见 `ROADMAP.md` R-11 |
| 彩蛋自定义 / 用户输入参与文案 | 明确不做 | 违反零回显与安全渲染原则 |

## 9. 相关文档

- `DECISIONS.md`：ADR-015（插件权限）、ADR-019（信任域）、ADR-026（零回显）、ADR-027（ANSI 白名单，已由 ADR-072 扩充 `H`/`2J`）、ADR-046（展示时区）、ADR-061（彩蛋接入）、ADR-062（扩展三项）、ADR-072（全屏干净重绘，`/matrix` 真全屏动画依据）、ADR-075（删除 `/fortune`、`/xkcd`、`/blame` 三条彩蛋命令）、ADR-076（`/matrix` 电影化改造：字符池、屏内覆盖、两档绿、时长与并发兜底）。
- `archived/P3-PLAN.md §7`：M6 彩蛋立项与实施结果（历史快照）。
- `ROADMAP.md`：多色扩展、节日主题（R-11）与其余未做项的排期入口。
