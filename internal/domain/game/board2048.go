package game

import (
	"math/rand/v2"
	"strconv"
	"strings"
)

// 2048 棋盘：4×4，值为 2 的幂，0 表示空格。
const boardSize = 4

type board2048 struct {
	grid [boardSize][boardSize]int
}

func newBoard2048() *board2048 {
	b := &board2048{}
	b.spawn()
	b.spawn()
	return b
}

// spawn 在随机空格补一个 2（90%）或 4（10%）。无空格则不补。
func (b *board2048) spawn() {
	type pos struct{ r, c int }
	var empty []pos
	for r := 0; r < boardSize; r++ {
		for c := 0; c < boardSize; c++ {
			if b.grid[r][c] == 0 {
				empty = append(empty, pos{r, c})
			}
		}
	}
	if len(empty) == 0 {
		return
	}
	p := empty[rand.IntN(len(empty))]
	if rand.IntN(10) == 0 {
		b.grid[p.r][p.c] = 4
	} else {
		b.grid[p.r][p.c] = 2
	}
}

// move 按方向压缩合并一行/列。返回本次合并得分与是否有效移动。
// 每个数字每步最多合并一次（标准 2048 规则）。
func (b *board2048) move(d Direction) (int, bool) {
	var gained int
	var changed bool
	for i := 0; i < boardSize; i++ {
		line := b.line(i, d)
		merged, score := mergeLine(line)
		if score > 0 {
			gained += score
		}
		for j, v := range merged {
			if b.cells_at(i, j, d) != v {
				changed = true
			}
			b.set_at(i, j, d, v)
		}
	}
	if changed {
		b.spawn()
	}
	return gained, changed
}

// line 取第 i 条线（行或列），按移动方向的**起始端优先**排列。
func (b *board2048) line(i int, d Direction) [boardSize]int {
	var out [boardSize]int
	for j := 0; j < boardSize; j++ {
		switch d {
		case DirLeft:
			out[j] = b.grid[i][j]
		case DirRight:
			out[j] = b.grid[i][boardSize-1-j]
		case DirUp:
			out[j] = b.grid[j][i]
		case DirDown:
			out[j] = b.grid[boardSize-1-j][i]
		}
	}
	return out
}

func (b *board2048) cells_at(i, j int, d Direction) int {
	return b.line(i, d)[j]
}

func (b *board2048) set_at(i, j int, d Direction, v int) {
	switch d {
	case DirLeft:
		b.grid[i][j] = v
	case DirRight:
		b.grid[i][boardSize-1-j] = v
	case DirUp:
		b.grid[j][i] = v
	case DirDown:
		b.grid[boardSize-1-j][i] = v
	}
}

// mergeLine 压缩并合并一条线（起始端优先），返回新线与本次得分。
func mergeLine(in [boardSize]int) ([boardSize]int, int) {
	var vals []int
	for _, v := range in {
		if v != 0 {
			vals = append(vals, v)
		}
	}
	var out [boardSize]int
	var score int
	k := 0
	for i := 0; i < len(vals); i++ {
		if i+1 < len(vals) && vals[i] == vals[i+1] {
			out[k] = vals[i] * 2
			score += out[k]
			i++
		} else {
			out[k] = vals[i]
		}
		k++
	}
	return out, score
}

func (b *board2048) full() bool {
	for r := 0; r < boardSize; r++ {
		for c := 0; c < boardSize; c++ {
			if b.grid[r][c] == 0 {
				return false
			}
		}
	}
	return true
}

func (b *board2048) hasMoves() bool {
	for r := 0; r < boardSize; r++ {
		for c := 0; c < boardSize; c++ {
			v := b.grid[r][c]
			if c+1 < boardSize && v == b.grid[r][c+1] {
				return true
			}
			if r+1 < boardSize && v == b.grid[r+1][c] {
				return true
			}
		}
	}
	return false
}

// cellWidth 是 2048 每格的固定列宽（4 列右对齐）。4 格共 16 列，
// 加上行首缩进在 80×24 常见终端内宽裕，避免 ADR-076 的窄屏折行坑。
const cellWidth = 4

// cells 产出纯数据帧：数字转 4 列右对齐 ASCII 文本（0 用 '·' 单字符占位），
// 色阶按值分档。只用可打印 ASCII，规避 ADR-076 的片假名字形坑。
func (b *board2048) cells() [][]Cell {
	rows := make([][]Cell, boardSize)
	for r := 0; r < boardSize; r++ {
		rows[r] = make([]Cell, boardSize)
		for c := 0; c < boardSize; c++ {
			v := b.grid[r][c]
			if v == 0 {
				rows[r][c] = Cell{Text: "   .", Ink: InkBoard}
				continue
			}
			ink := InkValue
			if v >= 128 {
				ink = InkMerged
			}
			rows[r][c] = Cell{Text: padLeft(strconv.Itoa(v), cellWidth), Ink: ink}
		}
	}
	return rows
}

// padLeft 左填充空格到固定宽度（超宽直接截断为 '#' + 尾三位，2048 正常值不会触发）。
func padLeft(s string, width int) string {
	if len(s) >= width {
		if len(s) > width {
			return "#" + s[len(s)-(width-1):]
		}
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}
