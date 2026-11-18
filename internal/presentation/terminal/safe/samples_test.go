package safe

import (
	"strings"
	"testing"
)

// maliciousSamples 是 P0-23 的固化恶意样本集（20 条，>= 12 条验收要求）。
// 新增渲染分支时必须往这里补样本，不允许只靠人工看终端。
var maliciousSamples = []struct {
	name  string
	input string
}{
	{"CSI 清屏", "\x1b[2J"},
	{"光标归位 + 清屏", "\x1b[H\x1b[2J"},
	{"颜色注入", "hello \x1b[31mred\x1b[0m"},
	{"窗口标题 OSC 0", "\x1b]0;pwned\x07"},
	{"OSC 52 剪贴板", "\x1b]52;c;YWJj\x07"},
	{"备用屏幕切换", "\x1b[?1049h"},
	{"改终端尺寸", "\x1b[8;20t"},
	{"CSI 光标移动", "\x1b[10;20H\x1b[K"},
	{"回车覆盖", "hello\rPWNED"},
	{"CRLF + 清屏", "safe\r\n\x1b[2J"},
	{"C1 CSI（无 ESC）", "\u009b31m"},
	{"双向文本 RLO", "abc\u202ePWNED\u202cdef"},
	{"javascript 链接", "[click](javascript:alert(1))"},
	{"file 链接", "[x](file:///etc/passwd)"},
	{"data 链接", "[x](data:text/html;base64,PHNjcmlwdD4=)"},
	{"大小写混淆协议", "[x](JaVaScRiPt:alert(1))"},
	{"协议相对 URL", "[x](//evil.example/p)"},
	{"粘贴标记", "\x1b[200~payload\x1b[201~"},
	{"NUL 与 DEL", "a\x00b\x7fc"},
	{"制表符与宽字符", "中\t文😀"},
	{"Markdown 内嵌 ESC", "**\x1b]52;c;x\x07**"},
	{"超长单行", strings.Repeat("A", 1500)},
}

// 净化后的文本不得残留任何能驱动终端的控制字符。
func assertNoRawControl(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		switch {
		case r == 0x1b || r == '\r' || r == 0x7f:
			t.Errorf("净化后仍含控制字符 U+%04X: %q", r, s)
		case r < 0x20 && r != '\n':
			t.Errorf("净化后仍含 C0 控制字符 U+%04X: %q", r, s)
		case r >= 0x80 && r <= 0x9f:
			t.Errorf("净化后仍含 C1 控制字符 U+%04X: %q", r, s)
		case isBidiControl(r) || isInvisibleFormat(r):
			t.Errorf("净化后仍含双向文本/不可见格式字符 U+%04X: %q", r, s)
		}
	}
}

// 渲染输出里的每个 ESC 序列都必须是纯 SGR（\x1b[数字;...m）。
// 这一条同时挡住清屏、光标定位、窗口标题、OSC 52 剪贴板与 OSC 8 超链接。
func assertOnlySGR(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			i++
			continue
		}
		if i+1 >= len(s) || s[i+1] != '[' {
			t.Errorf("输出含非 CSI 的 ESC 序列（OSC/SS3）@%d: %q", i, s[i:min(i+8, len(s))])
			i++
			continue
		}
		j := i + 2
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
			j++
		}
		if j >= len(s) || s[j] != 'm' {
			t.Errorf("输出含非 SGR 的 CSI 序列（清屏/光标/标题等）@%d: %q", i, s[i:min(i+8, len(s))])
		}
		i = j + 1
	}
	if strings.Contains(s, "\r") {
		t.Error("渲染输出不得含 CR（会覆盖同行内容）")
	}
}

func TestMaliciousSampleSet(t *testing.T) {
	if len(maliciousSamples) < 12 {
		t.Fatalf("恶意样本集至少 12 条，实际 %d", len(maliciousSamples))
	}
	for _, s := range maliciousSamples {
		t.Run(s.name, func(t *testing.T) {
			clean := SanitizeText(s.input)
			assertNoRawControl(t, clean)

			out := RenderUser(s.input, Options{Width: 40})
			assertOnlySGR(t, out)
		})
	}
}

func TestCarriageReturnDoesNotOverwrite(t *testing.T) {
	clean := SanitizeText("hello\rPWNED")
	if !strings.Contains(clean, "hello\nPWNED") {
		t.Fatalf("CR 应转成换行，原文两段都要保留: %q", clean)
	}
	if strings.Contains(clean, "\r") {
		t.Fatal("净化后不得残留 CR")
	}
	crlf := SanitizeText("a\r\nb")
	if crlf != "a\nb" {
		t.Fatalf("CRLF 应归一为单个 LF，实际 %q", crlf)
	}
}

func TestLongLineIsWrappedNotTruncated(t *testing.T) {
	raw := strings.Repeat("A", 1500)
	wrapped := WrapText(raw, 40)
	if strings.Count(wrapped, "A") != 1500 {
		t.Fatal("折行不得丢失任何内容（截断不可接受）")
	}
	for _, line := range strings.Split(wrapped, "\n") {
		if len(line) > 40 {
			t.Fatalf("折行后单行长度 %d 超过 40", len(line))
		}
	}
	if out := RenderUser(raw, Options{Width: 40}); !strings.Contains(out, "\n") || strings.Count(out, "A") != 1500 {
		t.Fatal("RenderUser 的 Width 必须真正生效且不截断内容")
	}
}
