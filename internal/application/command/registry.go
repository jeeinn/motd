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
type Command struct {
	Name    string // 不含前导 /
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

// Invocation 是一次解析结果。Name 一定来自注册表（白名单），可安全用于回显。
type Invocation struct {
	Name string
	Args []string
}

// Registry 是不可变的命令注册表；所有查询返回防御性副本。
type Registry struct {
	cmds   []Command
	byName map[string]int
}

var defaultCommands = []Command{
	{Name: "top", Usage: "/top", Summary: "回到版块列表", MaxArgs: 0},
	{Name: "open", Usage: "/open <id>", Summary: "打开指定主题", MinArgs: 1, MaxArgs: 1},
	{Name: "new", Usage: "/new", Summary: "新建主题", MaxArgs: 0},
	{Name: "reply", Usage: "/reply", Summary: "回复当前主题", MaxArgs: 0},
	{Name: "login", Usage: "/login github", Summary: "GitHub Device Flow 登录", MinArgs: 1, MaxArgs: 1, Aliases: []string{"signin"}},
	{Name: "keys", Usage: "/keys", Summary: "管理已绑定公钥", MaxArgs: 0},
	{Name: "help", Usage: "/help", Summary: "显示本帮助", MaxArgs: 0, Aliases: []string{"?"}},
	{Name: "quit", Usage: "/quit", Summary: "退出会话", MaxArgs: 0, Aliases: []string{"exit"}},
}

var defaultBindings = []Binding{
	{Key: "j / ↓", Desc: "下移"},
	{Key: "k / ↑", Desc: "上移"},
	{Key: "h / ←", Desc: "返回上一层"},
	{Key: "l / → / Enter", Desc: "打开当前项"},
	{Key: "?", Desc: "显示本帮助（等价 /help）"},
	{Key: "q", Desc: "退出（等价 /quit）"},
	{Key: "r", Desc: "回复（等价 /reply）"},
	{Key: "Esc", Desc: "取消 / 关闭浮层"},
	{Key: "/", Desc: "进入斜杠命令输入"},
	{Key: "Ctrl-C / Ctrl-D", Desc: "取消输入 / 退出"},
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
func New(defs []Command) (*Registry, error) {
	r := &Registry{byName: make(map[string]int, len(defs)*2)}
	for i, c := range defs {
		if err := validateCommand(c); err != nil {
			return nil, fmt.Errorf("命令 #%d: %w", i, err)
		}
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
	if !ok {
		return Invocation{}, ErrUnknownCommand
	}
	args := fields[1:]
	if len(args) < cmd.MinArgs || (cmd.MaxArgs >= 0 && len(args) > cmd.MaxArgs) {
		return Invocation{}, ErrArgCount
	}
	return Invocation{Name: cmd.Name, Args: append([]string(nil), args...)}, nil
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
