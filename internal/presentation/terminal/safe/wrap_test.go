package safe

// W8 夹具自证：逻辑行→物理行映射在中/英/emoji、pending wrap、宽字符整字折、
// SGR 忽略、宽度未知降级等场景下的行数与折行点（DESIGN-v090 §5.1）。

import (
	"strings"
	"testing"
)

func TestMeasureLineWidthsAndBreaks(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		width       int
		wantWidth   int
		wantRows    int
		wantBreaks  []int
		wantPending bool
	}{
		{"ascii exact", "abcd", 4, 4, 1, nil, true}, // 恰好占满末列 → pending，不立即折
		{"ascii folds", "abcde", 4, 5, 2, []int{4}, false},
		{"cjk exact", "中文", 4, 4, 1, nil, true},
		{"cjk fold as unit", "A乙丙", 4, 5, 2, []int{2}, false}, // 乙占 1-2 列，丙 2 列放不下整字折
		{"emoji exact", "😀😀", 4, 4, 1, nil, true},
		{"emoji folds", "😀😀😀", 4, 6, 2, []int{2}, false},
		{"wide then pending", "甲乙", 4, 4, 1, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MeasureLine(tc.text, tc.width)
			if got.Width != tc.wantWidth || got.Rows != tc.wantRows || got.Pending != tc.wantPending {
				t.Fatalf("MeasureLine(%q,%d) = width=%d rows=%d pending=%v breaks=%v；want width=%d rows=%d pending=%v breaks=%v",
					tc.text, tc.width, got.Width, got.Rows, got.Pending, got.Breaks,
					tc.wantWidth, tc.wantRows, tc.wantPending, tc.wantBreaks)
			}
			if len(got.Breaks) != len(tc.wantBreaks) {
				t.Fatalf("折行点数不符: got %v want %v", got.Breaks, tc.wantBreaks)
			}
			for i := range tc.wantBreaks {
				if got.Breaks[i] != tc.wantBreaks[i] {
					t.Fatalf("折行点[%d] 不符: got %v want %v", i, got.Breaks, tc.wantBreaks)
				}
			}
		})
	}
}

func TestMeasureWrapMultipleLines(t *testing.T) {
	info := MeasureWrap("abcd\ne", 4)
	if info.Rows != 2 || len(info.Lines) != 2 {
		t.Fatalf("两逻辑行应共占 2 个物理行，实际 rows=%d lines=%d", info.Rows, len(info.Lines))
	}
	if info.OverWide {
		t.Fatal("各逻辑行宽度均未超过 4，不应判超宽")
	}
	if !info.Lines[0].Pending {
		t.Fatal("首逻辑行恰好占满末列，pending 应为 true")
	}
	if info.Lines[1].Pending {
		t.Fatal("第二行仅 1 列，pending 应为 false")
	}
}

func TestMeasureWrapCRLFTail(t *testing.T) {
	// 末尾 \r\n 不产生额外逻辑行；行尾 \r 不得在 pending 状态下触发多余折行。
	info := MeasureWrap("abcd\r\n", 4)
	if info.Rows != 1 || len(info.Lines) != 1 {
		t.Fatalf("单行 CRLF 文本应占 1 个物理行，实际 rows=%d lines=%d", info.Rows, len(info.Lines))
	}
	if !info.Lines[0].Pending {
		t.Fatal("行尾 \r 不应清掉占满末列的 pending 状态")
	}
	if got := MeasureWrap("abcd\r\ne\r\n", 4); got.Rows != 2 || len(got.Lines) != 2 {
		t.Fatalf("两行 CRLF 文本应占 2 个物理行，实际 %+v", got)
	}
}

func TestOverWideAndSGRIgnored(t *testing.T) {
	if !OverWide("abcde", 4) {
		t.Fatal("5 列文本在 4 列终端应判超宽")
	}
	if OverWide("abcd", 4) {
		t.Fatal("恰好 4 列不算超宽（pending 由终端处理，不改变行数）")
	}
	// SGR 序列不计列：拼接避免 ESC 字面量（守卫只允许 ansi.go 出现）。
	esc := string(rune(0x1b))
	s := esc + "[2m" + strings.Repeat("a", 4) + esc + "[0m"
	if OverWide(s, 4) {
		t.Fatalf("SGR 应按 0 列计，4 个 a 在 4 列终端不应超宽: %q", s)
	}
	if got := MeasureLine(s, 4); got.Width != 4 || got.Rows != 1 {
		t.Fatalf("含 SGR 的 4 列文本应 width=4 rows=1，实际 %+v", got)
	}
}

func TestMeasureLineUnknownWidthDegrades(t *testing.T) {
	got := MeasureLine("abcde", 0)
	if got.Width != 5 || got.Rows != 1 || got.Pending || len(got.Breaks) != 0 {
		t.Fatalf("宽度未知应单行不折且宽度如实累加，实际 %+v", got)
	}
	if OverWide("abcde", 0) {
		t.Fatal("宽度未知时不得判超宽")
	}
}
