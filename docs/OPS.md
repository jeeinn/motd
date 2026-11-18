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
motdctl backup  [--db <path>] [--dir <dir>]
motdctl verify  <backup-file>
motdctl restore <backup-file> --target <path>
```

环境变量：`MOTD_DB_PATH`（默认 `./data/motd.db`）、`MOTD_BACKUP_DIR`（默认 `./data/backups`）、`MOTDCTL_BIN`（脚本用的工具路径）、`MOTD_GITHUB_CLIENT_ID`（P0-15 起由 `config.Load` 读取，默认空；见下方凭据小节）。

### SSH 入口（P0-21）

- `MOTD_SSH_ADDR`（默认 `:2222`，Q4 已拍板）、`MOTD_SSH_MAX_CONNS`（默认 100）、`MOTD_SSH_IDLE_TIMEOUT`（默认 `10m`）。
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

- **开发 / 测试不需要设置它**：`httptest` 假端点覆盖 Device Flow 全部分支，不依赖真实凭据与外网。未设置时 `/login` 返回明确提示，不 panic、不静默失败；`/healthz` 不回显该值。
- **P0-15 落地事实**（ADR-021）：`config.Load` 读取该变量并注入 `infrastructure/github.Client`；两个需要 `client_id` 的 POST 在缺值时返回 `auth.ErrClientIDMissing`，不发出任何请求。启动日志只输出 `client_id_configured=true|false` 与 `scope`，**不打印凭据值**，也不会因缺值 panic。
- 出站要求：VPS 需允许访问 `github.com:443`（Device Flow 两端点）与 `api.github.com:443`（`/user`、`/user/emails`）。本机 `httptest` 测试不需要外网。
- 真实联调（D2 手工验收）与 P0-28 部署自检前必须就位。

### Device Flow 状态表（P0-16）

- `device_auth_states` 在 `0001_init.sql` 已建好，**部署时无需额外迁移**；启动日志 `migrations_applied=[1 2 3]` 即表示就绪，另有 `设备码状态机就绪 store=device_auth_states`。
- 该表**只存设备码中间态，绝不存令牌**：`access_token` 只在服务端进程内交 P0-17 查 `/user` 后立即丢弃（Q3 / ADR-022）。备份文件里也不会出现令牌。
- 状态四态 `pending / authorized / denied / expired`；终态不可变，过期码不再轮询。轮询间隔 `polling_interval` 会随 GitHub `slow_down` 持久化放大，启动后按新会话重新从 5s 起算。
- 表会**保留所有历史行**（含过期与终态），只增不删；`idx_device_auth_states_expiry` 已存在。P0 数据量极小，无需运维干预；P1 可加定期清理 job（清理条件 `status <> 'pending' AND expires_at < now - 保留窗口`，且须避开在途登录）。**不要手工 DELETE 表内行**——正在轮询的会话会因 `ErrDeviceAuthNotFound` 中断。
- 排查登录卡住时优先看三点：`MOTD_GITHUB_CLIENT_ID` 是否注入、VPS 出站 `github.com:443` / `api.github.com:443` 是否放行、状态行是否停在 `pending` 但 `expires_at` 已过（下轮门禁会自动迁 `expired` 并提示重新发起）。

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
