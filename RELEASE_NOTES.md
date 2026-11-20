# Release Notes — v0.3.1

**发布日期**：2026-11-19
**Git tag**：[`v0.3.1`](https://github.com/jeeinn/motd/releases/tag/v0.3.1)
**Module**：`github.com/jeeinn/motd`

> 体验优化小版本。`.env` 自动加载、`/verify` 先验码、命令提示去重、展示时区可配置。无 schema 迁移、无新依赖，测试从 717 增至 749。

## 本版变化

- **`.env` 自动加载**：程序启动自动读 `.env`，查找顺序为**当前工作目录 → 可执行文件所在目录**（命中第一个即停、不合并）。**进程已有环境变量优先，文件只填空缺**——systemd `EnvironmentFile=` 注入的凭据不会被旁路 `.env` 覆盖。开关 `MOTD_DOTENV_DISABLED=true` 可关闭。启动日志只输出 `配置来源 file=<路径>` 或 `配置来源 env_only`，不打任何键值。不再需要手动 `source .env`。
- **`/verify` 先验码**：无效 / 过期 / 已消费的验证码在收密码**之前**就报错，不再让用户白输两遍 19MiB argon2 密码。预检严格只读、不消费、不计数；正式 `Verify()` 仍独立校验。防爆破用独立限流（邮箱维度跨 IP 共享 20 次/小时）。
- **命令输入提示去重**：每会话首次按 `/` 显示一次提示，后续静默；重连重新显示。长会话不再刷屏。
- **展示时区可配置**：新增 `MOTD_DISPLAY_TIMEZONE`（默认 `Asia/Shanghai`），修复帖子/回复与 `/keys` 时间固定按 UTC 显示、中国用户看到相差 8 小时的问题。仅影响终端呈现，存储层归一化不变、历史数据无需迁移；论坛格式 `MM-DD HH:MM` 不变，`/keys` 后缀按时间戳时刻的时区缩写显示。非法时区值启动即失败、不静默回落；二进制内嵌 IANA 时区库，精简镜像无需挂载 zoneinfo。

## 升级提示

从 v0.3.0 升级**无需迁移**（仍止于 0007），替换二进制即可。

**注意**：本版起 `.env` 会被自动加载。若部署目录下有历史遗留 `.env`，请确认其中值不会意外补齐本该由 systemd / 容器注入的配置（进程已设环境变量优先级更高、不会被覆盖；只影响原本"没设置、回落默认值"的键）。完全不希望自动加载时设 `MOTD_DOTENV_DISABLED=true`。

## 安装与运行

```bash
go install github.com/jeeinn/motd/cmd/server@v0.3.1
# 或从源码构建
go build -o motd ./cmd/server && ./motd
```

配置见 [README](README.md) 与 [`.env.sample`](.env.sample)。复制 `.env.sample` 为 `.env`（0600）后**直接启动即可**，无需 source；systemd 仍可用 `EnvironmentFile=`。

## 变更明细

见仓库内 [`CHANGELOG.md`](CHANGELOG.md) 的 `[0.3.1]` 段落。

## 已知问题与限制

沿用 v0.3.0：限流计数纯内存；验证码 pepper 重启失效；`/recover` 对未注册邮箱回统一文案但实际不发信；正文超宽折行与行内左右移动不做；Ctrl+Enter 不可靠，正文提交用 `.` 或 Ctrl-D。

预检通过后到两遍密码输完之间，验证码若因并发消费 / 超时失效，正式 `Verify()` 仍会拒绝——此时会白输密码，这是 TOCTOU 正确性优先的可接受残留。

## 破坏性变更

无。
