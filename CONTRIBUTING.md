# Contributing to motd

感谢你考虑为 motd 做贡献。本项目处于早期阶段（`v0.x`），公共接口尚未稳定，贡献前建议先开一个 Issue 讨论方向，避免做无用功。

## 开发环境

- Go `1.26.3`（见 `go.mod`，Go module 为 `github.com/jeeinn/motd`）
- 支持 VT 序列的终端（Windows Terminal / 现代 conhost / xterm）
- 可选：`curl`（探活）、一个 SSH 客户端（连入验证）

## 提交前必跑（与 CI 门禁逐项对齐）

CI（`.github/workflows/ci.yml`）对 `main` 与所有 PR 依次执行 4 个门禁：**Verify formatting → Build → Vet → Test**。本地必须按同样口径全部跑通再提交 / 发版：

```bash
# ① Verify formatting —— 必须只列出【版本库内】未格式化的 .go 文件，正确输出为空
git ls-files '*.go' | xargs gofmt -l
#   等价修复：git ls-files '*.go' | xargs gofmt -w
go build ./...          # ② Build，对应 CI 的 Build
go vet ./...            # ③ Vet，对应 CI 的 Vet
go test -count=1 ./...  # ④ Test，对应 CI 的 Test（禁用缓存）
```

**为什么不用 `gofmt -l .` / `gofmt -w ./...`**（两条都踩过坑）：

- CI 跑的是 `gofmt -l .`，但它作用于**干净 checkout**——不存在 `data/` 演练副本（v7/v8/m0/m1 的源码快照，被 `.gitignore` 排除）。本地直接跑 `gofmt -l .` 会把这 4 个历史快照一并列出而误报，因此本地改用 `git ls-files '*.go'`，只检查入库文件，**与 CI 的判定范围等价**。
- `gofmt` 不接受 Go 的包模式 `./...`——它会把 `...` 当目录打开并报 `open ...: no such file`。要格式化全仓库请用上表的 `git ls-files | xargs gofmt -w`。

**发版前额外门禁**（发版 commit / 打 tag 之前必做，v0.4.0 CI 失败的根因就是漏了 ①）：

```bash
git ls-files '*.go' | xargs gofmt -l   # 必须零输出
git status --short                      # 确认无未提交变更、暂存区无多余文件
```

> 2026-11-20 复盘：v0.4.0 首次发版只跑了 build/vet/test，漏跑格式化检查，7 个 P2 新增文件未 gofmt，push 后 CI 在 Verify formatting 步骤失败（run `44711752405`）；补 `gofmt -w` 后重跑通过（run `44723878859`）。发版自检清单据此与 CI 对齐。

涉及终端闭环 / 持久化的改动，额外跑：

```bash
go test -count=1 ./cmd/server -run TestMVPSSHClosedLoop
go test -count=1 ./cmd/server -run TestMVPRestartAndBackupWithRealData
```

## 代码约定

- **依赖方向单向**：`transport → application → domain`，`presentation → application`，`infrastructure` 实现 `application` 定义的接口，`domain` 不依赖任何上层。请勿反向 import。
- **端口定义在使用方**：外部能力（SQLite、GitHub、邮件）的接口由 `application` 声明，`infrastructure` 只做实现。
- **终端输出分层渲染**：所有面向终端的文本走 `internal/presentation/terminal/safe`，不要直接拼接转义序列；新增 SGR 样式先登记到唯一常量表。
- **改 ANSI 白名单必须逐项同步，缺一不可**：① `internal/presentation/terminal/safe/ansi.go` 定义 → ② `internal/archtest/safe_test.go` 守卫（正向允许集 + 负向注入）→ ③ `internal/presentation/terminal/frame_test.go` 白名单断言 → ④ `docs/DECISIONS.md` 新增或修订 ADR 并同步 `AGENTS.md §3`。只改其中一处会留下"守卫放行但文档未登记"或"文档允许但测试仍拒"的裂缝（ADR-072 教训）。
- **网络调用在写事务之外**：外部请求不得持有数据库写事务；超时与失败要显式分类。
- **凭据不落库、不进日志、不进呈现结构**：`access_token` / `device_code` 等不写库、不回显。
- 新增环境变量请在 README「配置」表登记；不要提交 `.env`、密钥或备份文件（`.gitignore` 已覆盖）。

## 提交与 PR

- Commit message 采用 [Conventional Commits](https://www.conventionalcommits.org/)（如 `feat:`, `fix:`, `docs:`, `test:`, `refactor:`）。
- 一个 PR 一件事；描述里写清「改了什么 / 为什么 / 怎么验证」，关联对应 Issue。
- 行为变更必须带测试；修 bug 请附一个能复现原问题的测试。
- 首版处于 `0.x`，任何破坏性变更请在 PR 标题标注 `BREAKING`，并同步更新 `CHANGELOG.md`。

## 自动发版（tag 触发）

跨平台二进制与 GitHub Release 由 `.github/workflows/release.yml` 自动完成，无需手工打包上传。发起流程：

1. **先在 `CHANGELOG.md` 登记该版本段**：`## [x.y.z] - YYYY-MM-DD` 起，到下一个 `## [` 止。发布说明直接提取这一段，**不存在则 workflow 失败**（fail-fast，避免发出无说明的 Release）。
2. 按 `AGENTS.md` §5 取得发版确认，commit → push → PR → CI 双绿 → merge → main CI 绿。
3. 打 annotated tag 并推送：`git tag -a vX.Y.Z -m "说明" && git push origin vX.Y.Z`。tag 需匹配 `vMAJOR.MINOR.PATCH`（预发布 `-rc1` 等亦可）。
4. workflow 自动产出 6 个归档（linux / windows / darwin × amd64 / arm64）+ `checksums.txt`，注入 `main.version`，并以 CHANGELOG 段作为 Release 正文。

本地校验（不改仓库文件）：

```bash
python3 scripts/extract_release_notes.py 0.10.0 CHANGELOG.md /tmp/notes.md  # 验证提取
```

构建配置见 `.goreleaser.yml`（`CGO_ENABLED=0`、`-trimpath`、提交时间作时间戳，保证可复现）。**生产部署不随 Release 自动进行**，仍按 `docs/OPS.md` §10 单独执行。

## 报告问题

使用 GitHub Issue 模板提交 Bug 或功能建议。安全问题**不要**用公开 Issue，按 [SECURITY.md](SECURITY.md) 的方式私下报告。

参与贡献即表示你同意遵守 [行为准则](CODE_OF_CONDUCT.md)。
