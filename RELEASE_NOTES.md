# Release Notes — v0.3.0

**发布日期**：2026-11-19
**Git tag**：[`v0.3.0`](https://github.com/jeeinn/motd/releases/tag/v0.3.0)
**Module**：`github.com/jeeinn/motd`

> 本版补齐账号自助能力与编辑手感：邮箱注册/登录/找回、正文跨行退格合并。无 schema 迁移，测试从 651 增至 717。

## 亮点

- **邮箱注册 / 登录 / 找回**：`/register` / `/verify <码>` / `/resend` / `/recover` / `/login email`。自带 SMTP（163 隐式 TLS 465 + 授权码），验证码只存 HMAC 散列、单事务原子消费；发信在写事务提交后才触网；argon2id(19MiB, t=2, p=1) 口令；复用 v0.2.0 的双维限流 / 审计 / 会话撤销。`/verify` 设密码需**两次确认**，找回成功后该账号全部会话失效。
- **正文跨行退格合并**：空行行首按退格会把上一行并入、光标停在拼接处；回显只用已有的 SGR+EL+CUU 白名单序列，未放宽 CUP/CUD/ED，安全边界零变化。
- **SSH 登录引导**：连接时 banner 始终提示「非 root 用户名 + 公钥登录 / 未绑定公钥以访客进入 / 禁 root 禁密码」，解决 `Permission denied (publickey)` 无从下手的问题。
- **实测修复**：邮箱流程结束后密码掩码不再泄漏到后续命令输入；`/verify` 两次密码不一致时回退重输、不消费验证码。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.3.0
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)。

**邮箱配置**：复制 `.env.sample` 为 `.env`（0600），填 `MOTD_EMAIL_ENABLED=true` 与 `MOTD_SMTP_HOST` / `PORT` / `USER` / `PASS` / `FROM`（163 用**授权码**，非登录密码；不进仓库、不进 systemd unit）。**程序不自动读 `.env` 文件**——systemd 用 `EnvironmentFile=`，手动运行先 `set -a; source .env; set +a` 再启动。

**升级提示**：从 v0.2.x 升级**无需迁移**（仍止于 0007），替换二进制即可。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.3.0]` 段落。

## 已知问题与限制

- 限流计数纯内存：进程重启清零、多实例不共享。
- 验证码 HMAC pepper 为进程随机：**重启会使在途验证码失效**，重新 `/resend` 即可（换取库泄漏无法离线爆破 6 位码）。
- `/recover` 对未注册邮箱同样回"若邮箱已注册则已发送"（防枚举），但实际**不会发信**；因此可能出现"以为发了但没收到"的困惑——这是防账号存在性泄露的既定取舍。
- 正文单逻辑行超终端宽度自动折行时，跨行退格的 CUU 只上移一个物理行；行内左右移动不做。
- 访客只读；Ctrl+Enter 在通用终端不可靠，正文提交用 `.` 或 Ctrl-D。
- `kill -9` + WAL 恢复演练已在隔离环境通过；`-race` 因本机无 CGO/gcc 未启用；1000 在线未压测。

## 破坏性变更

无。
