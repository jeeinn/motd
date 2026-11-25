package command

// v0.9.2 / ADR-076：/matrix 雨幕状态机与字符池回归。

import "testing"

// 字符池必须是纯 ASCII 的英文大小写 + 数字 + 下划线 + 竖线。
//
// 换池的根本理由不是审美：半角片假名（U+FF61..）在 Windows Terminal 默认字体
// 无字形，回落字体会被放大成全角 → 整行字符网格错位。ASCII 池每字 1 字节
// 1 列，跨终端零差异。
func TestMatrixGlyphsAreASCIISafe(t *testing.T) {
	allowed := make(map[rune]bool, 64)
	for _, r := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_|" {
		allowed[r] = true
	}
	if len(matrixGlyphs) != 64 {
		t.Errorf("字符池大小应为 64，实际 %d", len(matrixGlyphs))
	}
	for _, r := range matrixGlyphs {
		if !allowed[r] {
			t.Errorf("字符池含非法字符 %q（U+%04X）", r, r)
		}
		if r < 0x20 || r > 0x7e {
			t.Errorf("字符池含非可打印 ASCII: %q", r)
		}
	}
}

// 行宽必须精确等于传入宽度——雨帘宽度跟随终端宽度，不能再硬编码 60 列
// （窄终端会物理折行，每帧变成 2 个物理行，帧数假设与退出复原全错位）。
func TestMatrixRainRowWidthMatches(t *testing.T) {
	rain := NewMatrixRain(37, 4)
	if rain.Width() != 37 || rain.Height() != 4 {
		t.Fatalf("尺寸不符: %dx%d", rain.Width(), rain.Height())
	}
	for y := 0; y < rain.Height(); y++ {
		if got := len(rain.Row(y)); got != 37 {
			t.Fatalf("第 %d 行宽度应为 37，实际 %d", y, got)
		}
	}
}

// 初始帧必须全是空格：雨头要等各列随机等待结束后才进入。
//
// 注意空格也归入暗绿底色档（MatrixTail）——这是带宽设计，不是"未着色"；
// 渲染层据此把每行的色阶跳变压到个位数。
func TestMatrixRainStartsBlank(t *testing.T) {
	rain := NewMatrixRain(20, 5)
	for y := 0; y < rain.Height(); y++ {
		for _, cell := range rain.Row(y) {
			if cell.Tier != MatrixTail {
				t.Fatalf("初始帧应全为暗绿底色档，第 %d 行出现 %v", y, cell.Tier)
			}
			if cell.Rune != ' ' {
				t.Fatalf("空白格必须是空格，第 %d 行出现 %q", y, cell.Rune)
			}
		}
	}
}

// 雨头逐帧下移，拖尾紧随其上方——这是电影 Matrix 观感的核心结构。
func TestMatrixRainHeadAdvancesWithTailAbove(t *testing.T) {
	rain := NewMatrixRain(1, 10)
	headRow := -1
	for i := 0; i < 80; i++ {
		rain.Advance()
		headRow = matrixHeadRow(rain)
		if headRow >= 3 {
			break
		}
	}
	if headRow < 3 {
		t.Fatalf("应能推进出位于中下部的雨头，实际行 %d", headRow)
	}
	head := rain.Row(headRow)[0]
	if head.Tier != MatrixHead {
		t.Fatalf("雨头行色阶应为 head，实际 %v", head.Tier)
	}
	tail := rain.Row(headRow - 1)[0]
	if tail.Tier != MatrixTail || tail.Rune == ' ' {
		t.Fatalf("雨头正上方应为带字符的拖尾，实际 tier=%v rune=%q", tail.Tier, tail.Rune)
	}
	if head.Rune == 0 || tail.Rune == 0 {
		t.Fatal("雨头与拖尾都应带字符")
	}
}

// 拖尾的字符在下落过程中必须保持稳定（每帧重摇会变成闪烁的噪点）。
//
// 判据：某个格在上一帧是拖尾、本帧仍是拖尾时，字符必须一致；变成雨头
// （雨头会写入新字符）或变空白则允许变化。
func TestMatrixRainTailCharIsStable(t *testing.T) {
	rain := NewMatrixRain(1, 10)
	prev := map[int]rune{}
	for i := 0; i < 80; i++ {
		rain.Advance()
		cur := map[int]rune{}
		for y := 0; y < rain.Height(); y++ {
			// 只有"带字符的暗绿格"才是拖尾；空格虽同属暗绿档，但它是底色。
			if cell := rain.Row(y)[0]; cell.Tier == MatrixTail && cell.Rune != ' ' {
				cur[y] = cell.Rune
			}
		}
		for y, r := range cur {
			if old, ok := prev[y]; ok && old != r {
				t.Fatalf("仍是拖尾的格字符不应变化（第 %d 帧第 %d 行）: %q → %q", i, y, old, r)
			}
		}
		prev = cur
	}
}

// 雨头落地后该列（含拖尾）必须整列清空，并进入随机等待。
func TestMatrixRainClearsColumnOnLanding(t *testing.T) {
	rain := NewMatrixRain(1, 3)
	sawLanding := false
	for i := 0; i < 60 && !sawLanding; i++ {
		rain.Advance()
		if matrixHeadRow(rain) < 0 {
			sawLanding = true
			for y := 0; y < rain.Height(); y++ {
				for _, cell := range rain.Row(y) {
					if cell.Rune != ' ' {
						t.Fatalf("落地等待期该列应全空白，第 %d 行 %q", y, cell.Rune)
					}
				}
			}
		}
	}
	if !sawLanding {
		t.Fatal("多帧内应至少发生一次落地")
	}
}

// 任何帧的格子都不得含控制字符或非 ASCII（安全渲染硬约束）。
func TestMatrixRainCellsArePrintableASCII(t *testing.T) {
	rain := NewMatrixRain(40, 8)
	for i := 0; i < 20; i++ {
		rain.Advance()
		for y := 0; y < rain.Height(); y++ {
			for _, cell := range rain.Row(y) {
				if cell.Rune < 0x20 || cell.Rune > 0x7e {
					t.Fatalf("雨帘含非可打印 ASCII: %q（U+%04X）", cell.Rune, cell.Rune)
				}
			}
		}
	}
}

// matrixHeadRow 返回雨头的行号；没有雨头时返回 -1。
func matrixHeadRow(r *MatrixRain) int {
	for y := 0; y < r.Height(); y++ {
		for _, cell := range r.Row(y) {
			if cell.Tier == MatrixHead {
				return y
			}
		}
	}
	return -1
}
