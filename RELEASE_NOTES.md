# Release Notes — v0.2.0

**发布日期**：2026-11-19
**Git tag**：[`v0.2.0`](https://github.com/jeeinn/motd/releases/tag/v0.2.0)
**Module**：`github.com/jeeinn/motd`

> 第二阶段发布。在 v0.1.0 的可用闭环之上补齐认证安全、论坛互动与编辑体验。迁移扩展到 0007，测试从 456 增至 632。

## 亮点

- **论坛互动**：投票 `/vote up|down|rm`（幂等、改票按差值计分）、全文搜索 `/search`（FTS5 trigram，中文短词回退 LIKE，特殊字符按字面量）。投票只改分数、不推进主题活跃度，不影响排序。
- **认证安全**：`auth_events` 审计（溢出 fail-closed）、按 IP/用户/邮箱的内存限流、`/logout` 撤销该账号全部在线会话、`/keys <指纹>` 公钥详情（防指纹枚举）。
- **多行正文**：Enter 换行，单独一行 `.` 或 Ctrl-D 提交；粘贴整块进正文、内部换行不误提交；标题仍 Enter 确认。
- **登录呈现**：`/login` 发起屏改为论坛同款 Screen，状态栏与版块页一致。
- **数据完整性**：主题软删（`threads.status`）、回复父子同 thread 约束、领域事件 outbox（事务内落库、提交后投递、崩溃重扫）。
- **运维**：设备码过期清理、公钥 `last_used_at` 节流、`motdctl fts-rebuild`、`.env.sample` 全量配置示例（17 项中文注释）。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.2.0
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

启动后：探活 `curl -s http://127.0.0.1:8080/healthz`，连入 `ssh -p 2222 localhost`。首次启动自动在 `./data` 建库并执行迁移。

**升级提示**：从 v0.1.0 的数据库启动会自动应用 0004~0007 四个迁移（只向前）。升级前请先备份 `data/motd.db`。从备份恢复后，必须执行 `motdctl fts-rebuild --db <path>` 重建全文索引，否则搜索返回空。

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.2.0]` 段落。

## 已知问题与限制

- **邮箱注册 / 登录 / 密码找回**独立为下一阶段，本版本不含；`.env.sample` 已预留其变量名（当前不生效）。
- 限流计数纯内存：进程重启清零、多实例不共享。
- Ctrl+Enter 在通用 SSH 终端字节级不可区分，本版本不做提交键；正文提交用 `.` 或 Ctrl-D，Ctrl+Enter 增强留待后续。
- `kill -9` + WAL 恢复演练已在隔离环境通过；`-race` 因本机无 CGO/gcc 未启用，不声称 race-free。
- 100 连接压测、目标 1000 在线（P3）尚未验证；systemd 部署附件与浏览器 TUI 未包含。
- 编辑器退格不跨行回到上一行；`label` 字段无设置入口、恒为空。

## 破坏性变更

无公共接口破坏性变更。内部行为变化：投票分数走独立路径、恢复后 FTS 索引需手动重建。

## 致谢

感谢所有参与设计评审、实现与测试的贡献者。
