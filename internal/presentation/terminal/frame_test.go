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
	got, n := RenderFrame(text, 0, 80, 24)
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
	got, n := RenderFrame(text, 3, 80, 24)
	if n != 3 {
		t.Fatalf("内容行数应为 3，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[3A") {
		t.Fatalf("应先上移 3 行回到屏首，实际 %q", got)
	}
	if got := strings.Count(got, "\r\x1b[K"); got != 4 {
		t.Fatalf("3 行内容 + 1 行归位空行都应 \\r + EL，实际 %d 次", got)
	}
	// ADR-071：帧末的归位空行不写 CRLF——3 行内容各带 1 个行间换行（共 3），
	// 末行归位空行没有 CRLF，避免多滚一行把内容顶出可视区。
	if strings.Count(got, "\r\n") != 3 {
		t.Fatalf("应只有 3 个行间换行（末行无 CRLF），实际 %d: %q", strings.Count(got, "\r\n"), got)
	}
}

func TestRenderFrameClearsShrunkScreen(t *testing.T) {
	text := crlf("新一", "新二")
	got, n := RenderFrame(text, 5, 80, 24)
	// ADR-071：返回内容行数（2），不再返回补齐后的写入行数（5），
	// 这样下次 CUU(n) 才能精确回到屏首；多余旧行仍被逐个 EL 清空。
	if n != 2 {
		t.Fatalf("应返回内容行数 2，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[5A") {
		t.Fatalf("应按旧屏行数上移，实际 %q", got)
	}
	// n=2、up=5 → total=5：2 行内容 + 3 行补清空行，共 5 次 EL。
	if got := strings.Count(got, "\r\x1b[K"); got != 5 {
		t.Fatalf("2 行内容 + 3 行清空都应带 EL，实际 %d 次", got)
	}
	// 3 行补清：前 2 行带 CRLF，末行（归位行）不带——模式为 2 个 "\r\x1b[K\r\n" + 1 个 "\r\x1b[K"。
	if !strings.Contains(got, "新二") || !strings.Contains(got, "\r\x1b[K\r\n\r\x1b[K\r\n\r\x1b[K") {
		t.Fatalf("新屏更短时多余行必须被清空: %q", got)
	}
	// total=5、n=2 → 归位 CUU(total-1-n)=CUU(2)，把光标抬回第 2 行。
	if !strings.Contains(got, "\x1b[2A") {
		t.Fatalf("应发出归位 CUU(2) 把光标从末行抬回内容行: %q", got)
	}
}

func TestRenderFrameFallsBackOnNarrowTerminal(t *testing.T) {
	text := crlf(strings.Repeat("中", 40)) // 80 列宽字符占 80 列以上
	got, n := RenderFrame(text, 5, 40, 24)
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
	got, _ := RenderFrame(text, 2, 80, 24)
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
			got, n := RenderFrame(tc.text, 2, tc.width, 24)
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
			// ADR-071：返回内容行数（不再返回 max(逻辑行数, prevLines)）。
			if n != logical {
				t.Fatalf("RenderFrame 应返回内容行数 %d，实际 %d", logical, n)
			}
			if !strings.HasPrefix(got, "\x1b[2A") {
				t.Fatalf("不超宽应走原地重绘，实际 %q", got)
			}
		})
	}
}

// TestRenderFrameClampsCUUToViewport 验证 ADR-071 高度维钳制：
// 上一屏逻辑行数超过可视高度时，CUU 只回退到可视顶（不会被终端钳在错误行），
// 且归位量与内容行数自洽——连续两次重绘返回行数恒定。
func TestRenderFrameClampsCUUToViewport(t *testing.T) {
	text := crlf("列表一", "列表二", "列表三")
	const height = 24
	got, n := RenderFrame(text, 30, 80, height) // 详情屏 30 行 > 可视 24 行
	if n != 3 {
		t.Fatalf("应返回内容行数 3，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[24A") {
		t.Fatalf("CUU 应钳制到可视高度 24，实际 %q", got)
	}
	if strings.HasPrefix(got, "\x1b[30A") {
		t.Fatal("不得按上一屏的 30 行上移（会跨 scrollback 落错行）")
	}
	// total=24、n=3 → 归位 CUU(24-1-3)=CUU(20)。
	if !strings.Contains(got, "\x1b[20A") {
		t.Fatalf("应发出归位 CUU(20)，实际 %q", got)
	}
	// 末行不写 CRLF：23 个行间换行（24 行写入 - 1）。
	if c := strings.Count(got, "\r\n"); c != 23 {
		t.Fatalf("末行不写 CRLF，应有 23 个行间换行，实际 %d", c)
	}
	assertOnlySGRAndCursor(t, got)

	// 稳定性：下一次用返回的行数重绘，行数不变。
	_, n2 := RenderFrame(text, n, 80, height)
	if n2 != n {
		t.Fatalf("连续重绘行数应稳定：%d → %d", n, n2)
	}
}

// TestRenderFrameNoHeightFallsBack 验证 height<=0（未知）时退回旧口径：
// 不钳制，按 prevLines 计算，兼容没有 Window 高度信息的调用点。
func TestRenderFrameNoHeightFallsBack(t *testing.T) {
	text := crlf("甲", "乙")
	got, n := RenderFrame(text, 5, 80, 0)
	if n != 2 {
		t.Fatalf("应返回内容行数 2，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[5A") {
		t.Fatalf("height 未知时应按 prevLines=5 上移，实际 %q", got)
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
