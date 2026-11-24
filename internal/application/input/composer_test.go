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

func TestComposerBackspaceDeletesWithinCurrentLine(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "abc")
	feed(t, c, KeyMsg{Kind: KeyEnter})
	runesOf(c, "xy")
	op := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if op.Kind != OpBackspace || !op.Deleted || op.MergedLines {
		t.Fatalf("本行非空时退格应只删本行末字符: %+v", op)
	}
	if got, want := c.Text(), "abc\nx"; got != want {
		t.Fatalf("本行退格应删掉本行末字符: got=%q want=%q", got, want)
	}
}

func TestComposerBackspaceMergesEmptyLineIntoPrevious(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "abc")
	feed(t, c, KeyMsg{Kind: KeyEnter}) // 新行是空行
	op := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if op.Kind != OpBackspace || !op.MergedLines || op.Deleted {
		t.Fatalf("空行退格应报告跨行合并: %+v", op)
	}
	if op.MergedText != "abc" {
		t.Fatalf("合并后的上一行文本不符: got=%q want=%q", op.MergedText, "abc")
	}
	if got := c.Lines(); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("跨行合并后缓冲应只剩上一行: %q", got)
	}
	if got, want := c.Text(), "abc"; got != want {
		t.Fatalf("跨行合并后正文不符: got=%q want=%q", got, want)
	}
	// 合并后继续输入应接在上一行末尾（即两行拼接处）。
	runesOf(c, "d")
	if got, want := c.Text(), "abcd"; got != want {
		t.Fatalf("合并后输入应接在末尾: got=%q want=%q", got, want)
	}
}

func TestComposerBackspaceFirstEmptyLineNoAction(t *testing.T) {
	c := NewComposer()
	c.Start()
	op := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if op.Kind != OpBackspace || op.Deleted || op.MergedLines || op.MergedText != "" {
		t.Fatalf("首行空行退格应无动作: %+v", op)
	}
	if got := c.Lines(); len(got) != 1 || got[0] != "" {
		t.Fatalf("首行空退格不得改动缓冲: %q", got)
	}
}

func TestComposerBackspaceConsecutiveEmptyLinesMergeOneByOne(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "a")
	feed(t, c, KeyMsg{Kind: KeyEnter}) // 第 2 行空
	feed(t, c, KeyMsg{Kind: KeyEnter}) // 第 3 行空
	if got := len(c.Lines()); got != 3 {
		t.Fatalf("应有 3 行，实际 %d", got)
	}

	op1 := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if !op1.MergedLines || op1.MergedText != "" {
		t.Fatalf("第一次应并入空的上一行: %+v", op1)
	}
	if got := len(c.Lines()); got != 2 {
		t.Fatalf("第一次合并后应剩 2 行，实际 %d", got)
	}

	op2 := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if !op2.MergedLines || op2.MergedText != "a" {
		t.Fatalf("第二次应并入含 'a' 的上一行: %+v", op2)
	}
	if got, want := c.Text(), "a"; got != want {
		t.Fatalf("连续合并后正文不符: got=%q want=%q", got, want)
	}

	op3 := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if !op3.Deleted || op3.MergedLines {
		t.Fatalf("并入首行后应退回单行删字: %+v", op3)
	}
	if got := c.Text(); got != "" {
		t.Fatalf("首行字符应被删掉: %q", got)
	}

	op4 := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if op4.Deleted || op4.MergedLines {
		t.Fatalf("首行空退格应无动作: %+v", op4)
	}
}

func TestComposerBackspaceMergeWideChars(t *testing.T) {
	c := NewComposer()
	c.Start()
	runesOf(c, "中文")
	feed(t, c, KeyMsg{Kind: KeyEnter})
	op := feed(t, c, KeyMsg{Kind: KeyBackspace})
	if !op.MergedLines || op.MergedText != "中文" {
		t.Fatalf("宽字符行合并文本不符: %+v", op)
	}
	if got, want := c.Text(), "中文"; got != want {
		t.Fatalf("宽字符合并后正文不符: got=%q want=%q", got, want)
	}
	runesOf(c, "字")
	if got, want := c.Text(), "中文字"; got != want {
		t.Fatalf("宽字符合并后输入应接在末尾: got=%q want=%q", got, want)
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

func TestComposerLoadRestoresMultilineAndNormalizesCRLF(t *testing.T) {
	c := NewComposer()
	if err := c.Load("行1\r\n行2\r行3"); err != nil {
		t.Fatalf("Load 意外失败: %v", err)
	}
	if !c.Active() {
		t.Fatal("Load 成功后应激活")
	}
	if got, want := c.Text(), "行1\n行2\n行3"; got != want {
		t.Fatalf("CRLF/CR 应归一化为 LF: got=%q want=%q", got, want)
	}
	if got, want := len(c.Lines()), 3; got != want {
		t.Fatalf("应有 3 行，实际 %d", got)
	}
}

func TestComposerLoadEmptyGivesSingleEmptyLine(t *testing.T) {
	c := NewComposer()
	if err := c.Load(""); err != nil {
		t.Fatalf("Load 空文本意外失败: %v", err)
	}
	if !c.Active() {
		t.Fatal("Load 空文本后应激活")
	}
	if got, want := c.Text(), ""; got != want {
		t.Fatalf("空文本应为空正文: got=%q", got)
	}
	if got, want := len(c.Lines()), 1; got != want {
		t.Fatalf("空文本应为单行空缓冲，实际 %d 行", got)
	}
}

func TestComposerLoadSentinelLineStaysInBuffer(t *testing.T) {
	// 防回归：整块 Load 不得把独立的 "." 行当成提交哨兵。
	c := NewComposer()
	if err := c.Load("第一行\n.\n第三行"); err != nil {
		t.Fatalf("Load 意外失败: %v", err)
	}
	if !c.Active() {
		t.Fatal("含独立 '.' 行的 Load 不得触发提交")
	}
	if got, want := c.Text(), "第一行\n.\n第三行"; got != want {
		t.Fatalf("哨兵行应整块留在正文: got=%q want=%q", got, want)
	}
	// Load 后继续 Feed 仍正常（缓冲可续输）。
	if _, err := c.Feed(KeyMsg{Kind: KeyRune, Rune: 'x'}); err != nil {
		t.Fatalf("Load 后 Feed 意外失败: %v", err)
	}
	if got := c.Text(); !strings.HasSuffix(got, "x") {
		t.Fatalf("Load 后续输应接在末尾: %q", got)
	}
}

func TestComposerLoadTooManyLinesRejectedKeepsState(t *testing.T) {
	c := NewComposer(WithMaxLines(2))
	if err := c.Load("a\nb\nc"); !errors.Is(err, ErrTooManyLines) {
		t.Fatalf("超行数上限应报 ErrTooManyLines，实际 %v", err)
	}
	if c.Active() {
		t.Fatal("超限 Load 不得激活")
	}
	if c.lines != nil {
		t.Fatalf("超限 Load 不得改动缓冲: %q", c.Text())
	}
}

func TestComposerLoadTooManyBytesRejectedKeepsState(t *testing.T) {
	c := NewComposer(WithMaxBytes(4))
	if err := c.Load("ab\ncd"); !errors.Is(err, ErrTooManyBytes) {
		t.Fatalf("超字节上限应报 ErrTooManyBytes，实际 %v", err)
	}
	if c.Active() {
		t.Fatal("超限 Load 不得激活")
	}
	if c.lines != nil {
		t.Fatalf("超限 Load 不得改动缓冲: %q", c.Text())
	}
}
