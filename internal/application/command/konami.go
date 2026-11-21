package command

// Konami 码状态机（v0.6.0）：↑↑↓↓←→←→BA 解锁隐藏版块 /dev/null。
// 纯状态机，不依赖 transport；由 cmd 层在浏览态喂入归一化的按键事件。
// 只识别四个方向键 + b/a 两个字母，其它按键重置序列。

import "github.com/jeeinn/motd/internal/application/input"

type konamiKey uint8

const (
	kUp konamiKey = iota
	kDown
	kLeft
	kRight
	kB
	kA
)

// 经典 Konami 序列：上上下下左右左右BA。
var konamiSequence = [...]konamiKey{
	kUp, kUp, kDown, kDown, kLeft, kRight, kLeft, kRight, kB, kA,
}

// KonamiDetector 逐键检测；完成后 Complete() 为 true 并自动复位。
// 完全匹配才推进，任何偏差都从头开始（若按下的键正好是首键 kUp 则从 1 起）。
type KonamiDetector struct {
	step int
}

// Feed 喂入一个按键，返回本次是否刚刚完成序列。
func (d *KonamiDetector) Feed(m input.KeyMsg) bool {
	var got konamiKey
	switch {
	case m.Kind == input.KeyArrowUp:
		got = kUp
	case m.Kind == input.KeyArrowDown:
		got = kDown
	case m.Kind == input.KeyArrowLeft:
		got = kLeft
	case m.Kind == input.KeyArrowRight:
		got = kRight
	case m.Kind == input.KeyRune && m.Rune == 'b':
		got = kB
	case m.Kind == input.KeyRune && m.Rune == 'a':
		got = kA
	default:
		d.step = 0
		return false
	}
	if got != konamiSequence[d.step] {
		d.step = 0
		if got == kUp {
			d.step = 1 // 错键后若正是首键，允许立刻重新开始
		}
		return false
	}
	d.step++
	if d.step >= len(konamiSequence) {
		d.step = 0
		return true
	}
	return false
}

// Reset 复位检测器。
func (d *KonamiDetector) Reset() { d.step = 0 }
