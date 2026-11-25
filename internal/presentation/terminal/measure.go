package terminal

// 行高测量（R-06 详情屏分页装箱用）。
//
// 为什么需要它：分页要把「正文 + 回复」按终端高度装箱，而一页能装多少取决于
// 每块内容**渲染后**占几行。渲染把 Markdown/SGR/自动折行都算进去，只有本层
// 知道口径，因此测量函数必须放在 presentation，供上层（cmd）调用。
//
// 单一口径约定：这里的所有计算都复用 RenderScreenWithLocale 用到的同一批
// 辅助函数（prefixCol / metaID / authorSegment / metaTail / RenderUser /
// MeasureWrap），避免出现「测量一套、渲染另一套」——后者会让分页错位一至数行。
//
// 纯函数：零 IO、零 ANSI 输出、不读时钟，可表驱动测试。

import (
	"fmt"
	"strings"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// ContentWidth 返回行内容的可用列数：总宽减去行前缀预留列。
// width <= 0 时返回 0，调用方据此降级（不要拿 0 去折行）。
func ContentWidth(width int) int {
	if width <= 0 {
		return 0
	}
	cw := width - prefixCol
	if cw < minContentCol {
		cw = minContentCol
	}
	return cw
}

// BodyLines 返回详情屏正文在 width 下占用的物理行数。
//
// 口径与 view.go 一致：正文按**整宽**渲染（`ext(&b, s.Detail.Body, width)`），
// 与标题、行表用的 contentWidth 不同，别混用。
// RenderUser 会先按 width 折行并按行渲染 SGR，因此渲染结果里「一行 = 一个物理行」，
// 折行信息由 MeasureWrap 统一给出（width<=0 时 Rows=1，配合空正文返回 0）。
func BodyLines(body string, width int) int {
	if body == "" || width <= 0 {
		return 0
	}
	rendered := safe.RenderUser(body, safe.Options{Width: width})
	return safe.MeasureWrap(rendered, width).Rows
}

// RowsLines 返回每个行在 width 下占用的物理行数（与 rows 等长）。
//
// 拼装顺序必须与 view.go 的渲染循环逐段一致：前缀 → 回复数（仅主题列表）
// → 正文/标题 → id= → 作者 → 尾部元信息 → 版块 slug。
// 前缀宽度固定为 2 列标记（"> " 或 "  "），故这里用两个空格测量——标记只换字符不换列宽。
func RowsLines(rows []viewmodel.Row, kind viewmodel.ScreenKind, width int) []int {
	if len(rows) == 0 || width <= 0 {
		return nil
	}
	cw := ContentWidth(width)
	out := make([]int, len(rows))
	for i, r := range rows {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("  %2d. ", r.Index))
		if kind == viewmodel.ScreenThreads {
			b.WriteString(fmt.Sprintf("[%d] ", r.Score))
		}
		text := r.Title
		if text == "" {
			text = r.Body
		}
		b.WriteString(safe.RenderUser(text, safe.Options{Width: cw}))
		b.WriteString(metaID(r, kind))
		b.WriteString(authorSegment(r))
		b.WriteString(metaTail(r, kind))
		b.WriteString(r.Meta.Node)
		out[i] = safe.MeasureWrap(b.String(), width).Rows
	}
	return out
}
