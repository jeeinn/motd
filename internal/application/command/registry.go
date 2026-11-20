// Package command 是斜杠命令与默认键位的唯一事实来源（评审稿 §3.2）。
//
// 边界：只描述「有哪些命令、怎么解析、怎么生成帮助」，不执行业务；
// 业务动作由 application 用例提供，P0-24 的页面控制器负责调用。
// 帮助文本与键位说明都由注册表生成，不允许在页面里手写第二份命令清单。
package command

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmptyCommand   = errors.New("空命令")
	ErrNotCommand     = errors.New("不是斜杠命令")
	ErrUnknownCommand = errors.New("未知命令")
	ErrArgCount       = errors.New("参数个数不符")
)

// Command 描述一条斜杠命令的元数据。MaxArgs = -1 表示不限参数个数。
// Sub 非空表示这是某条命令的子命令（如 /node create）：Name 相同但 Sub 唯一，
// MinArgs/MaxArgs 约束的是**子命令之后**的参数个数。
type Command struct {
	Name    string // 不含前导 /
	Sub     string // 子命令名，可为空
	Usage   string // 如 "/open <id>"
	Summary string
	Aliases []string
	MinArgs int
	MaxArgs int
}

// Binding 是「键位 → 语义」的说明，P1 的鼠标 / 快捷键也映射到同一张表。
type Binding struct {
	Key  string
	Desc string
}

// Invocation 是一次解析结果。Name / Sub 一定来自注册表（白名单），可安全用于回显。
type Invocation struct {
	Name string
	Sub  string
	Args []string
}

// Registry 是不可变的命令注册表；所有查询返回防御性副本。
type Registry struct {
	cmds     []Command
	byName   map[string]int
	bySub    map[string]int
	subNames map[string]bool
}

func subKey(name, sub string) string { return name + "\x00" + sub }

var defaultCommands = []Command{
	{Name: "top", Usage: "/top", Summary: "回到版块列表", MaxArgs: 0},
	{Name: "open", Usage: "/open <id>", Summary: "打开指定主题", MinArgs: 1, MaxArgs: 1},
	{Name: "new", Usage: "/new", Summary: "新建主题", MaxArgs: 0},
	{Name: "reply", Usage: "/reply", Summary: "回复当前主题", MaxArgs: 0},
	// v0.2.1：无参删当前主题，带一个 id 则删当前主题下的该条回复（仅作者）。
	{Name: "delete", Usage: "/delete [id]", Summary: "删除当前主题 / 指定回复（仅作者）", MinArgs: 0, MaxArgs: 1},
	// P2-M2：与 /delete 对称的作者权。无参重新输入当前主题正文（首帖），
	// 带 id 则重新输入指定回复全文（仅作者）。MinArgs=0 让无参进入 handler 解析首帖。
	{Name: "edit", Usage: "/edit [id]", Summary: "编辑当前主题正文 / 指定回复（仅作者）", MinArgs: 0, MaxArgs: 1},
	// P2-M1b：与 /delete 对称的治理口径。无参隐藏当前主题，带 id 隐藏回复（仅管理员）。
	{Name: "hide", Usage: "/hide [id]", Summary: "隐藏当前主题 / 指定回复（仅管理员）", MinArgs: 0, MaxArgs: 1},
	// MinArgs=0 是刻意的：/login 不带参数时不落到通用「参数个数不符」，
	// 而是进入 handle 的 provider 白名单分支，给出支持的 provider 提示。
	{Name: "login", Usage: "/login github | /login email", Summary: "GitHub Device Flow / 邮箱密码登录", MinArgs: 0, MaxArgs: 1, Aliases: []string{"signin"}},
	// P1-M5 邮箱认证：交互输入邮箱/密码，验证码经 SMTP 发送；不做 HTTP 端点。
	{Name: "register", Usage: "/register", Summary: "邮箱注册（交互输入邮箱，收验证码）", MaxArgs: 0},
	{Name: "verify", Usage: "/verify <验证码>", Summary: "提交验证码并设置密码（完成注册 / 找回）", MinArgs: 1, MaxArgs: 1},
	{Name: "resend", Usage: "/resend", Summary: "重发邮箱验证码（60 秒冷却）", MaxArgs: 0},
	{Name: "recover", Usage: "/recover", Summary: "邮箱找回密码（交互输入邮箱）", MaxArgs: 0},
	// P1-M4：/keys 带一个指纹参数时查单钥详情（label / created_at / last_used_at）。
	{Name: "keys", Usage: "/keys [fingerprint]", Summary: "列出公钥 / 查看单钥详情", MinArgs: 0, MaxArgs: 1},
	// P0-18：/key add 绑定「当前连接」的公钥，不接受任何公钥文本参数；
	// /key rm <fingerprint> 只接指纹标识符（由 auth.ParseKeyCommand 严格收口）。
	{Name: "key", Usage: "/key add | /key rm <fingerprint>", Summary: "绑定当前连接公钥 / 解绑指定指纹", MinArgs: 1, MaxArgs: 2},
	// P1-M2：主动注销撤销该账号全部在线会话，无参数（不给「只注销某台」的假承诺）。
	{Name: "logout", Usage: "/logout", Summary: "注销当前账号并撤销其全部在线会话", MaxArgs: 0},
	// P1-M3：投票只作用于当前打开的主题（value 为 up/down/rm）。
	{Name: "vote", Usage: "/vote <up|down|rm>", Summary: "给当前主题投赞成/反对票或撤销", MinArgs: 1, MaxArgs: 1},
	// 关键词可含空格：MaxArgs=-1 后在页面层用空格拼回，仍只把整串作为查询词。
	{Name: "search", Usage: "/search <关键词>", Summary: "检索正文", MinArgs: 1, MaxArgs: -1},
	// P2-M1：节点治理四条子命令。参数区间各自独立，缺子命令/多参数都在注册表层挡掉。
	{Name: "node", Sub: "list", Usage: "/node list", Summary: "列出全部版块（含归档，仅管理员）", MaxArgs: 0},
	{Name: "node", Sub: "create", Usage: "/node create <slug> <name>", Summary: "创建版块（仅管理员）", MinArgs: 2, MaxArgs: 2},
	{Name: "node", Sub: "rename", Usage: "/node rename <slug> <name>", Summary: "版块改名（仅管理员）", MinArgs: 2, MaxArgs: 2},
	{Name: "node", Sub: "archive", Usage: "/node archive <slug>", Summary: "归档版块（仅管理员）", MinArgs: 1, MaxArgs: 1},
	{Name: "help", Usage: "/help", Summary: "显示本帮助", MaxArgs: 0, Aliases: []string{"?"}},
	{Name: "quit", Usage: "/quit", Summary: "退出会话", MaxArgs: 0, Aliases: []string{"exit"}},
}

var defaultBindings = []Binding{
	{Key: "j / ↓", Desc: "下移"},
	{Key: "k / ↑", Desc: "上移"},
	{Key: "q / h / ←", Desc: "返回上一层"},
	{Key: "l / → / Enter", Desc: "打开当前项"},
	{Key: "?", Desc: "显示本帮助（等价 /help）"},
	{Key: "r", Desc: "回复（等价 /reply）"},
	{Key: "n / p", Desc: "主题列表下一页 / 回第一页"},
	{Key: "Esc", Desc: "取消 / 关闭浮层"},
	{Key: "/", Desc: "进入斜杠命令输入"},
	{Key: "Ctrl-C / Ctrl-D", Desc: "退出会话（等价 /quit）"},
}

var defaultRegistry = mustNew(defaultCommands)

// Default 返回内置的 8 条 P0 命令。命令表是编译期常量，校验失败属程序 bug。
func Default() *Registry { return defaultRegistry }

func mustNew(defs []Command) *Registry {
	r, err := New(defs)
	if err != nil {
		panic("内置命令表非法: " + err.Error())
	}
	return r
}

// New 校验并构造注册表。重名、空名、非法字符、别名冲突、参数区间矛盾一律拒绝。
// 同名不同 Sub 是子命令，合法；同一个 (Name,Sub) 重复仍拒绝。
func New(defs []Command) (*Registry, error) {
	r := &Registry{
		byName:   make(map[string]int, len(defs)*2),
		bySub:    make(map[string]int, len(defs)),
		subNames: make(map[string]bool, len(defs)),
	}
	for i, c := range defs {
		if err := validateCommand(c); err != nil {
			return nil, fmt.Errorf("命令 #%d: %w", i, err)
		}
		if c.Sub == "" {
			if _, exists := r.byName[c.Name]; exists {
				return nil, fmt.Errorf("命令重名: %q", c.Name)
			}
			r.byName[c.Name] = i
			for _, a := range c.Aliases {
				if _, exists := r.byName[a]; exists {
					return nil, fmt.Errorf("别名 %q 与已有命令冲突", a)
				}
				r.byName[a] = i
			}
		} else {
			key := subKey(c.Name, c.Sub)
			if _, exists := r.bySub[key]; exists {
				return nil, fmt.Errorf("子命令重名: %q", c.Name+"/"+c.Sub)
			}
			r.bySub[key] = i
			r.subNames[c.Name] = true
		}
		copyCmd := c
		copyCmd.Aliases = append([]string(nil), c.Aliases...)
		r.cmds = append(r.cmds, copyCmd)
	}
	return r, nil
}

func validateCommand(c Command) error {
	if c.Name == "" || strings.HasPrefix(c.Name, "/") {
		return fmt.Errorf("命令名不合法: %q", c.Name)
	}
	for i, ch := range c.Name {
		ok := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-'
		if i == 0 {
			ok = ch >= 'a' && ch <= 'z'
		}
		if !ok {
			return fmt.Errorf("命令名只含小写字母/数字/中划线且以字母开头: %q", c.Name)
		}
	}
	for i, ch := range c.Sub {
		ok := ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-'
		if i == 0 {
			ok = ch >= 'a' && ch <= 'z'
		}
		if !ok {
			return fmt.Errorf("%q 的子命令名不合法: %q", c.Name, c.Sub)
		}
	}
	if c.Sub != "" && len(c.Aliases) > 0 {
		return fmt.Errorf("子命令 %q 不支持别名", c.Name+"/"+c.Sub)
	}
	if c.Usage == "" || c.Summary == "" {
		return fmt.Errorf("%q 缺少 Usage 或 Summary", c.Name)
	}
	if c.MinArgs < 0 || c.MaxArgs < -1 || (c.MaxArgs >= 0 && c.MaxArgs < c.MinArgs) {
		return fmt.Errorf("%q 的参数区间不合法", c.Name)
	}
	for _, a := range c.Aliases {
		if a == "" || strings.HasPrefix(a, "/") {
			return fmt.Errorf("%q 的别名不合法: %q", c.Name, a)
		}
	}
	return nil
}

// All 返回按定义顺序的命令副本。
func (r *Registry) All() []Command {
	out := make([]Command, len(r.cmds))
	copy(out, r.cmds)
	return out
}

// Lookup 支持别名解析。
func (r *Registry) Lookup(name string) (Command, bool) {
	i, ok := r.byName[name]
	if !ok {
		return Command{}, false
	}
	return r.cmds[i], true
}

// Parse 解析一行斜杠命令。
//
// 错误信息**不携带用户提交的原始文本**：调用方可能把它写回终端，
// 一旦原文含 ESC / CSI 就会变成渲染旁路（P0-23 的硬约束提前落地）。
func (r *Registry) Parse(line string) (Invocation, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return Invocation{}, ErrEmptyCommand
	}
	if !strings.HasPrefix(trimmed, "/") {
		return Invocation{}, ErrNotCommand
	}
	fields := strings.Fields(trimmed)
	name := strings.TrimPrefix(fields[0], "/")
	cmd, ok := r.Lookup(name)
	sub := ""
	args := fields[1:]
	if r.subNames[name] {
		// 子命令名同样来自注册表白名单；未知子命令只回固定错误，不回显原文。
		if len(args) == 0 {
			return Invocation{}, ErrArgCount
		}
		sub = args[0]
		i, found := r.bySub[subKey(name, sub)]
		if !found {
			return Invocation{}, ErrUnknownCommand
		}
		cmd = r.cmds[i]
		args = args[1:]
	} else if !ok {
		return Invocation{}, ErrUnknownCommand
	}
	if len(args) < cmd.MinArgs || (cmd.MaxArgs >= 0 && len(args) > cmd.MaxArgs) {
		return Invocation{}, ErrArgCount
	}
	return Invocation{Name: cmd.Name, Sub: sub, Args: append([]string(nil), args...)}, nil
}

// Bindings 返回默认键位说明副本。
func (r *Registry) Bindings() []Binding {
	out := make([]Binding, len(defaultBindings))
	copy(out, defaultBindings)
	return out
}

// Help 由 All() 与 Bindings() 生成，是帮助屏的唯一来源。
// 页面若要展示命令，必须调这里，不允许另写一份清单（§3.2 验收项）。
func (r *Registry) Help() string {
	var b strings.Builder
	b.WriteString("命令（唯一入口）：\r\n")
	for _, c := range r.All() {
		_, _ = fmt.Fprintf(&b, "  %-18s %s\r\n", c.Usage, c.Summary)
	}
	b.WriteString("\r\n键位：\r\n")
	for _, k := range r.Bindings() {
		_, _ = fmt.Fprintf(&b, "  %-18s %s\r\n", k.Key, k.Desc)
	}
	return b.String()
}
