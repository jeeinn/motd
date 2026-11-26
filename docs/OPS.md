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

- `MOTD_SSH_ADDR`（默认 `:2222`，Q4 已拍板）、`MOTD_SSH_MAX_CONNS`（默认 100）。**无会话空闲超时配置**——v0.4.2 已彻底移除 `MOTD_SSH_IDLE_TIMEOUT`。
- **会话生存期（v0.4.2）**：会话没有「无按键就断开」的机制，用户停止浏览但连接仍在时**不会被断开**。唯一的生存期约束是**绝对上限 6 小时**（硬编码，无需配置）：客户端静默消失（机器断电 / 网络中断、SSH 层未感知）或超长挂连到点强制收口，释放 `MaxConns` 槽位；关闭前下发固定提示文案「motd: 会话达到最长在线时长（6 小时），连接已关闭」并回 exit-status（退出码 0），不要误判为链路故障。
- **历史**：v0.4.0 及更早用 `MOTD_SSH_IDLE_TIMEOUT`（默认 10m）按「用户无按键」断开，停一会儿就被断；v0.4.1 改为按「连接无心跳」判定后，该计时器被心跳持续重置、实际永不触发，成了给用户虚假控制感的死配置；v0.4.2 据此移除该配置项与整套 idle timer 机制。从 v0.4.1 及更早版本升级时，若 `.env` 里仍有 `MOTD_SSH_IDLE_TIMEOUT`，会被静默忽略（不影响启动），建议手动删除该行。
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
  # /etc/motd 在 Debian/Ubuntu 常是目录，在 RHEL/CentOS 常是普通登录横幅文件——先判类型
  if [ -d /etc/motd ]; then ENV_FILE=/etc/motd/motd.env; else ENV_FILE=/etc/sysconfig/motd; fi
  sudo install -m 0600 -o root -g root /dev/null "$ENV_FILE"
  # 写入：MOTD_GITHUB_CLIENT_ID=<你的 client_id>
  echo "$ENV_FILE"   # 记住实际路径，unit 的 EnvironmentFile= 必须指向它
  ```

  unit 内只写 `EnvironmentFile=<上面得到的路径>`；不要在 unit 里写明文 `Environment=MOTD_GITHUB_CLIENT_ID=...`（unit 是 world-readable）。仓库当前不提供 `deploy/motd.service`，单元模板见 §9.3。

- **开发 / 测试不需要设置它**：`httptest` 假端点覆盖 Device Flow 全部分支，不依赖真实凭据与外网。未设置时 `/login github` 会返回专门的固定提示「未配置 MOTD_GITHUB_CLIENT_ID，无法发起 GitHub 登录…」（`auth.LoginTextClientIDMissing`），不 panic、不静默失败、不回显上游错误原文；`/healthz` 不回显该值。
- **P0-15 落地事实**（ADR-021）：`config.Load` 读取该变量并注入 `infrastructure/github.Client`；两个需要 `client_id` 的 POST 在缺值时返回 `auth.ErrClientIDMissing`，不发出任何请求。启动日志只输出 `client_id_configured=true|false` 与 `scope`，**不打印凭据值**，也不会因缺值 panic。
- 出站要求：VPS 需允许访问 `github.com:443`（Device Flow 两端点）与 `api.github.com:443`（`/user`、`/user/emails`）。本机 `httptest` 测试不需要外网。
- 真实联调（D2 手工验收）前必须就位；若上线时暂不提供，保持留空并在交付报告中明确「GitHub 登录不可用」，不要谎称已配置（2026-11-20 首次 VPS 部署即按留空交付）。

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

- **投票** `/vote <up|down|rm> [回复id]`：默认对**当前打开的 thread**；带 `回复id`（详情页 `id=<n>`）则对该回复投票（R-15）。访客拒绝。投票走 `AdjustScore` 只改 `threads.score`、**不推进 `last_reply_at`**，因此不影响 `SortLatest` 排序（ADR-032）；同一用户对同一目标重复投票不加分（幂等），改票按差值计（`-1 → +1` = +2）。回复票的分数与 karma 均记到父主题/回复作者，事件 `PostID` 回填（ADR-057）。
- **搜索** `/search <关键词>`：≥3 码点走 FTS5 trigram，<3 码点回退 LIKE；结果页 `n/p` 翻页、Enter 打开主题，页长锁 20、摘要 ≤240 字节。用户输入的 `*`/`"`/`%`/`_` 均按字面量处理（防 FTS 语法注入）。
- **恢复后索引重建**：见上方 `motdctl fts-rebuild`——恢复备份后 FTS 索引不会自动回来，必须手动重建，否则搜索返回空。这是刻意设计（重建时机由运维掌控，避免启动时整库扫描）。
- 审计事件 `EventVoteCast` 随投票写同事务入 outbox；幂等键固定，插件只感知**首次投票**（改票/撤销事件被去重）。

### 正文编辑与后台运维（P1-M4）

**多行正文编辑（P1-M4，v0.3.0 增强）**：`/new` 与 `/reply` 进入多行编辑态——正文 **Enter 换行**，提交用**单独一行 `.` 后按 Enter** 或 **Ctrl-D**；标题字段仍是单行、Enter 确认。粘贴（bracketed paste）整块进正文，内部换行不拆成提交，粘贴进来的 `.` 行不会被误判为哨兵。**通用 SSH 终端不支持可靠的 Ctrl+Enter**（字节级与 Enter 相同），服务端固定文案不含该组合字样；若某终端显示 Ctrl+Enter 提示，那是过时版本。编辑器有行数与字节双上限，超限给明确提示。详见 `DECISIONS.md ADR-034`。

**跨行退格合并（v0.3.0）**：正文空行行首按退格，现在会删除该空行并把上一行文本并入，光标停在拼接处可继续输入（此前该操作无动作）；首行空退格保持无动作。回显只用 ADR-027 白名单内的 CUU+EL（+SGR）整行重绘，**未放宽 CUP/CUD/ED**，用户文本与控制序列仍走两条独立写入路径。已知边界：单逻辑行超终端宽度自动折行时，CUU 只上移一个物理行（与既有宽字符退格限制一致，本版不处理）；行内左右移动本版不做。

**提交失败临时草稿（ADR-067，仅本次连接内存）**：命中限流（60s 冷却 / 1h 10 帖）时编辑器原地保留，内容不丢，冷却到点直接 Ctrl-D（或单独一行 `.`）重发；按 Esc 为**挂起**（保草稿回浏览屏，回到原帖按 `r` 恢复，覆盖原「取消」语义），Ctrl-C 为取消并清草稿。只读/冻结拒绝时回浏览屏并保留输入，处理完成后重新 `/reply` / `/new` 或回原帖按 `r` 即可恢复预填。草稿只存当前 SSH 连接的控制器内存，**断连即失、不落库、不进日志**，文案统一「临时草稿（仅本次连接）」；`/edit` 不检出草稿（避免覆盖原文）。无 schema 迁移、无新环境变量、无键位/渲染改动。

**两项后台行为（均无 schema 变更，迁移仍止于 0007）**：

1. **`device_auth_states` 清理**：启动时立即执行一次、之后每小时一次；只删**自身 `expires_at` 已过**的行（四态共用该条件）。未过期的 `pending` 代表在途设备码登录，绝不删除；参数化 DELETE 走唯一写队列的单事务，可重复执行（第二次零删除）。停机时先等清理循环退出再关写队列。pending 严格按 `expires_at` 清理——客户端恰在过期瞬间仍在轮询时，下一次会拿到"码不存在"（既定口径，见 ADR-035）。
2. **`ssh_keys.last_used_at`**：公钥认证成功且命中已绑定用户后，同一指纹 **≥5min 最多排一次写**，异步经写队列完成，失败只记日志不影响认证。采用「先标记窗口、后异步写」，写失败时该钥匙需等下一个窗口才重试——详情页最近使用时间**可能滞后一个窗口**（ADR-036）。

**`/keys <fingerprint>`**：查看单把钥匙的标签 / 创建时间 / 最近使用时间；标签当前无输入口、恒为空。跨用户与不存在返回同一错误（防指纹枚举）；只覆盖未吊销钥匙（ADR-037）。

### 作者删除（v0.2.1）

- `/delete` 是**软删**：仅把 `threads.status` 或 `posts.status` 置为 `deleted`，不物理删除、不级联。无参删当前打开的主题（成功后回列表），带 `<id>` 删指定回复（成功后刷新详情）；必须在主题详情页使用，否则给固定提示。
- 只有作者本人能删自己的内容；非作者、访客、目标不存在或已删，一律回「仅作者可删除」——统一文案，不泄露状态或事务细节。所有权判定集中在 application 层一个 `canModify` 函数，将来引入版主只在这一处加分支。
- 被删主题下的**回复行保留在库**，但详情、列表与全文检索均不可见——检索的两条 SQL 都按所属主题 `status='active'` 过滤。运维从 DB 层手工改 `status` 时遵循同一口径：`deleted` 对公开读等价于不存在；`hidden` 自 v0.4.0 起由管理员 `/hide` / `/unhide` / `/unthread` 命令治理（详见上方「社区治理」小节），不再只是预留位。
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

### 社区治理（P2 / v0.4.0）

**管理员配置 `MOTD_ADMIN_FINGERPRINTS`**（在 `.env` 里配置，见 `.env.sample`）：逗号分隔的 `SHA256:` 公钥指纹，留空 = 无人可管。获取方式 `ssh-keygen -lf ~/.ssh/id_rsa.pub`，把 `SHA256:...` 整串填入；多个管理员用逗号分隔。405 端口/HTTP 无管理端点——治理只走 SSH 命令。

- **指纹来源**：比对的是 SSH **认证阶段服务端算出的会话指纹**，不是命令文本。公开注册用户无法自我提权；换钥等于自动失权。
- **热重载（R-15 / ADR-079）**：改 `.env` 的 `MOTD_ADMIN_FINGERPRINTS` 后执行 `kill -HUP <pid>`（systemd: `systemctl reload motd` 或 `kill -HUP $(systemctl show -p MainPID motd --value)`），服务端从文件强制重读并以原子快照换入 `forum.Service`，无需重启、不中断在线会话。非法指纹（非 `SHA256:` 格式）**拒绝重载并保留旧名单**，日志告警，不 half-apply、不静默放行；`.env` 无该键时回退进程环境（`Environment=` 注入的部署方式仍需在 systemd 改后重启）。启动时非法指纹依旧失败关闭。
- **主题置顶（R-15）**：管理员在详情页执行 `/pin` / `/unpin`，每版块最多 1 条置顶，新置顶自动清同版块旧置顶（同事务原子）。置顶主题在版块内与 `/latest` 列表前置并带琥珀 `[置顶]` 前缀；置顶作为独立前缀层，不改变 `SortLatest`/`SortScore`，翻页靠 v2 游标（兼容旧 v1）。
- **权限单点**：所有治理命令经 `requireAdmin` 收口，与作者权 `canModify` 严格分离；访客与普通用户调用统一返回拒绝文案，不泄露命令细节。

**节点（版块）管理**：

| 命令 | 作用 | 约束 |
| --- | --- | --- |
| `/node list` | 列出全部版块，含 `active` / `archived` 状态 | 管理员 |
| `/node create <slug> <name>` | 创建版块 | slug 走白名单 + 层级深度校验 |
| `/node rename <slug> <name>` | 改名 | **seed 节点拒绝改名**（slug/name 保持稳定） |
| `/node archive <slug>` | 归档版块 | seed 节点允许归档；只改 `node.status`，不动 threads |

- **归档是软删**：只置 `node.status='archived'`，列表查询带 `status='active'` 过滤；历史帖详情页仍显示版块名（`GetNodeBySlug` 不过滤 archived）。绝不物理删节点——`threads.node_id` 有外键，有帖节点物理删会失败。
- **迁移**：`0008_nodes_status.sql` 加 `status TEXT NOT NULL DEFAULT 'active'`。升级前建议备份（§2）；启动自动应用。v7→v8 迁移 / 回滚 / kill-9 + WAL 演练记录见 §5（2026-11-20，隔离环境，生产库未触碰）。

**内容治理**：

| 命令 | 作用 | 约束 |
| --- | --- | --- |
| `/hide [id]` | 隐藏当前主题 / 指定回复 | hidden ≠ deleted，可恢复 |
| `/unhide [id]` | 恢复当前主题 / 指定回复 | 复用 `requireAdmin` |
| `/unthread <thread-id>` | 从任意页面恢复被隐藏主题 | 解决 `/unhide` 无参对已隐藏主题不可达 |

- `hidden` 是可治理状态、不是删除：被藏内容从列表 / 详情 / 检索不可见，但数据保留，可用恢复命令还原。这点与作者 `/delete` 的 `deleted` 语义不同——后者无公开恢复入口。
- 所有治理动作经 outbox 异步写入 `auth_events`（谁 / 何时 / 命令 / 目标 / 结果），审计 queue full 时 fail-closed；`/healthz` 的 `audit_dropped > 0` 应立即告警。DomainBus 去重在 queue full 时可能吞掉重投，属已知限制。

**作者编辑（`/edit [id]`）**：仅作者本人（复用 `canModify`），详情页显示「已编辑 MM-DD HH:MM」，时间走 `MOTD_DISPLAY_TIMEZONE`；编辑不推进 `last_reply_at`。迁移 `0009_edited_at.sql` 加 `edited_at`，启动自动应用。

**回复数显示**：主题列表的 `rN` 由读时聚合 `ReplyCountsByThreadIDs` 计算（`COUNT-1` 去首帖），无迁移。使用 `INDEXED BY idx_posts_thread` 锁计划，索引名硬绑定为技术债——若将来重命名该索引，须同步改查询或 EXPLAIN 会退化全表扫。

**帮助**：6 条管理员命令在帮助里单独归组 + 固定说明行，不隐藏、不动权限架构。

## 4. 恢复流程

1. **停服**：`systemctl stop motd`。
2. **保留现场**：把现有库移走，**不要直接删除**：
   ```bash
   mv data/motd.db        data/motd.db.broken-YYYYMMDD
   mv data/motd.db-wal    data/motd.db-wal.broken-YYYYMMDD    2>/dev/null || true
   mv data/motd.db-shm    data/motd.db-shm.broken-YYYYMMDD    2>/dev/null || true
   ```
3. **恢复**：`scripts/restore.sh <backup-file> data/motd.db`。脚本会先做校验和 + 完整性验收，任一失败即中止。
4. **重建全文索引（必须）**：`motdctl fts-rebuild --db data/motd.db`，输出应为 `indexed=N`。FTS5 外部内容表的索引不随备份恢复，漏掉这步会让 `/search` 返回空（ADR-033）。
5. **启动**：`systemctl start motd`，`curl -s http://127.0.0.1:8080/healthz`（应返回 `ok <version> queue=0/256 online=0 active=0 commits=0`）。
6. **抽查**：用户数、最新主题、迁移版本是否与备份点一致；补做一次 `/search` 确认索引已恢复。
7. **记录**：把命令、结果、耗时写回本文件 §5 的演练记录。

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

- `/healthz` 返回 `ok <version> queue=<depth>/<cap> online=<n> active=<n> commits=<n> tx_bytes=<n> rx_bytes=<n>`：`queue` 观察写入背压，`online/active` 是内存在线注册表（重启归零，非业务数据），`commits` 是进程启动以来成功提交的事务数。全部为运维可观测字段，不承载业务流量（ADR-006 / ADR-016）。
- **带宽探针（P4-T2）**：`tx_bytes` / `rx_bytes` 是进程级 SSH 字节累计值（`tx` 服务端→客户端，`rx` 客户端→服务端），由 `Session.Read/Write` 用 atomic 在内存中单调累加，重启归零。**它们不写 SQLite、不改变 `commits`、不进领域模型或 ViewModel，也不在终端页面呈现**；只用于粗略判断链路是否在传数据。`tx_bytes`/`rx_bytes` 增长而 `online/active=0` 说明连接已断但注册表状态滞后，`commits` 增长则与带宽无关（那是真实写库）。
- **连接级心跳**：每条活 SSH 连接每 10s 在内存 `Touch` 一次，不写库；20s `Reap` 因此只清理真正断开的僵尸条目。等待 Device Flow 授权期间即使几十秒无按键，`online` 也不应归零；若观察到归零或「登录成功但仍为访客」，排查 `sshd.Session.heartbeatLoop` 是否被关闭，详见 `docs/MVP-DELIVERY.md §9`。
- **终端 VT 兼容（ADR-027）**：终端页面用服务端白名单序列 `\x1b[K`（EL）与 `\x1b[nA`（CUU）做原地重绘，依赖客户端启用 VT 支持（Windows Terminal / 现代 conhost / xterm 均默认支持）。若用户反馈「导航仍刷屏」或看到字面 `\x1b[K`，优先确认终端 VT 设置，而不是服务端渲染；未启用 VT 时服务端会退化为追加输出，业务功能不受影响。清屏 / OSC / 备用屏等禁止序列由 `internal/archtest` 负向注入守卫持续校验。
- 待接入告警：最近备份时间超过 26 h、`verify` 失败、队列深度持续接近容量。

## 9. 部署与上线清单（2026-11-20 实战验证版）

> 单个二进制部署不变，但**运行形态有两种**：方式一 systemd 托管（§9.1–§9.5），方式二便携版自部署（§9.6）。先按 §9.0 选形态，再读对应步骤。
> 本节按 **CentOS Stream 9 / 阿里云 ECS** 实机部署重新校准。国内 VPS 上 `github.com` 与 `proxy.golang.org` 可能超时，因此默认给出「目标机直接构建」路径，同时保留原「本地交叉编译 + 上传」路径。仓库当前**没有** `deploy/motd.service`，用 §9.3 的模板。下文 `<VERSION>` 取实际版本（首次实战基线 `v0.4.2`，当前版本见 `README.md`）。

### 9.0 形态选择：systemd 托管 vs 便携版自部署

| 维度 | 方式一：systemd 托管（推荐生产） | 方式二：便携版自部署 |
| --- | --- | --- |
| 运行身份 | 系统用户 `motd`，非 root | 当前普通用户，无需 root |
| 配置来源 | `/etc/sysconfig/motd`（0600 root:root），`MOTD_DOTENV_DISABLED=true`；`.env` **不生效** | 自部署目录 `.env`（0600，运行用户可读），默认自动加载；**不要**设 `MOTD_DOTENV_DISABLED=true` |
| 目录 | 程序 `/opt/motd`，数据 `/var/lib/motd` 分离 | 同一目录（如 `/data/motd`）放二进制、`.env`、`data/` |
| 端口 | SSH `:2222`，healthz `127.0.0.1:18080`（避让已有服务） | 自选并与方式一错开，如 SSH `:2223` + healthz `127.0.0.1:18081` |
| 启停 / 日志 | `systemctl restart/stop motd`；`journalctl -u motd` | `screen -S motd ...`；`screen -L` 或重定向 `motd.log` |
| 开机自启 | `systemctl enable motd` | screen 不提供；需 cron `@reboot` 或 systemd --user |
| 适用 | 长期运行、公网服务 | 个人验证、多版本并存、无 root 权限 |

**同机并存必须错开 SSH 与 healthz 端口**；两种方式的配置不要双写：systemd 模式以环境文件为唯一来源，便携版以 `.env` 为唯一来源。本次 Jeeinn 服务器采用方式一，用户新增 `/opt/motd/.env` 后必须把所需键迁入 `/etc/sysconfig/motd` 才生效（见 §9.3）。

| # | 步骤 | 交付物 / 验收点 |
| --- | --- | --- |
| 1 | 构建并注入版本（§9.1），两个构建路径二选一 | `/opt/motd/bin/motd` + `motdctl`，`/healthz` 回显版本而非 `dev` |
| 2 | 非 root 运行账号与目录权限（§9.2） | `0750 root:motd` 程序目录、`0750 motd:motd` 数据目录，`motd` 用户能 `cd` 且能执行二进制 |
| 3 | 环境文件（§9.3） | `/etc/sysconfig/motd` 或 `/etc/motd/motd.env`，0600 root:root；凭据不进 unit、不进仓库 |
| 4 | systemd 单元（§9.3） | `enabled`、`active (running)`、`Restart=on-failure` |
| 5 | SSH host key 首次生成与**重启复用**检查（§9.4） | 重启前后指纹一致，客户端无 `host key changed` |
| 6 | 端口放行：firewalld 加 `2222/tcp`；对外 22→2222 转发仍由部署方决定 | 外部 `ssh -p 2222 <vps>` 进入访客会话 |
| 7 | 启动自检 + `/healthz`（§9.4） | 迁移版本 `[1..9]`、host key、`ok <version> queue=... online=...` |
| 8 | 备份冒烟 + 每日备份策略（§9.5） | 至少一次 `motdctl backup` + sha256；定时策略由用户拍板后建立 |
| 9 | 逐条记录实测结果与偏差 | 写回本手册；未配置项（client_id / 管理员指纹 / SMTP）必须显式列出 |

以下 §9.1–§9.5 均为**方式一（systemd 托管）**的具体步骤；方式二见 §9.6。

### 9.1 构建：两条路径，版本必须注入

> **公开下载用的跨平台产物走自动流程**：推送 `v*` tag 由 `.github/workflows/release.yml` 用 GoReleaser 构建 6 个平台归档（linux / windows / darwin × amd64 / arm64）+ `checksums.txt`，Release 正文从 `CHANGELOG.md` 对应版本段自动提取。用户可在 GitHub Releases 页直接下载；版本号经 `-X main.version=` 注入（`/healthz` 回显）。下文两条路径是**自行部署到目标机**时的手工方式，与自动产物互为补充；发版触发步骤见 `CONTRIBUTING.md`「自动发版」。

**路径 A（原约定）：本地交叉编译后上传。** 适合本地网络好、目标机不便装 Go 的场景。

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -p 2 -ldflags "-X main.version=<VERSION> -s -w" -o bin/motd-linux-amd64 ./cmd/server
sha256sum bin/motd-linux-amd64            # 记录到交付报告
# 再由 scp / SFTP 上传到目标机 /opt/motd/bin/motd
```

**路径 B（2026-11-20 实战）：目标机上构建。** 先探测网络：`curl -I -m 10 https://proxy.golang.org/` 若超时，改用国内代理。

> **探针口径（2026-11-24 实测修正）：不要用 `.info` 测镜像可用性，必须测 `.zip` 或直接 `go mod download`。**
> `.info` 只有几百字节，走的是完全不同的链路成功率；用它测速会得出相反结论。同机实测：
>
> | 镜像 | `@v/<VERSION>.info` | `@v/<VERSION>.zip` |
> | --- | --- | --- |
> | `proxy.golang.com.cn` | 200 / 2.66s（看着最快） | **`connection reset`**（下载中断） |
> | `goproxy.cn` | 200 / 12.1s（看着最慢） | 未走通 |
> | `mirrors.aliyun.com/goproxy` | 200 / 5.4s | 200 / **459ms 完成** |
>
> 结论：该机优先 `https://mirrors.aliyun.com/goproxy,direct`；判定可用性用 `curl -o /dev/null -w '%{http_code}' -m 45 <mirror>/github.com/jeeinn/motd/@v/<VERSION>.zip`。

```bash
dnf install -y --setopt=install_weak_deps=False golang   # 1.27.3，满足 go.mod 的 1.26.3
export GOPROXY=https://mirrors.aliyun.com/goproxy,direct GOSUMDB=off GOTOOLCHAIN=local
# 备选：GOPROXY=https://goproxy.cn,direct（.info 慢但可用于小仓库；大 zip 优先 aliyun）

mkdir -p /root/motd-build/src && cd /root/motd-build
go mod init local/motdbuild 2>/dev/null || true
go mod download github.com/jeeinn/motd@<VERSION>
MODDIR="$(go env GOPATH)/pkg/mod/github.com/jeeinn/motd@<VERSION>"
cp -a "$MODDIR/." /root/motd-build/src/ && chmod -R u+w /root/motd-build/src
cd /root/motd-build/src
# 小内存 VPS（实测 1.6 GiB）必须限制并行度；module cache 里的文件只读，故先复制到可写目录
GOMEMLIMIT=900MiB GOGC=50 go build -p 2 -ldflags "-X main.version=<VERSION> -s -w" -o /opt/motd/bin/motd ./cmd/server
GOMEMLIMIT=900MiB GOGC=50 go build -p 2 -ldflags "-s -w" -o /opt/motd/bin/motdctl ./cmd/motdctl
sha256sum /opt/motd/bin/motd /opt/motd/bin/motdctl
```

**判据**：启动日志 `探针监听 ... version=<VERSION>` 或 `/healthz` 回显该版本。回显 `dev` 说明漏了 `-X main.version=`。

### 9.2 目录、账号与权限（最易踩坑）

```bash
id motd >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin motd

install -d -m 0750 -o root -g motd /opt/motd /opt/motd/bin
install -d -m 0750 -o motd -g motd /var/lib/motd /var/lib/motd/backups
install -m 0750 -o root -g motd /path/to/motd     /opt/motd/bin/motd
install -m 0750 -o root -g motd /path/to/motdctl  /opt/motd/bin/motdctl

# 验收：motd 用户既能进入工作目录，也能执行二进制
su -s /bin/bash motd -c 'cd /opt/motd && /opt/motd/bin/motdctl --help' | head -5
```

**真实故障**：若把 `/opt/motd`、`bin/`、二进制建成 `0750 root:root`，systemd 以 `motd` 用户启动时会在 CHDIR 阶段失败，日志为 `Changing to the requested working directory failed: Permission denied` + `status=200/CHDIR`，并进入 `Restart=on-failure` 的重启循环。修法是赋值组为 `motd`（或放宽为 0755），不要靠改 `User=root` 绕过。

### 9.3 环境文件与 systemd 单元模板

**先判端口占用**：`ss -tlnp | grep ':8080'`。若已有服务占用（实测是 `matea-linux-amd`），把 `MOTD_HTTP_ADDR` 改为回环上的空闲端口（实战用 `127.0.0.1:18080`），不要改 SSH 入口。

**环境文件路径**：`/etc/motd` 在 RHEL/CentOS 是**普通登录横幅文件**，不是目录，不能 `mkdir`；用下面的判定逻辑选路径。

```bash
if [ -d /etc/motd ]; then ENV_FILE=/etc/motd/motd.env; else ENV_FILE=/etc/sysconfig/motd; fi
install -m 0600 -o root -g root /dev/null "$ENV_FILE"
cat > "$ENV_FILE" <<'EOF'
MOTD_DOTENV_DISABLED=true
MOTD_DATA_DIR=/var/lib/motd
MOTD_DB_PATH=/var/lib/motd/motd.db
MOTD_BACKUP_DIR=/var/lib/motd/backups
MOTDCTL_BIN=/opt/motd/bin/motdctl
MOTD_HTTP_ADDR=127.0.0.1:18080
MOTD_SSH_ADDR=:2222
MOTD_SSH_MAX_CONNS=100
MOTD_LOG_LEVEL=info
MOTD_LOG_FORMAT=text
MOTD_DISPLAY_TIMEZONE=Asia/Shanghai
# 可选凭据：由用户提供后填入本文件（0600），改完需重启生效
# MOTD_GITHUB_CLIENT_ID=
# MOTD_ADMIN_FINGERPRINTS=
# MOTD_EMAIL_ENABLED=false
EOF
chmod 600 "$ENV_FILE" && chown root:root "$ENV_FILE"
```

`MOTD_DOTENV_DISABLED=true` 让进程只认 systemd 注入的环境，避免工作目录或二进制目录里的 `.env` 旁路覆盖凭据。凭据只写这个文件，unit 只引用 `EnvironmentFile=<实际路径>`。

**单元模板**（写入 `/etc/systemd/system/motd.service`，`0644 root:root`）：

```ini
[Unit]
Description=motd - SSH-first terminal forum
Documentation=https://github.com/jeeinn/motd
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=motd
Group=motd
WorkingDirectory=/opt/motd
EnvironmentFile=<实际路径>
Environment=GOMEMLIMIT=800MiB
Environment=GOGC=50
ExecStart=/opt/motd/bin/motd
Restart=on-failure
RestartSec=3s
KillMode=mixed
TimeoutStopSec=20s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/var/lib/motd
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
```

把模板里的 `<实际路径>` 替换为 §9.3 判定的真实值（实战为 `/etc/sysconfig/motd`）；占位符原样写入会让服务启动失败。

```bash
systemctl daemon-reload && systemctl enable --now motd
systemctl status motd --no-pager -l | head -20
```

`ProtectSystem=full` 下只有 `ReadWritePaths=/var/lib/motd` 可写；不要把数据目录留在 `/opt/motd/data`，否则运行期写库会被只读挂载挡住。

**Description 版本标签**：unit `Description` 括号内的版本号仅作展示，不参与运行判定。跨版本升级后若标签滞后，可同步为新版本号；改 `Description` 只需 `systemctl daemon-reload`，**无需重启服务进程、不中断在线会话**。版本真相源以 `/healthz` 回显与构建时 `-X main.version=` 注入为准，不以 unit 标签为准。

### 9.4 启动自检（逐条留证）

```bash
# ① 探针：版本、队列、迁移后的在线计数
curl -sS http://127.0.0.1:18080/healthz
# 期望：ok <VERSION> queue=0/256 online=0 active=0 commits=N audit_dropped=0 ...

# ② 关键启动日志
journalctl -u motd --no-pager -b | grep -E '配置来源|数据库就绪|host key|client_id_configured|探针监听|SSH 监听'
# 期望：配置来源 env_only；数据库就绪 migrations_applied=9 applied_versions="[1 2 3 4 5 6 7 8 9]"

# ③ host key 重启复用
F1=$(journalctl -u motd --no-pager -b | grep -o 'fingerprint=SHA256:[^ ]*' | tail -1)
systemctl restart motd && sleep 2
F2=$(journalctl -u motd --no-pager -b | grep -o 'fingerprint=SHA256:[^ ]*' | tail -1)
[ "$F1" = "$F2" ] && echo HOSTKEY_STABLE=yes || echo HOSTKEY_STABLE=no

# ④ 防火墙与访客连入
firewall-cmd --permanent --add-port=2222/tcp && firewall-cmd --reload
printf '/quit\r' | timeout 12 ssh -tt -o BatchMode=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -p 2222 guest@127.0.0.1
```

### 9.5 备份与未决项

```bash
# 冒烟（不依赖 scripts/*.sh，避免 cron 里 source .env 的坑）
su -s /bin/bash motd -c \
  '/opt/motd/bin/motdctl backup --db /var/lib/motd/motd.db --dir /var/lib/motd/backups'
ls -lh /var/lib/motd/backups
```

- 首次部署**默认不建立**每日 cron/timer；是否启用、周期与保留策略由用户拍板（§2 初值为每日 03:00、保留 14 天且不自动删除）。
- 若后续用 `scripts/backup.sh`：该脚本只单独读 `MOTD_DB_PATH` / `MOTD_BACKUP_DIR` / `MOTDCTL_BIN` 三项，**不加载** `.env`；cron 里必须显式 export 或用 systemd timer 的 `EnvironmentFile=`。
- 交付报告必须明确列出未配置项：`MOTD_GITHUB_CLIENT_ID`（Device Flow 不可用）、`MOTD_ADMIN_FINGERPRINTS`（留空 = 无管理员）、邮箱 SMTP，以及与默认值的偏差（如探针端口、环境文件路径）。

### 9.5.1 升级前回退点（`motdctl backup` 本身不够）

> 2026-11-24 `v0.8.2 → v0.9.0` 实战补充：`motdctl backup` **只产 `.db` 与 `.sha256`**，不含 host key、不含旧二进制。只靠它无法把一台正在运行的机器回退到升级前状态。

升级场景（§10.2 判定为「升级」）在动任何文件之前执行：

```bash
set -e
D=/var/lib/motd/pre-upgrade-<旧版本号>        # 如 pre-upgrade-v082

# ① 归档副本（供日后按 §4 恢复数据）
su -s /bin/bash motd -c \
  '/opt/motd/bin/motdctl backup --db /var/lib/motd/motd.db --dir /var/lib/motd/backups'

# ② 回退目录：db + wal + shm + host key + 双旧二进制
install -d -m 0700 "$D"
cp -a /var/lib/motd/motd.db /var/lib/motd/motd.db-wal /var/lib/motd/motd.db-shm "$D"/
cp -a /var/lib/motd/ssh_host_ed25519_key "$D"/
cp -a /opt/motd/bin/motd /opt/motd/bin/motdctl "$D"/

# ③ 校验：副本必须与现网逐字节一致
sha256sum /var/lib/motd/motd.db "$D/motd.db"
sha256sum /var/lib/motd/ssh_host_ed25519_key "$D/ssh_host_ed25519_key"
```

要点：

- **`-wal` / `-shm` 必须一起备份**：只拷 `motd.db` 可能丢掉尚未 checkpoint 的事务，回滚后数据会退到更早的时点。
- **host key 必须备份**：换了 key 客户端直接报 `host key changed`（§10.5），数据库恢复补救不了。
- **旧二进制必须备份**：最短回滚路径是「放回旧二进制 + 重启」，完全不碰数据库。
- 目录 `0700`，仅 root 可读；不得在报告或日志里输出其中任何文件内容。
- **只回滚二进制**：`systemctl stop motd` → 放回 `motd` / `motdctl` → `start`；不需要 §4 的 FTS 重建。
- **连数据库一起回滚**：必须走 §4 全流程，**包括 `motdctl fts-rebuild`**，漏掉会让 `/search` 返回空（ADR-033）。
- 回退目录在升级成功且观察期满后再清理；删除属破坏性操作，须用户确认。

### 9.6 方式二：便携版自部署（二进制 + `.env` + `screen`）

> 适用：用户在自己的目录（如 `/data/motd`）用自己编译的二进制运行，不装系统服务、不需要 root。
> 与方式一最大的区别：**`.env` 是唯一配置入口**——不要设置 `MOTD_DOTENV_DISABLED=true`，否则 `.env` 自身失效。

**目录布局**（属主为运行用户，不要 `root:root`）：

```text
/data/motd/
├── motd          # 服务端二进制（带 -X main.version=...）
├── motdctl       # 备份工具，可选
├── .env          # 0600，运行用户可读
├── data/         # 自动建库、host key、备份
└── motd.log      # 日志（可选）
```

**步骤**：

1. **构建**（版本必须注入，口径见 §9.1；本地交叉编译或目标机 `go build` 均可）：

   ```bash
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -p 2 \
     -ldflags "-X main.version=<VERSION> -s -w" -o /data/motd/motd ./cmd/server
   # 需要备份能力时再构建 motdctl 到同目录
   ```

2. **写 `.env`**（从仓库 `.env.sample` 复制，逐项改）：

   ```bash
   cd /data/motd
   cp /path/to/repo/.env.sample .env && chmod 600 .env
   # 必改：MOTD_DATA_DIR=./data、MOTD_DB_PATH=./data/motd.db
   # 端口错开 systemd 实例：MOTD_SSH_ADDR=:2223、MOTD_HTTP_ADDR=127.0.0.1:18081
   # 不要写 MOTD_DOTENV_DISABLED=true
   ```

3. **用 screen 启动并验收**：

   ```bash
   cd /data/motd
   screen -S motd -L -dm ./motd     # -L 写屏日志
   screen -ls                       # 应有 motd 的 detached 会话
   curl -sS http://127.0.0.1:18081/healthz
   ```

   不需要屏日志时：`screen -S motd -dm bash -c './motd >> /data/motd/motd.log 2>&1'`。

4. **查看与优雅停止**：

   ```bash
   screen -r motd                    # 查看输出；Ctrl-a d 脱离，不要 Ctrl-a k
   screen -S motd -X stuff $'\003'   # 发 Ctrl-C，触发程序优雅收口
   # 确认进程退出后再按需 screen -S motd -X quit
   ```

   不要用 `kill -9`：虽然 WAL 恢复演练通过（§5），但正常停机应先让写队列收口。

5. **备份与升级**：

   ```bash
   cd /data/motd
   ./motdctl backup --db ./data/motd.db --dir ./data/backups
   # 升级：优雅停止 → 替换 motd 二进制 → 重新启动；data/ 与 host key 原样保留
   ```

6. **开机自启**：screen 本身不提供；需要时加 cron `@reboot cd /data/motd && screen -S motd -L -dm ./motd`，或改用 systemd --user。若希望纳入完整生命周期管理，迁移到方式一。

**便携版特有坑**：

- `.env` 查找顺序为 **cwd → 可执行文件所在目录**，命中第一个即停、**不合并两份**。务必 `cd /data/motd` 再启动；两处都有 `.env` 时只认 cwd 那份。
- **进程已有环境变量优先于 `.env`**：若 shell 里已 `export MOTD_*` 同名变量，`.env` 的值不会生效。
- 非法行静默忽略；启动日志只输出 `配置来源 file=<path>`，绝不输出键值。排障先看这一行确认读到了哪份 `.env`。
- 端口冲突：`:2222` / `:8080` 若被方式一占用，必须改端口，否则启动失败。
- host key 在 `./data/ssh_host_ed25519_key`；换目录会重新生成并导致客户端报 `host key changed`，迁移时要连 `data/` 一起搬。

## 10. 面向 AI AGENTS 的自动部署指南

> 适用：智能体通过具备 SSH / SFTP 能力的工具（如 nyaterm MCP 会话）在目标机执行 §9。
> 目标是把部署变成**可探测、可判定、可回退、可留证**的流程，而不是让智能体"照着 README 猜"。
> **自动部署 ≠ 自动发版**：智能体不得 `commit` / `tag` / `push` / 建 GitHub Release，见 [`../AGENTS.md`](../AGENTS.md) §5。

### 10.1 输入契约

| 项 | 必填 | 默认 / 口径 | 说明 |
| --- | --- | --- | --- |
| 目标连接标识 | 是 | — | SSH 连接名 / ID 或 `user@host`；方式一须能取得 root 或等价 sudo |
| 部署形态 | 是 | 有 root 时默认 systemd（方式一） | 便携版（方式二）须用户明确给出目录（如 `/data/motd`）与端口；无 root 时只能用方式二（§9.6） |
| 部署版本 | 是 | 当前发布 tag | 写成完整 tag，如 `v0.4.3`；构建时必须 `-X main.version=<VERSION>` |
| 对外方式 | 否 | 仅 `:2222` 监听 + firewalld 放行 | 22→2222 转发属用户决策；未授权不得改现有 sshd 配置 |
| 探针地址 | 否 | 8080 被占用则用 `127.0.0.1:18080` | 先 `ss -tlnp` 判定，不得盲目 bind 默认端口 |
| `MOTD_GITHUB_CLIENT_ID` | 否 | 留空 | 未提供就明说 Device Flow 不可用；不要编造或复用历史值 |
| `MOTD_ADMIN_FINGERPRINTS` | 否 | 留空 | 留空 = 无人可管；指纹须由用户提供，不从公钥文件自行推断其管理意图 |
| 邮箱 SMTP | 否 | 关闭 | 授权码属高敏凭据，只写 0600 环境文件 |

用户未提供的项：按默认值推进，但在最终报告里逐项标注「未配置 / 未授权」，不得静默省略。

### 10.2 前置探测（只读，先探测再动手）

```bash
echo '=== IDENTITY ===';  whoami; hostname; cat /etc/os-release | head -4; uname -m
echo '=== TOOLS ===';     command -v go git curl systemctl ss firewall-cmd 2>&1
echo '=== EXISTING ===';  systemctl status motd --no-pager 2>&1 | head -10
echo '=== DIRS ===';      ls -ld /opt/motd /var/lib/motd /etc/motd /etc/motd/motd.env /etc/sysconfig/motd 2>&1   # 只看 /etc/motd 是目录还是文件；`/etc/motd/motd.env` 报 No such file 是 CentOS 的正常现象，不代表异常
echo '=== PORTS ===';     ss -tlnp | grep -E ':(2222|8080|18080)\b' || echo 'no listener'
echo '=== FIREWALL ===';  firewall-cmd --list-ports 2>&1
echo '=== RESOURCES ==='; df -h / | tail -1; free -h | head -2
echo '=== NETWORK ===';   for m in https://mirrors.aliyun.com/goproxy https://goproxy.cn https://proxy.golang.com.cn; do echo -n "$m zip="; curl -sS -o /dev/null -w '%{http_code} %{time_total}s\n' -m 45 "$m/github.com/jeeinn/motd/@v/<VERSION>.zip"; done   # 必须测 .zip；.info 会误判（见 §9.1 对照表）
```

**STOP 条件（命中即停，不继续写文件）**：

| 观测 | 判定 | 处置 |
| --- | --- | --- |
| `motd.service` 已存在且 `active` | 这是升级，不是首装 | 先按 §9.5.1 建**回退点**（db + wal + shm + host key + 旧二进制，并校验 sha256），再走升级流程；**禁止覆盖** `/var/lib/motd/motd.db` 与 `ssh_host_ed25519_key` |
| `2222` 被其他进程占用 | 端口冲突 | 报告占用者，请用户指定端口；不要 `kill` 他人进程 |
| 无 root / 无 sudo | 无法建目录、装包、改 firewall | STOP 并回报所需权限 |
| 磁盘可用 < 2 GiB 或内存可用 < 1 GiB | 构建/运行资源不足 | 降低 `-p 1` 并设 `GOMEMLIMIT`，或 STOP |
| 用户只授权「检查」未授权「变更」 | 越界 | 只输出计划与 diff，不执行变更 |

### 10.3 决策表

| 观测 | 决策 |
| --- | --- |
| `github.com` / `proxy.golang.org` 超时 | 目标机构建，按 §9.1 对照表选镜像：**优先 `https://mirrors.aliyun.com/goproxy,direct`**（实测 `.zip` 459ms 下完）；`proxy.golang.com.cn` 的 `.info` 虽快但 `.zip` 会 `connection reset`，不可用 |
| 内存总 1.6 GiB / 可用约 1.1 GiB（小规格 VPS 常见） | 构建必须 `GOMEMLIMIT=900MiB GOGC=50 -p 2`（§9.1 路径 B）；不加限制有 OOM 被 kill 风险。依赖已缓存时构建约 5s，超时可先怀疑网络而非内存 |
| 工具只有**文本** SFTP 写入能力，没有二进制上传 | 走目标机构建；不要把 14 MiB 二进制 base64 分块塞进工具参数（实测不可行且污染上下文） |
| `/etc/motd` 是目录 | `ENV_FILE=/etc/motd/motd.env` |
| `/etc/motd` 是普通文件（CentOS/RHEL 常见） | `ENV_FILE=/etc/sysconfig/motd`（实战路径） |
| `ss` 显示 `8080` 已被占用 | `MOTD_HTTP_ADDR=127.0.0.1:18080`；不要动占用方 |
| module cache 源目录只读 | `cp -a` 到 `/root/motd-build/src` 后 `chmod -R u+w` 再构建 |
| 用户无 root / 明确要求自有目录 + screen | 走便携版（§9.6）：`/data/motd` + `.env` 0600 + 错开端口；不要创建 systemd 单元 |
| 同机已跑方式一，又要求便携版 | SSH 与 healthz 必须换端口（如 `:2223` / `127.0.0.1:18081`），并在报告里写明 |
| 用户给了 `client_id` / 指纹 | 写入 `0600 root:root` 的 `$ENV_FILE` 注释位并 `systemctl restart motd`；不得写进 unit 或命令行历史 |

### 10.4 幂等执行序列（每步带验收门）

> **升级场景（§10.2 判定为「升级」）必须在第 1 步之前先建回退点**（§9.5.1：db + wal + shm + host key + 旧二进制）。第 6 步的 `motdctl backup` 只是归档副本，**不能替代回退点**。

1. **工具链与源码**：按 §10.2 结果安装 Go / 设置代理，下载指定 tag 的源码（§9.1 路径 B）。
   *门*：`go version` 满足 `go.mod`，`$MODDIR/go.mod` 存在。
2. **账号与目录**：按 §9.2 创建用户、目录、权限，二进制就位。
   *门*：`su -s /bin/bash motd -c 'cd /opt/motd && /opt/motd/bin/motdctl --help'` 有输出。权限错误在这里拦住，进入 systemd 就变成 `200/CHDIR` 重启循环。
3. **环境文件与单元**：方式一先 `ls -ld /etc/motd` 判定 `$ENV_FILE`——**CentOS/RHEL 上它是普通登录横幅文件，直接写 `/etc/motd/motd.env` 会失败**，应落到 `/etc/sysconfig/motd`（§9.3 判定逻辑）；再按 §9.3 判端口、写 `$ENV_FILE`（0600）、写 `/etc/systemd/system/motd.service`，`daemon-reload && enable --now`；方式二按 §9.6 写 `/data/motd/.env` 并用 `screen` 启动。
   *门*：方式一 `systemctl is-active motd` = `active`、日志 `配置来源 env_only`；方式二 `screen -ls` 有会话、日志 `配置来源 file=/data/motd/.env`。两者均须 `migrations_applied=9`。
4. **网络放行**：仅在用户授权对外时 `firewall-cmd --permanent --add-port=2222/tcp && firewall-cmd --reload`。
   *门*：`firewall-cmd --list-ports` 含 `2222/tcp`；外部 `Test-NetConnection` 或 `ssh -p 2222` 可达。
5. **功能验收**：执行 §9.4 全部四项，特别是 host key 重启指纹一致与访客 `ssh` 进入。
   *门*：`/healthz` 版本等于目标 tag；`HOSTKEY_STABLE=yes`；访客能看到版块列表。
6. **备份冒烟**：执行 §9.5 的 `motdctl backup`，记录文件名与 sha256（这是**归档副本**，不是回退点，回退点见 §9.5.1）；定时策略另行征询。
7. **输出报告**：按 §10.6 模板逐项填报，含偏差与未配置项。

任何一步验收门失败：**停止后续步骤**，保留现场（日志、路径、命令），报告失败点与已尝试的修复，不做破坏性重试。

### 10.5 失败模式速查

| 现象 | 根因 | 修复 |
| --- | --- | --- |
| `status=200/CHDIR` + `Changing to the requested working directory failed` | `/opt/motd` 或二进制为 `0750 root:root`，`motd` 用户无执行/进入权 | `chgrp motd` 程序目录与二进制（或 0755），**不要**改 `User=root` |
| `/healthz` 回显 `dev` | 构建漏了 `-X main.version=<VERSION>` | 用带 ldflags 的命令重建并替换二进制，重启 |
| `bind: address already in use`（8080） | 已有探针/服务占用默认端口 | 改用 `127.0.0.1:18080`，更新 `$ENV_FILE` 后重启 |
| 模块下载全超时 | `github.com` / `proxy.golang.org` 在国内 VPS 不可达 | 换 `GOPROXY=https://mirrors.aliyun.com/goproxy,direct GOSUMDB=off` |
| `.info` 200 但 `go mod download` 报 `connection reset` / `unexpected EOF` | 用 `.info` 判镜像可用性会误判；`.zip` 走的是另一条链路（实测 `proxy.golang.com.cn` 如此） | 换 `mirrors.aliyun.com/goproxy` 重下；判定一律用 `.zip` 端点（§9.1） |
| `go build` 无输出即被 `Killed` / `signal: killed` | 小内存 VPS 未限并行与堆上限，OOM | `GOMEMLIMIT=900MiB GOGC=50 go build -p 2`（§9.1 路径 B） |
| `permission denied` 写 module cache | module cache 只读（0555） | `cp -a` 到可写目录 + `chmod -R u+w` 再 `go build` |
| 客户端报 `host key changed` | host key 被重新生成（通常是数据目录被删/换目录） | 停止变更；从备份恢复 `ssh_host_ed25519_key`，确认 `MOTD_DATA_DIR` 未变 |
| `audit_dropped > 0` 或 `audit_failed` 持续增长 | 审计缓冲溢出，认证动作 fail-closed | 先查 SQLite 写队列堵塞，不要盲目调小 flush 间隔（§3） |
| 登录提示「未配置 MOTD_GITHUB_CLIENT_ID」 | 凭据留空 | 属预期降级；向用户索取 client_id 并写入 0600 环境文件后重启 |

### 10.6 交付报告模板（智能体必须逐项填）

```text
形态：<systemd 托管 | 便携版自部署>（目录 / 端口）
版本 / 构建：<VERSION>，<本地交叉编译 | 目标机构建>，sha256=<...>
目标机：<hostname> / <os> / <arch>，公网入口 <host:2222>
路径：程序 /opt/motd/bin/motd，数据 /var/lib/motd，环境文件 <$ENV_FILE>，单元 /etc/systemd/system/motd.service
端口：SSH :2222（firewall 已放行 / 未授权放行）；healthz 127.0.0.1:<port>
探针原文：ok <version> queue=... online=... active=... commits=...
启动证据：migrations_applied=9；配置来源 env_only；client_id_configured=<true|false>
host key：SHA256:<...>，重启前后一致 = yes/no
验收：访客 SSH 进入 yes/no；备份冒烟 <文件名> + sha256；恢复演练 <未做/已做>
回退点：<未做（首装）/ 目录 + db 与 host key 的 sha256 与现网一致 = yes/no>
偏差：探针端口、环境文件路径、unit 来源（仓库无模板）、镜像选择（aliyun / goproxy.cn / 其他）
未配置：GitHub client_id / 管理员指纹 / 邮箱 SMTP / 每日备份策略
```

### 10.7 护栏清单（不可协商）

- 不执行 `rm -rf` 或任何会清除 `/var/lib/motd`、`data/`、备份目录的操作；升级前先备份。
- 不把凭据写进 systemd unit（`0644` world-readable）、命令行参数、日志或报告；只写 `0600` 环境文件。
- 不启用 shell 的 `set -x` 处理含凭据的文件；报告里只写「已配置 / 未配置」。
- 不修改现有 sshd 配置、不占用 22 端口；`2222/tcp` 放行仅在用户授权范围内执行。
- 不做 `commit` / `tag` / `push` / Release；不在未授权情况下修改版本文件。
- 每一步变更后立即验收；失败即停，保留现场并上报，不做"再试一次也许就好了"的循环。
