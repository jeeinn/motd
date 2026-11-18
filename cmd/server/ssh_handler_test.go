package main

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
)

func runeMsg(r rune) input.KeyMsg { return input.KeyMsg{Kind: input.KeyRune, Rune: r} }

func TestApplyKeyCommandLine(t *testing.T) {
	h := newInputHandler()
	if h.reg != command.Default() {
		t.Fatal("inputHandler 必须使用唯一的默认注册表")
	}

	cl := &commandLine{}
	if a, _, _ := applyKey(cl, runeMsg('?')); a != actionHelp {
		t.Fatalf("? 应触发帮助，实际 %v", a)
	}
	if a, _, _ := applyKey(cl, runeMsg('/')); a != actionCommandStart || !cl.active {
		t.Fatalf("/ 应进入命令编辑态，实际 action=%v active=%v", a, cl.active)
	}
	for _, r := range "open 3" {
		if a, _, _ := applyKey(cl, runeMsg(r)); a != actionNone {
			t.Fatalf("编辑态下可打印字符不应触发动作，实际 %v", a)
		}
	}
	// Backspace 不应删掉开头那个 '/'
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyBackspace}); a != actionNone {
		t.Fatal("Backspace 不应触发动作")
	}
	_, _, _ = applyKey(cl, runeMsg('3'))
	a, line, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEnter})
	if a != actionSubmit || line != "/open 3" {
		t.Fatalf("Enter 应提交 /open 3，实际 action=%v line=%q", a, line)
	}
	if cl.active {
		t.Fatal("提交后必须退出编辑态")
	}
	inv, err := h.reg.Parse(line)
	if err != nil || inv.Name != "open" {
		t.Fatalf("提交的命令行应能解析为 /open，实际 inv=%+v err=%v", inv, err)
	}
}

func TestApplyKeyCancelQuitAndPaste(t *testing.T) {
	cl := &commandLine{}
	_, _, _ = applyKey(cl, runeMsg('/'))
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEsc}); a != actionCancel || cl.active {
		t.Fatal("编辑态 Esc 应取消并退出编辑态")
	}
	_, _, _ = applyKey(cl, runeMsg('/'))
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlC}); a != actionCancel {
		t.Fatal("编辑态 Ctrl-C 应取消命令输入")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlD}); a != actionQuit {
		t.Fatal("非编辑态 Ctrl-D 应退出会话")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyCtrlC}); a != actionQuit {
		t.Fatal("非编辑态 Ctrl-C 应退出会话")
	}
	if a, _, _ := applyKey(cl, input.KeyMsg{Kind: input.KeyEsc}); a != actionEscNotice {
		t.Fatal("非编辑态 Esc 应给出取消提示")
	}
	if a, _, n := applyKey(cl, input.KeyMsg{Kind: input.KeyPaste, Text: "abc"}); a != actionPasteNotice || n != 3 {
		t.Fatalf("非编辑态粘贴应只提示字节数，实际 action=%v n=%d", a, n)
	}
}

func TestFormatResultNeverEchoesUserInput(t *testing.T) {
	reg := command.Default()
	malicious := []string{
		"/x\x1b[2J",
		"/open 3\x1b]52;c;YWJj\x07 4",
		"\x1b[31mhello",
		"/help\x1b]0;pwned\x07",
	}
	for _, line := range malicious {
		inv, err := reg.Parse(line)
		out := formatResult(reg, inv, err)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") || strings.Contains(out, "hello") {
			t.Errorf("回显泄露了用户提交的原始文本: line=%q out=%q", line, out)
		}
	}

	// /help 必须由注册表生成，包含全部命令用法。
	inv, err := reg.Parse("/help")
	out := formatResult(reg, inv, err)
	for _, c := range reg.All() {
		if !strings.Contains(out, c.Usage) {
			t.Errorf("/help 输出缺少 %q", c.Usage)
		}
	}

	// 参数属于用户输入，不回显；命令名来自注册表白名单，可以出现。
	inv, err = reg.Parse("/open 3")
	out = formatResult(reg, inv, err)
	if !strings.Contains(out, "/open") || strings.Contains(out, " 3") {
		t.Fatalf("只应回显白名单命令名，不应回显参数: %q", out)
	}
}
