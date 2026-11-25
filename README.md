# motd — 命令行论坛

[![CI](https://github.com/jeeinn/motd/actions/workflows/ci.yml/badge.svg)](https://github.com/jeeinn/motd/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/jeeinn/motd)](https://github.com/jeeinn/motd/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/jeeinn/motd)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

SSH 优先的终端论坛。用户 `ssh` 上来，用键盘和斜杠命令浏览、发帖、回复；所有交互都在终端内完成，无需浏览器。

- 版本：`v0.9.3`（2026-11-25）
- Go module：`github.com/jeeinn/motd`
- 许可证：MIT

## 在线体验

服务器已部署，直接用 SSH 连入试用（无需注册）：

```bash
ssh -p 2222 guest@jeeinn.com
```

用户名任意非 root 即可；未绑定公钥时以**访客**进入（可浏览、可 `/register`），用 `/login github` 登录后可发帖。

## 预览

**版块列表首屏**——连入后按 `j/k` 移动、`Enter` 打开、`h/g` 返回：

![SSH 连入后的版块列表](docs/images/terminal-node-list.png)

**GitHub Device Flow 登录与发帖**——`/login github` 发起授权，浏览器打开 `github.com/login/device` 输入验证码即可自动创建账号并绑定当前公钥，随后用 `/new` 发帖：

![GitHub Device Flow 登录与发帖](docs/images/login-github.png)

更多截图见 [`docs/images/`](docs/images/)。

## 功能

- SSH 入口：仅公钥认证，禁 root，默认端口 `:2222`；停止浏览但连接仍在时不会断连（会话最长在线 6 小时，到点提示后关闭）
- 论坛核心：5 个版块、keyset 分页、软删除过滤、访客只读、发帖与回复
- 最新动态：`/latest` 一条命令看全论坛最新主题（跨版块、按活跃度排序），新回复自动置顶
- 社区看板：`/stats` 一屏查看今日/昨日登录、在线峰值、历史峰值、注册用户与累计登录人次
- 互动：投票（`/vote up|down|rm`，幂等）、全文搜索（`/search`，FTS5 + 中文短词回退）、作者软删自己的主题/回复（`/delete`）、作者编辑（`/edit`，详情页显示「已编辑」）、主题列表回复数（`rN`）
- 社区治理（管理员）：版块管理（`/node list|create|rename|archive`）、隐藏 / 恢复他人内容（`/hide` / `/unhide` / `/unthread`）；权限来自公钥指纹白名单，治理动作写入审计日志
- 登录：GitHub Device Flow / 邮箱密码两条路径；公钥绑定与软吊销，断线重连凭公钥恢复登录态
- 邮箱自助：`/register` / `/verify` / `/resend` / `/recover` 自助注册与密码找回（SMTP 配置见 `.env.sample`）
- 认证安全：认证限流与审计事件、`/logout` 撤销该账号全部会话、`/keys <指纹>` 公钥详情
- 终端界面：版块 / 主题 / 详情 / 编辑四页，正文支持多行（Enter 换行、`.` 或 Ctrl-D 提交）
- 运维：`/healthz` 探活、配置 / 日志 / 优雅停机、SQLite 版本化迁移、写入队列、备份恢复

详细变更见 [CHANGELOG.md](CHANGELOG.md)，当前版本说明见 [RELEASE_NOTES.md](RELEASE_NOTES.md)。配置见 [`.env.sample`](.env.sample)。

## 快速开始

安装（Go 1.26.3+）：

```bash
go install github.com/jeeinn/motd/cmd/server@v0.7.0   # 生成 server 二进制
```

或从源码构建并启动（Windows 下把 `-o motd` 改为 `-o motd.exe`）：

```bash
go build -o motd ./cmd/server
./motd
```

启动后默认监听：运维探针 `127.0.0.1:8080`，SSH 入口 `:2222`，数据落在 `./data`（首次启动自动建库并执行迁移）。

```bash
curl -s http://127.0.0.1:8080/healthz   # => ok dev queue=0/256 online=0 active=0 commits=0
ssh -p 2222 localhost
```

进入后按 `/` 会自动填入前导斜杠，直接输入命令名即可（例如 `login github`，登录命令是 `/login github`），Enter 提交；`j/k` 移动、`Enter` 打开、`q` 返回、`/quit` 退出。终端需支持 VT 序列（Windows Terminal / 现代 conhost / xterm）；旧版终端会退化为追加输出，功能不受影响。

## 配置

全部通过环境变量，空值视为未设置：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MOTD_DATA_DIR` | `./data` | 数据根目录（库、host key、备份） |
| `MOTD_DB_PATH` | `$MOTD_DATA_DIR/motd.db` | SQLite 文件路径 |
| `MOTD_HTTP_ADDR` | `127.0.0.1:8080` | 运维探针监听地址，不承载业务流量 |
| `MOTD_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `MOTD_LOG_FORMAT` | `text` | `text` / `json` |
| `MOTD_SSH_ADDR` | `:2222` | SSH 入口监听地址 |
| `MOTD_SSH_MAX_CONNS` | `100` | SSH 最大并发连接数 |
| `MOTD_GITHUB_CLIENT_ID` | 空 | GitHub OAuth App 的 `client_id`；未设置时 `/login` 返回明确提示，不 panic、日志不回显值 |
| `MOTD_AUTH_LIMIT_*` / `MOTD_AUDIT_*` | 见示例 | 认证限流与审计参数（P1 新增），共 6 项 |
| `MOTD_ADMIN_FINGERPRINTS` | 空 | 管理员公钥指纹白名单（P2 新增），逗号分隔；留空 = 无人可管。比对已认证会话指纹，变更需重启生效 |
| `MOTD_MATRIX_DURATION` | `60s` | `/matrix` 数字雨单次播放时长上限；填 `0` = 不限时（单会话约 30 KB/s，服务端另有并发上限 2） |

全部环境变量（25 个 `MOTD_*` + 脚本用的 `MOTDCTL_BIN`，共 26 项）及其默认值见 [`.env.sample`](.env.sample)。复制为 `.env` 后按需修改：`cp .env.sample .env && chmod 600 .env`——**程序启动时会自动加载**（先当前目录、再可执行文件目录；进程已有环境变量优先、文件只填空缺），无需手动 source；可用 `MOTD_DOTENV_DISABLED=true` 关闭。凭证不要提交到仓库（`.gitignore` 已覆盖）。

## 开发

```bash
git ls-files '*.go' | xargs gofmt -l  # 格式检查（必须零输出；对齐 CI 的 Verify formatting）
git ls-files '*.go' | xargs gofmt -w  # 需要修复时执行
go build ./...          # 构建
go vet ./...            # 静态检查
go test -count=1 ./...  # 全量测试
```

> 注意：`gofmt` 不接受包模式 `./...`（会把 `...` 当目录打开而报错），请用上面的 `git ls-files | xargs` 写法；CI 在干净 checkout 上跑 `gofmt -l .`，本地用 `git ls-files` 只检查入库文件以避开 `data/` 演练副本的误报。发版前门禁见 [CONTRIBUTING.md](CONTRIBUTING.md)「提交前必跑」。

开发约定、依赖方向、提交规范见 [CONTRIBUTING.md](CONTRIBUTING.md)。安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，不要开公开 Issue。

## 备份与恢复

```bash
go build -o bin/motdctl ./cmd/motdctl
scripts/backup.sh                                   # 生成备份 + sha256
scripts/restore.sh --verify <backup-file>            # 只校验
scripts/restore.sh <backup-file> <target-path>      # 恢复（目标必须不存在）
go build -o bin/motdctl ./cmd/motdctl               # 构建管理工具
bin/motdctl fts-rebuild --db <target-path>          # 恢复后重建全文索引（必须执行）
```

从备份恢复后须显式执行 `fts-rebuild`，否则搜索返回空（索引不随备份自动重建）。

## 目录结构

```text
cmd/server/              进程入口
cmd/motdctl/             备份恢复工具
internal/
  transport/             ssh / http / websocket —— 只做协议适配
  application/           auth / forum / user / viewmodel / presence —— 用例编排、事务边界、权限判断
  domain/                user / node / thread / post / vote —— 纯业务模型，不依赖上层
  infrastructure/        sqlite / github / email —— 外部实现
  presentation/          terminal（分层安全渲染）/ browser（规划中）
  plugin/                registry / events / demo / social —— 编译期注册、事件分化
docs/                    决策（DECISIONS.md）、运维（OPS.md）、路线图（ROADMAP.md）、彩蛋（eggs.md）、归档（archived/）
scripts/                 备份恢复脚本
```

依赖方向单向：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。

## 许可证

[MIT](LICENSE) © 2026 Jeeinn
