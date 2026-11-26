# AGENTS.md — motd 项目智能体协作指南

> 本文件面向在本仓库工作的 AI 智能体与人类贡献者，记录**必须遵守的硬约束**与**易踩坑点**。
> 与 `CONTRIBUTING.md` 分工：后者面向人类贡献者（提交规范、Issue 流程）；本文件额外强调智能体最容易违反的边界。
> 事实来源：`README.md`、`CONTRIBUTING.md`、`docs/DECISIONS.md`（ADR-001~079）、`.github/workflows/ci.yml`、实际 git/代码状态。

## 1. 项目定位

SSH 优先的命令行论坛：用户 `ssh` 连入，用键盘与斜杠命令浏览版块、发帖、回复，**无需浏览器**。

- Go module：`github.com/jeeinn/motd`，Go `1.26.3`
- 当前版本：`v0.11.0`（2026-11-26），处于 `0.x`，对外契约未稳定
- 单进程二进制：`cmd/server`（服务）+ `cmd/motdctl`（离线备份恢复）
- 依赖极少：`golang.org/x/crypto`（SSH）、`modernc.org/sqlite`（纯 Go SQLite）。**非必要不引入新依赖**

## 2. 架构与依赖方向（硬约束）

```
cmd/                 装配层，唯一能同时看到多层的入口
internal/
  transport/         ssh / http / websocket —— 只做协议适配
  application/       auth / forum / user / viewmodel / presence —— 用例编排、事务边界、权限判断
  domain/            user / node / thread / post / vote / event —— 纯业务模型
  infrastructure/    sqlite / github / email —— 外部实现
  presentation/      terminal（分层安全渲染）/ browser（规划中）
  plugin/            registry / events / demo —— 编译期注册
  archtest/          架构守卫（依赖方向、安全渲染、单一事实来源）
```

- **依赖单向**：`transport → application → domain`；`presentation → application`；`infrastructure` 实现 `application` 定义的接口；`domain` 不依赖任何上层。端口（接口）定义在**使用方**。
- `internal/archtest/` 有一批静态守卫，违反依赖方向或安全渲染会直接测试失败。**改完先跑 archtest**。
- 统计量级：1133 个测试函数，全量 `go test -count=1 ./...` 约 10 秒。

## 3. 必须遵守的安全与实现约定

| 约定 | 说明 |
| --- | --- |
| 终端输出分层渲染 | 所有面向终端的文本走 `internal/presentation/terminal/safe`；服务端固定文本走 `RenderSystem`，DB/用户文本走 `RenderUser`。新增 SGR 样式先登记到唯一常量表。**不要直接拼 ANSI 转义序列** |
| 信任域隔离 | 绝不把用户提交文本拼进 `RenderSystem`；固定文案不含用户数据（ADR-019） |
| 网络调用在写事务之外 | 外部请求（GitHub / SMTP）不得持有 SQLite 写事务；3s 写超时扛不住网络 |
| 凭据不落库/不进日志/不进呈现 | `access_token`、`device_code`、SMTP 授权码等；验证码只存 HMAC 散列 |
| 权限单点 | 作者权 `canModify`、管理员权 `requireAdmin` 各只有一个判定点，不新造第二套 |
| 软删不硬删 | `threads`/`posts` 用 `status` 过滤；节点归档只改 `node.status`，`threads` 不动（FK） |
| 会话生存期 | 无按键不断连；唯一约束是 6 小时绝对上限（`sessionMaxLifetime`），关闭前下发固定文案 + exit-status。**已移除 `MOTD_SSH_IDLE_TIMEOUT`（v0.4.2，ADR-055）** |
| 迁移 | 版本化 SQL 在 `internal/infrastructure/sqlite/migrations/`，当前止于 `0011`；新增迁移不得修改历史迁移 |
| 光标控制白名单边界 | 服务端固定序列**仅** SGR / EL / CUU / 无参 CUP home `\x1b[H` / 无参 ED 2 `\x1b[2J`，只在 `internal/presentation/terminal/safe/ansi.go` 定义、由 `terminal/frame.go` 消费（`RenderFrame` 局部覆盖 + `RenderCleanFrame` 整屏重绘）；用户文本永远走 `RenderUser` / `SanitizeText`。**禁**带参 CUP（`\x1b[r;cH`）、CUD、DECSET、OSC、带参 EL、`1J`/`3J`（ADR-072） |

## 4. 开发命令（与 CI 门禁一致）

CI（`.github/workflows/ci.yml`）对 `main` 与所有 PR 依次执行 4 个门禁。**本地按同口径全跑通再提交**：

```bash
git ls-files '*.go' | xargs gofmt -l   # ① 必须零输出
go build ./...                          # ② Build
go vet ./...                            # ③ Vet
go test -count=1 ./...                  # ④ Test（禁用缓存）
```

**易踩坑**：

- `gofmt` **不接受包模式** `./...`——它会把 `...` 当目录打开并报 `open ...: no such file`。全仓库格式化用 `git ls-files '*.go' | xargs gofmt -w`。
- 本地**不要**用 `gofmt -l .`：会把 `data/` 下 4 个演练副本（被 `.gitignore` 排除、v7/v8/m0/m1 源码快照）一并列出误报。CI 在干净 checkout 上跑 `gofmt -l .`，本地用 `git ls-files` 才是等价范围。
- 涉及终端闭环 / 持久化的改动，额外跑：
  ```bash
  go test -count=1 ./cmd/server -run TestMVPSSHClosedLoop
  go test -count=1 ./cmd/server -run TestMVPRestartAndBackupWithRealData
  ```

## 5. 发版流程（最高优先级硬约束）

> **任何发版动作必须先向用户说明并取得明确确认，不得擅自执行。**
> 包括但不限于：`git commit`、`git tag`、`git push`、GitHub Release、版本号落定。
> 即使用户此前说过"按方案推进 / 开始实施 / 准备发布"，也**不等于**授权发版。
> 用户原话（2026-11-20）："下次发版前必须经过我确认，不得擅自发版。"

智能体可以：改代码、改文档、跑测试、把改动暂存（`git add`）、准备发布说明草稿。
智能体**不可以**（除非用户当次明确确认）：commit / tag / push / 建 Release。

确认后的发版顺序（历史教训：v0.4.0 先建 Release 后 CI 失败）：

1. 留回退点分支：`git branch backup/pre-vX.Y.Z-YYYYMMDD HEAD`
2. 核对暂存区为空，再精确 `git add <明确文件列表>`（**避开 `.env`、`data/`、`motd.exe`、`bin/`**）
3. commit（Conventional Commits）→ annotated tag → push main + tag
4. **等 CI 全绿**（`gh run watch <id>`）→ 再 `gh release create`
5. 发版说明正文用 `RELEASE_NOTES.md`，可把"待打 tag"措辞在临时副本里改为"已发布"，不改仓库文件

高风险 git 操作（`reset` / `force push` / 删已发布 tag）前必须先留回退点。

## 6. 文档索引（改前先读对应文档）

| 文档 | 内容 | 何时读 |
| --- | --- | --- |
| `README.md` | 用户入口：功能、安装、配置表、快速开始 | 改对外描述 / 配置 |
| `CONTRIBUTING.md` | 开发约定、提交前门禁、PR 规范 | 任何提交前 |
| `docs/DECISIONS.md` | ADR-001~079，所有已落地的技术决策与理由（078 为 R-14 游戏立项、079 为 R-15 社区运营优化立项，均未实施） | **做设计决策前必读**，避免推翻既有结论 |
| `docs/OPS.md` | 部署、备份恢复、演练记录、监控告警；§9 实战部署清单、§10 AI 智能体自动部署指南 | 改运维相关 / 部署；智能体执行远程部署前**必读 §10** |
| `docs/ROADMAP.md` | 未实施方向的单一入口：待办总览（R-01~R-13）+ 逐项详情 + 已否决项 | **排期或接新需求前必读**，避免重复评估已否决项 |
| `docs/archived/` | 已完成的设计稿与阶段计划（P0/PHASE1/PHASE2/P2/P3/P4/P4.5、6 份 DESIGN、原始评审稿），索引见其 `README.md` | 追溯需求来源 / 查历史方案 |
| `docs/MVP-DELIVERY.md` | MVP 交付复盘（D1~D7 判定） | 理解项目起点 |
| `docs/DEVIATIONS.md` | 对设计评审稿的偏差登记 | 改设计相关 |
| `docs/eggs.md` | 彩蛋清单、实现位置与未做项 | 改彩蛋 |
| `CHANGELOG.md` / `RELEASE_NOTES.md` | 变更记录 / 当前版本说明 | 发版 |
| `docs/images/` | README 引用的截图，命名规范见其 README | 新增截图 |

**归档约定**：设计稿对应功能发版、结论固化进 ADR 后移入 `docs/archived/`，并把未完成项迁到 `docs/ROADMAP.md`。历史 ADR 中以 `docs/xxx.md` 行内代码引用的旧路径**不回改**（`DECISIONS.md` 是历史原文），查同名文件到 `docs/archived/`。

**惯例**：历史结论不删改，只标注"已修订"并追加新 ADR / 新章节。新增 ADR 从当前最大号顺延（现为 ADR-079）。

## 7. 项目特有陷阱速查

- **`data/` 是本地运行时/演练目录**，被 `.gitignore` 排除；里面有 v7/v8 源码副本，**不要**格式化、不要提交、不要当生产代码改。
- **`.env` 含真实凭据**（163 SMTP 授权码等），已被 `.gitignore` 排除；`motd.exe`、`bin/`、`dist/` 同理。任何 `git add` 都要先看 `git status`。
- **配置文件真相源**：`internal/config/config.go`；`.env.sample` 是示例，新增环境变量两处都要登记 + README 配置表。
- **README 环境变量计数**：改配置后同步更新（当前 25 个 `MOTD_*` + `MOTDCTL_BIN` = 26 项）。
- **CI 的 Node.js 20 deprecated 警告**不影响结果（actions 被强制跑在 Node 24），忽略即可。
- **用户信息无 `display` 字段投影**：终端只显示 `@username`（v0.3.3 起），改显示契约按 ADR-014 三层守卫评估。
- **回复计数用 `INDEXED BY idx_posts_thread`**：索引名硬绑定是已知技术债，重命名索引须同步改查询。

## 8. 截图与图片资源

统一放 `docs/images/`，全小写连字符英文命名，相对路径引用，alt 文本写清画面内容。详见 `docs/images/README.md`。**不要在仓库根目录散落图片文件**。

## 9. 协作风格

- 中文交流；结论先行、结构化输出（表格 / P0-P1 标签 / 具体行号定位）。
- **先审后改**：定位源码 → 交叉核对 ADR / 约定 → 输出方案或 diff → 经确认再实际改动（高风险动作）。
- 改进逐项推进：验证逻辑 → 自测 → 清晰提交信息 → 结构化汇报。
- 用户偏好被动接收后台任务完成通知，不主动轮询状态。
