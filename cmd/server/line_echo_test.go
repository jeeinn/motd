package main

// ADR-026 回归：正常输入必须有回显，但回显绝不能成为终端注入旁路。
// 重点验证「用户字节走净化路径」与「编辑控制由服务端生成」两条边界。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/input"
)

func TestLineEchoRuneAndWideBackspace(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.rune('A')
	e.rune('中')
	if got := buf.String(); got != "A中" {
		t.Fatalf("普通字符与中文都应回显，实际 %q", got)
	}

	e.backspace(true) // 中文占 2 列，必须擦两列
	if got := buf.String(); !strings.HasSuffix(got, "\b \b\b \b") {
		t.Fatalf("宽字符退格应按 2 列擦除，实际 %q", got)
	}
	e.backspace(true) // ASCII 占 1 列
	if got := buf.String(); !strings.HasSuffix(got, "\b \b") {
		t.Fatalf("ASCII 退格应按 1 列擦除，实际 %q", got)
	}

	before := buf.String()
	e.backspace(true) // 栈已空，不得再输出任何东西
	if buf.String() != before {
		t.Fatal("空输入行继续退格不得产生输出")
	}
}

func TestLineEchoNeverWritesRawControlBytes(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.rune(0x1b)     // ESC：应转成可见控制图示
	e.rune('\r')     // CR 覆盖：应转 LF
	e.rune(0x9b)     // C1 CSI 起始：应剥离
	e.rune('\u202e') // RLO 双向文本：应剥离
	got := buf.String()

	if strings.Contains(got, "\x1b") {
		t.Fatalf("回显泄露了原始 ESC: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("回显泄露了 CR（同行覆盖攻击面）: %q", got)
	}
	if !strings.Contains(got, "␛") {
		t.Fatalf("ESC 应转成可见控制图示，实际 %q", got)
	}
	if strings.Contains(got, "\u202e") {
		t.Fatalf("双向文本控制字符必须剥离，实际 %q", got)
	}
}

func TestLineEchoSecretMasksInput(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.setSecret(true)
	e.paste("p@ssw0rd")
	if got := buf.String(); got != "********" {
		t.Fatalf("敏感字段只应回显掩码，实际 %q", got)
	}
	e.backspace(true)
	if got := buf.String(); !strings.HasSuffix(got, "\b \b") {
		t.Fatalf("敏感字段退格仍应擦除掩码，实际 %q", got)
	}
}

func TestLineEchoPasteAndNewlineReset(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.paste("你好 world")
	if !strings.Contains(buf.String(), "你好 world") {
		t.Fatalf("粘贴块应完整回显，实际 %q", buf.String())
	}
	e.newline()
	if !strings.HasSuffix(buf.String(), "\r\n") {
		t.Fatal("提交应换行")
	}
	if len(e.cols) != 0 {
		t.Fatal("换行后列宽栈必须清空")
	}
	before := buf.String()
	e.backspace(true)
	if buf.String() != before {
		t.Fatal("换行后退格不得擦到上一行内容")
	}
}

// 跨行退格重绘只允许 ADR-027 白名单：CUU(\x1b[nA) + EL(\x1b[K) + SGR，
// 绝不发 CUP(H/f)/CUD(B)/ED/DECSET；重绘文本逐 rune 经 SanitizeText。
func TestLineEchoMergeBackspaceWhitelistedOnly(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.mergeBackspace("合并后")
	got := buf.String()

	if !strings.Contains(got, "\x1b[1A") {
		t.Fatalf("跨行合并应先 CUU 上移一行: %q", got)
	}
	if !strings.Contains(got, "\x1b[K") {
		t.Fatalf("跨行合并应发 EL 清行: %q", got)
	}
	if !strings.Contains(got, "合并后") {
		t.Fatalf("重绘文本应是合并后的内容: %q", got)
	}
	cmdOnlyWhitelistedANSI(t, got)
	for _, banned := range []string{"\x1b[H", "\x1b[f", "\x1b[B", "\x1b[2J", "\x1b[J", "\x1b[?1049h"} {
		if strings.Contains(got, banned) {
			t.Fatalf("跨行合并不得发出禁用序列 %q: %q", banned, got)
		}
	}
	if len(e.cols) != 3 || e.cols[0] != 2 || e.cols[1] != 2 || e.cols[2] != 2 {
		t.Fatalf("重绘后列宽栈应记录 3 个汉字各 2 列: %v", e.cols)
	}
}

func TestLineEchoMergeBackspaceSanitizesMergedText(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.mergeBackspace("a\x1b[2Jb") // 合并文本里夹带清屏注入
	got := buf.String()
	if !strings.Contains(got, "␛") {
		t.Fatalf("合并文本里的 ESC 应转可见控制图示: %q", got)
	}
	cmdOnlyWhitelistedANSI(t, got) // 注入的 2J 不得变成真正的清屏序列
}

func TestLineEchoMergeBackspaceWideCharColumns(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)
	e.mergeBackspace("中文")
	if len(e.cols) != 2 || e.cols[0] != 2 || e.cols[1] != 2 {
		t.Fatalf("宽字符合并后列宽栈不符: %v", e.cols)
	}
	before := buf.String()
	e.backspace(true)
	if got := buf.String(); !strings.HasSuffix(got, before+"\b \b\b \b") {
		t.Fatalf("合并后续接的退格应按 2 列擦除: %q", got)
	}
}

func TestEchoCommandKeyGuardsPreloadedSlash(t *testing.T) {
	var buf bytes.Buffer
	e := newLineEcho(&buf)

	// 进入命令态：'/' 由服务端预置，必须在提示语之后回显。
	echoCommandKey(e, input.KeyMsg{Kind: input.KeyRune, Rune: '/'}, false, 0, 1, actionCommandStart)
	if buf.Len() != 0 {
		t.Fatal("actionCommandStart 的回显由 handle 在提示之后单独触发")
	}
	e.preloadedSlash()
	if got := buf.String(); got != "/" {
		t.Fatalf("应回显预置的 '/'，实际 %q", got)
	}
	echoCommandKey(e, input.KeyMsg{Kind: input.KeyRune, Rune: 'l'}, true, 1, 2, actionNone)
	if !strings.HasSuffix(buf.String(), "l") {
		t.Fatalf("编辑态应回显输入字符，实际 %q", buf.String())
	}
	echoCommandKey(e, input.KeyMsg{Kind: input.KeyBackspace}, true, 2, 1, actionNone)
	if !strings.HasSuffix(buf.String(), "\b \b") {
		t.Fatalf("退格应擦除刚回显的字符，实际 %q", buf.String())
	}

	// applyKey 保护开头的 '/'：长度不变时不得擦除。
	before := buf.String()
	echoCommandKey(e, input.KeyMsg{Kind: input.KeyBackspace}, true, 1, 1, actionNone)
	if buf.String() != before {
		t.Fatal("未删除字符时不得擦除回显")
	}

	echoCommandKey(e, input.KeyMsg{Kind: input.KeyEnter}, true, 1, 0, actionSubmit)
	if !strings.HasSuffix(buf.String(), "\r\n") {
		t.Fatal("Enter 提交应换行")
	}
}
