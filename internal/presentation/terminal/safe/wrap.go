package safe

// 逻辑行→物理行映射（W8，DESIGN-v090-display-i18n.md §4.3）。
//
// 终端自动折行时，一个「逻辑行」（用户的一次输入行 / 一屏文本的一行）可能占用
// 多个「物理行」（屏幕行）。本文件提供纯函数计算：给定显示宽度，返回每个逻辑行
// 占用的物理行数、折行点，以及 pending wrap 状态。
//
// pending wrap：字符恰好占满末列时，光标留在行末、不立即 row++；下一个字符到达
// 才触发折行。这决定 EL 清哪一行、CUU 落哪一行，是窄屏错位缺陷的判定点。
//
// 边界：
//   - 纯计算、零 IO、不输出任何 ANSI；与 SanitizeText / DisplayWidth 共用宽度口径；
//   - 只做物理层建模，不追求终端级真值（LNM、组合字符渲染等由人工 PTY 终验覆盖）；
//   - 宽字符整字折，剩余列不足时整字换到下一物理行，绝不拆成半字；
//   - SGR（ESC [ ... m）按 0 列计，与 frame.go 原 visibleWidth 口径一致。

import (
	"strings"
	"unicode/utf8"
)

// LineWrap 是单个逻辑行在给定宽度下的折行结果。
type LineWrap struct {
	Width   int   // 逻辑行显示宽度（SGR 不计列）
	Rows    int   // 占用的物理行数
	Breaks  []int // 折行点：每个后续物理行起始处的 rune 索引（按净化后 rune 序列计）
	Pending bool  // 末尾是否占满末列：光标留在行末，待下个字符到达才折行
}

// WrapInfo 是一段可含 \n 的文本在给定宽度下的折行结果。
type WrapInfo struct {
	Lines    []LineWrap // 逐逻辑行（按 \n 切分）
	Rows     int        // 总物理行数（各逻辑行之和）
	OverWide bool       // 是否有逻辑行宽度 > width（RenderFrame 据此退回追加模式）
}

// MeasureLine 计算单个逻辑行在 width 下的折行信息。
// width <= 0 表示宽度未知：不折行、Rows=1、Pending=false，供调用方安全降级。
func MeasureLine(line string, width int) LineWrap {
	rs := stripSGRRunes(line)
	lw := LineWrap{}
	if width <= 0 {
		for _, r := range rs {
			lw.Width += DisplayWidth(r)
		}
		lw.Rows = 1
		return lw
	}

	col := 0
	pending := false
	for i, r := range rs {
		w := DisplayWidth(r)
		if i > 0 && (pending || col+w > width) {
			// pending 触发的折行（上一字符占满末列）或宽字符剩余列不足：
			// 都从当前 rune 起另起一个物理行，且一个 rune 最多记一个折行点。
			lw.Breaks = append(lw.Breaks, i)
			col = 0
			pending = false
		}
		lw.Width += w
		col += w
		pending = col >= width
	}
	lw.Rows = 1 + len(lw.Breaks)
	lw.Pending = pending
	return lw
}

// MeasureWrap 计算整段文本（可含 \n）的折行信息。每个逻辑行独立从列 0 起算——
// 与 \r\n 的 \r 清 pending、\n 下移一行语义一致，不产生额外空物理行。
func MeasureWrap(s string, width int) WrapInfo {
	var info WrapInfo
	lines := strings.Split(s, "\n")
	// 末尾 \r\n 会产生一个空段，它不是新的逻辑行——与 RenderFrame 的
	// TrimSuffix(text, "\r\n") 口径保持一致。
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		// 去掉 CRLF 的 \r：它是回车首控制，不是可见 rune，更不能在前行 pending
		// 状态下被当作第 0 宽字符触发一次多余折行。
		lw := MeasureLine(strings.TrimSuffix(line, "\r"), width)
		info.Lines = append(info.Lines, lw)
		info.Rows += lw.Rows
		if width > 0 && lw.Width > width {
			info.OverWide = true
		}
	}
	return info
}

// OverWide 判断文本是否有逻辑行宽度超过 width，供 RenderFrame 降级判定复用，
// 消除「编辑器回显层 vs 整屏渲染」双口径。
func OverWide(s string, width int) bool {
	return MeasureWrap(s, width).OverWide
}

// stripSGRRunes 去掉服务端 SGR 序列，返回参与宽度计算的 rune 序列。
// 仅识别 ESC [ ... m；其他 ESC 字节按普通字符交 DisplayWidth 处理（得 0 列）。
// 用数值 0x1b 而非字面量，遵守「ESC 序列只能定义在 ansi.go」的 archtest 守卫。
func stripSGRRunes(s string) []rune {
	var rs []rune
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == 0x1b && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rs = append(rs, r)
		i += size
	}
	return rs
}
