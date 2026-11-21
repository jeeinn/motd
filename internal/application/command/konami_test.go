package command

// v0.6.0：Konami 码状态机回归。

import (
	"testing"

	"github.com/jeeinn/motd/internal/application/input"
)

func feedSequence(d *KonamiDetector, keys ...input.KeyMsg) bool {
	got := false
	for _, k := range keys {
		if d.Feed(k) {
			got = true
		}
	}
	return got
}

func konamiKeys() []input.KeyMsg {
	return []input.KeyMsg{
		{Kind: input.KeyArrowUp}, {Kind: input.KeyArrowUp},
		{Kind: input.KeyArrowDown}, {Kind: input.KeyArrowDown},
		{Kind: input.KeyArrowLeft}, {Kind: input.KeyArrowRight},
		{Kind: input.KeyArrowLeft}, {Kind: input.KeyArrowRight},
		{Kind: input.KeyRune, Rune: 'b'}, {Kind: input.KeyRune, Rune: 'a'},
	}
}

func TestKonamiExactSequence(t *testing.T) {
	d := &KonamiDetector{}
	if !feedSequence(d, konamiKeys()...) {
		t.Error("完整 Konami 码应解锁")
	}
	// 完成后自动复位，再喂一次应再次触发（幂等演示）。
	if !feedSequence(d, konamiKeys()...) {
		t.Error("复位后应可再次触发")
	}
}

func TestKonamiWrongKeyResets(t *testing.T) {
	d := &KonamiDetector{}
	keys := konamiKeys()
	// 中途插入错误键（x），应从头开始。
	wrong := append(append([]input.KeyMsg{}, keys[:4]...), input.KeyMsg{Kind: input.KeyRune, Rune: 'x'})
	wrong = append(wrong, keys[4:]...)
	if feedSequence(d, wrong...) {
		t.Error("插入错误键不应解锁")
	}
}

func TestKonamiSingleArrowNoTrigger(t *testing.T) {
	d := &KonamiDetector{}
	if feedSequence(d, input.KeyMsg{Kind: input.KeyArrowUp}) {
		t.Error("单个方向键不应解锁")
	}
}

func TestMatrixFrameSafe(t *testing.T) {
	s := MatrixFrame()
	if len(s) == 0 {
		t.Error("MatrixFrame 不应为空")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			t.Errorf("MatrixFrame 含控制字符: %q", r)
			break
		}
	}
}
