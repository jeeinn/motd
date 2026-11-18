# motd — 命令行论坛

SSH 优先的终端论坛。用户 `ssh` 上来，用键盘和斜杠命令浏览、发帖、回复；同一份应用状态未来可被浏览器 TUI 复用渲染。

设计依据：[docs/design/命令行论坛整体设计方案-评审稿v0.2.md](docs/design/命令行论坛整体设计方案-评审稿v0.2.md)（评审稿 v0.2）。实施计划：[docs/P0-PLAN.md](docs/P0-PLAN.md)。技术决策记录：[docs/DECISIONS.md](docs/DECISIONS.md)。**MVP 交付与复盘：[docs/MVP-DELIVERY.md](docs/MVP-DELIVERY.md)**。

- 远端：`git@github.com:jeeinn/motd.git`（分支 `main`）
- Go module：`github.com/jeeinn/motd`
- Q1–Q7 架构决策已于 2026-11-18 全部拍板，结论见 `docs/DECISIONS.md ADR-009`

## 当前状态

**MVP 已交付（2026-11-18）**：单个 Go 二进制启动即建库迁移，SSH 连上后可浏览版块 / 主题、发帖、回复，另一个账号可见；断开重连凭公钥直接恢复登录态。审查结论、范围调整、验证结果、已知限制与变更记录见 [docs/MVP-DELIVERY.md](docs/MVP-DELIVERY.md)。

- ✅ M0 地基（P0-01~07）：配置 / 日志 / 优雅停机 / `/healthz`、SQLite 双池与 PRAGMA 自检、版本化迁移（3 个）、写入队列（容量 256 快速失败、3s 超时、提交后才确认）、备份恢复、依赖方向守卫
- ✅ M1 业务内核（P0-08~13）：仓储端口在使用方、5 个版块种子、论坛核心用例（keyset 分页 / 软删除过滤 / 访客只读 / Q6 限流）、统一 ViewModel、领域事件与插件骨架、纯内存在线注册表
- ✅ M2 认证（P0-14~19）：公钥三状态解析、GitHub Device Flow 客户端、设备码状态机、账号登录事务、公钥绑定与软吊销、`guest → user` 会话升级
- ✅ M3 终端（P0-21~25）：`x/crypto/ssh` 入口（仅公钥、禁 root、默认 2222）、输入归一化与命令注册表、分层安全渲染（22 条恶意样本、唯一 SGR 常量表）、**四页终端页面**（版块 / 主题 / 详情 / 编辑）与键位收尾
- ✅ M4 闭环（P0-26）：真实 SSH 客户端 E2E 冒烟（D1/D3/D4）；带真实数据的重启持久化与备份恢复演练（D6 正常关闭路径 / D7）
- ✅ 全量 `go build` / `go vet` / `go test -count=1 ./...` 全绿（456 个测试函数）
- ⚠️ **D2 待复测**：离线假端点链路已通过；本机实测已完成**真实 GitHub Device Flow 授权**，但当时因连接心跳缺陷导致会话升级失败（已修，`docs/MVP-DELIVERY.md §9`）。重启服务后再跑一次 `/login github` 确认完整闭环；VPS 上的凭据注入与出站放行仍未验证
- ⬜ 明确不做并列为已知限制：P0-20 安全日志 / 认证类限流（P1）、100 连接压测（P3）、systemd 部署附件、浏览器 TUI 与搜索 / 投票等 P1 能力、`kill -9` 硬杀演练与 `-race`

## 快速开始

```bash
# 编译
go build -o motd.exe ./cmd/server

# 启动（运维探针 127.0.0.1:8080；SSH 入口默认 :2222；数据落在 ./data）
./motd.exe

# 探活
curl -s http://127.0.0.1:8080/healthz   # => ok dev queue=0/256 online=0 active=0 commits=0

# SSH 连入（仅公钥认证；host key 首次启动生成在 <DataDir>/ssh_host_ed25519_key）
ssh -p 2222 localhost
```

> 进入后按 `/` 会**自动填入前导斜杠**，直接输入命令名即可（例如 `login github`），Enter 提交；**输入过程不回显**是 P0-22 的安全约束，不是没反应。`j/k` 移动、`Enter` 打开、`q` 返回、`/quit` 退出。登录命令是 `/login github`（不是 `/login`）。

首次启动会在 `./data` 下创建 `motd.db` 并自动执行迁移。

## 配置

全部通过环境变量，空值视为未设置：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MOTD_DATA_DIR` | `./data` | 数据根目录（库、host key、备份） |
| `MOTD_DB_PATH` | `$MOTD_DATA_DIR/motd.db` | SQLite 文件路径 |
| `MOTD_HTTP_ADDR` | `127.0.0.1:8080` | 运维探针监听地址，不承载业务流量 |
| `MOTD_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `MOTD_LOG_FORMAT` | `text` | `text` / `json` |
| `MOTD_SSH_ADDR` | `:2222` | SSH 入口监听地址（Q4） |
| `MOTD_SSH_MAX_CONNS` | `100` | SSH 最大并发连接数 |
| `MOTD_SSH_IDLE_TIMEOUT` | `10m` | 会话空闲超时 |
| `MOTD_GITHUB_CLIENT_ID` | 空 | Q7 的 OAuth App `client_id`；未设置时 `/login` 返回明确提示，不 panic、日志不回显值 |

> 只声明当前真正被读取的配置项。`client_id` 不进仓库、不写 unit，注入方式见 `docs/OPS.md §3`。

## 开发

```bash
go build ./...       # 构建
go vet ./...         # 静态检查
go test ./...        # 全量测试（约束/PRAGMA/写入队列/备份恢复/依赖守卫/仓储/用例/ViewModel/插件/在线状态/终端页面）
go test -count=1 ./cmd/server -run TestMVPSSHClosedLoop  # 真实 SSH 客户端闭环冒烟（D1/D3/D4）
go test -count=1 ./cmd/server -run TestMVPRestartAndBackupWithRealData  # 带数据重启 + 恢复演练
```

## 备份与恢复

```bash
go build -o bin/motdctl ./cmd/motdctl
scripts/backup.sh                                   # 生成备份 + sha256
scripts/restore.sh --verify <backup-file>            # 只校验
scripts/restore.sh <backup-file> <target-path>      # 恢复（目标必须不存在）
```

完整策略、cron 配置、恢复流程与演练记录见 [docs/OPS.md](docs/OPS.md)。

## 目录结构

```text
cmd/server/              进程入口
internal/
  transport/             ssh / http / websocket —— 只做协议适配
  application/           auth / forum / user / viewmodel / presence —— 用例编排、事务边界、权限判断、统一语义状态、在线注册表
  domain/                user / node / thread / post / vote —— 纯业务模型，不依赖上层
  infrastructure/        sqlite / github / email —— 外部实现
  presentation/          terminal（view.go + safe/ 分层渲染）/ browser（P1） —— 两个独立 Renderer
  plugin/                registry / events / demo —— 编译期注册、事件分化、彩蛋示例
docs/                    设计与计划
```

依赖方向单向：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。
