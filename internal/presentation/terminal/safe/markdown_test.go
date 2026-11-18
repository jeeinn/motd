package safe

import (
	"strings"
	"testing"
)

func TestAllowedLinkSchemes(t *testing.T) {
	allowed := []string{
		"[x](https://example.com/a)",
		"[x](http://example.com)",
		"[x](ssh://git@example.com/repo)",
	}
	for _, line := range allowed {
		inlines := ParseInline(line)
		found := false
		for _, in := range inlines {
			if in.Kind == KindLink && in.Href != "" {
				found = true
			}
		}
		if !found {
			t.Errorf("%q 应解析为允许的链接", line)
		}
	}
}

func TestRejectedLinkSchemesBecomePlainText(t *testing.T) {
	rejected := []string{
		"[x](javascript:alert(1))",
		"[x](JaVaScRiPt:alert(1))",
		"[x](file:///etc/passwd)",
		"[x](data:text/html;base64,PHNjcmlwdD4=)",
		"[x](//evil.example/p)",
		"[x](ftp://example.com)",
	}
	for _, line := range rejected {
		inlines := ParseInline(line)
		for _, in := range inlines {
			if in.Kind == KindLink || in.Href != "" {
				t.Errorf("%q 的链接必须降级为纯文本，实际 %+v", line, in)
			}
		}
		// 降级后原文仍在（作为纯文本，不产生 OSC 8 / 样式）
		out := RenderUser(line, Options{})
		if !strings.Contains(out, "x") {
			t.Errorf("%q 降级后不应丢内容", line)
		}
		assertOnlySGR(t, out)
	}
}

func TestWhitelistMarkdownSyntax(t *testing.T) {
	inlines := ParseInline("**粗体** 和 *斜体* 与 `代码`")
	want := []Kind{KindBold, KindText, KindItalic, KindText, KindCode}
	var got []Kind
	for _, in := range inlines {
		got = append(got, in.Kind)
	}
	if len(got) != len(want) {
		t.Fatalf("节点数不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("节点 #%d = %v，期望 %v（全部 %v）", i, got[i], want[i], got)
		}
	}

	// 不在白名单里的语法一律纯文本，不生成额外节点。
	for _, line := range []string{"# 标题", "> 引用", "- 列表", "<script>x</script>", "![img](x)"} {
		for _, in := range ParseInline(line) {
			if in.Kind != KindText {
				t.Errorf("%q 不应产生 %v 节点", line, in.Kind)
			}
		}
	}
}

func TestParseInlineKeepsUTF8Intact(t *testing.T) {
	inlines := ParseInline("中文**粗体**😀")
	var b strings.Builder
	for _, in := range inlines {
		b.WriteString(in.Text)
	}
	if got := b.String(); got != "中文粗体😀" {
		t.Fatalf("按 rune 解析不得破坏 UTF-8，实际 %q", got)
	}
}

func TestLinkParserDoesNotSwallowUnrelatedBrackets(t *testing.T) {
	// 回归 P0-23 实测缺陷：恶意样本里 "[2J␛]0;..." 以 '[' 开头但并非链接，
	// 解析器不能把它与后面 "[x](...)" 之间的 **粗体** 整段吞成纯文本。
	line := SanitizeText("标题[2J\x1b]0;pwned\x07 **粗体** [x](https://example.com)")
	foundBold := false
	for _, in := range ParseInline(line) {
		if in.Kind == KindBold && in.Text == "粗体" {
			foundBold = true
		}
	}
	if !foundBold {
		t.Fatalf("非链接的 '[' 不应吞掉后续白名单语法: %+v", ParseInline(line))
	}
}
