# Release Notes — v0.1.0

**发布日期**：2026-11-19
**Git tag**：[`v0.1.0`](https://github.com/jeeinn/motd/releases/tag/v0.1.0)
**Module**：`github.com/jeeinn/motd`

> 首个 GitHub 开源版本。此前项目在本地已可用，本次发布不改变功能，仅将其以语义化版本正式公开。

## 亮点

- **命令行论坛，SSH 即入口**：`ssh -p 2222 localhost` 连入，用键盘和斜杠命令浏览版块 / 主题、发帖、回复；仅公钥认证，禁 root。
- **完整业务闭环**：5 个版块种子、keyset 分页、软删除过滤、访客只读、GitHub Device Flow 登录与公钥绑定、断开重连凭公钥恢复会话。
- **四页终端界面**：版块 / 主题 / 详情 / 编辑，原地重绘导航，分层安全渲染。
- **运维就绪**：`/healthz` 探活、配置 / 日志 / 优雅停机、SQLite 版本化迁移、写入队列、备份恢复工具与演练脚本。
- **质量基线**：全量 `go build` / `go vet` / `go test -count=1 ./...` 通过，456 个测试函数，含真实 SSH 客户端闭环与带数据重启演练。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.1.0
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

启动后：探活 `curl -s http://127.0.0.1:8080/healthz`，连入 `ssh -p 2222 localhost`。首次启动自动在 `./data` 建库并执行迁移。完整配置见 README。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.1.0]` 段落。

## 已知问题与限制

- 真实 GitHub Device Flow 授权链路已修但因连接心跳缺陷需重启后再复测一次完整闭环；VPS 上的凭据注入与出站放行尚未验证。
- 认证类限流 / 安全日志（P1）、100 连接压测（P3）、systemd 部署附件、浏览器 TUI 与搜索 / 投票等 P1 能力暂未包含。
- 未做 `kill -9` 硬杀演练与 `-race` 检测。

## 破坏性变更

无。这是首个版本。

## 致谢

感谢所有参与设计评审、实现与测试的贡献者。
