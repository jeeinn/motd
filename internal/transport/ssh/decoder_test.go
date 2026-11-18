package ssh

import (
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/input"
)

func newDecoderWithClock() (*KeyDecoder, *time.Time) {
	now := time.Unix(0, 0).UTC()
	d := NewKeyDecoder(WithDecoderClock(func() time.Time { return now }))
	return d, &now
}

func kinds(msgs []input.KeyMsg) []input.Kind {
	out := make([]input.Kind, len(msgs))
	for i, m := range msgs {
		out[i] = m.Kind
	}
	return out
}

func eq(a, b []input.Kind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDecoderSingleKeys(t *testing.T) {
	d, _ := newDecoderWithClock()
	msgs := d.Feed([]byte("jk?hl\r\n\t\x7f\x03\x04"))
	want := []input.Kind{
		input.KeyRune, input.KeyRune, input.KeyRune, input.KeyRune, input.KeyRune,
		input.KeyEnter, input.KeyEnter, input.KeyTab, input.KeyBackspace,
		input.KeyCtrlC, input.KeyCtrlD,
	}
	if !eq(kinds(msgs), want) {
		t.Fatalf("单键解码结果 %v，期望 %v", kinds(msgs), want)
	}
	if msgs[0].Rune != 'j' || msgs[2].Rune != '?' {
		t.Fatalf("可打印字符解码错误: %+v", msgs[:3])
	}
}

func TestDecoderArrowsAndSS3(t *testing.T) {
	d, _ := newDecoderWithClock()
	msgs := d.Feed([]byte("\x1b[A\x1b[B\x1b[C\x1b[D\x1bOA"))
	want := []input.Kind{
		input.KeyArrowUp, input.KeyArrowDown, input.KeyArrowRight,
		input.KeyArrowLeft, input.KeyArrowUp,
	}
	if !eq(kinds(msgs), want) {
		t.Fatalf("方向键解码结果 %v，期望 %v", kinds(msgs), want)
	}
}

func TestDecoderEscAloneResolvedByPoll(t *testing.T) {
	d, now := newDecoderWithClock()
	if got := d.Feed([]byte{escByte}); len(got) != 0 {
		t.Fatalf("孤立 ESC 应先等待决策窗口，实际立即产出 %v", kinds(got))
	}
	if got := d.Poll(); len(got) != 0 {
		t.Fatal("窗口未到时不应判定为 Esc")
	}
	*now = now.Add(escDecisionWindow + time.Millisecond)
	got := d.Poll()
	if !eq(kinds(got), []input.Kind{input.KeyEsc}) {
		t.Fatalf("窗口到时后应产出 Esc，实际 %v", kinds(got))
	}
	if got := d.Poll(); len(got) != 0 {
		t.Fatal("Esc 不应被重复产出")
	}
}

func TestDecoderEscMergesAcrossChunks(t *testing.T) {
	d, _ := newDecoderWithClock()
	if got := d.Feed([]byte{escByte}); len(got) != 0 {
		t.Fatal("第一片 ESC 应待定")
	}
	got := d.Feed([]byte("[A"))
	if !eq(kinds(got), []input.Kind{input.KeyArrowUp}) {
		t.Fatalf("跨 chunk 的箭头序列必须合并，实际 %v", kinds(got))
	}
}

func TestDecoderEscFollowedByPlainByte(t *testing.T) {
	d, now := newDecoderWithClock()
	_ = now
	if got := d.Feed([]byte{escByte}); len(got) != 0 {
		t.Fatal("第一片 ESC 应待定")
	}
	got := d.Feed([]byte("x"))
	if !eq(kinds(got), []input.Kind{input.KeyEsc, input.KeyRune}) {
		t.Fatalf("ESC 后跟普通字节应产出 Esc + Rune，实际 %v", kinds(got))
	}
}

func TestDecoderUTF8SplitAcrossChunks(t *testing.T) {
	d, _ := newDecoderWithClock()
	if got := d.Feed([]byte{0xe4}); len(got) != 0 {
		t.Fatal("不完整的 UTF-8 首字节应待定")
	}
	got := d.Feed([]byte{0xb8, 0xad})
	if len(got) != 1 || got[0].Kind != input.KeyRune || got[0].Rune != '中' {
		t.Fatalf("跨 chunk 的中文应解码为单个 Rune，实际 %+v", got)
	}
}

func TestDecoderControlSequencesNeverBecomeActions(t *testing.T) {
	d, _ := newDecoderWithClock()
	// 清屏、颜色、OSC 改标题（含 OSC 52 剪贴板形态）都只能降级为 Unknown。
	raw := "\x1b[2J\x1b[31m\x1b]0;pwned\x07\x1b]52;c;YWJj\x07"
	msgs := d.Feed([]byte(raw))
	if len(msgs) == 0 {
		t.Fatal("控制序列应被显式消费，不能被静默丢弃")
	}
	for _, m := range msgs {
		if m.Kind != input.KeyUnknown {
			t.Fatalf("控制序列不得产生语义按键，实际 %v (%+v)", m.Kind, m)
		}
	}
}

func TestDecoderBracketedPasteIsPlainText(t *testing.T) {
	d, _ := newDecoderWithClock()
	raw := pasteStart + "hello\x1b[2Jworld\r\n" + pasteEnd
	msgs := d.Feed([]byte(raw))
	if len(msgs) != 1 || msgs[0].Kind != input.KeyPaste {
		t.Fatalf("粘贴块应产出 1 条 Paste，实际 %+v", msgs)
	}
	if msgs[0].Text != "hello\x1b[2Jworld\r\n" {
		t.Fatalf("粘贴内容必须原样保留为文本（由 P0-23 净化），实际 %q", msgs[0].Text)
	}
}

func TestDecoderPasteSplitAcrossChunks(t *testing.T) {
	d, _ := newDecoderWithClock()
	if got := d.Feed([]byte(pasteStart + "hel")); len(got) != 0 {
		t.Fatal("粘贴块未结束前不应产出")
	}
	if got := d.Feed([]byte("lo" + pasteEnd[:5])); len(got) != 0 {
		t.Fatal("结束标记被拆开时不应提前结束")
	}
	got := d.Feed([]byte(pasteEnd[5:]))
	if len(got) != 1 || got[0].Kind != input.KeyPaste || got[0].Text != "hello" {
		t.Fatalf("跨 chunk 的粘贴块应还原为 hello，实际 %+v", got)
	}
}

func TestDecoderPasteBounded(t *testing.T) {
	d, _ := newDecoderWithClock()
	raw := pasteStart + strings.Repeat("x", maxPasteBytes+100) + pasteEnd
	msgs := d.Feed([]byte(raw))
	if len(msgs) != 1 || msgs[0].Kind != input.KeyPaste {
		t.Fatalf("超长粘贴仍应产出 1 条 Paste，实际 %+v", msgs)
	}
	if len(msgs[0].Text) != maxPasteBytes {
		t.Fatalf("粘贴块必须限长 %d，实际 %d", maxPasteBytes, len(msgs[0].Text))
	}
}
