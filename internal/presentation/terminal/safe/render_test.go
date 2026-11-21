package safe

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderUserOnlySGR(t *testing.T) {
	raw := "标题\x1b[2J\x1b]0;pwned\x07 **粗体** [x](javascript:alert(1))"
	out := RenderUser(raw, Options{})
	assertOnlySGR(t, out)
	if !strings.Contains(out, "␛[2J") {
		t.Fatal("用户提交的 ESC 应变成可见字符，而不是透传或丢弃")
	}
	if !strings.Contains(out, "\x1b[1m粗体\x1b[0m") {
		t.Fatalf("白名单粗体应套用受控 SGR，实际 %q", out)
	}
}

func TestRenderUserNeverGeneratesOSCHyperlink(t *testing.T) {
	out := RenderUser("[x](https://example.com)", Options{})
	assertOnlySGR(t, out)
	if !strings.Contains(out, "https://example.com") || !strings.Contains(out, "\x1b[4m") {
		t.Fatalf("允许的链接应以纯文本 URL + 下划线样式呈现，实际 %q", out)
	}
	if strings.Contains(out, "\x1b]8") {
		t.Fatal("P0 不得生成 OSC 8 超链接")
	}
}

func TestRenderSystemRejectsControlText(t *testing.T) {
	for _, bad := range []string{"\x1b[2J", "a\x00b", "\u009b31m", "x\u202ey", "a\x7fb"} {
		out, err := RenderSystem(bad, StyleBold)
		if !errors.Is(err, ErrUnsafeSystemText) {
			t.Errorf("服务端文本 %q 含控制字符时必须报错，实际 err=%v out=%q", bad, err, out)
		}
		if out != "" || strings.Contains(out, "\x1b") {
			t.Errorf("报错时不得输出任何内容，实际 %q", out)
		}
	}
}

func TestRenderSystemDoesNotShareSanitize(t *testing.T) {
	// 服务端路径保留 \t / \n 原样（不按 8 列展开），证明没有走 SanitizeText。
	out, err := RenderSystem("a\tb\nc", StyleBold)
	if err != nil {
		t.Fatal(err)
	}
	if out != "\x1b[1ma\tb\nc\x1b[0m" {
		t.Fatalf("服务端文本应原样套样式，实际 %q", out)
	}
	if got := RenderUser("a\tb", Options{}); got != "a       b" {
		t.Fatalf("用户路径应展开 Tab，实际 %q", got)
	}
}

func TestUserAndSystemPathsAreDifferent(t *testing.T) {
	userOut := RenderUser("a\x00b", Options{})
	if !strings.Contains(userOut, "␀") {
		t.Fatalf("用户路径应把 NUL 变成可见字符，实际 %q", userOut)
	}
	if _, err := RenderSystem("a\x00b", StylePlain); !errors.Is(err, ErrUnsafeSystemText) {
		t.Fatal("服务端路径应拒绝而非转义，两条路径语义必须不同")
	}
}

// P4-T1：三个配色 Style 只映射为 SGR 前景，且只由 RenderSystem 消费。
func TestSystemPaletteSGRMapping(t *testing.T) {
	for _, tc := range []struct {
		style Style
		sgr   string
	}{
		{StyleAccent, "\x1b[34m"},
		{StyleNotice, "\x1b[33m"},
		{StyleError, "\x1b[31m"},
	} {
		got, err := RenderSystem("L3", tc.style)
		if err != nil {
			t.Fatalf("style=%d 服务端文本渲染失败: %v", tc.style, err)
		}
		want := tc.sgr + "L3" + "\x1b[0m"
		if got != want {
			t.Errorf("style=%d 期望 %q，实际 %q", tc.style, want, got)
		}
	}
	// StyleCode 的 36 是行内代码语义，必须保持原样、不被新配色复用。
	if got := openANSI(StyleCode); got != "\x1b[36m" {
		t.Errorf("StyleCode 不得改为 UI 色: %q", got)
	}
}

// P4-T1：用户 Markdown 无论怎么写，都不可能选中新增的三种配色。
func TestUserMarkdownNeverSelectsSystemPalette(t *testing.T) {
	raw := "**粗体** `代码` [链接](https://example.com) \x1b[31m红\x1b[0m"
	out := RenderUser(raw, Options{})
	assertOnlySGR(t, out)
	for _, palette := range []string{"\x1b[34m", "\x1b[33m", "\x1b[31m"} {
		if strings.Contains(out, palette) {
			t.Errorf("用户内容选中了系统配色 %q: %q", palette, out)
		}
	}
}
