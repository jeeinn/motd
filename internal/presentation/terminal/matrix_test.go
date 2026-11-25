package terminal

// v0.9.2 / ADR-076：/matrix 屏内帧组装回归。
//
// 守三件事：帧结构（清屏 / CUU 回顶 / 底行不写 CRLF）、SGR 合规（只用基础绿，
// 不碰 256 色与真彩）、带宽（单帧字节上限）。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
)

// matrixRowsAfter 造一帧推进若干次后的雨区内容。
func matrixRowsAfter(width, height, frames int) MatrixRows {
	rain := command.NewMatrixRain(width, height)
	for i := 0; i < frames; i++ {
		rain.Advance()
	}
	rows := make(MatrixRows, height)
	for y := 0; y < height; y++ {
		rows[y] = rain.Row(y)
	}
	return rows
}

// 进入帧必须以「清屏 + 复位 SGR + 提示行」开头。
func TestRenderMatrixScreenStartsWithClearAndHint(t *testing.T) {
	out := RenderMatrixScreen(matrixRowsAfter(20, 5, 3), "[Esc 返回]\r\n")
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J\x1b[0m[Esc 返回]") {
		t.Fatalf("进入帧应以 H+2J+复位+提示开头: %q", out[:min(40, len(out))])
	}
}

// 每帧必须用 CUU 回到雨区首行；**屏幕底行不写 CRLF**（否则触发终端滚屏、
// 把雨区顶出可视区，并让 scrollback 无限增长）。
func TestRenderMatrixFrameUsesCUUAndNoTrailingCRLF(t *testing.T) {
	rows := matrixRowsAfter(20, 5, 3)
	out := RenderMatrixFrame(rows, 5)
	// 雨区 5 行 → 光标从屏幕底行回雨区首行需上移 5-1=4 行。
	if !strings.Contains(out, "\x1b[4A") {
		t.Fatalf("帧首应含 CUU(4): %q", out[:min(40, len(out))])
	}
	if strings.HasSuffix(out, "\r\n") {
		t.Fatalf("帧末不得写 CRLF（会触发终端滚屏）: %q", out[max(0, len(out)-16):])
	}
	// 雨区 5 行 → 行间 4 个换行；首尾各一个 "\r"。
	if got := strings.Count(out, "\r\n"); got != 4 {
		t.Fatalf("雨区 5 行应有 4 个行间换行，实际 %d", got)
	}
}

// 带宽门禁：80 列 × 23 雨区的单帧字节数上限。
//
// 12fps 下帧字节 × 12 = 单会话稳态 KB/s；整站上行 1 Mbps ≈ 125 KB/s，单会话
// 预算约 30 KB/s，即单帧约 2.5 KB。此处留 60% 余量做门禁，超出即说明色阶
// 跳变失控（逐列独立雨头会把一帧切出几十个色段，字节数翻数倍）。
func TestRenderMatrixFrameByteBudget(t *testing.T) {
	rows := matrixRowsAfter(80, 23, 10)
	out := RenderMatrixFrame(rows, 23)
	if len(out) > 4096 {
		t.Fatalf("单帧字节 %d 超出预算 4096（12fps 即约 %d KB/s，超单会话预算）",
			len(out), len(out)*12/1024)
	}
}

// 退出帧必须显式复位 SGR：ED 2 只清可视区、不重置 SGR，漏复位会让绿色漏到
// 调用方随后重绘的整页论坛上。
func TestRenderMatrixExitResetsSGR(t *testing.T) {
	if got := RenderMatrixExit(); got != "\x1b[H\x1b[2J\x1b[0m" {
		t.Fatalf("退出帧应为 H+2J+复位: %q", got)
	}
}

// 颜色只能是基础 SGR 的两档绿：不得出现 256 色或真彩（ROADMAP §4 冻结决策）。
func TestRenderMatrixUsesOnlyBasicGreen(t *testing.T) {
	out := RenderMatrixScreen(matrixRowsAfter(80, 23, 10), "hint\r\n")
	for _, bad := range []string{"\x1b[38;5;", "\x1b[38;2;"} {
		if strings.Contains(out, bad) {
			t.Fatalf("不得使用 256 色 / 真彩（%q）", bad)
		}
	}
	if !strings.Contains(out, "\x1b[2;32m") {
		t.Fatal("应使用暗绿底色 SGR")
	}
	if !strings.Contains(out, "\x1b[1;92m") {
		t.Fatal("应使用亮绿雨头 SGR")
	}
}

// 帧内不得出现任何白名单外的 ANSI 序列。
func TestRenderMatrixHasNoForbiddenSequences(t *testing.T) {
	out := RenderMatrixScreen(matrixRowsAfter(40, 12, 5), "hint\r\n") +
		RenderMatrixFrame(matrixRowsAfter(40, 12, 5), 12) +
		RenderMatrixExit()
	for _, bad := range []string{
		"\x1b[?1049h", "\x1b[?1049l", // 备用屏
		"\x1b[3J", "\x1b[1J", "\x1b[J", // 其他 ED
		"\x1b[2;3H", "\x1b[1;1H", // 带参 CUP
		"\x1b[B",                       // CUD
		"\x1b]52", "\x1b]0;", "\x1b]8", // OSC
	} {
		if strings.Contains(out, bad) {
			t.Fatalf("帧泄露禁止序列 %q", bad)
		}
	}
}
