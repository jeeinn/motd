package terminal

// /matrix 屏内雨帘的帧组装（v0.9.2 / ADR-076）。
//
// 帧结构（与 ADR-071 决策 2、ADR-072 决策 3 同源）：
//   - 进入：CursorHome + ClearScreen → 第 1 行固定提示 → 雨区铺满（第 2..H 行）；
//   - 每帧：\r + CursorUp 回雨区首行 → 逐行重写；**屏幕底行不写 CRLF**，否则
//     触发终端滚屏把雨区顶出可视区，并让 scrollback 以每秒十几行增长；
//   - 退出：CursorHome + ClearScreen + Reset，调用方随后整屏重绘原页面。
//
// 为什么是"屏内覆盖"而不是"逐行追加"：
//   - 追加式（每帧写一行、靠终端滚屏）无法回改已打印行，做不出电影 Matrix 的
//     雨头 + 拖尾；且不限时会把用户的 scrollback 历史冲掉（2J 只清可视屏）。
//   - 屏内覆盖只重画可视区，scrollback 零增长；代价是每帧要写整屏（约 2~3 KB）。
//
// SGR 跨行保持（跳变式）：只在色阶跳变处发序列。逐字符上色会把单帧字节抬到
// 万字节级（12fps 即超整站 1 Mbps 上行的 100%），因此电影感只能靠"雨头 + 拖尾"
// 的两档色阶实现——这正是用户拍板的 16 色方案。

import (
	"strings"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// MatrixRows 是雨区的一帧：按行给出格子（纯数据，来自 application/command）。
type MatrixRows [][]command.MatrixCell

// RenderMatrixScreen 组装"进入整屏"：清屏 + 复位 SGR + 提示行 + 雨区首帧。
//
// hint 是第 1 行的固定提示（服务端常量，不含用户数据）。
func RenderMatrixScreen(rows MatrixRows, hint string) string {
	var b strings.Builder
	b.WriteString(safe.CursorHome())
	b.WriteString(safe.ClearScreen())
	b.WriteString(safe.ResetInk())
	b.WriteString(hint)
	b.WriteString("\r\n")
	cur := -1 // -1 = 尚未上色，首格必发一次 SGR
	writeMatrixRows(&b, rows, &cur)
	return b.String()
}

// RenderMatrixFrame 组装一帧覆盖：回雨区首行后逐行重写。
//
// rainHeight 是雨区行数（= 终端高度 - 1，第 1 行留给提示）。光标停在屏幕底行
// 行尾，需要上移 rainHeight-1 行才回到雨区首行。
//
// 帧首的 "\r" 不能省：光标在屏幕底行行尾处于 pending-wrap 状态，先回车再上移
// 才能保证列位置确定（各行内容宽度一致，理论上已回车，但回车成本仅 1 字节）。
func RenderMatrixFrame(rows MatrixRows, rainHeight int) string {
	var b strings.Builder
	b.WriteString("\r")
	b.WriteString(safe.CursorUp(rainHeight - 1))
	cur := -1 // -1 = 尚未上色，首格必发一次 SGR
	writeMatrixRows(&b, rows, &cur)
	return b.String()
}

// RenderMatrixExit 组装退出收口：清屏 + 复位 SGR。
//
// 必须显式复位：ED 2（\x1b[2J）只清可视区，**不重置 SGR**，不补这一次复位，
// 亮绿会漏到调用方随后重绘的整页论坛上。
func RenderMatrixExit() string {
	return safe.CursorHome() + safe.ClearScreen() + safe.ResetInk()
}

// writeMatrixRows 逐行写出雨区内容；最后一行不写 CRLF（避免触发终端滚屏）。
//
// cur 是跨行保持的 SGR 状态：只有色阶跳变处才发序列，行内连续同色只发一次。
// 初值 -1 表示"尚未上色"，首格必发一次（进入时已显式复位过）。
func writeMatrixRows(b *strings.Builder, rows MatrixRows, cur *int) {
	for y, row := range rows {
		b.WriteString("\r")
		for _, cell := range row {
			tier := int(cell.Tier)
			if tier != *cur {
				switch cell.Tier {
				case command.MatrixHead:
					b.WriteString(safe.MatrixHeadInk())
				default:
					b.WriteString(safe.MatrixTailInk())
				}
				*cur = tier
			}
			b.WriteRune(cell.Rune)
		}
		if y < len(rows)-1 {
			b.WriteString("\r\n")
		}
	}
}
