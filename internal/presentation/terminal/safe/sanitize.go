package safe

import (
	"strings"
	"unicode"
)

const (
	visibleESC = '␛' // U+241B：ESC 的可见控制图示
	visibleDEL = '␡' // U+2421：DEL 的可见控制图示
	tabStop    = 8
)

// wideRanges 覆盖常见 East Asian Wide/Fullwidth 与 Emoji 区间。
// P0 不引 x/text/width（依赖最小集），这张表足够让中文与 emoji 在 P0-24
// 的状态栏 / 列表里对齐；遇到未覆盖的宽字符按 1 处理，不会破坏安全性。
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // Hangul Jamo
	{0x2E80, 0x303E},   // CJK 部首、康熙、标点
	{0x3041, 0x33FF},   // 假名、CJK 符号
	{0x3400, 0x4DBF},   // CJK 扩展 A
	{0x4E00, 0x9FFF},   // CJK 统一表意文字
	{0xA000, 0xA4CF},   // 彝文
	{0xAC00, 0xD7A3},   // 韩文音节
	{0xF900, 0xFAFF},   // CJK 兼容表意文字
	{0xFE10, 0xFE6F},   // 竖排 / 全角标点
	{0xFF00, 0xFF60},   // 全角 ASCII
	{0xFFE0, 0xFFE6},   // 全角符号
	{0x1F300, 0x1FAFF}, // Emoji
	{0x20000, 0x3FFFD}, // CJK 扩展 B 及以后
}

// SanitizeText 把用户提交的任意文本净化成「不含任何终端控制序列的纯文本」。
//
// 规则：
//   - ESC / C0 / DEL 转成可见控制图示字符（U+2400 系列），其后的 CSI/OSC
//     参数字节因此退化成普通文本，不再构成控制序列；
//   - C1（0x80..0x9f）与双向文本 / 不可见格式字符一律剥离；
//   - CR / CRLF 统一成 LF，杜绝 \r 覆盖同行内容；
//   - Tab 按 8 列展开为空格（按显示宽度对齐，不是简单替换一个空格）。
//
// 这是 RenderUser 的第一段，用户内容必须经过它才能进入输出流。
func SanitizeText(s string) string {
	var b strings.Builder
	col := 0
	prevCR := false
	for _, r := range s {
		if r == '\r' {
			b.WriteByte('\n')
			col = 0
			prevCR = true
			continue
		}
		if r == '\n' {
			if prevCR {
				prevCR = false // CRLF 已在 \r 分支产出换行
				continue
			}
			b.WriteByte('\n')
			col = 0
			continue
		}
		prevCR = false

		switch {
		case r == 0x1b:
			b.WriteRune(visibleESC)
			col++
		case r == '\t':
			n := tabStop - col%tabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case r < 0x20:
			b.WriteRune(rune(0x2400 + int(r))) // ␀..␟
			col++
		case r == 0x7f:
			b.WriteRune(visibleDEL)
			col++
		case r >= 0x80 && r <= 0x9f:
			// C1 控制字符（含 \u009b 这类无需 ESC 的 CSI 起始）直接剥离。
		case isBidiControl(r), isInvisibleFormat(r):
			// RLO/LRO/PDF/isolate/LRM/RLM/ALM、BOM、软连字符、零宽字符等剥离，
			// 防止显示顺序被篡改或藏入不可见内容。
		default:
			b.WriteRune(r)
			col += DisplayWidth(r)
		}
	}
	return b.String()
}

// WrapText 按显示宽度折行（不是截断）：内容一个 rune 都不丢。
// width <= 0 时原样返回，供调用方在不知道终端宽度时安全降级。
func WrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(wrapLine(line, width))
	}
	return b.String()
}

func wrapLine(line string, width int) string {
	var b strings.Builder
	col := 0
	for _, r := range line {
		w := DisplayWidth(r)
		if col+w > width {
			b.WriteByte('\n')
			col = 0
		}
		b.WriteRune(r)
		col += w
	}
	return b.String()
}

// DisplayWidth 返回 rune 的终端显示列数：组合字符 0，宽字符 2，其余 1。
func DisplayWidth(r rune) int {
	if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200D {
		return 0 // 组合音标、ZWJ
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
}

func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E: // LRE/RLE/PDF/LRO/RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI/RLI/FSI/PDI
		return true
	case r == 0x200E || r == 0x200F || r == 0x061C: // LRM/RLM/ALM
		return true
	}
	return false
}

func isInvisibleFormat(r rune) bool {
	switch {
	case r == 0x00AD || r == 0xFEFF || r == 0x2060:
		return true
	case r >= 0x200B && r <= 0x200F:
		return true
	case r >= 0x2061 && r <= 0x2064:
		return true
	}
	return false
}
