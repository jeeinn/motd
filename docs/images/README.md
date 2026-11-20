# 截图资源

本目录存放 README 与文档中引用的截图（SVG / PNG / JPG / WebP）。

## 命名规范

- 全小写、连字符分隔、英文：`terminal-node-list.png`、`terminal-thread-detail.png`。
- 名称描述**画面内容**，不用 `image1.png` / `截图.png` 这类无信息命名。
- 同一界面的多张图用序号后缀：`terminal-search-1.png`、`terminal-search-2.png`。

## 新增截图

1. 图片放入本目录，按上述规范命名。
2. 在 README 或对应文档中用相对路径引用：`![说明](docs/images/<文件名>)`（从仓库根算）。
3. `alt` 文本写清画面内容，不要留 `alt text` 占位——这是可访问性要求。
4. 截图前确认终端尺寸一致（建议 80×24），避免同一组图宽窄不一。

当前截图：

| 文件 | 内容 | README 引用 |
| --- | --- | --- |
| `terminal-node-list.png` | SSH 连入后的版块列表首屏（含 `guest@jeeinn.com` 在线体验命令） | 预览 |
| `login-github.png` | GitHub Device Flow 登录到 `/new` 发帖的完整流程 | 预览 |

> 尺寸说明：两张截图来自不同终端窗口，`login-github.png` 为宽屏、内容较长；新增截图时尽量保持与同组图一致的终端宽度（建议 80 列）。
