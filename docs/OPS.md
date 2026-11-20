# 运维手册（OPS）

> 适用范围：P0 阶段的单机部署。恢复策略的初值会随 P3 压测与真实数据量修订。

## 1. 部署形态与目录

单个 Go 二进制 + 一个 SQLite 文件，systemd 托管（单元文件在 P0-28 落地）。

| 路径 | 用途 | 是否持久化 |
| --- | --- | --- |
| `data/motd.db` | 主库（WAL 模式） | 是，必须备份 |
| `data/motd.db-wal` / `-shm` | WAL 运行时文件 | 否，恢复时会被清理 |
| `data/backups/motd-YYYYMMDD-HHMMSS.db` | 备份快照 | 是 |
| `data/backups/*.db.sha256` | 校验和 | 是，与备份同生共死 |
| `data/ssh_host_ed25519_key` | SSH host key（P0-21） | 是，丢失会导致客户端告警 |

备份目录在 `.gitignore` 的 `data/` 之下，不会进仓库。

## 2. 备份策略（P0 初值）

| 项 | 初值 | 说明 |
| --- | --- | --- |
| 周期 | 每日 03:00 | cron / systemd timer |
| 校验 | sha256 + `PRAGMA integrity_check` + 关键表行数 | 每次备份后立即 `verify`，失败即告警 |
| 本地保留 | 14 天 | **不自动删除**，见 §5 |
| 异地 | 待定 | 需同步到另一台机器或对象存储，目标由 V 先生确定 |
| RPO | ≤ 24 h | 每日备份的最坏丢失窗口 |
| RTO | ≤ 15 min | 本次演练实测恢复 < 1 min，余量留给排查与人工确认 |

关键表行数核对清单：`users`、`nodes`、`threads`、`posts`、`schema_migrations`。前三张证明业务数据到位，最后一张证明迁移版本随备份一起恢复，恢复后不会重复执行迁移。

> 行数基线会随迁移变化，核对时以「备份前主库的实测值」为准，不要硬编码：目前（2026-11-18）共 3 个迁移（`0001_init` / `0002_seed_nodes` / `0003_rate_limit_indexes`），空库阶段 `nodes=5`（版块种子）、`schema_migrations=3`。

## 3. 命令

```bash
# 构建运维工具
go build -o bin/motdctl ./cmd/motdctl

# 每日备份（供 cron 调用）
scripts/backup.sh

# 只校验，不恢复
scripts/restore.sh --verify data/backups/motd-20261118-112724.db

# 恢复（目标路径必须不存在）
scripts/restore.sh data/backups/motd-20261118-112724.db data/motd.db
```

cron 示例：

```cron
0 3 * * * cd /opt/motd && ./scripts/backup.sh >> /var/log/motd-backup.log 2>&1
```

`motdctl` 也支持直接调用：

```bash
motdctl backup      [--db <path>] [--dir <dir>]
motdctl verify      <backup-file>
motdctl restore     <backup-file> --target <path>
motdctl fts-rebuild [--db <path>]        # P1-M3：恢复备份后重建全文索引
```

**FTS 索引维护（P1-M3）**：迁移 0007 引入 FTS5 外部内容表 `posts_fts` 与跟踪表 `posts_fts_indexed`。**从备份恢复后必须显式执行** `motdctl fts-rebuild --db <path>`（输出 `indexed=N`）重建索引，**不做启动自动重建**。只索引 `status='normal'` 的正文；`fts-rebuild` 内部用 FTS5 `delete-all` 而非 `DELETE FROM posts_fts`（后者会踩 `disk image is malformed`）。详见 `DECISIONS.md ADR-033`。

环境变量：`MOTD_DB_PATH`（默认 `./data/motd.db`）、`MOTD_BACKUP_DIR`（默认 `./data/backups`）、`MOTDCTL_BIN`（脚本用的工具路径）、`MOTD_GITHUB_CLIENT_ID`（P0-15 起由 `config.Load` 读取，默认空；见下方凭据小节）。

**配置入口：`.env.sample`**（仓库根目录）。它是全部环境变量的带注释清单——数据位置、监听地址、日志、GitHub 凭据、认证安全（P1-M2）共 16 项，末段还预留了下一阶段邮箱包的变量名（当前设置不生效）。部署时复制为 `.env` 再修改：

```bash
cp .env.sample .env
# 编辑 .env 填入实际值，尤其是 MOTD_GITHUB_CLIENT_ID
chmod 600 .env
```

`.env` 含凭据、已被 `.gitignore` 排除；`.env.sample` 可安全提交。**空值 = 未设置 = 回落默认值**，想用默认值就留空或注释掉该行。

**`.env` 自动加载（v0.3.1 起，默认开启）**：程序启动时自动读 `.env`，查找顺序为**当前工作目录 → 可执行文件所在目录**（可执行文件软链会先解析），命中第一个即停、不合并；两处都没有则静默走纯环境变量，不报错。**进程已有环境变量优先**，`.env` 只填空缺——systemd `EnvironmentFile=` 注入的凭据不会被旁路文件覆盖。开关 `MOTD_DOTENV_DISABLED=true` 可完全关闭自动加载（systemd 全靠 EnvironmentFile 时用；该开关建议由 `Environment=` 外部注入，写进被加载的 `.env` 要下次启动才生效）。启动日志只输出 `配置来源 file=<路径>` 或 `配置来源 env_only`，**绝不输出任何键值**。解析口径：整行注释/空行忽略、可选 `export` 前缀、值可成对引号包裹、未加引号的 `#` 是普通字符（授权码可能含它）、非法行静默忽略、单行上限 1MiB。

脚本 `backup.sh` / `restore.sh` 只单独读 `MOTD_DB_PATH` / `MOTD_BACKUP_DIR` / `MOTDCTL_BIN` 三项，**不加载整个 `.env`**——cron 里要么显式 export，要么在脚本外层 source。

### 展示时区（MOTD_DISPLAY_TIMEZONE，v0.3.1-patch）

- 变量名 `MOTD_DISPLAY_TIMEZONE`，默认 `Asia/Shanghai`。支持 IANA 名（`Asia/Shanghai`、`America/New_York`）、`UTC`、`Local`；空串按未设置回落默认。
- **仅影响终端呈现**：论坛列表/详情时间（`MM-DD HH:MM`）与 `/keys` 公钥详情的创建 / 最近使用时间（绝对时间 + 时区缩写，如 `CST`）。存储层仍以 Unix 秒归一化，历史数据无需迁移、无新迁移。
- **非法值启动期直接失败**并打印含变量名的错误，绝不静默回落 UTC——静默回落正是本次要修的体验 bug。进程环境变量 / systemd `EnvironmentFile` 优先于仓库 `.env`（复用 v0.3.1 的自动加载优先级）。
- 二进制已内嵌 IANA 时区库（`time/tzdata`），distroless / scratch 镜像无需额外挂载 zoneinfo；代价是二进制体积增加数百 KB。
- 时区是**进程级展示策略**（等价 locale），启动期注入一次、之后只读（`atomic.Pointer`），多 SSH 会话并发渲染无锁安全；不是会话状态，不随连接变化。

### SSH 入口（P0-21）

- `MOTD_SSH_ADDR`（默认 `:2222`，Q4 已拍板）、`MOTD_SSH_MAX_CONNS`（默认 100）、`MOTD_SSH_IDLE_TIMEOUT`（默认 `10m`）。
- **登录引导 banner**：连接时服务端**始终**下发两行固定提示——「请用任意非 root 用户名 + 本机 SSH 公钥登录（禁 root、禁密码认证）」与「未绑定公钥将以访客进入；访客可浏览并使用 /register 完成注册」。满员时前面叠加一行「连接数已满（N/M）」。这解决 root / 密码登录被拒时客户端只显示通用 `Permission denied (publickey)`、用户不知如何操作的问题。banner 是服务端固定文案（ADR-019），不含任何用户数据或凭据。
- **正确连入方式**：`ssh-keygen -t ed25519` 生成本地密钥后，`ssh -i ~/.ssh/id_ed25519 -p 2222 <任意非 root 用户名>@127.0.0.1`。用户名只是客户端声明，任意非 root 均可；**未绑定的公钥以访客身份放行**（可浏览、可 `/register`），登录后才绑定为登录态。禁 root、禁密码、禁匿名。
- host key 落 `<MOTD_DATA_DIR>/ssh_host_ed25519_key`，首次启动生成、权限 0600；**重启必须复用同一把**，否则客户端报 host key changed。备份时应一并纳入该文件的保护范围。
- 仅公钥认证，禁密码、禁 root。对外 22 → 本机 2222 的转发与防火墙放行由部署方完成（目标为 Linux/systemd，用户已确认放行）。
- 满员时新连接在握手前收到明文拒绝提示；`/healthz` 的 `online` / `active` 反映内存注册表（ADR-016），重启归零。

### GitHub OAuth App 凭据（Q7 已完成）

- **2026-11-18：V 先生已注册 OAuth App 并启用 Device Flow，`client_id` 已取得。**
- 变量名 **`MOTD_GITHUB_CLIENT_ID`**，由 `internal/config` 在 P0-15 读取后注入 `internal/infrastructure/github`。换 token **不需要** `client_secret`（P0-PLAN §7 R1），凭证只放服务端环境变量。
- **纪律**：`client_id` 不进仓库、不写进 systemd unit、不留在命令行历史。unit 是 world-readable，明文 `Environment=` 等同于泄露；用独立环境文件：

  ```bash
  sudo install -d -m 0750 /etc/motd
  sudo install -m 0600 -o root -g root /dev/null /etc/motd/motd.env
  # 写入：MOTD_GITHUB_CLIENT_ID=<你的 client_id>
  ```

  unit 内只写 `EnvironmentFile=/etc/motd/motd.env`（`deploy/motd.service` 由 P0-28 产出）。

- **开发 / 测试不需要设置它**：`httptest` 假端点覆盖 Device Flow 全部分支，不依赖真实凭据与外网。未设置时 `/login github` 会返回专门的固定提示「未配置 MOTD_GITHUB_CLIENT_ID，无法发起 GitHub 登录…」（`auth.LoginTextClientIDMissing`），不 panic、不静默失败、不回显上游错误原文；`/healthz` 不回显该值。
- **P0-15 落地事实**（ADR-021）：`config.Load` 读取该变量并注入 `infrastructure/github.Client`；两个需要 `client_id` 的 POST 在缺值时返回 `auth.ErrClientIDMissing`，不发出任何请求。启动日志只输出 `client_id_configured=true|false` 与 `scope`，**不打印凭据值**，也不会因缺值 panic。
- 出站要求：VPS 需允许访问 `github.com:443`（Device Flow 两端点）与 `api.github.com:443`（`/user`、`/user/emails`）。本机 `httptest` 测试不需要外网。
- 真实联调（D2 手工验收）与 P0-28 部署自检前必须就位。

### Device Flow 状态表（P0-16）

- `device_auth_states` 在 `0001_init.sql` 已建好，**部署时无需额外迁移**；启动日志 `migrations_applied=[1 2 3]` 即表示就绪，另有 `设备码状态机就绪 store=device_auth_states`。
- 该表**只存设备码中间态，绝不存令牌**：`access_token` 只在服务端进程内交 P0-17 调 `/user` 与 `/user/emails` 完成建号或登录后立即丢弃（Q3 / ADR-022 / ADR-023）。备份文件里也不会出现令牌。
- 状态四态 `pending / authorized / denied / expired`；终态不可变，过期码不再轮询。轮询间隔 `polling_interval` 会随 GitHub `slow_down` 持久化放大，启动后按新会话重新从 5s 起算。
- 表会**保留所有历史行**（含过期与终态），只增不删；`idx_device_auth_states_expiry` 已存在。P0 数据量极小，无需运维干预；P1 可加定期清理 job（清理条件 `status <> 'pending' AND expires_at < now - 保留窗口`，且须避开在途登录）。**不要手工 DELETE 表内行**——正在轮询的会话会因 `ErrDeviceAuthNotFound` 中断。
- 排查登录卡住时优先看三点：`MOTD_GITHUB_CLIENT_ID` 是否注入、VPS 出站 `github.com:443` / `api.github.com:443` 是否放行、状态行是否停在 `pending` 但 `expires_at` 已过（下轮门禁会自动迁 `expired` 并提示重新发起）。

### 账号与身份（P0-17）

- `users` / `identities` 在 `0001_init.sql` 已建好，**部署无需额外迁移**；启动日志 `账号登录事务就绪 store=users+identities provider=github` 即表示装配成功。
- **身份主键是 `(provider, provider_uid)`，`provider_uid` 取 GitHub 数字 id**（不是 login、不是邮箱）。因此改 GitHub 用户名不影响账号归属，`users.id` 也不会变。
- **`users.primary_email` 允许重复，不要给它加唯一索引**：这是「禁止按邮箱自动合并」的结构保证（ADR-023 / §7 R7）。两个不同 GitHub UID 用同一邮箱登录会各自建号，并给后者返回「请先登录原账号再 `/link`」的提示——这是预期行为，不是数据异常。
- **`identities` 的 `UNIQUE (provider, provider_uid)` 不能删**：它是并发首登幂等的最后防线。撞唯一约束时用例会整体重试并转为登录，不会产生第二个账号。
- 用户名由 GitHub login 派生：两字中文会补下划线（`张三` → `张三_`），超长截断到 20 字符，同名冲突追加数字后缀（`alice` → `alice2`）。`users.username` 是 `COLLATE NOCASE UNIQUE`，大小写变体算同名。
- **账号行不要手工改 `status` 再期望能登录**：登录只接受 `active`；`pending / deleting / deleted` 会被拒绝（失败关闭）。注销流程应由应用侧驱动，不要直接 UPDATE。
- 令牌不落库：`users` / `identities` 两张表内不存在任何 token 列。备份文件里不会出现 `gho_` 前缀的访问令牌。
- 排查登录失败：`GitHub 用户资料失败` / `邮箱列表失败` 看 VPS 出站 443 与 client_id；`账号状态不允许登录` 看 `users.status`；`用户名候选全部被占用` 属极端情况（同一 login 下 20 个候选全被占），可人工改 `users.username` 后重试。

### 登录交互（P0-19，最小交互层）

- 启动日志 `登录交互就绪 mode=minimal_in_process presentation=forum_screen provider=github` 表示 `/login github` 已装配；发起屏在 P1-M2 已替换为论坛同款 Screen（见下方 M2 小节），整条链路仍在进程内（Q3）。
- **整条链路在进程内**（Q3）：`SSH session → application/auth → infrastructure/github`，**没有任何业务 HTTP 端点**，`/healthz` 之外不要为登录放行端口。出站仍需 `github.com:443` / `api.github.com:443`。
- 终端只展示 `user_code` / `verification_uri` / 剩余时间与固定状态行；**`device_code` 与 `access_token` 永不写终端、永不落库、永不进日志**。备份与 `device_auth_states` 里都不会出现 `gho_` 前缀令牌。
- 轮询在服务端按 `interval`（钳制到 5–60 秒）进行，间隔由 GitHub 响应的 `interval` / `slow_down` 决定，**运维侧不要试图改小**。连续网络错误重试 3 次后自动终止并提示重新 `/login github`，不会无限占用连接。
- `Esc` 取消会把该次 `device_auth_states` 迁为 `denied`；状态停在 `pending` 且 `expires_at` 已过时，下轮门禁自动迁 `expired`。**不要手工 DELETE 该表行**（在途登录会因 `ErrDeviceAuthNotFound` 中断）。
- 登录成功后当前连接公钥自动绑定（D2）；断开重连由 `ssh_keys` 指纹直接解析为登录态（D3），无需再次 `/login`。
- **已知边界（R19）**：若当前连接公钥已属于其他账号，账号会创建成功但**本次会话保持访客态**，终端给出固定提示；此时 `device_auth_states` 是 `authorized` 终态，不是失败。换一把未绑定的公钥重连后重新 `/login` 即可（UID 命中即登录）。详见 `DECISIONS.md ADR-025`。

### 认证安全（P1-M2）

**新增环境变量**（`MOTD_` 前缀，空值回落默认；见 `DECISIONS.md ADR-030`）：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MOTD_AUTH_LIMIT_MAX_FAILURES` | `5` | 窗口内允许的最大失败/尝试次数 |
| `MOTD_AUTH_LIMIT_WINDOW` | `5m` | 滑动窗口长度 |
| `MOTD_AUTH_LIMIT_LOCKOUT` | `15m` | 超限锁定时长 |
| `MOTD_AUTH_LIMIT_MAX_KEYS` | `4096` | 内存限流键上限（超限淘汰最旧） |
| `MOTD_AUDIT_CAPACITY` | `512` | 审计环形缓冲容量 |
| `MOTD_AUDIT_FLUSH_INTERVAL` | `1s` | 批量落库周期（仅 `cmd/server` 持有定时器） |

**`/healthz` 新增指标**：`audit_depth` / `audit_cap` / `audit_dropped` / `audit_flushed` / `audit_failed` / `limit_keys`。

- **`audit_dropped > 0` 或 `audit_failed` 持续增长应立即告警**：此时审计缓冲溢出、认证动作正被 fail-closed 拒绝（用户会看到"操作过于频繁"）。处理方向先查 SQLite 写队列是否堵塞，再考虑调大 `MOTD_AUDIT_CAPACITY`；不要靠调小 flush 间隔掩盖写通道饱和。
- **限流计数纯内存**：进程重启清零、多实例不共享；SSH 对端 IP 在 application 层只以 `hash(ip)` 出现，明文 IP 只在 Session 内存里，不入库/终端/日志。M2 无邮箱流程，登录失败键实际只用 `hash(ip)`（emailHash 留接口待 M5）。
- 四类场景共用一份额度：login_failure / device_start / key_binding / verify，默认 5 次/5m/锁 15m；同一用户 5 分钟内第 6 次登录或 key 操作会被锁，这是当前口径（是否放宽待真实数据支撑）。

**`/logout` 命令**：撤销该账号**全部**在线会话（含当前连接），访客拒绝；先撤销后写审计，审计失败不回滚撤销（已踢下线的会话不恢复，见 `ADR-031`）。已吊销公钥的已有连接由 P0-18 的 `RevokeSessions` 覆盖。

**`/login` 呈现（M2 替换）**：发起屏已改为论坛同款 Screen（`ScreenLogin` + 共享 `Renderer` + 同源状态栏），`RenderSystem` 固定文案与 `RenderUser` 外部字段严格分离；`device_code` / `access_token` 仍永不进 view（ADR-021）。轮询过程的 waiting/retry/终态仍是逐行固定文本（进度反馈，不做整屏重绘）。

**审计落库**：`auth_events` 表复用 0001 已有 schema 与 `idx_auth_events_time`，无新迁移；application 只入队，落库走 `Writer.Submit` 批量 INSERT。

**本地复现**：`./scripts/repro_auth_limit.sh`（隔离临时库，不碰 `data/`），实测输出 `accepted=3 rejected=2 retry_after=2m0s auth_events=5 dropped=1`。

### 论坛互动（P1-M3）

- **投票** `/vote <up|down|rm>`：只对**当前打开的 thread**，访客拒绝。投票走 `AdjustScore` 只改 `threads.score`、**不推进 `last_reply_at`**，因此不影响 `SortLatest` 排序（ADR-032）；同一用户对同一目标重复投票不加分（幂等），改票按差值计（`-1 → +1` = +2）。
- **搜索** `/search <关键词>`：≥3 码点走 FTS5 trigram，<3 码点回退 LIKE；结果页 `n/p` 翻页、Enter 打开主题，页长锁 20、摘要 ≤240 字节。用户输入的 `*`/`"`/`%`/`_` 均按字面量处理（防 FTS 语法注入）。
- **恢复后索引重建**：见上方 `motdctl fts-rebuild`——恢复备份后 FTS 索引不会自动回来，必须手动重建，否则搜索返回空。这是刻意设计（重建时机由运维掌控，避免启动时整库扫描）。
- 审计事件 `EventVoteCast` 随投票写同事务入 outbox；幂等键固定，插件只感知**首次投票**（改票/撤销事件被去重）。

### 正文编辑与后台运维（P1-M4）

**多行正文编辑（P1-M4，v0.3.0 增强）**：`/new` 与 `/reply` 进入多行编辑态——正文 **Enter 换行**，提交用**单独一行 `.` 后按 Enter** 或 **Ctrl-D**；标题字段仍是单行、Enter 确认。粘贴（bracketed paste）整块进正文，内部换行不拆成提交，粘贴进来的 `.` 行不会被误判为哨兵。**通用 SSH 终端不支持可靠的 Ctrl+Enter**（字节级与 Enter 相同），服务端固定文案不含该组合字样；若某终端显示 Ctrl+Enter 提示，那是过时版本。编辑器有行数与字节双上限，超限给明确提示。详见 `DECISIONS.md ADR-034`。

**跨行退格合并（v0.3.0）**：正文空行行首按退格，现在会删除该空行并把上一行文本并入，光标停在拼接处可继续输入（此前该操作无动作）；首行空退格保持无动作。回显只用 ADR-027 白名单内的 CUU+EL（+SGR）整行重绘，**未放宽 CUP/CUD/ED**，用户文本与控制序列仍走两条独立写入路径。已知边界：单逻辑行超终端宽度自动折行时，CUU 只上移一个物理行（与既有宽字符退格限制一致，本版不处理）；行内左右移动本版不做。

**两项后台行为（均无 schema 变更，迁移仍止于 0007）**：

1. **`device_auth_states` 清理**：启动时立即执行一次、之后每小时一次；只删**自身 `expires_at` 已过**的行（四态共用该条件）。未过期的 `pending` 代表在途设备码登录，绝不删除；参数化 DELETE 走唯一写队列的单事务，可重复执行（第二次零删除）。停机时先等清理循环退出再关写队列。pending 严格按 `expires_at` 清理——客户端恰在过期瞬间仍在轮询时，下一次会拿到"码不存在"（既定口径，见 ADR-035）。
2. **`ssh_keys.last_used_at`**：公钥认证成功且命中已绑定用户后，同一指纹 **≥5min 最多排一次写**，异步经写队列完成，失败只记日志不影响认证。采用「先标记窗口、后异步写」，写失败时该钥匙需等下一个窗口才重试——详情页最近使用时间**可能滞后一个窗口**（ADR-036）。

**`/keys <fingerprint>`**：查看单把钥匙的标签 / 创建时间 / 最近使用时间；标签当前无输入口、恒为空。跨用户与不存在返回同一错误（防指纹枚举）；只覆盖未吊销钥匙（ADR-037）。

### 作者删除（v0.2.1）

- `/delete` 是**软删**：仅把 `threads.status` 或 `posts.status` 置为 `deleted`，不物理删除、不级联。无参删当前打开的主题（成功后回列表），带 `<id>` 删指定回复（成功后刷新详情）；必须在主题详情页使用，否则给固定提示。
- 只有作者本人能删自己的内容；非作者、访客、目标不存在或已删，一律回「仅作者可删除」——统一文案，不泄露状态或事务细节。所有权判定集中在 application 层一个 `canModify` 函数，将来引入版主只在这一处加分支。
- 被删主题下的**回复行保留在库**，但详情、列表与全文检索均不可见——检索的两条 SQL 都按所属主题 `status='active'` 过滤。运维从 DB 层手工改 `status` 时遵循同一口径：`deleted` 对公开读等价于不存在，`hidden` 预留给将来治理角色、当前无入口。
- FTS 索引由 0007 触发器在 `posts.status` 变更时同步；删除主题本身不动 posts 行，索引行仍在但被查询层过滤，这是预期行为，**无需用 `fts-rebuild` 清理**。无公开恢复入口（被删内容如需找回只能从 DB 层操作，属非目标）。
- 本轮无新迁移（仍止于 0007），无需回填。冻结账号（status 非 active）当前仍可删自己内容，统一提示口径优先。

### 邮箱认证（v0.3.0-email）

**环境变量**（在 `.env` 里配置，见 `.env.sample`）：`MOTD_EMAIL_ENABLED`（默认 `false`）、`MOTD_SMTP_HOST`、`MOTD_SMTP_PORT`（默认 465）、`MOTD_SMTP_USER`、`MOTD_SMTP_PASS`、`MOTD_EMAIL_FROM`。

- **凭据纪律**：163 用**授权码**（不是邮箱登录密码）；只从 `.env`（0600 root:root）读，不进仓库、不进 systemd unit、不进日志。启动日志只打 `enabled/host/port`，不打 user/pass。开启时六项缺任一即报错、明确点名缺哪项，不静默兜底。
- **通道**：465 隐式 TLS + PLAIN AUTH（163 已实测连通）；发信有界并发 4、独立 10s 超时；单一 Sender 实现，不接第三方邮件 API。IMAP 993 未接入（找回走发码 + SSH 输入，不读邮箱）。
- **终端命令**：`/register`（交互输邮箱 → 发码）、`/verify <验证码>`（见下方密码确认流程）、`/resend`、`/recover`（统一响应"若邮箱已注册则已发送"防枚举）、`/login email`。`/login` 无参提示改为「仅支持 /login github 或 /login email」。
- **密码二次确认（用户实测反馈后新增）**：`/verify` 的密码阶段（注册设密码 / 找回新密码）输入两遍——第一遍后提示「请再次输入密码确认（Esc 取消）」，两遍一致才提交 service（**此时才消费验证码**）。不一致时提示「两次密码不一致，请重新输入 /verify <验证码>。」并回到首次输入，验证码不消费、不计尝试次数。两遍均掩码回显 `*`；`/login email` 仍是单次密码输入（登录可重试，不做确认）。
- **掩码复位（用户实测 bug 修复）**：邮箱流程在 done / cancelled / Esc / 新命令取代旧流程三类出口都会复位 `setSecret(false)`，不会把 `*` 掩码泄漏到后续命令输入（如 `/login`）。
- **验证码**：TTL 10min、只存 HMAC 散列不存明文；60s 重发冷却、每小时 5 封、5 次尝试（错码 5 次锁 15min），叠加 IP 维度。`HMAC pepper 为进程随机`——**服务重启会使在途验证码失效**，用户重新 `/resend` 即可；这是为"库泄漏无法离线爆破 6 位码"接受的代价。
- **账号口径**：同一邮箱不会自动合并到已有账号（`primary_email` 无 UNIQUE 是结构性保证）；占用时提示「不会自动合并；如需变更归属请联系管理员处理」——当前无 `/unlink` 命令，变更归属只能从数据层处理。`/recover` 成功后该用户**全部 SSH 会话立即失效**，需用 `/login email` 重新登录。
- **无新迁移**：`email_tokens` / `users` / `identities` / `local_credentials` 均已在 0001 建好，迁移仍止于 0007。发信在写事务**提交之后**执行（3s 写超时扛不住 SMTP），失败则 token 自然过期、不补偿删除。
- 用户后期更换邮箱凭据：只改 `.env` 三行（USER / PASS / FROM），代码无需改动。

## 4. 恢复流程

1. **停服**：`systemctl stop motd`。
2. **保留现场**：把现有库移走，**不要直接删除**：
   ```bash
   mv data/motd.db        data/motd.db.broken-YYYYMMDD
   mv data/motd.db-wal    data/motd.db-wal.broken-YYYYMMDD    2>/dev/null || true
   mv data/motd.db-shm    data/motd.db-shm.broken-YYYYMMDD    2>/dev/null || true
   ```
3. **恢复**：`scripts/restore.sh <backup-file> data/motd.db`。脚本会先做校验和 + 完整性验收，任一失败即中止。
4. **启动**：`systemctl start motd`，`curl -s http://127.0.0.1:8080/healthz`（应返回 `ok <version> queue=0/256 online=0 active=0 commits=0`）。
5. **抽查**：用户数、最新主题、迁移版本是否与备份点一致。
6. **记录**：把命令、结果、耗时写回本文件 §4 的演练记录。

> 恢复目标已存在时 `motdctl restore` 会直接拒绝（`ErrTargetExists`）。这是刻意设计：防止误把备份恢复覆盖到正在运行的库上。

## 5. 恢复演练记录

### 2026-11-18 11:27（首次，实际执行）

| 步骤 | 命令 | 实测结果 |
| --- | --- | --- |
| 备份 | `motdctl backup --db ./data/motd.db --dir ./data/backups` | `data/backups/motd-20261118-112724.db`，sha256 `3ad13345ad1435fc439840d50111e1202a4a6c957e2da1d2d4ef64e920206b0f`，size 151552 |
| 校验 | `motdctl verify <file>` | `integrity=ok`；`schema_migrations=1`，`users/nodes/threads/posts=0`（当时只有 `0001_init`，且为空库阶段） |
| 恢复 | `motdctl restore <file> --target ./data/restore-drill/motd.db` | `restored=./data/restore-drill/motd.db`，产物 151552 字节 |
| 启动验证 | 以恢复库启动 `motd.exe` | `数据库就绪 migrations_applied=0`、`写入队列就绪 capacity=256`、`探针监听` —— 自检全部通过 |

**结论**：D7（从备份恢复到空目录且可运行）达成。恢复后 `Migrate` 应用 0 个版本，说明迁移版本随备份保存。

> 该演练发生在 `0002` / `0003` 迁移落地之前。现在空库基线为 `nodes=5`、`schema_migrations=3`；恢复流程不变，但**校验时的行数必须与备份前主库实测值比对**，不能沿用上面的旧数字。`VerifyBackup` 只报数不做跨版本硬编码判断，备份测试也已改为相对基线断言。

**待办**：当前库无业务数据。P0 闭环跑出真实帖子/账号后，必须再做一次**带数据的恢复演练**，核对 `users` / `posts` 行数与最新内容，不能只靠空库演练结案。

演练中还修掉一个真实缺陷：`flag` 包遇到首个位置参数即停止解析，导致 `restore <file> --target <path>` 把 `--target` 吞掉。已在 `cmd/motdctl` 做参数归一化并加回归测试。

### 2026-11-19 13:57（P1-M0 带数据 kill-9 + WAL 恢复演练，隔离环境实际执行）

> 关闭上方「带数据的恢复演练」待办。本节为**隔离环境**实测：`X:/motd/data/d6-drill-20261119/`，独立端口 18080/22222，**未触碰生产 `data/` 与生产实例**。原始输出与崩溃态三件套保留在同目录 `evidence/`。

| 步骤 | 命令 | 实测结果 |
| --- | --- | --- |
| 基线 | 隔离 DataDir/端口启动，`/healthz` | `ok dev queue=0/256 online=0 active=0 commits=0`；`journal_mode=wal`、`integrity=ok`、`schema_migrations=3`、`nodes=5` |
| 写基线数据 | Go 工具经 `Writer.Submit` 事务写 b1（无 SSH 密钥，写入路径与线上同构） | users=4 / threads=3 / posts=6；最新帖 `D6-MARK-b1-P06-ts=1795067820895680200` |
| 备份 | `MOTD_DB_PATH/MOTD_BACKUP_DIR/MOTDCTL_BIN=隔离路径 scripts/backup.sh` | `motd-20261119-135707.db`，sha256 `934544a86ffda3822fb776c426ce9f95a4256d9005839c1e110f164ef7490412`，159744 B，548 ms |
| 校验 | `scripts/restore.sh --verify <file>` | `integrity=ok`；五表行数与 b1 一致 |
| 备份后写入 | 同工具写 b2（RPO 窗口内） | users+2 / threads+2 / posts+3；kill 前主库 4096 B、WAL **280192 B**（事务在 WAL 未 checkpoint） |
| 硬杀 | `taskkill /PID <隔离PID> /F`（13:57:28.537） | PID 命令行二次校验为隔离实例、非生产 36604 |
| WAL 恢复 | 直接打开 kill 后库 | `integrity=ok`；users=6 / threads=5 / posts=9、mark b2=3 —— **已提交事务全部重放**，硬杀不损坏库 |
| 恢复 | `scripts/restore.sh <file> <新target>` | 577 ms；`integrity=ok`，精确回到 b1（users=4 / posts=6、mark b2=0、最新帖回到 b1 标记） |
| 恢复库启动 | 以恢复库另起实例（端口 18081/18082） | `/healthz` ok（两次）；`migrations_applied=0`、`nodes=5`；启动到就绪 692 ms |

**结论**：D6（kill-9 硬杀 + WAL 恢复 + 备份恢复）达成。RPO 口径 = 最后一次成功备份完成到故障时刻的写入：实测窗口 20.8 s，窗口内丢失 1 个已提交批次（2 用户 / 2 主题 / 3 帖）；**生产策略初值仍为每日备份（RPO ≤ 24h）**，本次证明的是「备份机制在 WAL 写入下可用且恢复精确」，不等于生产 RPO 已收紧。RTO 纯技术口径 = 恢复 577 ms + 启动就绪 692 ms ≈ **1.3 s**；端到端口径（kill→恢复库首次 `/healthz` ok，含取证开销）≈ **50.6 s**，均优于 §2 的 RTO ≤ 15 min 初值。小库结论，大数据量下 `VACUUM INTO` 耗时需按 §7 条件重测。

> 是否把每日备份收紧到小时级以降低 RPO，属业务取舍，未替用户决定。

### 2026-11-19 14:32（P1-M1 迁移应用与回滚演练，隔离环境实际执行）

> 目的：M1 三条前置债迁移（0004/0005/0006）在**带历史数据的 v3 库**上应用、验证、回滚。全程隔离 `X:/motd/data/m1-drill-20261119/`，独立端口 18090/22230、18091/22231，**未触碰生产 `data/`**（生产库 mtime 保持 13:57 不变）。v3 源码由 `git archive HEAD` 导出编译，不动工作区与 `.git`。

| 步骤 | 命令 / 工具 | 实测结果 |
| --- | --- | --- |
| v3 基线库 | v3 二进制（`schema_migrations=3`）启动隔离 DataDir | `migrations_applied=3 applied_versions=[1 2 3]`、`nodes=5` |
| 造历史数据 | `m1verify seed`（经 `sqlite.Open`→`Writer.Submit`→事务提交的生产同构写路径） | users=1 / threads=2 / posts=4；`threads.status` 列**不存在**、`domain_event_outbox` 表**不存在** |
| 迁移前备份 | `motdctl backup`（回滚锚点） | `motd-20261119-143217.db`，sha256 `2cf5e9575b42667ed7887adc0e78899007a457e8861db5abdae1ba52c064c2d5`，159744 B |
| 应用迁移 | M1 二进制启动同一库 | `migrations_applied=3 applied_versions=[4 5 6]`；`/healthz` ok；日志 `outbox 恢复就绪 requeued=0 redelivered=0 failed=0` |
| R9 验证 | `m1verify inspect` | `schema_migrations=6`、`threads.status[active]=2`（**历史行自动回填**） |
| R8 验证 | `SELECT name FROM sqlite_master WHERE type='trigger'` | `posts_parent_same_thread_insert` + `posts_parent_same_thread_update` 两条均建成；业务数据 threads=2/posts=4 完整 |
| R12 验证 | `m1verify inspect` | `domain_event_outbox=0`（表已建、空表） |
| 回滚 | `motdctl restore <迁移前备份> --target <新路径>` | 恢复成功；`schema_migrations=3`、`threads.status=<column absent>`、无 outbox 表、无触发器；users=1/threads=2/posts=4 数据完整 |

**结论**：M1 三项迁移在带历史数据库上应用成功、效果符合 Spec、从迁移前备份回滚精确回到 v3（schema 与数据双一致）。全量 `go build`/`go vet`/`go test -count=1 ./...` 绿，测试函数 456→506。`ADR-028`（outbox）、`ADR-029`（数据完整性前置）已记录。崩溃态证据与中间产物保留在 `data/m1-drill-20261119/`。

### 2026-11-20 11:18（P2-M0 节点 status 迁移 v7→v8 / 回滚 / kill-9 + WAL，隔离环境实际执行）

> 目的：补 P1 欠账的 `0008_nodes_status.sql`（ADR-048）在**带历史数据的 v7 库**上应用、回滚、硬杀恢复三项验收。全程隔离 `X:/motd/data/m0-drill-20261120/`，独立端口 18090/22230，**未触碰生产 `data/motd.db` 与生产实例**（生产 PID 9292 保持运行；演练库 main 文件 mtime 保持 10:19:13、sha256 `9a0ce9ce23957c0f57036a19d27222b1c1d39a529f45fc6a12cca1780f6a83cb` 前后一致）。v7 源码由 `git archive HEAD` 导出编译，v8 源码为工作区副本，均不动 `.git` 与生产二进制。原始输出保留在同目录 `evidence/`，服务日志在 `logs/`。

| 步骤 | 命令 / 工具 | 实测结果 |
| --- | --- | --- |
| v7 基线 | `m0verify-v7 init <隔离库>` | `init applied=[1 2 3 4 5 6 7]`；`schema_migrations=7`、`nodes=5`、`nodes.status=<column absent>`、`integrity=ok`、`journal_mode=wal` |
| 造历史数据 | `m0verify-v7 seed <库> B1`（经 `sqlite.Open`→`Writer.Submit`→事务提交的生产同构写路径） | users=1 / threads=2 / posts=4；`mark[B1]=2`、`mark[B2]=0` |
| 迁移前备份 | `motdctl-v7 backup --db <库> --dir <备份目录>` | `motd-20261120-111813.db`，sha256 `c9e3fdcfcdc86f66653762367a58398928a5ad8a2f5faae93ba3527412a6e155`，196608 B |
| 备份校验 | `motdctl-v7 verify <备份>` | `integrity=ok`；`schema_migrations=7` 行、users=1 / nodes=5 / threads=2 / posts=4 |
| 应用迁移 | 以 v8 二进制启动同一库（隔离端口 18090/22230） | 日志 `migrations_applied=1 applied_versions=[8]`；`/healthz` ok（`dev queue=0/256 online=0 active=0 commits=2 audit_depth=0 …`）；`schema_migrations=8` |
| R 验证 | `m0verify-v8 inspect <库>` | `nodes.status[active]=5`（**5 个 seed 自动回填 active**）；threads/posts 2/4 完整；`mark[B1]=2` |
| 迁移后写入 | 服务存活期间 `m0verify-v8 seed <库> B2`（WAL 窗口内） | users=2 / threads=4 / posts=8；`mark[B1]=2`、`mark[B2]=2`；主库 196608 B、WAL **197792 B**（事务在 WAL 未 checkpoint） |
| 硬杀 | `taskkill /PID 29928 /F`（11:18:35.695） | PID 经 `Win32_Process.CommandLine` 二次校验为 `data/m0-drill-20261120/bin/server-v8.exe`，非生产 9292；18090 端口随即不再监听 |
| WAL 恢复 | 直接打开 kill 后库 | `integrity=ok`、`schema_migrations=8`；users=2 / threads=4 / posts=8、`mark[B2]=2` —— **已提交事务全部重放**，硬杀不损坏迁移后库 |
| 回滚 | `motdctl-v8 restore <迁移前备份> --target <新路径>` | 218 ms；`integrity=ok`；`schema_migrations=7`、`nodes.status=<column absent>`；users=1 / threads=2 / posts=4、`mark[B1]=2`、`mark[B2]=0`（schema 与数据双精确） |

**结论**：`0008` 在带历史数据的 v7 库上迁移成功，5 个 seed 自动回填 active、业务数据无损；从迁移前备份回滚精确回到 v7；迁移后带数据硬杀 + WAL 重放完整，三项 DoD 全达成。全量 `go build ./...`/`go vet ./...`/`go test -count=1 ./...` 绿，测试函数 768→777（`0008` 回填与回滚 2、公开列表过滤 1、节点守卫与负向注入 6）。P0 工程门禁对 M0 改动文件零新增命中（与 HEAD 基线同为 54 处既有命中，均在未触碰代码）。`ADR-048` 已记录；演练证据与 v7/v8 源码副本保留在 `data/m0-drill-20261120/`。生产 `data/motd.db` 主文件 mtime 与 sha256 前后一致，未受演练影响。

> RPO/RTO 口径沿用 2026-11-19 13:57 条（本次为迁移正确性演练，不构成生产备份策略变更）。节点归档为软删，M1 CRUD 落地时须由代码层守卫拦截 seed 删/改名，见 `ADR-048`。

## 6. 保留与清理（不自动删除）

脚本只生成备份，**不做任何自动删除**。列出过期候选：

```bash
find data/backups -name 'motd-*.db' -mtime +14 -print
```

人工核对后，删除时同时删除对应的 `.sha256`。把删除逻辑放进定时脚本风险过高：一次路径变量写错就可能清掉全部备份，而备份是唯一回退点。

## 7. `VACUUM INTO` 的代价与切换条件

- 产出一致性快照，WAL 下不需要停服，也不阻塞读。
- 代价是**整库重写**：库越大，IO 峰值越明显；期间写事务在队列中排队。若队列被打满，写请求会拿到 `ErrQueueFull`，属于预期降级而非故障。
- 本进程写连接恒为 1，单机部署下不存在跨进程写锁竞争。

满足以下任一条件时，评估切换到 SQLite Online Backup API（增量页拷贝）：

1. 库文件 > 500 MB，或单次备份耗时 > 60 s；
2. 备份窗口内 `ErrQueueFull` 出现频率已影响业务；
3. 备份期间 p99 首屏时间相对 P0-27 基线明显劣化。

以上三项目前均未测量，P0 保持 `VACUUM INTO`。

## 8. 监控与告警（P3 落地）

- `/healthz` 返回 `ok <version> queue=<depth>/<cap> online=<n> active=<n> commits=<n>`：`queue` 观察写入背压，`online/active` 是内存在线注册表（重启归零，非业务数据），`commits` 是进程启动以来成功提交的事务数。全部为运维可观测字段，不承载业务流量（ADR-006 / ADR-016）。
- **连接级心跳**：每条活 SSH 连接每 10s 在内存 `Touch` 一次，不写库；20s `Reap` 因此只清理真正断开的僵尸条目。等待 Device Flow 授权期间即使几十秒无按键，`online` 也不应归零；若观察到归零或「登录成功但仍为访客」，排查 `sshd.Session.heartbeatLoop` 是否被关闭，详见 `docs/MVP-DELIVERY.md §9`。
- **终端 VT 兼容（ADR-027）**：终端页面用服务端白名单序列 `\x1b[K`（EL）与 `\x1b[nA`（CUU）做原地重绘，依赖客户端启用 VT 支持（Windows Terminal / 现代 conhost / xterm 均默认支持）。若用户反馈「导航仍刷屏」或看到字面 `\x1b[K`，优先确认终端 VT 设置，而不是服务端渲染；未启用 VT 时服务端会退化为追加输出，业务功能不受影响。清屏 / OSC / 备用屏等禁止序列由 `internal/archtest` 负向注入守卫持续校验。
- 待接入告警：最近备份时间超过 26 h、`verify` 失败、队列深度持续接近容量。

## 9. 部署与上线清单（P0 收尾交付）

> 目标形态（V 先生 2026-11-18 13:43 确认）：**本地交叉编译出单个二进制 → 上传测试 VPS → Linux/systemd 托管 → 放行 `2222`**（对外 22→2222 转发由部署方完成）。**P0 全部完成后，按本节逐项交付可直接复制的命令与实测结果**；`deploy/motd.service` 与启动自检脚本由 P0-28 产出。本节是交付约定，不是已完成事实。

| # | 步骤 | 交付物 / 验收点 |
| --- | --- | --- |
| 1 | 交叉编译 `GOOS=linux GOARCH=amd64`，`-ldflags` 注入 version | `bin/motd-linux-amd64` + sha256，上传 VPS |
| 2 | 目录与运行账号：`/opt/motd`（程序）、`/var/lib/motd`（数据），非 root 运行 | 目录 0750、数据目录属主正确；`data/` 不进仓库 |
| 3 | `/etc/motd/motd.env`（0600 root:root）注入 `MOTD_GITHUB_CLIENT_ID` | 见 §3 凭据小节；unit 只引用 `EnvironmentFile=` |
| 4 | 安装 `deploy/motd.service`（`GOMEMLIMIT=800MiB` / `GOGC=50` / `Restart=on-failure`） | `systemctl daemon-reload && systemctl enable --now motd` |
| 5 | SSH host key 首次生成与**重启复用**检查 | 重启前后指纹一致，客户端无 `host key changed` |
| 6 | 端口放行与 22→2222 转发（部署方执行） | `ssh -p 2222 <vps>` 进入访客会话 |
| 7 | 启动自检 + `/healthz` | 迁移版本、host key、`ok <version> queue=... online=...` |
| 8 | 备份 cron + 一次**带真实数据**的恢复演练 | 结案 D7；见 §5 待办 |
| 9 | D1–D7 验收清单逐条记录实测结果 | 写回本手册 |
