package input

import (
	"errors"
	"strings"
	"testing"
)

func feed(t *testing.T, c *Composer, m KeyMsg) ComposerOp {
	t.Helper()
	op, err := c.Feed(m)
	if err != nil {
		t.Fatalf("Feed(%v) 意外失败: %v", m.Kind, err)
	}
	return op
}

func runesOf(c *Composer, s string) {
	for _, r := range s {
		_, _ = c.Feed(KeyMsg{Kind: KeyRune, Rune: r})
	}
}

func TestComposerEnterInsertsNewlineNotSubmit(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "第一行")
	if op := feed(t, c, KeyMsg{Kind: KeyEnter}); op.Kind != OpNewline {
		t.Fatalf("正文 Enter 应换行，实际 %v", op.Kind)
	}
	runesOf(c, "第二行")
	if !c.Active() {
		t.Fatal("正文 Enter 不得提交")
	}
	if got, want := c.Text(), "第一行\n第二行"; got != want {
		t.Fatalf("正文应保留换行: got=%q want=%q", got, want)
	}
	if got, want := len(c.Lines()), 2; got != want {
		t.Fatalf("应有 2 行，实际 %d", got)
	}
}

func TestComposerSentinelSubmits(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "正文一行")
	feed(t, c, KeyMsg{Kind: KeyEnter})
	runesOf(c, SubmitSentinel)
	op := feed(t, c, KeyMsg{Kind: KeyEnter})
	if op.Kind != OpSubmit {
		t.Fatalf("哨兵行 + Enter 应提交，实际 %v", op.Kind)
	}
	if c.Active() {
		t.Fatal("提交后应退出编辑态")
	}
	if got, want := c.Text(), "正文一行"; got != want {
		t.Fatalf("哨兵行不得进正文: got=%q want=%q", got, want)
	}
	if _, err := c.Feed(KeyMsg{Kind: KeyRune, Rune: 'x'}); !errors.Is(err, ErrComposerInactive) {
		t.Fatalf("提交后 Feed 应报未激活，实际 %v", err)
	}
}

func TestComposerCtrlDSubmitsImmediately(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "Ctrl-D 提交")
	op := feed(t, c, KeyMsg{Kind: KeyCtrlD})
	if op.Kind != OpSubmit {
		t.Fatalf("Ctrl-D 应提交，实际 %v", op.Kind)
	}
	if got, want := c.Text(), "Ctrl-D 提交"; got != want {
		t.Fatalf("提交的正文不对: got=%q want=%q", got, want)
	}
}

func TestComposerPasteWithNewlinesDoesNotSubmit(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "开头")
	paste := "a\nb\n.\nc\r\nd"
	op := feed(t, c, KeyMsg{Kind: KeyPaste, Text: paste})
	if op.Kind != OpPaste {
		t.Fatalf("粘贴应产出 OpPaste，实际 %v", op.Kind)
	}
	if !c.Active() {
		t.Fatal("粘贴内部换行 / 哨兵行不得触发提交")
	}
	want := "开头a\nb\n.\nc\nd"
	if got := c.Text(); got != want {
		t.Fatalf("粘贴块应整块进正文（含内部的 '.' 行）: got=%q want=%q", got, want)
	}
}

func TestComposerBackspaceDoesNotCrossLines(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "abc")
	feed(t, c, KeyMsg{Kind: KeyEnter})
	// 新行是空行，退格不得吞掉上一行的 'c'。
	op := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if op.Deleted {
		t.Fatal("空行退格应报告未删除")
	}
	if got := c.Lines(); len(got) != 2 || got[0] != "abc" || got[1] != "" {
		t.Fatalf("退格跨行破坏了缓冲: %q", got)
	}
	runesOf(c, "xy")
	feed(t, c, KeyMsg{Kind: KeyBackspace})
	if got := c.Text(); got != "abc\nx" {
		t.Fatalf("当前行退格应删掉本行末字符: %q", got)
	}
}

func TestComposerMaxLines(t *testing.T) {
	c := NewComposer(WithMaxLines(2))
	c.Start()
	runesOf(c, "a")
	feed(t, c, KeyMsg{Kind: KeyEnter}) // 2 行
	runesOf(c, "b")
	if _, err := c.Feed(KeyMsg{Kind: KeyEnter}); !errors.Is(err, ErrTooManyLines) {
		t.Fatalf("超过行数上限应报错，实际 %v", err)
	}
	if got := c.Text(); got != "a\nb" {
		t.Fatalf("超限后缓冲不得改变: %q", got)
	}
	if !c.Active() {
		t.Fatal("超限不应提交")
	}
}

func TestComposerMaxBytes(t *testing.T) {
	c := NewComposer(WithMaxBytes(4))
	c.Start()
	runesOf(c, "abcd")
	if _, err := c.Feed(KeyMsg{Kind: KeyRune, Rune: 'e'}); !errors.Is(err, ErrTooManyBytes) {
		t.Fatalf("超过字节上限应报错，实际 %v", err)
	}
	if got := c.Text(); got != "abcd" {
		t.Fatalf("超限后缓冲不得改变: %q", got)
	}
	// 行间 LF 也计入字节数：用 paste 一次性构造 5 字节，应被整体拒绝且状态不变。
	c2 := NewComposer(WithMaxBytes(4))
	c2.Start()
	_, err := c2.Feed(KeyMsg{Kind: KeyPaste, Text: "ab\ncd"})
	if !errors.Is(err, ErrTooManyBytes) {
		t.Fatalf("粘贴超字节上限应报错，实际 %v", err)
	}
	if got := c2.Text(); got != "" {
		t.Fatalf("超限粘贴整体拒绝，缓冲应为空: %q", got)
	}
}

func TestComposerMultibyteBytesCountedNotRunes(t *testing.T) {
	c := NewComposer(WithMaxBytes(5))
	c.Start()
	// 三个中文 = 9 字节，超过 5 字节上限，即使只有 3 个 rune。
	if _, err := c.Feed(KeyMsg{Kind: KeyPaste, Text: strings.Repeat("中", 3)}); !errors.Is(err, ErrTooManyBytes) {
		t.Fatalf("多字节字符必须按字节计数，实际 %v", err)
	}
}

func TestComposerLinesReturnsDefensiveCopy(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "abc")
	lines := c.Lines()
	lines[0] = "篡改"
	if got := c.Text(); got != "abc" {
		t.Fatalf("Lines() 副本被改动影响了内部状态: %q", got)
	}
}
