# Contributing to motd

感谢你考虑为 motd 做贡献。本项目处于早期阶段（`v0.x`），公共接口尚未稳定，贡献前建议先开一个 Issue 讨论方向，避免做无用功。

## 开发环境

- Go `1.26.3`（见 `go.mod`，Go module 为 `github.com/jeeinn/motd`）
- 支持 VT 序列的终端（Windows Terminal / 现代 conhost / xterm）
- 可选：`curl`（探活）、一个 SSH 客户端（连入验证）

## 提交前必跑

```bash
gofmt -w ./...          # 格式化
go build ./...          # 构建
go vet ./...            # 静态检查
go test -count=1 ./...  # 全量测试（禁用缓存）
```

涉及终端闭环 / 持久化的改动，额外跑：

```bash
go test -count=1 ./cmd/server -run TestMVPSSHClosedLoop
go test -count=1 ./cmd/server -run TestMVPRestartAndBackupWithRealData
```

CI 会对 `main` 与所有 PR 执行构建、vet 与全量测试；请在本地跑通再提 PR。

## 代码约定

- **依赖方向单向**：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。请勿反向 import。
- **端口定义在使用方**：外部能力（SQLite、GitHub、邮件）的接口由 `application` 声明，`infrastructure` 只做实现。
- **终端输出分层渲染**：所有面向终端的文本走 `internal/presentation/terminal/safe`，不要直接拼接转义序列；新增 SGR 样式先登记到唯一常量表。
- **网络调用在写事务之外**：外部请求不得持有数据库写事务；超时与失败要显式分类。
- **凭据不落库、不进日志、不进呈现结构**：`access_token` / `device_code` 等不写库、不回显。
- 新增环境变量请在 README「配置」表登记；不要提交 `.env`、密钥或备份文件（`.gitignore` 已覆盖）。

## 提交与 PR

- Commit message 采用 [Conventional Commits](https://www.conventionalcommits.org/)（如 `feat:`, `fix:`, `docs:`, `test:`, `refactor:`）。
- 一个 PR 一件事；描述里写清「改了什么 / 为什么 / 怎么验证」，关联对应 Issue。
- 行为变更必须带测试；修 bug 请附一个能复现原问题的测试。
- 首版处于 `0.x`，任何破坏性变更请在 PR 标题标注 `BREAKING`，并同步更新 `CHANGELOG.md`。

## 报告问题

使用 GitHub Issue 模板提交 Bug 或功能建议。安全问题**不要**用公开 Issue，按 [SECURITY.md](SECURITY.md) 的方式私下报告。

参与贡献即表示你同意遵守 [行为准则](CODE_OF_CONDUCT.md)。
