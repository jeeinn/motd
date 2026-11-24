package terminal

// 帧组装（ADR-027 / ADR-071 / ADR-072）。
//
// RenderScreen 是无状态纯函数，只负责「一屏长什么样」；帧组装放在这一层，
// 由调用方按场景选函数：
//
//   - RenderCleanFrame：整屏干净重绘（H + 2J + n 行），普通导航页/登录屏的
//     整屏状态更新用它。不依赖上一屏行数，天然规避 resize 错位与空白带；
//   - RenderFrame：局部覆盖（CUU + EL）或追加，彩蛋帧间、编辑器回显等需要
//     「保留局部更新不闪」的场景用它。
//
// 安全：本文件产生的 ESC 序列全部来自 safe 的服务端白名单
// （SGR / EL / CUU / 无参 H / 无参 2J），用户提交的 ESC 早已被 RenderUser
// 净化成可见图示，不可能在这里变成控制序列。用户内容路径（safe.RenderUser /
// SanitizeText）不调用任何光标控制，由 archtest 守卫。

import (
	"strings"

	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// RenderFrame 把一屏已 CRLF 化的文本转换成可原地重绘的字节序列。
//
// prevLines <= 0 表示屏幕连续性被打破（首次渲染、命令提示、编辑器回显、
// 登录输出等），退回追加模式。width > 0 时若任一行显示宽度超出终端，
// 说明终端会自动折行、行数不可靠，同样退回追加模式（宁可刷屏也不错位）。
//
// height 是终端可视区行数（ADR-071）：>0 时 CursorUp 的上移量以可视区为界。
// 上一屏行数超过可视高度时，顶部行已滚入 scrollback，终端把 CUU 钳制在可视顶，
// 若仍按 prevLines 上移会落在错误行；钳到 height 后落点确定。height<=0 表示
// 未知，退回按 prevLines 计算的旧口径（兼容无 Window 信息的调用点）。
//
// 返回**内容行数 n**（不是写入行数）：帧末不写 CRLF、并在补清多余行后用
// 白名单 CUU 把光标抬回内容末行的下一行（第 n 行），下次以该值 CursorUp
// 即可精确回到屏首——避免"补空行后光标停在屏幕底部"的错位（ADR-071）。
func RenderFrame(text string, prevLines, width, height int) (string, int) {
	lines := strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
	n := len(lines)
	if n == 0 {
		return text, 0
	}
	// 与编辑器回显层共用 safe.OverWide 的折行口径，消除双口径：
	// 超宽则退回追加模式（终端会自动折行、行数不可靠，宁可刷屏也不错位）。
	if prevLines <= 0 || (width > 0 && safe.OverWide(text, width)) {
		return text, n
	}

	// 高度维钳制：CUU 只回退到可视区顶，不试图跨 scrollback（ADR-071）。
	up := prevLines
	if height > 0 && up > height {
		up = height
	}
	// 至少写 n+1 行：末行留作光标归位点，使下次 CUU(n) 精确回到屏首。
	total := n + 1
	if up > total {
		total = up
	}

	var b strings.Builder
	b.WriteString(safe.CursorUp(up))
	for i := 0; i < total; i++ {
		// \r 回到行首 → EL 清掉旧内容 → 写新行 →（除末行）换行。
		b.WriteString("\r")
		b.WriteString(safe.ClearToEOL())
		if i < n {
			b.WriteString(lines[i])
		}
		if i < total-1 {
			b.WriteString("\r\n") // 末行不写：避免多滚一行把内容顶出可视区
		}
	}
	// 补清了多余行时，光标停在末行；抬回第 n 行（内容末行的下一行）。
	if total-1 > n {
		b.WriteString(safe.CursorUp(total - 1 - n))
	}
	return b.String(), n
}

// RenderCleanFrame 用 ESC[H（回左上角）+ ESC[2J（清当前屏）整屏重绘，
// 不依赖上一屏行数，因此天然规避 resize 错位与空白带（ADR-072）。
//
// 帧结构：H + 2J + n 行内容 + 光标落在输入位。规则：
//   - n < height：每行都以 CRLF 结尾（共 n 个），光标自然到第 n+1 行行首；
//   - n >= height：末行的 CRLF 不写，避免在屏幕底行换行触发滚屏、把首行顶出
//     可视区（重演 ADR-071 修掉的问题）；光标停在末行行尾；
//   - height<=0（可视高度未知）：保守按「不补末行 CRLF」处理，绝不冒险滚屏。
//
// 只清当前屏，不清 scrollback（2J 而非 3J）。用户文本的 ESC 净化在 RenderUser
// 层完成，本函数不改写文本。
func RenderCleanFrame(text string, height int) (string, int) {
	lines := strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
	n := len(lines)
	if n == 0 {
		return "", 0
	}
	var b strings.Builder
	b.WriteString(safe.CursorHome())
	b.WriteString(safe.ClearScreen())
	for i, line := range lines {
		b.WriteString(line)
		if i < n-1 || (height > 0 && n < height) {
			b.WriteString("\r\n")
		}
	}
	return b.String(), n
}
