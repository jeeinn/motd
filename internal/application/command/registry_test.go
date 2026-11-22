package command

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultRegistryHasP0Commands(t *testing.T) {
	// P1-M2 追加 /logout，P1-M3 追加 /vote 与 /search，v0.2.1 追加 /delete；
	// P2-M1b 追加 /hide；P2-M2 追加 /edit（紧跟 /delete，同为作者权）；
	// ADR-053 追加 /unhide 与 /unthread（后者可从任意屏恢复隐藏主题）。
	// B 方案（帮助归组）把 7 条管理员命令集中到表尾：顺序固定，帮助文本由注册表生成。
	// 2026-11-22（v0.7.1 / P4.5）：/search 之后追加 /latest（跨版块最新）与 /stats（统计看板）。
	want := []string{"top", "open", "new", "reply", "delete", "edit", "login", "register", "verify", "resend", "recover", "keys", "key", "logout", "vote", "search", "latest", "stats", "help", "quit", "hide", "unhide", "unthread", "node", "node", "node", "node"}
	all := Default().All()
	if len(all) != len(want) {
		t.Fatalf("命令应为 %d 条，实际 %d", len(want), len(all))
	}
	for i, name := range want {
		if all[i].Name != name {
			t.Errorf("命令 #%d 应为 %q，实际 %q", i, name, all[i].Name)
		}
	}
	subs := []string{"list", "create", "rename", "archive"}
	nodeStart := len(want) - len(subs)
	for i, sub := range subs {
		if all[nodeStart+i].Sub != sub {
			t.Errorf("/node 第 %d 条子命令应为 %q，实际 %q", i, sub, all[nodeStart+i].Sub)
		}
	}
}

// B 方案回归：所有命令仍对所有人可见，只是管理员命令归组并加固定说明行。
func TestHelpGroupsAdminCommandsWithoutHiding(t *testing.T) {
	r := Default()
	help := r.Help()
	header := adminGroupHelpHeader
	if n := strings.Count(help, header); n != 1 {
		t.Fatalf("管理员分组说明必须恰好出现一次，实际 %d 次", n)
	}
	headerAt := strings.Index(help, header)
	keysAt := strings.Index(help, "\r\n键位：")

	// 7 条管理员命令全在说明行之后、键位表之前；一条都不少（不隐藏）。
	adminUsages := []string{"/node list", "/node create", "/node rename", "/node archive", "/hide [id]", "/unhide [id]", "/unthread <thread-id>"}
	for _, usage := range adminUsages {
		at := strings.Index(help, usage)
		if at <= headerAt || at >= keysAt {
			t.Fatalf("管理员命令 %q 应在分组说明之后、键位表之前: headerAt=%d at=%d keysAt=%d", usage, headerAt, at, keysAt)
		}
	}
	// 普通命令（含作者权的 /delete、/edit）不受影响，仍在说明行之前。
	for _, usage := range []string{"/top", "/delete [id]", "/edit [id]", "/help", "/quit"} {
		at := strings.Index(help, usage)
		if at < 0 || at >= headerAt {
			t.Fatalf("普通命令 %q 不得被归组或隐藏: headerAt=%d at=%d", usage, headerAt, at)
		}
	}
	// 作者权命令不能带管理员展示标记（权限边界不能被排版混淆）。
	for _, c := range r.All() {
		if (c.Name == "delete" || c.Name == "edit") && c.Group == GroupAdmin {
			t.Errorf("作者权命令 %q 不得归入管理员组", c.Name)
		}
		if c.Group == GroupAdmin && c.Name != "node" && c.Name != "hide" && c.Name != "unhide" && c.Name != "unthread" {
			t.Errorf("意外的管理员分组命令 %q", c.Name)
		}
	}
	// 每条注册命令都能在帮助里找到：归组不是隐藏。
	for _, c := range r.All() {
		if !strings.Contains(help, c.Usage) {
			t.Errorf("帮助缺少命令 %q", c.Name)
		}
	}
}

func TestHelpGroupMetadataIsPresentationOnly(t *testing.T) {
	// 分组说明是固定文案，不含用户数据 / 转义序列。
	if strings.ContainsAny(adminGroupHelpHeader, "\x1b\x00") ||
		!strings.Contains(adminGroupHelpHeader, "管理员命令") {
		t.Fatalf("分组说明必须是固定纯文本: %q", adminGroupHelpHeader)
	}
	// Parse 不把说明行当命令；两条新管理员命令仍可解析。
	if _, err := Default().Parse(adminGroupHelpHeader); !errors.Is(err, ErrNotCommand) {
		t.Fatalf("分组说明行不得被当成命令，实际 %v", err)
	}
	for line, name := range map[string]string{"/hide": "hide", "/unhide": "unhide"} {
		inv, err := Default().Parse(line)
		if err != nil || inv.Name != name || len(inv.Args) != 0 {
			t.Fatalf("Parse(%q) = %+v, %v，期望 %s（0 参数）", line, inv, err, name)
		}
	}
	if _, err := Default().Parse("/unhide 7"); err != nil {
		t.Fatalf("/unhide <id> 应可解析: %v", err)
	}
	if inv, err := Default().Parse("/unthread 4"); err != nil || inv.Name != "unthread" || len(inv.Args) != 1 || inv.Args[0] != "4" {
		t.Fatalf("/unthread <thread-id> 应可解析: inv=%+v err=%v", inv, err)
	}
	if _, err := Default().Parse("/unthread"); !errors.Is(err, ErrArgCount) {
		t.Fatalf("/unthread 缺 ID 应报参数个数不符，实际 %v", err)
	}

	// 自定义注册表：有管理员分组才插说明行，分组紧贴在其前一条之后；无分组则完全不变。
	mixed, err := New([]Command{
		{Name: "ping", Usage: "/ping", Summary: "探针", MaxArgs: 0},
		{Name: "hide", Usage: "/hide [id]", Summary: "隐藏", MinArgs: 0, MaxArgs: 1, Group: GroupAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := mixed.Help()
	if strings.Index(h, "/ping") >= strings.Index(h, adminGroupHelpHeader) ||
		strings.Index(h, adminGroupHelpHeader) >= strings.Index(h, "/hide [id]") {
		t.Fatalf("自定义注册表的分组位置不对: %q", h)
	}
	plain, err := New([]Command{{Name: "ping", Usage: "/ping", Summary: "探针", MaxArgs: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Help(), adminGroupHelpHeader) {
		t.Fatal("没有管理员命令时不得输出分组说明行")
	}
}

func TestParseNodeSubcommands(t *testing.T) {
	r := Default()
	cases := []struct {
		line string
		sub  string
		args []string
		err  error
	}{
		{"/node list", "list", nil, nil},
		{"/node create /dev/x 新节点", "create", []string{"/dev/x", "新节点"}, nil},
		{"/node rename /dev/x 改名", "rename", []string{"/dev/x", "改名"}, nil},
		{"/node archive /dev/x", "archive", []string{"/dev/x"}, nil},
		{"/node list extra", "", nil, ErrArgCount},
		{"/node create /dev/x", "", nil, ErrArgCount},
		{"/node archive /dev/x 多一个", "", nil, ErrArgCount},
		{"/node", "", nil, ErrArgCount},
		{"/node \x1b[2J", "", nil, ErrUnknownCommand},
	}
	for _, tc := range cases {
		inv, err := r.Parse(tc.line)
		if !errors.Is(err, tc.err) {
			t.Errorf("Parse(%q) err = %v，期望 %v", tc.line, err, tc.err)
			continue
		}
		if tc.err != nil {
			continue
		}
		if inv.Name != "node" || inv.Sub != tc.sub || strings.Join(inv.Args, ",") != strings.Join(tc.args, ",") {
			t.Errorf("Parse(%q) = %+v，期望 node/%s %v", tc.line, inv, tc.sub, tc.args)
		}
	}
}

func TestHelpIsGeneratedFromRegistry(t *testing.T) {
	r := Default()
	help := r.Help()
	for _, c := range r.All() {
		if !strings.Contains(help, c.Usage) || !strings.Contains(help, c.Summary) {
			t.Errorf("帮助文本缺少命令 %q（用法 %q）", c.Name, c.Usage)
		}
	}

	// 键位表也必须由注册表生成，且 ? 明确指向 /help。
	foundQuestion := false
	for _, b := range r.Bindings() {
		if strings.Contains(b.Key, "?") && strings.Contains(b.Desc, "/help") {
			foundQuestion = true
		}
		if !strings.Contains(help, b.Key) {
			t.Errorf("帮助文本缺少键位 %q", b.Key)
		}
	}
	if !foundQuestion {
		t.Fatal("键位表里必须有 ? → /help 的映射")
	}

	// 换成自定义命令表，帮助必须自动跟着变——证明没有第二份手写清单。
	custom, err := New([]Command{{Name: "ping", Usage: "/ping", Summary: "自定义探针", MaxArgs: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if h := custom.Help(); !strings.Contains(h, "/ping") || strings.Contains(h, "/top") {
		t.Fatalf("自定义注册表的帮助未随注册表变化: %q", h)
	}
}

func TestParseCommands(t *testing.T) {
	r := Default()
	cases := []struct {
		line string
		name string
		args []string
		err  error
	}{
		{"/open 3", "open", []string{"3"}, nil},
		{"/login github", "login", []string{"github"}, nil},
		{"/signin foo", "login", []string{"foo"}, nil}, // 别名
		{"/?", "help", nil, nil},                       // 别名
		{"/help", "help", nil, nil},
		{"/open", "", nil, ErrArgCount},
		{"/open 3 4", "", nil, ErrArgCount},
		{"/quit extra", "", nil, ErrArgCount},
		{"/nope", "", nil, ErrUnknownCommand},
		{"hello", "", nil, ErrNotCommand},
		{"   ", "", nil, ErrEmptyCommand},
	}
	for _, c := range cases {
		inv, err := r.Parse(c.line)
		if !errors.Is(err, c.err) {
			t.Errorf("Parse(%q) err = %v，期望 %v", c.line, err, c.err)
			continue
		}
		if c.err != nil {
			continue
		}
		if inv.Name != c.name {
			t.Errorf("Parse(%q) name = %q，期望 %q", c.line, inv.Name, c.name)
		}
		if strings.Join(inv.Args, ",") != strings.Join(c.args, ",") {
			t.Errorf("Parse(%q) args = %v，期望 %v", c.line, inv.Args, c.args)
		}
	}
}

func TestParseNeverEchoesUserInput(t *testing.T) {
	// 恶意命令名带清屏序列；错误信息一旦带上原文，调用方回显就会变成渲染旁路。
	malicious := "/x\x1b[2J\x1b]0;pwned\x07"
	_, err := Default().Parse(malicious)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("期望 ErrUnknownCommand，实际 %v", err)
	}
	if strings.Contains(err.Error(), "\x1b") || strings.Contains(err.Error(), "pwned") {
		t.Fatalf("错误信息泄露了用户提交的原始文本: %q", err.Error())
	}
}

func TestNewRejectsInvalidDefinitions(t *testing.T) {
	valid := Command{Name: "ok", Usage: "/ok", Summary: "ok", MaxArgs: 0}
	cases := []struct {
		name string
		defs []Command
	}{
		{"重名", []Command{valid, valid}},
		{"空名", []Command{{Usage: "/x", Summary: "x", MaxArgs: 0}}},
		{"以斜杠开头", []Command{{Name: "/x", Usage: "/x", Summary: "x", MaxArgs: 0}}},
		{"大写", []Command{{Name: "X", Usage: "/X", Summary: "x", MaxArgs: 0}}},
		{"缺 Summary", []Command{{Name: "x", Usage: "/x", MaxArgs: 0}}},
		{"参数区间矛盾", []Command{{Name: "x", Usage: "/x", Summary: "x", MinArgs: 2, MaxArgs: 1}}},
		{"别名冲突", []Command{
			{Name: "a", Usage: "/a", Summary: "a", MaxArgs: 0},
			{Name: "b", Usage: "/b", Summary: "b", MaxArgs: 0, Aliases: []string{"a"}},
		}},
	}
	for _, c := range cases {
		if _, err := New(c.defs); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
}

func TestAllAndBindingsAreDefensiveCopies(t *testing.T) {
	r := Default()
	all := r.All()
	all[0].Name = "mutated"
	if r.All()[0].Name == "mutated" {
		t.Fatal("All() 必须返回副本")
	}
	bs := r.Bindings()
	bs[0].Key = "mutated"
	if r.Bindings()[0].Key == "mutated" {
		t.Fatal("Bindings() 必须返回副本")
	}
}
