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

// TestRenderFrameOnlyWhitelistedSequences 校验**局部覆盖路径**的控制序列集合。
// 注意：H 与 2J 现已属白名单（供 RenderCleanFrame 整屏重绘），但 RenderFrame 是
// 局部覆盖语义，不应发起整屏清屏或绝对定位——故仍在此处逐条排除。这样与
// assertOnlyWhitelistedANSI（按全局白名单放行 H/2J）构成"全局允许 + 本路径自限"
// 的双层约束，避免把"白名单有此能力"误当成"局部重绘可以用它"。
func TestRenderFrameOnlyWhitelistedSequences(t *testing.T) {
	text := crlf("> 1. \x1b[1m标题\x1b[0m", "状态栏")
	got, _ := RenderFrame(text, 2, 80, 24)
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x1b]0;", "\x1b[?1049h", "\x1b[H", "\x1b[2K"} {
		if strings.Contains(got, bad) {
			t.Errorf("局部覆盖路径不应出现整屏清屏/定位或禁止序列 %q", bad)
		}
	}
	assertOnlyWhitelistedANSI(t, got)
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
	assertOnlyWhitelistedANSI(t, got)

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

// TestRenderCleanFrameStartsWithHomeAndClear 验证整屏重绘的帧首是 H + 2J，
// 且整帧只出现一次 H 与一次 2J（不重复清屏、不夹带其他光标控制）。
func TestRenderCleanFrameStartsWithHomeAndClear(t *testing.T) {
	text := crlf("版块一", "版块二")
	got, n := RenderCleanFrame(text, 24)
	if n != 2 {
		t.Fatalf("应返回内容行数 2，实际 %d", n)
	}
	if !strings.HasPrefix(got, "\x1b[H\x1b[2J") {
		t.Fatalf("帧首应为 ESC[H + ESC[2J，实际 %q", got)
	}
	if c := strings.Count(got, "\x1b[H"); c != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[H，实际 %d", c)
	}
	if c := strings.Count(got, "\x1b[2J"); c != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[2J，实际 %d", c)
	}
	if !strings.Contains(got, "版块一") || !strings.Contains(got, "版块二") {
		t.Fatalf("内容行缺失: %q", got)
	}
	assertOnlyWhitelistedANSI(t, got)
}

// TestRenderCleanFrameTrailingCRLFWhenFits 验证 n < height 时末行也补 CRLF，
// 光标落到第 n+1 行行首（输入位），且不多写第二个 CRLF。
func TestRenderCleanFrameTrailingCRLFWhenFits(t *testing.T) {
	text := crlf("甲", "乙", "丙")
	const height = 24
	got, n := RenderCleanFrame(text, height)
	if n != 3 {
		t.Fatalf("应返回内容行数 3，实际 %d", n)
	}
	if !strings.HasSuffix(got, "丙\r\n") {
		t.Fatalf("n<height 时末行后应有 CRLF 把光标带到输入位，实际 %q", got)
	}
	if c := strings.Count(got, "\r\n"); c != n {
		t.Fatalf("CRLF 数应等于内容行数 %d，实际 %d", n, c)
	}
	assertOnlyWhitelistedANSI(t, got)
}

// TestRenderCleanFrameNoTrailingCRLFWhenFull 验证 n >= height 时末行不写 CRLF，
// 避免在屏幕底行换行触发滚屏、把首行顶出可视区。
func TestRenderCleanFrameNoTrailingCRLFWhenFull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines int
		h     int
	}{
		{"恰好占满", 24, 24},
		{"超过可视高度", 30, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := make([]string, tc.lines)
			for i := range items {
				items[i] = "行"
			}
			got, n := RenderCleanFrame(crlf(items...), tc.h)
			if n != tc.lines {
				t.Fatalf("应返回内容行数 %d，实际 %d", tc.lines, n)
			}
			if got := strings.Count(got, "\r\n"); got != n-1 {
				t.Fatalf("n>=height 时末行不写 CRLF，应只有 %d 个，实际 %d", n-1, got)
			}
			if strings.HasSuffix(got, "\r\n") {
				t.Fatalf("末行不得以 CRLF 结尾（会滚屏）: %q", got[max(0, len(got)-8):])
			}
			assertOnlyWhitelistedANSI(t, got)
		})
	}
}

// TestRenderCleanFrameNoHeight 验证 height<=0（可视高度未知）时保守不补末行 CRLF。
func TestRenderCleanFrameNoHeight(t *testing.T) {
	text := crlf("甲", "乙")
	got, n := RenderCleanFrame(text, 0)
	if n != 2 {
		t.Fatalf("应返回内容行数 2，实际 %d", n)
	}
	if c := strings.Count(got, "\r\n"); c != 1 {
		t.Fatalf("height 未知时只应有 n-1=1 个 CRLF，实际 %d", c)
	}
	if strings.HasSuffix(got, "\r\n") {
		t.Fatalf("height 未知时不得补末行 CRLF: %q", got)
	}
	assertOnlyWhitelistedANSI(t, got)
}

// TestRenderCleanFrameUserTextInjectionSanitized 验证帧内不出现第二个真实清屏：
// RenderCleanFrame 不改写文本（净化在 RenderUser 层），因此传入已净化的可见图示
// 时帧首的 H/2J 必须是该帧唯一控制序列；若文本被原样注入第二个 2J，
// 白名单断言会暴露「整帧控制序列不止帧首那一组」。
func TestRenderCleanFrameUserTextInjectionSanitized(t *testing.T) {
	// 已净化文本：ESC 被渲成 ␛（U+241B），[2J 只是普通字符。
	text := crlf("标题 ␛[2J 注入", "正文")
	got, n := RenderCleanFrame(text, 24)
	if n != 2 {
		t.Fatalf("应返回内容行数 2，实际 %d", n)
	}
	if c := strings.Count(got, "\x1b[2J"); c != 1 {
		t.Fatalf("真实清屏序列只应来自帧首，实际出现 %d 次: %q", c, got)
	}
	if !strings.Contains(got, "␛[2J") {
		t.Fatalf("净化后的可见图示应原样保留在内容里: %q", got)
	}
	assertOnlyWhitelistedANSI(t, got)
}

// assertOnlyWhitelistedANSI 检查输出里的每个 ESC 序列都属于白名单
// （ADR-027 SGR/EL/CUU + ADR-072 无参 H / 无参 2J）。
func assertOnlyWhitelistedANSI(t *testing.T, s string) {
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
			if c == 'm' || c == 'K' || c == 'A' || c == 'H' || c == 'J' {
				break
			}
			if (c < '0' || c > '9') && c != ';' {
				t.Fatalf("ESC 序列不属于白名单: %q", s[i:min(i+24, len(s))])
			}
			j++
		}
		if j >= len(s) {
			t.Fatalf("未闭合的 ESC 序列: %q", s[i:])
		}
		// ADR-072：H 只允许无参（\x1b[H），J 只允许 ED 2（\x1b[2J）。
		switch s[j] {
		case 'H':
			if j-i != 2 {
				t.Fatalf("只允许无参 CUP home \\x1b[H，实际 %q", s[i:j+1])
			}
		case 'J':
			if s[i:j+1] != "\x1b[2J" {
				t.Fatalf("只允许 ED 2 清屏 \\x1b[2J，实际 %q", s[i:j+1])
			}
		}
		i = j
	}
}
