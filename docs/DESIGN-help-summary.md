# 帮助命令摘要视图 + 分组下钻 — 设计方案（待评审）

> 状态：**已实施于分支 `feat/r16-help-summary`，待发版**（2026-11-26）；无迁移、未 commit/tag/push。评审过程稿已删除，结论以本文为准。
> 基线：v0.11.0（测试 1175，迁移止于 `0013`，ADR 止于 079）。
> 关联：ROADMAP **R-16**（评审通过后登记）；ADR-019（信任域）、ADR-026（零回显）、ADR-070（i18n 基建边界）、ADR-072（命令输出追加例外）、ADR-075 / ADR-079（命令全可见、只归组）。

## 0. 结论先行

`?` / `/help` 默认只出**摘要**：普通组命令 + 键位 + 其他分组索引行；管理员 / 游戏 / 彩蛋三组通过 `/help admin|game|egg` 下钻查看。纯文本层改动：不引入页面态、不新增键位、不按角色隐藏任何命令。

**拍板口径（2026-11-26，用户确认按推荐项）**：

| # | 决策点 | 结论 |
| --- | --- | --- |
| D1 | 摘要粒度 | **A1**：普通组 22 条全列 + 键位全量 + 分组索引；普通组不再单独下钻。 |
| D2 | 渲染收口 | 两处生产出口（`actionHelp`、`case "help"`）经 `writeSystem`（`RenderSystem`）；`formatResult` 是纯函数、**不接 `writeSystem`、不承担分组路由**（见 §4.2 与 §5 风险 1，评审 P1-1 补正）。 |
| D3 | section 名 | `admin` / `game` / `egg`，与注册表 `Group*` 常量一一对应，不另起译名。 |

## 1. 现场事实（代码取证）

| # | 事实 | 位置 |
| --- | --- | --- |
| F1 | 帮助有两个入口：浏览态按 `?` 走 `actionHelp`；命令 `/help`（别名 `?`）。两处都直接 `io.WriteString(sess, h.reg.Help())`，纯追加到 scrollback | `cmd/server/ssh_handler.go:538-539`、`:566-567` |
| F2 | 兜底出口 `formatResult` 的 `case "help"` 现返回 `reg.Help()`；**生产唯一调用点在解析失败分支**（`:555`），该分支 `err != nil` 在错误类型匹配处即返回，到不了 Name switch，`case "help"` 生产不可达、仅单测手工构造 `(reg, inv, nil)` 时命中 | `cmd/server/ssh_handler.go:553-557`、`:1120-1139` |
| F3 | 帮助清单唯一来源 `Registry.Help()`，由 `All()` + `Bindings()` 生成；验收项禁止页面手写第二份命令表 | `internal/application/command/registry.go:346-371` |
| F4 | 命令分组：普通组（`Group==""`）22 条、`GroupGame` 4 条、`GroupAdmin` 9 条、`GroupEgg` 3 条；键位 10 条 | `registry.go:97-161`、`:163-173`；`eggs.go:20-27` |
| F5 | 现状帮助出口**未走** `writeSystem`/`RenderSystem`；`writeSystem` 注释口径为"全由常量与本地数字拼成的多行文本"应走可信渲染链 | `ssh_handler.go:675-691` |
| F6 | `RenderSystem` 只拒控制字符 / DEL / C1 / Bidi / 不可见格式；中文、`·`、括号、`/`、数字、空格均允许 | `internal/presentation/terminal/safe/render.go:74-87` |
| F7 | 既有测试依赖帮助全量输出，分两类：直接调 `reg.Help()`（`registry_test.go:92-96`、`:207-225`、`egg_test.go:87-102`）；调 `formatResult`（`ssh_handler_test.go:132-139`、`key_binding_handler_test.go:55-67`）。分类核对见 §5 风险 1 | 同左 |
| F8 | 帮助标题与分组说明行全部硬编码中文，未进 i18n catalog；R-08 全量文案翻译是独立未开工项 | `registry.go:43/54/58/354/366`；ADR-070 §D3 |
| F9 | ADR-072 决策 7：命令输出（`/who`、`/whoami`、`/stats` 等）属"例外保持追加"；本期帮助仍在追加路径，不改这条取舍 | `DECISIONS.md` ADR-072 决策 7 |

### 现状量级（静态估算）

全量 `Help()` 约 **56~57 行**（标题 1 + 三个分组头各 2 行 + 38 条命令 + 键位段 2 行 + 10 条键位，含空行）；80×24 终端需滚约 **2.4 屏**，重复按 `?` 会再刷一遍。

A1 摘要约 **37 行**（标题 1 + 普通命令 22 + 空行 1 + 键位标题 1 + 键位 10 + 空行 1 + 索引行 1）；80×24 终端约滚 **1.5 屏**。

## 2. 问题定义

| # | 问题 | 后果 | 定性 |
| --- | --- | --- | --- |
| P1 | 全量命令一次铺开，约 2.4 屏 | 常用命令被管理员 / 游戏 / 彩蛋条目稀释；查找成本高；刷屏占用 scrollback | 体验优化，非功能性缺口 |
| P2 | 多组命令混排，无分组导航 | 用户不知道有哪些下钻入口；管理员/彩蛋条目与日常操作同屏竞争注意力 | 同上 |
| P3 | 帮助出口绕过可信渲染链 | 与 ADR-019/026 的固定文案口径不一致（当前内容虽无用户数据，属历史遗留） | 顺带收口，不单独立项 |

## 3. 候选方案与取舍

### 方案 A1：摘要 + 分组下钻（**采纳**）

`?`/`/help` 无参出摘要：普通组 22 条 + 键位 10 条 + 一行分组索引；`/help <section>` 下钻单组明细。

- 优点：改动集中在注册表文本生成与 handler 路由；不动帧模型、按键路由、页面状态；命令清单仍只来自注册表。
- 代价：摘要约 37 行（22 命令 + 键位段 + 索引），24 行终端约滚 1.5 屏——这是 A1 保留普通组全量的既定取舍。

### 方案 A2：首屏仅索引（未采纳）

摘要只留键位 + 分组索引（一屏内约 16 行），普通组与键位也走下钻（`/help common`、`/help keys`）。

- 未采纳理由：查日常命令要多一步；38 条规模下分层收益不抵操作成本。若 A1 上线后反馈仍嫌长，可再演进。

### 方案 B：整屏帮助页 + 分页（后置，不在本期）

`?` 进入 `ScreenHelp` 整屏帧，按组装箱分页，`n/p` 翻页、`q`/`Esc` 返回复原。`ScreenHelp` 枚举已预留（`viewmodel.go:24`），分页可复用 R-06 装箱模式。

- 未采纳理由：跨 viewmodel / presentation / handler 三层，且需正面修订 ADR-072 决策 7（帮助从"追加命令日志"改为"页面态"），要补新 ADR。作为后续独立项登记，不与本期混做。

### 对比

| 维度 | A1 摘要下钻 | A2 纯索引 | B 整屏分页页 |
| --- | --- | --- | --- |
| 首屏内容量 | 中（约 37 行） | 小（约 16 行） | 一屏内分页 |
| 查日常命令步数 | 1 | 2 | 1 |
| 改动层 | registry + handler 路由 | registry + handler 路由 | viewmodel + presentation + handler |
| 是否动帧模型 / 键位 | 否 | 否 | 是（帮助态按键路由） |
| 是否需新 ADR | 否 | 否 | 是（修订 ADR-072 决策 7） |
| 估工 | S~M | S~M | M |

## 4. 详细设计（A1）

### 4.1 命令表与注册表（`internal/application/command/registry.go`）

**命令表项 `help` 调整**：

- `MaxArgs: 0 → 1`（`MinArgs` 仍为 0）；`/help admin extra` 仍由解析层 `ErrArgCount` 挡下。
- `Usage`：`/help [admin|game|egg]`。
- `Summary`：`显示帮助（默认常用命令，可按分组下钻）`。
- `Aliases: ["?"]` 不变。

**新增方法 / 包级函数**（清单仍只来自 `All()`，无第二份手写表；`SectionGroup` 为包级函数，非方法）：

```go
// HelpSummary 输出摘要视图：普通组命令 + 键位 + 其他分组索引行。
func (r *Registry) HelpSummary() string

// HelpGroup 输出指定分组的明细；组内无命令时返回空串。
// group 取 GroupAdmin / GroupGame / GroupEgg；普通组不在下钻范围。
func (r *Registry) HelpGroup(group string) string

// SectionGroup（包级函数）把命令行 section 名映射到分组常量；未登记返回 ok=false。
func SectionGroup(section string) (group string, ok bool)
```

- `HelpSummary()` 结构（内部统一用 `\n` 换行，出口层转 CRLF，满足 `RenderSystem` 校验）。复用的 `groupHelpHeader()` 与 `helpTextFor` 固定提示自带 `\r\n`，由 `writeSystem` 出口统一规范化后转 CRLF；方法内部**不手工去 `\r`**，靠出口兜底即可：
  1. 标题 `命令（常用）：\n`（**仅摘要用此标题**；`Help()` 全量标题 `命令（唯一入口）：` 保持不变，见 §4.3 注）
  2. 遍历 `All()`，输出 `Group == ""` 的命令行（复用现有 `%-18s` 对齐格式）
  3. 键位段：`键位：` + 10 条绑定
  4. 索引行：格式 `其他分组：/help admin（管理员 N）· /help game（游戏 N）· /help egg（彩蛋 N）`
- **索引行条数口径**：N = `len(All()` 中 `Group == 对应常量` 的**条目数（含子命令）**，不按命令名去重。例如管理员组 9 条条目（`node` 4 条子命令各占一条）；若按名字去重会误算成 6。后续增删命令，条数自动更新，不硬编码。
- **索引行 slug 来源**：`admin`/`game`/`egg` 必须由 `SectionGroup` 白名单的**反向映射**生成，不得从 `Group` 字符串直接拼接。索引只枚举白名单内、且组内确有命令的分组；自定义注册表里的非白名单 `Group` 值不进索引，避免出现"索引可见但 `/help <slug>` 未命中白名单、下钻不可达"的假入口。
- `HelpGroup(group)` 复用现有 `groupHelpHeader(group)` 分组说明行，只输出该组命令；行为与全量 `Help()` 中对应段落一致。
- **保留 `Help()` 全量方法**：逐字节行为不变（含其标题与分组头），供既有测试与兜底口径使用；不在本期重构其内部拼接。

**section 白名单映射**（`SectionGroup`）：

| section | Group 常量 | 分组说明 |
| --- | --- | --- |
| `admin` | `GroupAdmin` | 管理员命令（需白名单指纹授权） |
| `game` | `GroupGame` | 游戏命令（无需权限，访客可玩但不上榜） |
| `egg` | `GroupEgg` | 彩蛋命令（无需权限，纯娱乐） |

### 4.2 出口路由（`cmd/server/ssh_handler.go`）

**路由只在两处生产出口实现**，抽一个共用纯函数复用（评审 P1-1 补正）：

```go
// helpTextFor（落点 cmd/server/ssh_handler.go）是帮助文本的唯一路由纯函数；inv 为零值时出摘要。
func helpTextFor(reg *command.Registry, inv command.Invocation) string
```

| 出口 | 现状 | 改后 |
| --- | --- | --- |
| `actionHelp`（浏览态 `?`） | `WriteString(reg.Help())` | 调 `helpTextFor(reg, Invocation{})` → 摘要，经 `writeSystem` 输出 |
| `case "help"`（命令提交，`:566-567`） | `WriteString(reg.Help())` | 调 `helpTextFor(reg, inv)`，经 `writeSystem` 输出 |
| `formatResult` 兜底（`:1120-1139`） | `case "help"` 返回 `reg.Help()` | **不承担 section 路由**；`case "help"` 仅改为返回 `HelpSummary()` 口径（无参摘要），维持纯函数、裸文本由调用侧写出，**不接 `writeSystem`** |

**为什么 `formatResult` 不接路由**：其生产唯一调用点在解析失败分支（`:553-557`），`err != nil` 时在错误类型匹配处即返回，到不了 Name switch；`case "help"` 生产不可达、仅单测手工构造 `(reg, inv, nil)` 命中。若把 section 路由写进 `formatResult`，`inv.Args` 在失败分支恒为 nil，相关分支永远无法被生产触发，成为与 `case "help"` 并存的第二套实现，必然漂移。

`helpTextFor` 路由规则：

1. 无参（或 `inv.Args` 为空）→ `HelpSummary()`。
2. 有参且 `SectionGroup(section)` 命中：
   - 组内有命令 → `HelpGroup(group)`；
   - `HelpGroup` 返回空串（自定义注册表缺该组）→ 由 `helpTextFor` **显式转换**为固定提示 `该分组暂无命令\r\n`（空串到提示的转换在路由函数内完成，不靠 `HelpGroup` 自己产提示）。
3. 未命中白名单 → 固定提示 `用法：/help [admin|game|egg]\r\n`；**不回显 `inv.Args` 原文**（ADR-026）。

**渲染收口（D2）**：两处生产出口改为经 `writeSystem(sess, text)` 输出（`:681-687`），不再裸 `io.WriteString`。`writeSystem` 先规范化换行、过 `RenderSystem` 校验、再转 CRLF；渲染失败静默不输出。`formatResult` 保持纯函数属性，不在其内碰 IO、不调渲染层。

### 4.3 文案样例

> 注：摘要标题用「命令（常用）：」；`Help()` 全量标题「命令（唯一入口）：」保持原样不变（`registry.go:354`），避免误改全量标题触发既有断言。

**`?` / `/help`（摘要，A1）**：

```
命令（常用）：
  /top                回到版块列表
  /open <id>          打开指定主题
  /new                新建主题
  /reply              回复当前主题
  /delete [id]        删除当前主题 / 指定回复（仅作者）
  /edit [id]          编辑当前主题正文 / 指定回复（仅作者）
  /login github | /login email  GitHub Device Flow / 邮箱密码登录
  ...（其余普通组命令，共 22 条）
  /quit               退出会话

键位：
  j / ↓               下移
  k / ↑               上移
  q / h / ←           返回上一层
  l / → / Enter       打开当前项
  ?                   显示本帮助（等价 /help）
  r                   回复（等价 /reply）
  n / p               主题列表下一页 / 回第一页
  Esc                 取消 / 关闭浮层
  /                   进入斜杠命令输入
  Ctrl-C / Ctrl-D     退出会话（等价 /quit）

其他分组：/help admin（管理员 9）· /help game（游戏 4）· /help egg（彩蛋 3）
```

**`/help admin`（下钻示例）**：

```
管理员命令（需白名单指纹授权）：
  /hide [id]          隐藏当前主题 / 指定回复（仅管理员）
  /unhide [id]        恢复当前主题 / 指定回复（仅管理员）
  /unthread <thread-id>  恢复被隐藏的主题（可从任意页面）
  /pin                置顶当前主题（仅管理员，每版块 1 条）
  /unpin              取消当前置顶（仅管理员）
  /node list          列出全部版块（含归档，仅管理员）
  /node create <slug> <name>  创建版块（仅管理员）
  /node rename <slug> <name>  版块改名（仅管理员）
  /node archive <slug>  归档版块（仅管理员）
```

### 4.4 安全与信任域

- 帮助内容全部来自注册表常量（Usage / Summary / Binding）与本地计数，**不含用户数据**；两处生产出口走 `RenderSystem`，不内联任何 ANSI 序列。
- section 参数只在白名单集合内匹配；未知 section 只回固定提示，`inv.Args` 绝不进入输出（ADR-026 零回显）。
- `Group` 仍是纯展示元数据，不参与任何鉴权；管理员命令仍只在 `requireAdmin` 单点判定（ADR-049/053/079）。
- 帮助里的管理员 / 彩蛋条目**没有被隐藏**：下钻可达，命令照常可解析；摘要不列 ≡ 隐藏。守 ADR-075"命令表是唯一发现入口"的边界。

### 4.5 边界

| 边界 | 处理 |
| --- | --- |
| `/help <未知 section>` | 固定用法提示，不回显原文；无 ESC / CSI 面 |
| `/help admin extra` | `MaxArgs=1` 解析层挡为 `ErrArgCount`，走既有参数个数不符提示 |
| 自定义注册表缺某分组 | 索引行不列该组；下钻该组返回"该分组暂无命令" |
| 别名 `/?` | 解析行为不变，仍命中 `help`、无参出摘要 |
| 浏览态 `?` 按键 | 不经命令解析，直接出摘要；与命令入口行为一致 |
| i18n | 新增文案硬编码中文，与现状一致；全量 UI 翻译属 R-08，不在本期 |
| 追加口径 | 帮助仍走 ADR-072 决策 7 的追加路径；不引入整屏帧、不动 scrollback 语义 |

## 5. 代价、风险

**改动文件（预计 2 个生产文件 + 3~4 个测试文件）**：

- `internal/application/command/registry.go`：`help` 表项 + 三个新方法 + section 映射。
- `cmd/server/ssh_handler.go`：两处生产出口（`actionHelp`、`case "help"`）路由 + 改走 `writeSystem`；`formatResult` 维持纯函数、不接路由、不接渲染层。
- 测试：`registry_test.go`、`eggs_test.go`（如需）、`ssh_handler_test.go`、`key_binding_handler_test.go`。

**无 schema 迁移、无新依赖、无新环境变量、无新键位、无命令增删**（仅 `help` 放开一个可选参数）。

**风险**：

1. **既有测试断言清单（按调用方式分类核对）**：
   - **直接调 `reg.Help()` 的测试——不受影响**：`Help()` 逐字节保留，`registry_test.go:92-96`、`:207-225`、`egg_test.go:87-102` 原样通过。
   - **调 `formatResult(reg, inv, nil)` 的测试——需逐条核对**：
     - `ssh_handler_test.go:132-139` `TestFormatResultNeverEchoesUserInput`：遍历 `reg.All()` 断言全量 Usage，摘要不含非普通组 → 该断言改为直接调 `reg.Help()` 保留全量口径。
     - `key_binding_handler_test.go:55-67` `TestKeyCommandHelpIsGeneratedFromRegistry`：断言 `/keys`、`/key add`、`/key rm <fingerprint>`，三者均普通组（`Group==""`），摘要保留 → **仍通过，无需改动**，实施时显式复核一次。
     - `ssh_handler_test.go:126`、`:143` 为恶意输入与 `/open 3` 用例，不依赖帮助全量，不受影响。
2. **摘要不列管理员 / 游戏 / 彩蛋，发现入口变弱**：索引行显式给出三条下钻路径与条数；命令仍可直接输入执行，不构成隐藏。上线后观察是否有用户反馈找不到彩蛋 / 管理命令。
3. **摘要仍约 37 行**：A1 既定取舍；若 24 行终端反馈仍偏长，再评估 A2 或方案 B。
4. **`writeSystem` 包裹 Plain SGR**：两处生产出口的输出从裸文本变为一对 Plain 开/闭样式；既有 `strings.Contains(usage)` 断言不受影响（子串仍在），但需回归确认终端显示无多余样式残留。

## 6. 验证计划

1. **注册表单测**（表驱动）：
   - 摘要含普通组命令与全部键位；不含 `/hide`、`/game play`、`/cowsay` 等非普通组用法。
   - 索引行含 `/help admin|game|egg` 与派生条数；自定义注册表缺彩蛋则索引行不含 `egg`。
   - 自定义注册表含非白名单 `Group` 值时，索引行不出现该分组 slug（防"可见但下钻不可达"的假入口）。
   - `HelpGroup(GroupAdmin)` 只出管理员命令；`HelpGroup(GroupEgg)` 只出彩蛋；空组返回空串。
   - `SectionGroup` 白名单命中 / 未命中；`/help admin` 解析成功且 `Args=["admin"]`；`/help a b` 为 `ErrArgCount`。
2. **Handler 单测**：
   - `/help admin` 输出管理员组、不含彩蛋；`/help egg` 同理。
   - 未知 section 固定提示，输出无 ESC、无原文（含恶意序列用例）。
   - `actionHelp`（`?`）出摘要、不含管理员明细。
   - `formatResult` 的 `case "help"` 返回摘要口径、不做 section 路由；解析失败分支仍只出固定错误提示。
3. **既有断言调整**：全量口径改调 `reg.Help()`；`/keys` 属普通组，摘要仍覆盖。
4. **门禁回归**：`gofmt -l` 零输出、`go build ./...`、`go vet ./...`、`go test -count=1 ./...` + archtest 全绿；测试函数数只增不减。
5. **真机 SSH 冒烟**（如环境允许）：80×24 下 `?` 出摘要 → `/help admin` 下钻 → 未知分组提示 → 返回原页无残影。

## 7. 不做项 / 遗留

- 不做整屏帮助页、分组内翻页、键位聚焦导航（方案 B，后置独立项）。
- 不做 `/help <关键词>` 全文搜索；不做按角色隐藏命令。
- 不做帮助文案 i18n（R-08 范围外）。
- 不为本期补 ADR：未改变 ADR-072 追加口径、未推翻 ADR-075/079 可见性决策；若后续演进到方案 B，再按顺延编号立新 ADR。
- 评审通过后，在 `docs/ROADMAP.md` 待办总览登记 **R-16**（P2/P3 小优化：帮助摘要 + 分组下钻），再进入实施。
