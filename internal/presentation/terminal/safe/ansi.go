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
	// P4-T1：三种配色 Style 只服务于服务端固定文案（标签/前缀），
	// 用户文本主体永不染色。用户内容路径的 styleFor() 不得返回它们，
	// archtest 的 color_test.go 会静态守卫这一约束。
	StyleAccent // 深蓝：状态栏 L3/L4 等级标签
	StyleNotice // 琥珀：[提示] 前缀（Warning）；公告/置顶字段到位后再消费
	StyleError  // 暗红：仅 [错误] 前缀，消息正文不上色
)

// 下面这张表是渲染输出中**唯一**允许出现的 ESC 序列来源（ADR-027，ADR-072 扩充）。
//
// 三类白名单，都只由服务端按会话状态生成，绝不来自用户内容：
//  1. SGR（\x1b[...m）：颜色与字重，不动光标、不清屏、不改标题、不写剪贴板；
//  2. 原地重绘/行编辑所需的光标控制：EL \x1b[K（清到行尾）、CUU \x1b[nA（上移 n 行）；
//  3. 整屏干净重绘（ADR-072）：**无参** CUP home \x1b[H（光标回左上角）、
//     **无参** ED 2 \x1b[2J（清当前屏，不清 scrollback）。
//
// 明确禁止：CUD B、DECSET ?1049h / ?1049l 备用屏、OSC 52 剪贴板、
// OSC 0/2 窗口标题、**带参** CUP（\x1b[r;cH）、**带参** EL、
// 除 2J 外的其他 ED（1J/3J/\x1b[J 无参 ED），以及任何带非数字参数的 CSI。
//
// 新序列零参数是关键：\x1b[H 与 \x1b[2J 的参数是字面常量，不接收任何运行时
// 输入，因此不存在「用户数据拼进 CSI 参数」的注入面；带参 CUP/CUD/ED 一律不放开。
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
	// P4-T1 新增的三种 SGR 前景色。只在此文件定义；
	// StyleCode 的 36 是行内代码语义，保持原样，不复用为 UI 色。
	accentANSI = "\x1b[34m" // 深蓝
	noticeANSI = "\x1b[33m" // 琥珀
	errorANSI  = "\x1b[31m" // 暗红
	// v0.9.2 / ADR-076：/matrix 雨幕的两档绿。基础 SGR（不含 256 色与真彩），
	// 与 ROADMAP §4「256 色 / 真彩明确不做」的决策一致——tmux/screen 无需任何
	// 配置即可正确显示，不引入兼容性负债。
	matrixHeadANSI = "\x1b[1;92m" // 亮绿 + 粗体：雨头
	matrixTailANSI = "\x1b[2;32m" // 暗绿：拖尾
)

// MatrixHeadInk 返回雨头的 SGR（亮绿 + 粗体）。v0.9.2 / ADR-076。
//
// 独立 accessor 而非新增 Style 枚举：Style 是给用户内容映射用的选色通道，
// 彩蛋色阶是服务端固定素材，不该占用那条通道（archtest/color_test.go 守门）。
func MatrixHeadInk() string { return matrixHeadANSI }

// MatrixTailInk 返回拖尾的 SGR（暗绿）。v0.9.2 / ADR-076。
func MatrixTailInk() string { return matrixTailANSI }

// ResetInk 返回显式复位序列（\x1b[0m）。
//
// SGR 状态在整帧内跨行保持（跳变式渲染），而 ED 2（\x1b[2J）**不重置 SGR**，
// 因此退出整屏动画时必须显式发一次，否则绿色会漏到随后重绘的论坛页面上。
func ResetInk() string { return resetANSI }

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
	case StyleAccent:
		return accentANSI
	case StyleNotice:
		return noticeANSI
	case StyleError:
		return errorANSI
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
// 服务端光标 / 行编辑 / 整屏重绘控制（ADR-027、ADR-072）
//
// 原地重绘：调用方必须先 \r 回到行首再 ClearToEOL，CursorUp 的 n 由
// 「上一屏实际占用行数」计算；整屏重绘：CursorHome + ClearScreen 均为零参数。
// 四条的控制参数全部来自服务端会话状态，绝不接收用户输入。
// safe 包内除本文件外的任何文件（尤其 RenderUser / SanitizeText）调用
// 这些函数都会被 archtest 判为违规。
// ---------------------------------------------------------------------------

const (
	clearToEOLANSI = "\x1b[K"   // EL：清除从光标到行尾
	cursorUpFmt    = "\x1b[%dA" // CUU 模板：光标上移 n 行
	// ADR-072 整屏干净重绘：两条都**零参数**，参数面为零即无注入面。
	// 2J 只清当前可视屏，不清 scrollback（3J 才是清历史，永禁）。
	cursorHomeANSI  = "\x1b[H"  // CUP home：光标回左上角
	clearScreenANSI = "\x1b[2J" // ED 2：清除当前屏
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

// CursorHome 把光标移回左上角（\x1b[H，无参）。
func CursorHome() string { return cursorHomeANSI }

// ClearScreen 清除当前可视屏（\x1b[2J，无参）；不影响 scrollback。
func ClearScreen() string { return clearScreenANSI }
