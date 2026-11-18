package command

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultRegistryHasP0Commands(t *testing.T) {
	want := []string{"top", "open", "new", "reply", "login", "keys", "key", "help", "quit"}
	all := Default().All()
	if len(all) != len(want) {
		t.Fatalf("P0 命令应为 %d 条，实际 %d", len(want), len(all))
	}
	for i, name := range want {
		if all[i].Name != name {
			t.Errorf("命令 #%d 应为 %q，实际 %q", i, name, all[i].Name)
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
