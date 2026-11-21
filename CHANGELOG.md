# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.0] - 2026-11-21

P4「可靠性收尾 + 基础多色」版本。终端基础多色（克制 3 色）+ `/healthz` 带宽探针；race 检测转 main 强制门禁。测试 935 → 941。

### Added

- **终端基础多色（克制 3 色）**：`safe/ansi.go` 新增 Accent（34 深蓝）/ Notice（33 琥珀）/ Error（31 暗红）三个 SGR 前景 Style，仅服务端固定文案；状态栏 `L3`/`L4` 走深蓝、提示行 `[错误]`/`[提示]` 前缀分色，用户文本正文仍走 `RenderUser` 不上色。不引入绿/品红/白/亮色/背景色/256 色/真彩，版块名不上色。
- **带宽探针**：`/healthz` 新增 `tx_bytes`/`rx_bytes`，`Server.bw`(2×atomic.Uint64) + `Session.bw`(atomic.Pointer) 纯内存计数，不写库、不改领域模型。`probeMux` 的 bw 参数 variadic 兼容旧调用。
- **archtest 配色守卫**：`TestUserMarkdownCannotSelectPalette`、`TestColorStylesOnlyUsedBySystemPath`（含负向注入），阻止用户 Markdown 选色与非授权文件引用配色 Style。

### Changed

- **race 检测转 required**：main 分支建立保护规则，`Build / Vet / Test` 与 `Race detector` 设为 required（strict）。
- `docs/P4-PLAN.md`（新增）、`docs/ROADMAP.md`（R-01 纳入 P4、新增 R-04 P5）、`docs/OPS.md`（§8 探针字段）。

### Notes

- 无 schema 迁移、无新环境变量、无新依赖；从 v0.6.x 升级替换二进制重启即可。
- CI 双 job 全绿（check 27s / race 1m31s）；QA 独立验证信任域未破防、karma 阈值正确、healthz 实测含字段且 commits 不变。

## [0.6.0] - 2026-11-21

彩蛋扩展版本。连续签到成就、合规数字雨、Konami 码解锁隐藏版块。含 schema 迁移（0010），测试 924 → 935。

### Added

- **每日签到与 7 天成就**：登录成功（GitHub Device Flow / 邮箱两路）幂等记录本地日期，连续 7 天解锁 `streak_7` 成就；断签归零。按 `MOTD_DISPLAY_TIMEZONE` 算本地日期（复用 ADR-046 配置）。
- **`/matrix` 滚动雨帘**：固定 24 行、50ms/行、SGR 青色 + 固定字符集，ctx 取消（连接断开）即停。只用 SGR，不触碰 ADR-027 禁止的 ED 清屏 / CUP 定位 / 备用屏。
- **Konami 码解锁 `/dev/null`**：浏览态输入 ↑↑↓↓←→←→BA 解锁隐藏版块，**按用户持久化**（`user_node_unlocks` 表）；解锁后该用户的版块列表末尾出现 `/dev/null`。未解锁用户不可见；管理员归档的其它版块不会混入彩蛋节点。
- `docs/ROADMAP.md`：后续路线图。含终端多色扩展（含节日主题）的技术评估与实施顺序。

### Changed

- `ViewContext` 加 `UnlockedHiddenNodes` 字段，由入口层注入已解锁节点；forum 层不做解锁判权、只做去重与并入（ADR-015 边界）。
- 成就码白名单加 `streak_7`；迁移版本 9 → 10。

### Notes

- 新增 0010 迁移：`daily_checkins`、`user_node_unlocks` 表 + `/dev/null` 种子（status=`archived`，复用 0008 枚举，避免重建 nodes 表）。
- 无新环境变量；升级替换二进制重启即可，迁移自动应用。
- 真全屏数字雨、30/100 天签到、更多隐藏版块评估见 `docs/ROADMAP.md`。

## [0.5.1] - 2026-11-20

彩蛋系统版本。P3-M6 落地，打通「彩蛋→成就」闭环。无 schema 迁移、无新依赖、无新环境变量；测试 914 → 924。

### Added

- **六条彩蛋命令**：`/fortune`（本地箴言）、`/cowsay`（ASCII 牛）、`/xkcd`（本地 xkcd 风格标题，不联网）、`/blame`（固定甩锅文案，不回显参数）、`/sudo`（假提权失败，呼应白名单+审计）、`/karma`（复用 KarmaReader 显示声望与等级）；`/quit` 约 12% 概率多说一句告别语。全部经 `safe.RenderSystem` 可信路径输出（ADR-061）。
- **状态类成就**：`night_owl`（02:00–05:00 **按 `MOTD_DISPLAY_TIMEZONE`** 发帖）与 `first_vote`（内容首次被投票，给作者不给投票者）两个既有成就码首次接通事件钩子；新建 `internal/plugin/social` 插件。
- `command.WithCommands`：插件命令合并进注册表（cmd 装配层剥前导 `/`），`inputHandler.withCommands` 注入；`newInputHandler()` 无参构造不变，既有测试零改动。

### Notes

- 高风险/需迁移项延后 v0.5.2：动态数字雨 `/matrix`、连续签到 7 天（需迁移）、Konami 码解锁 `/dev/null`（需输入序列检测）。
- 彩蛋不改库、不改计分；`/blame` 参数只做个数校验，绝不回显原文。

## [0.5.0] - 2026-11-20

P3 稳定运营版本：成就与 karma 从占位变为真实数据，CI 加竞态检测，最小压测集落地，备份调度可复制。无 schema 迁移、无新依赖、无新环境变量；测试 905 → 914。彩蛋命令（P3-M6）拆到 v0.5.1。

### Added

- **CI 竞态检测 job**：`.github/workflows/ci.yml` 追加独立 `race` job（`CGO_ENABLED=1`、`GORACE=halt_on_error=1`、`go test -race -timeout 15m`），初期非 required。本机 Windows 无 gcc 跑不了 race，Linux CI 可行（更正 PHASE1-PLAN 中"纯 Go 故 race 不可用"的因果，见 ADR-058）。
- **最小压测集合**：4 个同包 benchmark（SSH 握手 / 浏览列表 / 写队列背压 / 并发渲染+presence）+ `scripts/bench.sh`，不进 CI 默认 job。**诚实口径**：这是回环环境的相对延迟基线，不构成 1 Mbps / 1000 在线验收证据（ADR-059）。
- **备份调度模板**：`deploy/motd-backup.service` + `.timer`（每日 03:00 模板，需部署方替换 `ENV_FILE` 后启用）；`backup.sh` 生成备份后立即 `motdctl verify`（ADR-060）。
- `docs/P3-PLAN.md`：P3 完整方案（M1~M5 已实施，M6 拆 v0.5.1）。

### Changed

- **成就落 SQLite**：替换 P0 的内存占位 Store，新增 `repo_achievement.go`（`Has`/`Grant` 幂等/计数），进程重启后成就不丢；无迁移，0001 的 `achievements` 表已够用（ADR-056）。
- **karma 计分接线**：发主题 +2、回复 +1、被 upvote +1（被踩不扣作者、自投票不给、改票按差、删内容不回退）；状态栏 karma/等级从恒 0/L1 变为真实值。等级**仅显示、不做权限**（重申 ADR-014）。计分规则由用户确认，见 ADR-057 / P3-PLAN §3.2。
- 显示侧新增独立 `KarmaReader` 端口 + 可选接口断言，不扩 `StatusReader`，既有调用方与测试 fake 零改动。
- `docs/OPS.md` §4 恢复流程补上必须的 `motdctl fts-rebuild` 步骤（此前遗漏会导致恢复后 `/search` 返回空）。

### Notes

- 无新环境变量、无 schema 迁移；从 v0.4.x 升级替换二进制重启即可，成就/karma 数据从新发布起开始累积。
- 彩蛋命令（`/fortune` `/cowsay` `/xkcd` `/matrix` `/blame`）本版不做，已确认拆到 v0.5.1。

## [0.4.3] - 2026-11-20

文档与运维增强版本。无代码行为变更、无 schema 迁移、无新依赖；二进制功能与 v0.4.2 一致，测试仍为 905。重点是把首次 VPS 部署的实战经验固化为可复制的手册，并新增面向 AI 智能体的自动部署规程。

### Added

- `AGENTS.md`：智能体协作指南，记录架构依赖方向、CI 四门禁、发版硬约束（commit / tag / push / Release 均须当次显式确认）、文档索引与项目特有陷阱。
- README 新增在线体验入口（`ssh -p 2222 guest@jeeinn.com`）与两张终端截图：版块列表首屏、GitHub Device Flow 登录到 `/new` 发帖。
- `docs/OPS.md` §9.0 / §9.6：明确两种部署形态——systemd 托管（非 root + 0600 环境文件 + 单元模板 + 自检）与便携版自部署（自编译二进制 + 自有目录 `.env` + `screen`，端口错开、优雅停止、备份升级及 `.env` 查找顺序等特有坑）。
- `docs/OPS.md` §10：AI AGENTS 自动部署指南——输入契约、只读前置探测与 STOP 条件、决策表、7 步幂等序列与验收门、失败模式速查、交付报告模板与不可协商护栏。

### Changed

- `docs/OPS.md` §9 按 CentOS Stream 9 实机校准：`goproxy.cn` 应对国内 VPS 网络超时、module cache 只读需复制到可写目录、`-p 2` / `GOMEMLIMIT` 适配小内存；版本注入以 `/healthz` 回显为判据。
- 权限口径修正：`/opt/motd` 与二进制须 `0750 root:motd`，否则 systemd 以 `motd` 用户启动时 CHDIR 失败（`status=200/CHDIR`）并重启循环。
- 环境文件路径发行版感知：`/etc/motd` 在 RHEL/CentOS 是普通文件，改用 `/etc/sysconfig/motd`（0600）；systemd 模式保持 `MOTD_DOTENV_DISABLED=true`，凭据只写环境文件、不写 unit。
- 探针端口冲突处理：`127.0.0.1:8080` 被占用时改绑 `127.0.0.1:18080`；SSH 入口仍为 `:2222`。

### Notes

- 升级兼容：从 v0.4.2 起无需替换二进制或迁移；已部署实例不要双写 systemd 环境文件与 `.env`。
- 便携版 `screen` 不提供开机自启，需 cron `@reboot` 或迁移到 systemd。

## [0.4.2] - 2026-11-20

契约收敛版本。移除死配置 `MOTD_SSH_IDLE_TIMEOUT` 与整套 idle timer 机制；会话生存期只由 6 小时绝对上限约束。无 schema 迁移、无新依赖；测试 907 → 905（删 3 个 idle 测试、增 1 个反证测试）。

### Changed

- **移除会话空闲超时配置 `MOTD_SSH_IDLE_TIMEOUT` 与整套 idle timer 机制**：该配置自 v0.4.1 起被连接级心跳持续重置、实际永不触发，是"看起来能配、改了没效果"的死配置（仅在误设成小于心跳周期 10s 时产生意外断连）。`Session` 的 `idle` 字段 / `timer` / `resetIdle()` / `idleCloseNotice` 一并删除；`heartbeat()` 回归只刷新 presence 注册表。会话生存期现在只由 6 小时绝对上限约束，关闭序列（固定文案 → exit-status → close channel）不变。详见 ADR-055（ADR-054 原文保留供追溯）。
- 环境变量总数从 25 项减为 24 项。

### Notes

- 升级兼容：`.env` 里残留的 `MOTD_SSH_IDLE_TIMEOUT` 会被静默忽略（不报错、不影响启动），建议手动删除该行。从 v0.4.1 升级替换二进制重启即可，无需迁移。

## [0.4.1] - 2026-11-20

体验优化版本。修复「停止浏览一会儿就被 SSH 断连」问题，会话空闲语义由「无按键」改为「连接不存活」。无 schema 迁移、无新依赖、无新配置项；测试 903 → 907。

### Fixed

- **会话空闲断连**：原空闲计时器只由用户按键重置，停止浏览约 10 分钟（默认 `MOTD_SSH_IDLE_TIMEOUT`）服务端就主动关闭会话，客户端只看到裸 `Connection closed`（退出码 255）。现由连接级心跳（10s）持续重置该计时器——只要连接还在，用户不操作也不会被断开。
- **超时关闭无提示**：空闲超时与硬上限关闭前统一下发固定文案（「会话空闲超时，连接已关闭」/「会话达到最长在线时长（6 小时），连接已关闭」）并回 exit-status（退出码 0），不再让用户面对无解释断连。

### Changed

- 新增会话绝对存活上限 6 小时（无需配置）：兜底「客户端静默消失、SSH 层未感知断开」的连接，避免其永久占用 `MaxConns` 槽位。该计时器不被心跳重置，与空闲超时相互独立。
- `MOTD_SSH_IDLE_TIMEOUT` 语义更新为「连接无心跳超时」，不再是「用户无按键超时」；默认值 10m 不变。
- 会话收口统一为单一幂等序列（固定文案 → exit-status → 关闭 channel），消除正常退出与超时关闭之间的竞态。

## [0.4.0] - 2026-11-20

社区治理版本。P2 五个里程碑（M0/M1/M1b/M2/M3）合并一次发版，新增节点管理、内容治理、编辑与回复计数；迁移 0007 → 0009，ADR-048 → 053，测试 749 → 903。

### Added

- **节点（版块）管理**：管理员用 `/node list|create|rename|archive` 列出 / 创建 / 改名 / 归档版块。归档为软删（只改 `node.status`，不动 threads），历史帖详情页仍显示版块名；5 个 seed 节点可归档、不可改名。`MOTD_ADMIN_FINGERPRINTS` 公钥指纹白名单比对已认证会话指纹（拒绝用户提交文本），白名单为空则无人可管；治理动作经 outbox 写入 `auth_events` 审计（ADR-048/049）。
- **内容治理**：管理员用 `/hide [id]` 隐藏他人主题或回复（hidden ≠ deleted，数据可恢复）；`/unhide [id]` 与 `/unthread <thread-id>` 恢复隐藏内容，解决误隐藏不可逆（ADR-050/053）。
- **作者编辑**：`/edit [id]` 或无参编辑首帖，仅作者本人可改（复用 `canModify`，与治理权分离）；详情页显示「已编辑 MM-DD HH:MM」，服务端固定文案走 `RenderSystem`，不改投票语义、不推进 `last_reply_at`（ADR-051）。
- **回复数显示**：主题列表真实显示紧凑 `rN`（此前格式就位但生产从未赋值）。读时批量聚合 `ReplyCountsByThreadIDs`，`COUNT-1` 去首帖，分页 20 行 EXPLAIN 走 `idx_posts_thread`（ADR-052）。

### Changed

- 帮助命令按角色归组展示：6 条管理员命令单独归组 + 固定说明行，不隐藏、不动权限架构。
- 治理权单点收口 `requireAdmin`，与作者权 `canModify` 严格分离。

### Fixed

- 浏览态连按 `Esc` 不再向终端追加「（取消）」；编辑 / 命令 / 邮箱态的取消提示保留。
- 作者栏 `@username`（v0.3.3）与治理命令审计的显示路径统一，中文用户名按显示宽度截断。

### Notes

- 已知限制沿用：白名单变更需重启生效（SIGHUP 热重载推 P3）；审计 queue full 时 DomainBus 去重可能吞重投；回复计数 `INDEXED BY` 索引名硬绑定（技术债）。

## [0.3.3] - 2026-11-20

小修复版本，无 schema 迁移、无新依赖。

### Changed

- 主题列表与详情回复列表的作者栏由 `#<users.id>` 改为 `@<username>`（真实可读用户名）。中文用户名按显示宽度截断至 10 列、ASCII `~` 收尾；查不到 username 的历史行回退 `#ID`。分数 `[N]`、帖子 ID `id=`、`#` 等符号一律不变。
- 回复数格式 ` N回复` 改为紧凑 ` rN`（**该字段生产路径尚未赋值，实际不显示**，按用户拍板暂搁计数投影）。
- 新增 `UsernameReader.UsernamesByIDs` 只读端口，一屏一次批量取用户名；作者段走 `RenderUser` 净化链 + AST 守卫防误入 `RenderSystem`。

## [0.3.2] - 2026-11-20

小修复版本，无 schema 迁移、无新依赖。

### Added

- 主题列表、主题详情回复列表、搜索结果显示 `id=<数字>`（实体 ID），可直接作 `/open` / `/delete` 参数。与作者 `#ID`、分数 `[N]` 三种编号互不混淆；版块列表的 node.ID 不显示（非删除目标，避免误操作）。

## [0.3.1] - 2026-11-20

体验优化小版本，无 schema 迁移、无新依赖。

### Added

- **`.env` 自动加载**：启动时自动找当前工作目录、再找可执行文件目录的 `.env`（命中即停、不合并）；进程已有环境变量优先、文件只填空缺；`MOTD_DOTENV_DISABLED=true` 可关闭；启动日志只打来源路径，不打键值。
- `archtest` 新增「验证码预检不写库」守卫与负向注入。

### Changed

- `/verify <码>` 在收密码前先做只读预检：无效 / 过期 / 已消费码立即报错，不再白输两遍 argon2 密码；预检有独立防爆破限流（邮箱跨 IP 20 次/小时）。
- 命令输入提示改为每会话首次按 `/` 显示一次，重连重新显示，减少长会话滚动。

### Fixed

- 帖子/回复与 `/keys` 公钥详情时间此前固定按 UTC 显示，中国用户看到相差 8 小时的时间。新增 `MOTD_DISPLAY_TIMEZONE`（默认 `Asia/Shanghai`）统一控制展示时区；论坛格式 `MM-DD HH:MM` 不变，`/keys` 后缀按时间戳所在时刻的时区缩写（CST / UTC / EST…）显示，不再硬编码 " UTC"。存储层归一化未改动，历史数据无需迁移。非法时区值启动即失败，不静默回落 UTC。

## [Unreleased]

### Added

### Changed

### Fixed

### Security

## [0.3.0] - 2026-11-19

本版含正文跨行退格合并与邮箱注册/登录/找回，无 schema 迁移。

### Added

- **正文跨行退格合并**：空行行首按退格现在删除该空行并把上一行文本并入，光标停在拼接处可继续输入（此前无动作）；首行空退格保持无动作。新增白名单内 CUU+EL 整行重绘回显，ADR-027 白名单零改动，未放宽 CUP/CUD/ED。
- **邮箱注册/登录/密码找回**：`/register` / `/verify <code>` / `/resend` / `/recover` / `/login email`，自带 163 SMTP 隐式 TLS 通道；验证码只存 HMAC 散列、单事务原子消费；两段式发信（写事务提交后才触网）；argon2id(19MiB, t=2, p=1) 本地口令；复用 M2 双维限流 / 审计 / 会话撤销。表已在 0001，无新迁移。

### Changed

- `ComposerOp` 扩展跨行合并信息（`MergedLines` / `MergedText`）；标题阶段维持单行语义。
- `/login` 无参提示改为「仅支持 /login github 或 /login email」。
- `/verify` 的密码阶段（注册设密码 / 找回新密码）改为两次输入确认；两次不一致时回退重输、不消费验证码、不计尝试次数。`/login email` 仍单次输入密码。

### Fixed

- 修复邮箱流程结束后密码掩码（`setSecret(true)`）未复位、泄漏到后续命令输入的问题；done / cancelled / 新命令取代旧流程三类出口全部复位。
- SSH banner 现在始终下发登录引导（非 root 用户名 + 公钥、未绑定则访客、禁 root 禁密码），解决 root / 密码登录被拒时只见 `Permission denied (publickey)`、不知如何操作的问题；满员时叠加满员提示。

### Notes

- 邮箱凭据仅在本地 `.env` 配置，不进仓库。行内左右移动、超宽自动折行不在本版。

## [0.2.1] - 2026-11-19

低风险体验补齐，无 schema 迁移。

### Added

- 主题详情页标题与正文之间新增虚线分隔（服务端固定文案 + 暗色样式，无新增光标控制）。
- 新增 `/delete [id]`：作者可软删自己的主题或回复；无参删当前主题，带参删指定回复；非作者、访客及目标不存在统一提示「仅作者可删除」。

### Changed

- 软删不硬删、不级联：被删主题下的回复行保留，并随主题从详情、列表与全文检索中一并隐藏。检索 SQL 增加所属主题 `status='active'` 过滤，避免已删主题内容从搜索结果泄漏。

### Fixed

- 修复删除失败时写事务内部错误细节可能回显到终端的问题，统一为固定提示文案。

## [0.2.0] - 2026-11-19

第二阶段发布。补齐认证安全、论坛互动与编辑体验，迁移扩展到 0007，测试从 456 增至 632。

### Added

- **论坛互动**：投票/点赞（`/vote up|down|rm`，幂等、改票按差值计分）、全文搜索（`/search`，FTS5 trigram + 短词 LIKE 回退 + 特殊字符字面量处理）、搜索结果页（`n/p` 翻页、Enter 打开主题）。
- **认证安全**：`auth_events` 审计（有界缓冲 + 批量落库 + 溢出 fail-closed）、按 IP/用户/邮箱的认证限流（纯内存滑动窗口）、`/logout` 注销并撤销该账号全部在线会话、`/keys <指纹>` 公钥详情。
- **多行编辑**：正文 Enter 换行、单独一行 `.` 或 Ctrl-D 提交；粘贴整块进正文、内部换行不误提交；标题仍是 Enter 确认。
- **运维**：`device_auth_states` 过期清理、SSH 公钥 `last_used_at` 节流更新、`motdctl fts-rebuild` 恢复后重建索引、`.env.sample` 全量环境变量示例（17 项，带中文注释）。
- **数据完整性前置**：`threads.status` 主题软删、回复父子同 thread 约束、领域事件 outbox（事务内落库、提交后投递、崩溃可重扫）。
- 新增迁移 0004~0007；全量测试 632 个函数。

### Changed

- `/login` 发起屏改为论坛同款 Screen 与共享渲染器，状态栏与版块页一致；渐进状态行仍为固定文本。
- `TouchThread` 仅供发帖/回复；投票改用独立的 `AdjustScore`，不再影响主题排序（`last_reply_at`）。
- 恢复备份后 FTS 索引不再自动重建，需显式执行 `motdctl fts-rebuild`。

### Security

- 投票分数与主题活跃度正交，评价行为不再污染排序。
- 审计缓冲溢出 fail-closed：审计缺失期间拒绝认证动作，不做 fail-open 放行。
- `/keys` 详情将 userID 纳入查询条件，跨用户与不存在返回同一错误，防止指纹枚举。
- FTS 检索的 MATCH 串参数化并转义，用户输入的 `*`/`"`/`%`/`_` 按字面量处理。

## [0.1.0] - 2026-11-19

首个开源版本。将原本本地可用的 MVP 正式以 `v0.1.0` 公开，代码与功能未做破坏性调整。

### Added

- **SSH 优先的终端论坛入口**：用户通过 `ssh` 连入，用键盘与斜杠命令浏览版块 / 主题、发帖、回复；仅公钥认证，默认端口 `:2222`。
- **论坛业务内核**：5 个版块种子、keyset 分页、软删除过滤、访客只读、Q6 限流、统一 ViewModel。
- **GitHub Device Flow 登录**：公钥三状态解析、设备码状态机、账号登录事务、公钥绑定与软吊销、`guest → user` 会话升级。
- **四页终端页面**：版块 / 主题 / 详情 / 编辑，原地重绘导航、键位收尾与分层安全渲染（22 条恶意样本验证）。
- **M0 地基**：配置 / 日志 / 优雅停机 / `/healthz`、SQLite 双池与 PRAGMA 自检、版本化迁移（3 个）、写入队列（容量 256 快速失败、3s 超时）、备份恢复、依赖方向守卫。
- **端到端验证**：真实 SSH 客户端闭环冒烟（D1/D3/D4）、带真实数据的重启持久化与备份恢复演练（D6/D7）。
- 全量 `go build` / `go vet` / `go test -count=1 ./...` 通过，456 个测试函数。

### Changed

- README 由内部交付文档改写为开源版本，移除内部演练代号与私有文档引用，补充安装与贡献入口。

### Security

- 终端输入文本经净化后回显，仅 Secret 字段隐藏，避免终端注入与凭据回显。
- SSH 入口仅公钥认证、禁 root；host key 在数据目录首次启动时生成，不随仓库分发。
- 令牌不落库、不进日志、不进呈现结构。

[Unreleased]: https://github.com/jeeinn/motd/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/jeeinn/motd/compare/v0.5.0...v0.6.0
<!-- v0.5.1 未单独发 tag，其内容随 v0.6.0 一并发布；上方 [0.6.0] 的 compare 基准因此取 v0.5.0 -->
[0.5.0]: https://github.com/jeeinn/motd/compare/v0.4.3...v0.5.0
[0.4.3]: https://github.com/jeeinn/motd/compare/v0.4.2...v0.4.3
[0.4.2]: https://github.com/jeeinn/motd/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/jeeinn/motd/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/jeeinn/motd/compare/v0.3.3...v0.4.0
[0.3.3]: https://github.com/jeeinn/motd/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/jeeinn/motd/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/jeeinn/motd/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/jeeinn/motd/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/jeeinn/motd/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/jeeinn/motd/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/jeeinn/motd/releases/tag/v0.1.0
