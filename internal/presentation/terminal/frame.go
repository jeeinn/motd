package terminal

// 原地重绘帧（ADR-027）。
//
// RenderScreen 是无状态纯函数，只负责「一屏长什么样」；原地重绘需要会话状态
// （上一屏占了多少行），因此帧组装放在这一层，由 cmd 的 pageController 记住行数。
//
// 安全：本文件产生的 ESC 序列全部来自 safe 的服务端白名单（SGR / EL / CUU），
// 用户提交的 ESC 早已被 RenderUser 净化成可见图示，不可能在这里变成控制序列。
// 用户内容路径（safe.RenderUser / SanitizeText）不调用任何光标控制，由 archtest 守卫。

import (
	"strings"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// RenderFrame 把一屏已 CRLF 化的文本转换成可原地重绘的字节序列。
//
// prevLines <= 0 表示屏幕连续性被打破（首次渲染、命令提示、编辑器回显、
// 登录输出等），退回追加模式。width > 0 时若任一行显示宽度超出终端，
// 说明终端会自动折行、行数不可靠，同样退回追加模式（宁可刷屏也不错位）。
//
// 返回本屏占用的行数；下次以该值 CursorUp 即可回到屏首。
func RenderFrame(text string, prevLines, width int) (string, int) {
	lines := strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
	n := len(lines)
	if n == 0 {
		return text, 0
	}
	if prevLines <= 0 || (width > 0 && overWide(lines, width)) {
		return text, n
	}

	total := n
	if prevLines > n {
		total = prevLines // 新屏更短时，多出来的旧行必须显式清空
	}

	var b strings.Builder
	b.WriteString(safe.CursorUp(prevLines))
	for i := 0; i < total; i++ {
		// \r 回到行首 → EL 清掉旧内容 → 写新行 → 换行。
		b.WriteString("\r")
		b.WriteString(safe.ClearToEOL())
		if i < n {
			b.WriteString(lines[i])
		}
		b.WriteString("\r\n")
	}
	return b.String(), total
}

// overWide 检测任一行是否超出终端宽度。SGR 序列按 0 列计，避免误判。
func overWide(lines []string, width int) bool {
	for _, l := range lines {
		if visibleWidth(l) > width {
			return true
		}
	}
	return false
}

func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+2 < len(s) && s[i+1] == '[' {
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
		w += safe.DisplayWidth(r)
		i += size
	}
	return w
}
