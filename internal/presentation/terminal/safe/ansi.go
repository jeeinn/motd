package safe

import "fmt"

// Style 是服务端允许生成的样式白名单。用户内容无法选择样式，
// 只能由白名单 Markdown 解析器映射到这里的枚举。
type Style uint8

const (
	StylePlain Style = iota
	StyleBold
	StyleDim
	StyleItalic
	StyleCode
	StyleLink
	StyleHeader
)

// 下面这张表是渲染输出中**唯一**允许出现的 ESC 序列来源（ADR-027 扩充）。
//
// 两类白名单，都只由服务端按会话状态生成，绝不来自用户内容：
//  1. SGR（\x1b[...m）：颜色与字重，不动光标、不清屏、不改标题、不写剪贴板；
//  2. 原地重绘所需的光标控制：EL \x1b[K（清到行尾）、CUU \x1b[nA（上移 n 行）。
//
// 明确禁止：ED/清屏 2J、CUP H/f、CUD B、DECSET ?1049h 备用屏、
// OSC 52 剪贴板、OSC 0/2 窗口标题，以及任何带非数字参数的 CSI。
// archtest 守卫扫描本包全部文件：ESC 字面量只能出现在本文件，
// 且必须属于上述白名单；负向注入会逐条验证禁止序列仍被拒。
const (
	resetANSI  = "\x1b[0m"
	boldANSI   = "\x1b[1m"
	dimANSI    = "\x1b[2m"
	italicANSI = "\x1b[3m"
	codeANSI   = "\x1b[36m"
	linkANSI   = "\x1b[4m"
	headerANSI = "\x1b[1;4m"
)

func openANSI(s Style) string {
	switch s {
	case StyleBold:
		return boldANSI
	case StyleDim:
		return dimANSI
	case StyleItalic:
		return italicANSI
	case StyleCode:
		return codeANSI
	case StyleLink:
		return linkANSI
	case StyleHeader:
		return headerANSI
	default:
		return ""
	}
}

func closeANSI(s Style) string {
	if s == StylePlain {
		return ""
	}
	return resetANSI
}

// ---------------------------------------------------------------------------
// 服务端光标 / 行编辑控制（ADR-027）
//
// 只放开原地重绘必需的两条。调用方必须先 \r 回到行首再 ClearToEOL，
// CursorUp 的 n 由「上一屏实际占用行数」计算，绝不接收用户输入。
// safe 包内除本文件外的任何文件（尤其 RenderUser / SanitizeText）调用
// 这两个函数都会被 archtest 判为违规。
// ---------------------------------------------------------------------------

const (
	clearToEOLANSI = "\x1b[K"   // EL：清除从光标到行尾
	cursorUpFmt    = "\x1b[%dA" // CUU 模板：光标上移 n 行
)

// ClearToEOL 清除从当前光标到行尾的旧内容。
func ClearToEOL() string { return clearToEOLANSI }

// CursorUp 把光标上移 n 行；n<=0 时返回空串，避免生成畸形序列。
func CursorUp(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(cursorUpFmt, n)
}
