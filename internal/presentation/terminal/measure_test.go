package terminal

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// R-06 行高测量的口径测试：测量结果必须与渲染逐段一致，
// 否则分页装箱会把一页撑出屏幕（多一行就足以把标题顶出可视区）。

func TestContentWidth(t *testing.T) {
	cases := []struct {
		width, want int
	}{
		{80, 68},  // 80 - prefixCol(12)
		{40, 28},  // 40 - 12
		{20, 20},  // 20-12=8 < minContentCol(20)，抬到下限
		{0, 0},    // 宽度未知：返回 0，调用方降级
	}
	for _, c := range cases {
		if got := ContentWidth(c.width); got != c.want {
			t.Errorf("ContentWidth(%d) = %d，期望 %d", c.width, got, c.want)
		}
	}
}

func TestBodyLines(t *testing.T) {
	if got := BodyLines("", 80); got != 0 {
		t.Fatalf("空正文应为 0 行，实际 %d", got)
	}
	if got := BodyLines("短正文", 80); got != 1 {
		t.Fatalf("短正文应为 1 行，实际 %d", got)
	}
	if got := BodyLines("短正文", 0); got != 0 {
		t.Fatalf("宽度未知时应返回 0（调用方降级），实际 %d", got)
	}
	// 100 个中文字符 = 200 列；80 列终端每行 40 字 → 3 行。
	// 这条断言同时守「宽字符整字折、不拆半字」的口径：若按字节或按 rune 折，
	// 行数会变成 2 或 100，分页就会整体错位。
	if got := BodyLines(strings.Repeat("中", 100), 80); got != 3 {
		t.Fatalf("100 个中文字符在 80 列下应为 3 行，实际 %d", got)
	}
	// 显式换行也计入行数。
	if got := BodyLines("第一行\n第二行\n第三行", 80); got != 3 {
		t.Fatalf("三行正文应为 3 行，实际 %d", got)
	}
}

func TestRowsLines(t *testing.T) {
	if got := RowsLines(nil, viewmodel.ScreenThreadDetail, 80); got != nil {
		t.Fatalf("空行表应返回 nil，实际 %v", got)
	}
	rows := []viewmodel.Row{
		{Index: 1, ID: 10, Body: "短回复", Meta: viewmodel.RowMeta{Author: "alice", CreatedAt: 1700000000}},
		{Index: 2, ID: 11, Body: strings.Repeat("中", 100), Meta: viewmodel.RowMeta{Author: "bob", CreatedAt: 1700000000}},
	}
	got := RowsLines(rows, viewmodel.ScreenThreadDetail, 80)
	if len(got) != len(rows) {
		t.Fatalf("返回长度应等于行数 %d，实际 %d", len(rows), len(got))
	}
	if got[0] != 1 {
		t.Fatalf("短回复应占 1 行，实际 %d", got[0])
	}
	if got[1] < 3 {
		t.Fatalf("100 字回复在 80 列下至少 3 行（contentWidth=68 时更多），实际 %d", got[1])
	}
	for i, n := range got {
		if n < 1 {
			t.Fatalf("第 %d 行行高不应小于 1，实际 %d（零高度会让装箱把整页挤爆）", i, n)
		}
	}
}

func TestRowsLinesNeverZeroOnEmptyBody(t *testing.T) {
	// 正文为空但有元信息：行仍然占 1 行，不能被算成 0 行。
	rows := []viewmodel.Row{{Index: 1, ID: 7, Meta: viewmodel.RowMeta{Author: "carol"}}}
	got := RowsLines(rows, viewmodel.ScreenThreadDetail, 80)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("空正文行应占 1 行，实际 %v", got)
	}
}
