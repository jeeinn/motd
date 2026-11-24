package terminal

// ADR-027 回归：原地重绘只使用服务端白名单序列，且行数计算必须自洽。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

func crlf(lines ...string) string {
	return strings.Join(lines, "\r\n") + "\r\n"
}

func TestRenderFrameAppendsWhenNoPreviousScreen(t *testing.T) {
	text := crlf("第一行", "第二行", "第三行")
	got, n := RenderFrame(text, 0, 80)
	if got != text {
		t.Fatal("prevLines=0 应原样追加，不得插入光标控制")
	}
	if n != 3 {
		t.Fatalf("行数应为 3，实际 %d", n)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("追加模式不应产生 ESC 序列")
	}
}

func TestRenderFrameRedrawsInPlace(t *testing.T) {
	text := crlf("第一行", "第二行", "第三行")
	got, n := RenderFrame(text, 3, 80)
	if n != 3 {
		t.Fatalf("行数应为 3，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[3A") {
		t.Fatalf("应先上移 3 行回到屏首，实际 %q", got)
	}
	if got := strings.Count(got, "\r\x1b[K"); got != 3 {
		t.Fatalf("每行都应 \\r + EL 清行，实际 %d 次", got)
	}
	if strings.Count(got, "\r\n") != 3 {
		t.Fatalf("应输出 3 行，实际 %d 行", strings.Count(got, "\r\n"))
	}
}

func TestRenderFrameClearsShrunkScreen(t *testing.T) {
	text := crlf("新一", "新二")
	got, n := RenderFrame(text, 5, 80)
	if n != 5 {
		t.Fatalf("应补齐到旧屏高度 5 行，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[5A") {
		t.Fatalf("应按旧屏行数上移，实际 %q", got)
	}
	if got := strings.Count(got, "\r\x1b[K"); got != 5 {
		t.Fatalf("2 行内容 + 3 行清空都应带 EL，实际 %d 次", got)
	}
	if !strings.Contains(got, "新二") || !strings.Contains(got, "\r\x1b[K\r\n\r\x1b[K\r\n\r\x1b[K\r\n") {
		t.Fatalf("新屏更短时多余行必须被清空: %q", got)
	}
}

func TestRenderFrameFallsBackOnNarrowTerminal(t *testing.T) {
	text := crlf(strings.Repeat("中", 40)) // 80 列宽字符占 80 列以上
	got, n := RenderFrame(text, 5, 40)
	if got != text {
		t.Fatal("发生自动折行时必须退回追加模式，避免行数算错导致错位")
	}
	if n != 1 {
		t.Fatalf("降级后仍应返回逻辑行数 1，实际 %d", n)
	}
	if strings.Contains(got, "\x1b[5A") {
		t.Fatal("降级后不得输出 CursorUp")
	}
}

func TestRenderFrameOnlyWhitelistedSequences(t *testing.T) {
	text := crlf("> 1. \x1b[1m标题\x1b[0m", "状态栏")
	got, _ := RenderFrame(text, 2, 80)
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x1b]0;", "\x1b[?1049h", "\x1b[H", "\x1b[2K"} {
		if strings.Contains(got, bad) {
			t.Errorf("原地重绘泄露了禁止序列 %q", bad)
		}
	}
	assertOnlySGRAndCursor(t, got)
}

// TestRenderFrameWrapConsistency 交叉核对 RenderFrame 的降级判定与 W8 物理行计算：
// 不超宽时两者行数一致且走原地重绘；超宽时两者都判定降级追加（消除双口径）。
func TestRenderFrameWrapConsistency(t *testing.T) {
	esc := string(rune(0x1b))
	for _, tc := range []struct {
		name  string
		text  string
		width int
	}{
		{"ascii fits", crlf(strings.Repeat("a", 80)), 80},
		{"cjk fits", crlf(strings.Repeat("中", 40)), 80},
		{"ascii folds", crlf(strings.Repeat("a", 125)), 40},
		{"cjk folds", crlf(strings.Repeat("中", 40)), 40},
		{"sgr ignored", crlf(esc + "[2m" + strings.Repeat("a", 125) + esc + "[0m"), 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := safe.MeasureWrap(tc.text, tc.width)
			got, n := RenderFrame(tc.text, 2, tc.width)
			if info.OverWide {
				if got != tc.text {
					t.Fatalf("W8 判超宽时 RenderFrame 必须降级追加，实际 %q", got)
				}
				if strings.Contains(got, "\x1b[2A") {
					t.Fatal("降级后不得输出 CursorUp")
				}
				return
			}
			// 不超宽：每个逻辑行恰占一个物理行，W8 行数与 RenderFrame 返回行数一致。
			logical := strings.Count(strings.TrimSuffix(tc.text, "\r\n"), "\r\n") + 1
			if info.Rows != logical {
				t.Fatalf("W8 物理行数应等于逻辑行数 %d，实际 %d", logical, info.Rows)
			}
			wantN := logical
			if 2 > wantN {
				wantN = 2 // RenderFrame 返回 max(逻辑行数, prevLines)
			}
			if n != wantN {
				t.Fatalf("RenderFrame 行数应为 %d，实际 %d", wantN, n)
			}
			if !strings.HasPrefix(got, "\x1b[2A") {
				t.Fatalf("不超宽应走原地重绘，实际 %q", got)
			}
		})
	}
}

// assertOnlySGRAndCursor 检查输出里的每个 ESC 序列都属于白名单。
func assertOnlySGRAndCursor(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		if i+2 >= len(s) || s[i+1] != '[' {
			t.Fatalf("出现非 CSI 的 ESC 序列（位置 %d）: %q", i, s)
		}
		j := i + 2
		for j < len(s) {
			c := s[j]
			if c == 'm' || c == 'K' || c == 'A' {
				break
			}
			if (c < '0' || c > '9') && c != ';' {
				t.Fatalf("ESC 序列不属于 SGR/EL/CUU 白名单: %q", s[i:min(i+24, len(s))])
			}
			j++
		}
		if j >= len(s) {
			t.Fatalf("未闭合的 ESC 序列: %q", s[i:])
		}
		i = j
	}
}
