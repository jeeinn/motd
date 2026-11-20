# Release Notes — v0.4.3

**发布日期**：2026-11-20
**Git tag**：`v0.4.3`（已发布）
**Module**：`github.com/jeeinn/motd`

> 文档与运维增强版本。无代码行为变更、无 schema 迁移、无新依赖；二进制功能与 v0.4.2 一致，测试仍为 **905**。重点是把首次 VPS 部署的实战经验固化成可复制的运维手册，并新增面向 AI 智能体的自动部署规程。

## 本版变化

### Added

- **在线体验入口**：README 新增 `ssh -p 2222 guest@jeeinn.com` 直接连入试用说明，并补充「版块列表首屏」与「GitHub Device Flow 登录到 `/new` 发帖」两张截图（`docs/images/`）。
- **`AGENTS.md` 智能体协作指南**：明确架构依赖方向、CI 四门禁、发版硬约束（任何 commit / tag / push / Release 均须当次显式确认）、文档索引与项目特有陷阱。
- **两种部署形态（OPS.md §9.0 / §9.6）**：
  - 方式一：systemd 托管，非 root 用户运行、0600 环境文件、单元模板、host key 复用与启动自检。
  - 方式二：便携版自部署——用户自己编译的二进制 + 自有目录 `.env` + `screen` 启动，含端口错开、优雅停止、备份升级与 `.env` 查找顺序等特有坑。
- **AI AGENTS 自动部署指南（OPS.md §10）**：输入契约、只读前置探测与 STOP 条件、网络/路径/端口决策表、7 步幂等执行序列与验收门、失败模式速查、交付报告模板及不可协商护栏（不发版、不泄露凭据、不覆盖数据库与 host key）。

### Changed

- **OPS.md §9 按 CentOS Stream 9 实机重新校准**：国内 VPS 上 `github.com` / `proxy.golang.org` 超时的应对（`goproxy.cn`）、module cache 只读需复制到可写目录、小内存 VPS 的 `-p 2` / `GOMEMLIMIT` 构建参数、版本注入判据（`/healthz` 不得回显 `dev`）。
- **权限口径修正**：`/opt/motd` 与二进制须为 `0750 root:motd`；`0750 root:root` 会导致 systemd 在 CHDIR 阶段失败（`status=200/CHDIR`）并重启循环。
- **环境文件路径按发行版判定**：`/etc/motd` 在 RHEL/CentOS 是普通登录横幅文件，此时改用 `/etc/sysconfig/motd`（0600 root:root）。systemd 模式保持 `MOTD_DOTENV_DISABLED=true`，`.env` 不参与配置；凭据只写环境文件，不写 unit。
- **端口冲突处理**：默认健康探针 `127.0.0.1:8080` 若被占用（实测为 `matea-linux-amd`），改绑 `127.0.0.1:18080`；SSH 入口仍为 `:2222`。

## 升级说明

从 v0.4.2 升级无需迁移、无需替换二进制：本版仅文档与运维资料更新，程序行为、配置项、数据库 schema 与测试基线均未变化。

- 已按 systemd 部署的实例：不要同时维护 `/etc/sysconfig/motd` 与 `/opt/motd/.env` 两份配置；systemd 模式下 `.env` 默认不生效，所需键应迁入 0600 环境文件后 `systemctl restart motd`。
- 希望改用便携版的用户：按 OPS.md §9.6 自建目录与 `.env`，并与现有实例错开 SSH / healthz 端口。
- 从更早版本升级的迁移口径不变（`0001` → `0009` 顺序补齐）。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.4.3
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

部署形态选择、目录权限、环境变量与验收命令见 [`docs/OPS.md`](docs/OPS.md) §9；智能体执行自动部署前须读 §10。配置表见 [README](README.md) 与 [`.env.sample`](.env.sample)。

## 已知问题与限制

- 与 v0.4.2 相同：会话最长在线时长固定 6 小时、不可配置；白名单变更需重启生效；审计 queue full 时 DomainBus 去重可能吞掉重投；回复计数 `INDEXED BY idx_posts_thread` 索引名硬绑定为技术债。
- 终端 VT 兼容、限流纯内存、验证码 pepper 重启失效等前版限制不变。
- 便携版使用 `screen` 不提供原生开机自启；需要时须自行配置 cron `@reboot` 或迁移到 systemd。

## 验证

- 本地发版前四门禁全绿：`git ls-files '*.go' | xargs gofmt -l` 零输出、`go build ./...`、`go vet ./...`、`go test -count=1 ./...`（905 测试，0 FAIL），archtest 全绿；CI 门禁在 push 后复核。
- 实战验收（CentOS Stream 9 `/ jeeinn.com`）：`/healthz` = `ok v0.4.2 ...`；迁移 `[1..9]`；host key 指纹重启前后一致；内外网访客 `ssh -p 2222` 进入成功；`motdctl backup` 冒烟通过。

## 破坏性变更

无。二进制功能与 v0.4.2 完全一致，仅文档与运维流程变化。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.4.3]` 段落，技术决策见 [`docs/DECISIONS.md`](docs/DECISIONS.md)（当前 ADR 最大号仍为 ADR-055）。
