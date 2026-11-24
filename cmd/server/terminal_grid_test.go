package main

// ---------------------------------------------------------------------------
// 虚拟终端网格（W3 测试夹具）
//
// 定位声明：宽度已知、只做物理层，与真实 PTY E2E 互补，不追求终端级真值。
// 按 DESIGN-v090-display-i18n.md §5.1 的 7 种 token 逐字节推进二维 rune 网格
// 与光标 (row, col)，用于自动化回归"提示自动折行 / pending wrap / EL 清错行 /
// CUU 跳错行"这类物理层缺陷；真实终端的 LNM、DECAWM、组合字符渲染差异由
// 人工窄屏 PTY 终验覆盖，两者互补而非替代。
//
// 边界：建模 §5.1 的 7 种 token，另补 \b（\b \b 退格擦除）供 W10（B7）网格断言。
// 仍只做物理层，不追求终端级真值。
// ---------------------------------------------------------------------------

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

const (
	gridBlank = ' '     // 空白格
	wideTail  = rune(0) // 宽字符第二格的占位标记，rowText 输出时跳过
)

// terminalGrid 是一块宽度已知的物理屏幕。rows 每行定长 width，空格表示空白格；
// wrapPending 是 pending wrap 标志——字符恰好占满末列时光标留在行末，
// 不立即 row++，下一个字符到达才折行（本 bug 的判定点）。
type terminalGrid struct {
	width       int
	rows        [][]rune
	row, col    int
	wrapPending bool
}

func blankRow(width int) []rune {
	r := make([]rune, width)
	for i := range r {
		r[i] = gridBlank
	}
	return r
}

func newTerminalGrid(width int) *terminalGrid {
	if width <= 0 {
		width = 80
	}
	return &terminalGrid{width: width, rows: [][]rune{blankRow(width)}}
}

// ensureRowIdx 按需扩容到指定行；只增不减，便于断言历史行不受后续操作影响。
func (g *terminalGrid) ensureRowIdx(r int) {
	for r >= len(g.rows) {
		g.rows = append(g.rows, blankRow(g.width))
	}
}

func (g *terminalGrid) ensureRow() { g.ensureRowIdx(g.row) }

// putCleanRune 落一个已净化的可见 rune 到网格：宽字符整字折，不跨行拆字。
func (g *terminalGrid) putCleanRune(r rune) {
	if g.wrapPending {
		// pending wrap：上一个字符恰好占满末列，本字符先触发折行再写。
		g.row++
		g.col = 0
		g.wrapPending = false
	}
	w := safe.DisplayWidth(r)
	if w <= 0 {
		return // 组合音标 / ZWJ 等不占列，也不覆盖已有格子
	}
	if g.col+w > g.width {
		// 剩余列不足：宽字符整字折到下一行，绝不在当前行拆成半字。
		g.row++
		g.col = 0
	}
	g.ensureRow()
	g.rows[g.row][g.col] = r
	if w == 2 {
		g.rows[g.row][g.col+1] = wideTail // col+w <= width 已保证不越界
	}
	g.col += w
	if g.col >= g.width {
		g.col = g.width
		g.wrapPending = true // 占满末列但留在本行，等下一个字符才折
	}
}

// putRune 执行"普通 rune"token：用户文本先经 SanitizeText（唯一入口），
// 再逐可见 rune 落网格。裸 ESC 等非法控制字节在此被转成可见图示，不透传。
func (g *terminalGrid) putRune(r rune) {
	for _, c := range safe.SanitizeText(string(r)) {
		g.putCleanRune(c)
	}
}

// feed 逐字节执行一段输出。识别服务端生成的 CSI（SGR / EL / CUU）与 CR/LF，
// 其余字节按普通文本走 SanitizeText；非白名单 CSI 一律忽略控制动作。
func (g *terminalGrid) feed(s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0x1b: // ESC
			if i+1 >= len(s) || s[i+1] != '[' {
				g.putRune(rune(c)) // 非 CSI 裸 ESC 按净化文本处理
				continue
			}
			i += 2 // 跳过 ESC[
			start := i
			for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == ';') {
				i++
			}
			param := s[start:i]
			if i >= len(s) {
				return // 未闭合 CSI，忽略残段
			}
			switch term := s[i]; term {
			case 'm':
				// SGR：忽略，不改网格内容、不计列宽。
			case 'K':
				// EL：清空当前物理行（光标仍在 pending wrap 所在行）。
				g.ensureRow()
				g.rows[g.row] = blankRow(g.width)
			case 'A':
				// CUU：row = max(0, row - n)，缺省 n=1；n 来自服务端，非用户输入。
				n := 1
				if param != "" {
					if v, err := strconv.Atoi(param); err == nil && v > 0 {
						n = v
					}
				}
				g.row -= n
				if g.row < 0 {
					g.row = 0
				}
				g.wrapPending = false
				if g.col >= g.width {
					g.col = g.width - 1
				}
			default:
				// 非白名单 CSI（CUP/CUD/ED/DECSET 等）：控制动作不生效。
			}
		case '\b': // BS：光标左移一列（col=0 时不动，取 xterm LNM off 语义）
			// \b \b = 左移 → 写空格覆盖 → 左移；净效果擦掉左侧一格并退回。
			if g.col > 0 {
				g.col--
			}
			g.wrapPending = false
		case '\r': // CR：col=0
			g.col = 0
			g.wrapPending = false
		case '\n': // LF（含 \r\n 的后半段）：row+=1
			g.row++
			g.wrapPending = false
			if g.col >= g.width {
				g.col = g.width - 1
			}
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			g.putRune(r)
			i += size - 1
		}
	}
}

// rowText 返回第 r 个物理行（右侧空白裁剪）的可读文本。
func (g *terminalGrid) rowText(r int) string {
	if r < 0 || r >= len(g.rows) {
		return ""
	}
	var b strings.Builder
	for _, c := range g.rows[r] {
		if c == wideTail {
			continue // 宽字符占位尾格不输出，保证 rowText 文本连续
		}
		b.WriteRune(c)
	}
	return strings.TrimRight(b.String(), string(gridBlank))
}

func (g *terminalGrid) totalRows() int     { return len(g.rows) }
func (g *terminalGrid) cursor() (int, int) { return g.row, g.col }
func (g *terminalGrid) findRow(fragment string) int {
	for i := range g.rows {
		if strings.Contains(g.rowText(i), fragment) {
			return i
		}
	}
	return -1
}

// gridTextWidth 计算一段文本的总显示宽度（控制符先经 SanitizeText 转可见图示），
// 用于推导预期折行数。
func gridTextWidth(s string) int {
	w := 0
	for _, r := range safe.SanitizeText(s) {
		w += safe.DisplayWidth(r)
	}
	return w
}

// ---------------------------------------------------------------------------
// 夹具自证：喂已知序列，断言网格内容与光标位置（§5.1 七种 token 全覆盖）。
// ---------------------------------------------------------------------------

// TestTerminalGridPendingWrap 验证判定点：占满末列不立即折，EL 清的是当前行；
// 下一个字符到达才折行。若实现成"占满即折"，EL 会清错行、断言立即失败。
func TestTerminalGridPendingWrap(t *testing.T) {
	g := newTerminalGrid(4)
	g.feed("abcd") // 恰好占满 4 列
	if row, col := g.cursor(); row != 0 || col != 4 || !g.wrapPending {
		t.Fatalf("占满末列应留在 row0/col4 且 pending：row=%d col=%d pending=%v", row, col, g.wrapPending)
	}
	if got := g.rowText(0); got != "abcd" {
		t.Fatalf("首行内容不符：%q", got)
	}

	g.feed("\x1b[K") // EL：应清 row0（光标仍在 row0），不是尚未创建的 row1
	if got := g.rowText(0); got != "" {
		t.Fatalf("EL 应清空 pending wrap 所在的 row0，实际 %q", got)
	}

	g.feed("E") // 下一个字符到达才折
	if row, col := g.cursor(); row != 1 || col != 1 {
		t.Fatalf("下一字符应折到 row1/col1，实际 row=%d col=%d", row, col)
	}
	if got := g.rowText(1); got != "E" {
		t.Fatalf("折行后内容应在 row1，实际 %q", got)
	}
}

// TestTerminalGridWideCharWrap 验证宽字符整字折：剩余列不足时整字换到下一行，
// 不在当前行拆成半字；同时 SGR 被忽略、不计列宽。
func TestTerminalGridWideCharWrap(t *testing.T) {
	g := newTerminalGrid(4)
	g.feed("\x1b[2mA乙\x1b[0m丙") // A=1 列，乙=2 列（col=3）；丙=2 列放不下
	if got := g.rowText(0); got != "A乙" {
		t.Fatalf("row0 应为 A乙（丙整字折走），实际 %q", got)
	}
	if got := g.rowText(1); got != "丙" {
		t.Fatalf("row1 应为整字丙，实际 %q", got)
	}
}

// TestTerminalGridControls 覆盖 CR / LF / \r\n / EL / CUU / SGR 的字节语义。
func TestTerminalGridControls(t *testing.T) {
	g := newTerminalGrid(10)
	g.feed("abcdefghij\r\n") // 首行恰好占满，CRLF 只推进一行
	if row, _ := g.cursor(); row != 1 {
		t.Fatalf("CRLF 应只推进一行到 row1，实际 row=%d", row)
	}
	g.feed("12345")
	g.feed("\x1b[2m") // SGR：忽略
	if got := g.rowText(1); got != "12345" {
		t.Fatalf("SGR 不应改变网格，row1 实际 %q", got)
	}
	g.feed("\x1b[2A") // CUU(2)：row1 -> max(0, 1-2) = 0
	if row, col := g.cursor(); row != 0 || col != 5 {
		t.Fatalf("CUU(2) 应到 row0/col5，实际 row=%d col=%d", row, col)
	}
	g.feed("\r\x1b[KXY") // CR 回行首，EL 清整行，再写 XY
	if got := g.rowText(0); got != "XY" {
		t.Fatalf("row0 应被 EL 清空后写入 XY，实际 %q", got)
	}
	if got := g.rowText(1); got != "12345" {
		t.Fatalf("row1 不应受 row0 的 EL 影响，实际 %q", got)
	}
}

// TestTerminalGridFoldRowsAcrossWidths 验证 125 列文本在
// 24/40/80/125/200 列下分别折成 6/4/2/1/1 个物理行（W1+W3 联动自证）。
func TestTerminalGridFoldRowsAcrossWidths(t *testing.T) {
	text := strings.Repeat("x", 125) // 显示宽度恰为 125，与设计文档限流提示同宽
	if w := gridTextWidth(text); w != 125 {
		t.Fatalf("夹具文本显示宽度应为 125，实际 %d", w)
	}
	for _, tc := range []struct {
		width int
		rows  int
	}{{24, 6}, {40, 4}, {80, 2}, {125, 1}, {200, 1}} {
		g := newTerminalGrid(tc.width)
		g.feed(text)
		if got := g.totalRows(); got != tc.rows {
			t.Errorf("%d 列应折 %d 行，实际 %d 行", tc.width, tc.rows, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 复现用例：真实 A 路径限流输出 + 虚拟网格，验证"提示折 4 行 + 方案 B 追加重建"
// 下正文落在提示之后的末行、无 CUU、无碎片散落。组 A 曾用 wantScatter=true
// 刻画缺陷现状；组 B 止损后已反转，保留开关便于回退对照。
// ---------------------------------------------------------------------------

const wantScatter = false // 方案 B 已落地：CUU 归零、正文落提示后末行、无碎片

// TestNarrowHintCUUReproducesScatter 在 40 列窄屏下跑真实限流重建路径，
// 断言当前缺陷确实可被网格夹具复现：正文落进提示中间行，末行残留碎片。
func TestNarrowHintCUUReproducesScatter(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSessionWithWidth(40, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "回复正文你好")
	sess.reset()
	submitBody(t, ctx, sess, pc) // 限流 → A 路径 rebuildComposerFromDraft

	if pc.editor == nil || pc.draft == nil {
		t.Fatal("应进入 A 路径（编辑器与草稿都在）")
	}
	out := sess.output()

	// 提示显示宽度 125；40 列下折 4 行，加上 Ctrl-D 的 echo.newline() 先行 \r\n，
	// 故提示占 row1..row4，正确落点是 row5。
	cuu := cuuParams(out)
	g := newTerminalGrid(40)
	g.feed(out)
	bodyRow := g.findRow("回复正文你好")
	t.Logf("40 列网格（共 %d 行，CUU=%v，正文落 row%d）：", g.totalRows(), cuu, bodyRow)
	for i := 0; i < g.totalRows(); i++ {
		t.Logf("  row%-2d |%s|", i, g.rowText(i))
	}
	if bodyRow < 0 {
		t.Fatalf("网格内找不到重建正文，输出: %q", out)
	}

	if wantScatter {
		// 现状刻画：A 路径仍发 CUU(2)，up=2 硬编码假设"提示=1 物理行"，
		// 从 row5 跳回 row3（提示中间行）；row4 残留尾段，正文从未到达 row5。
		if len(cuu) != 1 || cuu[0] != 2 {
			t.Fatalf("当前缺陷应恰发一个 CUU(2)，实际 %v", cuu)
		}
		if bodyRow != 3 {
			t.Fatalf("缺陷下正文应覆盖提示中间行 row3，实际 row%d", bodyRow)
		}
		if got := g.rowText(4); !strings.Contains(got, "恢复") {
			t.Fatalf("row4 应残留提示尾段（散落），实际 %q", got)
		}
		if got := g.rowText(1); !strings.Contains(got, "[提示]") {
			t.Fatalf("提示首行应留在 row1，实际 %q", got)
		}
		if g.totalRows() != 5 {
			t.Fatalf("缺陷下正文不应写到 row5，网格应只有 5 行，实际 %d", g.totalRows())
		}

		// 同一份字节流喂给不同宽度：提示折行数随宽度变，CUU(2) 落点 = fold-1。
		// fold>=2（<125 列）时正文覆盖提示行且末行残留；125 列恰好 1 行时
		// 落在空行、散落不可见——与 §2「>=126 列才正常」的可达面定性一致。
		for _, width := range []int{24, 80, 125, 200} {
			fold := (125 + width - 1) / width
			gg := newTerminalGrid(width)
			gg.feed(out)
			br := gg.findRow("回复正文你好")
			if br != fold-1 {
				t.Errorf("%d 列（折 %d 行）正文应落 row%d，实际 row%d", width, fold, fold-1, br)
			}
			if fold >= 2 && gg.rowText(fold) == "" {
				t.Errorf("%d 列末提示行 row%d 应残留碎片，实际为空", width, fold)
			}
		}
	} else {
		// 方案 B 断言：无 CUU，正文紧随提示落在末行，提示折行完整无残留。
		if len(cuu) != 0 {
			t.Fatalf("B 路径应无 CUU，实际 %v", cuu)
		}
		if bodyRow != g.totalRows()-1 {
			t.Fatalf("B 路径正文应落在提示之后的末行，实际 row%d/%d", bodyRow, g.totalRows())
		}
		// 正文整段独占末行；提示折行完整、无正文碎片散落到提示中间行。
		if got := g.rowText(bodyRow); got != "回复正文你好" {
			t.Fatalf("B 路径末行正文应完整独占，实际 %q", got)
		}
		for r := 1; r < bodyRow; r++ {
			got := g.rowText(r)
			if got == "" {
				t.Fatalf("提示折行 row%d 不应为空（提示内容应完整）", r)
			}
			if strings.Contains(got, "回复正文") {
				t.Fatalf("提示行 row%d 不应残留正文碎片，实际 %q", r, got)
			}
		}
	}
}

// TestMergeBackspaceWideLineNoResidue 验证 B7（W10）：宽度已知时 mergeBackspace
// 按上一逻辑行占用的物理行数发 CUU，超宽行合并后无上方物理行残留。
// 旧的 CUU(1) 会跳进折行中间，本测试即转红。
func TestMergeBackspaceWideLineNoResidue(t *testing.T) {
	const width = 8
	var out bytes.Buffer
	e := newLineEchoWithWidth(&out, width)
	line := "甲乙丙丁戊" // 10 列：row0 占满 8 列 pending，戊折到 row1
	e.paste(line)
	e.newline() // \r\n → row2（空行行首）
	e.mergeBackspace(line)

	g := newTerminalGrid(width)
	g.feed(out.String())
	if got := g.rowText(0); got != "甲乙丙丁" {
		t.Fatalf("合并后 row0 应为上一逻辑行首物理行，实际 %q", got)
	}
	if got := g.rowText(1); got != "戊" {
		t.Fatalf("合并后 row1 应完整承接尾段，实际 %q", got)
	}
	if g.totalRows() != 2 {
		t.Fatalf("超宽行合并后不应残留多余物理行，实际共 %d 行（输出 %q）", g.totalRows(), out.String())
	}
}

// TestBackspaceAcrossFoldedLine 验证超宽末行退格删空该物理行后，光标按物理行
// 归位到上一物理行末；同时守住 v0.8.3 的单字退格精度（宽字 2 组 \b \b）。
func TestBackspaceAcrossFoldedLine(t *testing.T) {
	const width = 8
	var out bytes.Buffer
	e := newLineEchoWithWidth(&out, width)
	e.paste("甲乙丙丁戊")  // row0: 甲乙丙丁(8 列 pending)，row1: 戊
	e.backspace(true) // 删戊 → row1 擦空，光标应回 row0 末

	g := newTerminalGrid(width)
	g.feed(out.String())
	if got := g.rowText(0); got != "甲乙丙丁" {
		t.Fatalf("上一物理行内容不应被退格破坏，实际 %q", got)
	}
	if got := g.rowText(1); got != "" {
		t.Fatalf("末物理行应被擦空，实际 %q", got)
	}
	if len(e.cols) != 4 {
		t.Fatalf("退格后列宽栈应剩 4 项，实际 %v", e.cols)
	}
	if !strings.Contains(out.String(), "\x1b[1A") {
		t.Fatalf("跨物理行归位应发白名单 CUU(1)，实际 %q", out.String())
	}
}
