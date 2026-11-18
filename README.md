# motd — 命令行论坛

SSH 优先的终端论坛。用户 `ssh` 上来，用键盘和斜杠命令浏览、发帖、回复；同一份应用状态未来可被浏览器 TUI 复用渲染。

设计依据：[docs/design/命令行论坛整体设计方案-评审稿v0.2.md](docs/design/命令行论坛整体设计方案-评审稿v0.2.md)（评审稿 v0.2）。实施计划：[docs/P0-PLAN.md](docs/P0-PLAN.md)。技术决策记录：[docs/DECISIONS.md](docs/DECISIONS.md)。

- 远端：`git@github.com:jeeinn/motd.git`（分支 `main`）
- Go module：`github.com/jeeinn/motd`
- Q1–Q7 架构决策已于 2026-11-18 全部拍板，结论见 `docs/DECISIONS.md ADR-009`

## 当前状态

M0 地基 + M1 业务内核（P0-01 ~ P0-12）已落地，服务可编译、可启动、可建库，**无需 SSH 与 UI 即可跑通发帖业务规则、产出统一 ViewModel 并驱动插件事件链**：

- ✅ 模块化单体目录骨架，依赖方向写在每个包的 `doc.go` 里
- ✅ 配置（环境变量）、结构化日志、优雅停机、运维探针 `GET /healthz`
- ✅ SQLite 双池：写连接恒为 1、读连接按 CPU 核数；PRAGMA 逐连接生效并在启动时自检
- ✅ 版本化迁移：`schema_migrations` + 单事务执行 + 版本高于二进制时拒绝启动
- ✅ `0001_init` 建表（认证域 + 论坛核心域），含约束与外键回归测试
- ✅ 用户名规则按 2026-11-18 决策放宽为**中文 / 英文字母 / 数字 / `_` / `-`，3–20 字符，不允许空白**；domain 层校验为事实来源，DDL 只兜底长度与空白（ADR-005）
- ✅ 依赖方向守卫（`internal/archtest`）：注入反向 import 时测试必然失败（P0-03）
- ✅ 写入队列：单 worker、容量 256 快速失败、3s 超时、`SQLITE_BUSY` 有限重试、事务提交后才确认；写门面不暴露裸 `*sql.DB`（ADR-010）
- ✅ 备份恢复：`motdctl` + `scripts/*.sh` + `docs/OPS.md`，含首次实际恢复演练记录（ADR-011）
- ✅ 版块种子数据 `0002_seed_nodes`（5 个版块）、限流覆盖索引 `0003_rate_limit_indexes`；`INSERT OR IGNORE` 与 `IF NOT EXISTS` 保证重放幂等
- ✅ 仓储端口定义在使用方 `application/forum`，`infrastructure/sqlite` 实现；写入只暴露受限 `WriteTx`，application 层禁 import SQL（守卫实测会失败）
- ✅ 论坛核心用例：`ListNodes` / `ListThreads` / `GetThread` / `CreateThread` / `CreatePost`；keyset 分页、软删除过滤、标题正文校验、访客只读、仅 active 可写、首帖同事务、发帖限流（Q6）
- ✅ 真实链路集成测试：`forum.Service` 直驱 SQLite 仓储，验证 fake 与实现不漂移（P0-08 ~ P0-10）
- ✅ 统一 ViewModel：`application/viewmodel` 只含语义字段（枚举 / Unix 秒 / 值类型），`forum` 负责领域结果映射；Q5 等级分档 `karma → L1–L4`；三层守卫实测注入渲染概念后必然失败（ADR-014）
- ✅ 领域事件与插件骨架：领域/交互两条通道分离；`init()` 编译期注册 + demo 彩蛋订阅 `PostCreated` 申请成就；成就经 `application/user` 校验，P0 不写 SQLite；插件禁直连数据库守卫实测会失败（ADR-015）
- **M0 已完成；M1 已完成 P0-08 / 09 / 10 / 11 / 12**（每项均「测试通过 → 单独提交」）
- ⬜ M1 仅剩 P0-13 在线注册表；之后是 M2 认证、M3 终端、M4 闭环 —— 见 P0-PLAN

## 快速开始

```bash
# 编译
go build -o motd.exe ./cmd/server

# 启动（默认监听 127.0.0.1:8080 的运维探针，数据落在 ./data）
./motd.exe

# 探活
curl -s http://127.0.0.1:8080/healthz   # => ok dev queue=0/256
```

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

> 只声明当前真正被读取的配置项。SSH、OAuth、加密密钥等配置在对应模块实现时再加，避免出现「配置存在但没人读」的假接口。

## 开发

```bash
go build ./...       # 构建
go vet ./...         # 静态检查
go test ./...        # 测试（约束/PRAGMA/写入队列/备份恢复/依赖守卫/仓储/用例/ViewModel/插件/端到端集成）
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
  application/           auth / forum / user / viewmodel —— 用例编排、事务边界、权限判断、统一语义状态
  domain/                user / node / thread / post / vote —— 纯业务模型，不依赖上层
  infrastructure/        sqlite / github / email —— 外部实现
  presentation/          terminal / browser —— 两个独立 Renderer
  plugin/                registry / events / demo —— 编译期注册、事件分化、彩蛋示例
docs/                    设计与计划
```

依赖方向单向：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。
