package ssh

import (
	"bytes"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/input"
)

// EscPollInterval 是读循环建议调用 KeyDecoder.Poll 的周期，
// 也用作「孤立 ESC」的决策窗口：超过该时长没有后续字节即判定为 Esc 键。
const EscPollInterval = 25 * time.Millisecond

const (
	escByte            = 0x1b
	escDecisionWindow  = EscPollInterval
	maxPasteBytes      = 8 * 1024 // 与 P0-10 正文上限一致，防粘贴块无限增长
	maxOSCBytes        = 1024
	pasteStart         = "\x1b[200~"
	pasteEnd           = "\x1b[201~"
	shortestCSIEndByte = 0x40
	longestCSIEndByte  = 0x7e
)

// KeyDecoder 把 SSH 字节流增量解码成语义按键。
//
// 两个必须处理干净的边界：
//  1. UTF-8 多字节与转义序列可能跨 Read 分块，pending 要保留未决字节；
//  2. 孤立 ESC 既可能是 Esc 键，也可能是箭头序列的开头。用「短决策窗口 +
//     下次 Feed 合并」消除歧义：序列同包到达时一次解析完；跨包时第二次
//     Feed 开头合并；用户只按 Esc 则由 Poll 在 25ms 后判定。
//
// 所有控制序列（CSI / OSC / SS3）一律降级为 KindUnknown，绝不在这里产生
// 清屏、改标题、写剪贴板之类的动作 —— 那是 P0-23 安全渲染的职责。
type KeyDecoder struct {
	mu      sync.Mutex
	pending []byte
	escAt   time.Time
	now     func() time.Time

	inPaste bool
	paste   []byte
}

type KeyDecoderOption func(*KeyDecoder)

// WithDecoderClock 注入时钟，使 Esc 判定不必真等 25ms。
func WithDecoderClock(f func() time.Time) KeyDecoderOption {
	return func(d *KeyDecoder) {
		if f != nil {
			d.now = f
		}
	}
}

func NewKeyDecoder(opts ...KeyDecoderOption) *KeyDecoder {
	d := &KeyDecoder{now: time.Now}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Feed 追加一批字节并返回本批能确定解码出的全部事件。
func (d *KeyDecoder) Feed(data []byte) []input.KeyMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	var out []input.KeyMsg
	if len(d.pending) == 1 && d.pending[0] == escByte && data[0] == '[' {
		// 上次残留的孤立 ESC 与本次开头的 '[' 合并，避免箭头序列被 TCP 拆包后拆开。
		d.pending = append(d.pending, data...)
	} else {
		if len(d.pending) == 1 && d.pending[0] == escByte {
			// 后面来的不是序列开头，说明上次那个 ESC 就是独立的 Esc 键。
			out = append(out, input.KeyMsg{Kind: input.KeyEsc, Raw: []byte{escByte}})
			d.pending = nil
		}
		d.pending = append(d.pending, data...)
	}
	d.escAt = time.Time{}
	return append(out, d.parse()...)
}

// Poll 供读循环按短周期调用，把「只按了 Esc 且没有后续字节」的情况判定出来。
func (d *KeyDecoder) Poll() []input.KeyMsg {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 1 && d.pending[0] == escByte && !d.escAt.IsZero() &&
		d.now().Sub(d.escAt) >= escDecisionWindow {
		d.pending = nil
		d.escAt = time.Time{}
		return []input.KeyMsg{{Kind: input.KeyEsc, Raw: []byte{escByte}}}
	}
	return nil
}

func (d *KeyDecoder) parse() []input.KeyMsg {
	var out []input.KeyMsg
	for {
		if len(d.pending) == 0 {
			return out
		}

		if d.inPaste {
			idx := bytes.Index(d.pending, []byte(pasteEnd))
			if idx < 0 {
				// 保留末尾可能是结束标记前缀的字节，避免跨 chunk 被拆开时丢字符。
				keep := len(pasteEnd) - 1
				if keep > len(d.pending) {
					keep = len(d.pending)
				}
				consume := len(d.pending) - keep
				appendPaste(d, d.pending[:consume])
				d.pending = d.pending[consume:]
				return out
			}
			appendPaste(d, d.pending[:idx])
			d.pending = d.pending[idx+len(pasteEnd):]
			d.inPaste = false
			out = append(out, input.KeyMsg{Kind: input.KeyPaste, Text: string(d.paste)})
			d.paste = nil
			continue
		}

		b := d.pending[0]
		switch {
		case b == escByte:
			if len(d.pending) == 1 {
				if d.escAt.IsZero() {
					d.escAt = d.now()
				}
				return out // 等更多字节，或由 Poll 判定为独立 Esc
			}
			switch d.pending[1] {
			case '[':
				if len(d.pending) >= len(pasteStart) && string(d.pending[:len(pasteStart)]) == pasteStart {
					d.inPaste = true
					d.paste = nil
					d.pending = d.pending[len(pasteStart):]
					continue
				}
				msg, n, ok := parseCSI(d.pending)
				if !ok {
					return out // CSI 不完整
				}
				out = append(out, msg)
				d.pending = d.pending[n:]
				continue
			case 'O':
				if len(d.pending) < 3 {
					return out
				}
				out = append(out, parseSS3(d.pending[:3]))
				d.pending = d.pending[3:]
				continue
			case ']': // OSC：窗口标题 / 剪贴板等，整段吞掉
				n, ok := consumeOSC(d.pending)
				if !ok {
					return out
				}
				out = append(out, input.KeyMsg{Kind: input.KeyUnknown, Raw: append([]byte(nil), d.pending[:n]...)})
				d.pending = d.pending[n:]
				continue
			default:
				out = append(out, input.KeyMsg{Kind: input.KeyEsc, Raw: []byte{escByte}})
				d.pending = d.pending[1:]
				continue
			}

		case b == '\r' || b == '\n':
			out = append(out, input.KeyMsg{Kind: input.KeyEnter, Raw: []byte{b}})
			d.pending = d.pending[1:]
		case b == 0x7f || b == 0x08:
			out = append(out, input.KeyMsg{Kind: input.KeyBackspace, Raw: []byte{b}})
			d.pending = d.pending[1:]
		case b == '\t':
			out = append(out, input.KeyMsg{Kind: input.KeyTab, Raw: []byte{b}})
			d.pending = d.pending[1:]
		case b < 0x20:
			msg := input.KeyMsg{Kind: input.KeyUnknown, Raw: []byte{b}}
			switch b {
			case 0x03:
				msg.Kind = input.KeyCtrlC
			case 0x04:
				msg.Kind = input.KeyCtrlD
			}
			out = append(out, msg)
			d.pending = d.pending[1:]
		default:
			r, size := utf8.DecodeRune(d.pending)
			if r == utf8.RuneError && size == 1 {
				if !utf8.FullRune(d.pending) && len(d.pending) < utf8.UTFMax {
					return out // 多字节字符未收齐
				}
				d.pending = d.pending[1:] // 非法字节，丢弃但不透传
				continue
			}
			out = append(out, input.KeyMsg{
				Kind: input.KeyRune, Rune: r,
				Raw: append([]byte(nil), d.pending[:size]...),
			})
			d.pending = d.pending[size:]
		}
	}
}

func appendPaste(d *KeyDecoder, b []byte) {
	room := maxPasteBytes - len(d.paste)
	if room <= 0 {
		return
	}
	if len(b) > room {
		b = b[:room]
	}
	d.paste = append(d.paste, b...)
}

// parseCSI 消费一条完整的 CSI 序列；终结字节范围 0x40..0x7e。
// 只有四个方向键映射为语义按键，其余（清屏、颜色、光标定位…）全部 Unknown。
func parseCSI(p []byte) (input.KeyMsg, int, bool) {
	for i := 2; i < len(p); i++ {
		if p[i] < shortestCSIEndByte || p[i] > longestCSIEndByte {
			continue
		}
		seq := p[:i+1]
		msg := input.KeyMsg{Kind: input.KeyUnknown, Raw: append([]byte(nil), seq...)}
		switch string(seq) {
		case "\x1b[A":
			msg.Kind = input.KeyArrowUp
		case "\x1b[B":
			msg.Kind = input.KeyArrowDown
		case "\x1b[C":
			msg.Kind = input.KeyArrowRight
		case "\x1b[D":
			msg.Kind = input.KeyArrowLeft
		}
		return msg, i + 1, true
	}
	return input.KeyMsg{}, 0, false
}

func parseSS3(p []byte) input.KeyMsg {
	msg := input.KeyMsg{Kind: input.KeyUnknown, Raw: append([]byte(nil), p...)}
	switch p[2] {
	case 'A':
		msg.Kind = input.KeyArrowUp
	case 'B':
		msg.Kind = input.KeyArrowDown
	case 'C':
		msg.Kind = input.KeyArrowRight
	case 'D':
		msg.Kind = input.KeyArrowLeft
	}
	return msg
}

// consumeOSC 找到 OSC 的 BEL 或 ST 终止符，返回应消费的总长度。
// 没有终止符时先等待；超过上限则整段丢弃，避免恶意输入把 pending 撑爆。
func consumeOSC(p []byte) (int, bool) {
	for i := 2; i < len(p); i++ {
		if p[i] == 0x07 {
			return i + 1, true
		}
		if p[i] == escByte && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2, true
		}
		if i >= maxOSCBytes {
			return i + 1, true
		}
	}
	return 0, false
}
