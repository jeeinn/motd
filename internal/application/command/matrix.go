package command

// /matrix 雨幕（v0.9.2 / ADR-076）。产出「字符 + 色阶」的纯数据，**不含任何
// ESC**：颜色字节由 presentation/terminal 层组装（ADR-027 信任域与分层约束——
// application 不 import presentation）。
//
// v0.9.2 的三处修正：
//  1. 字符池从半角片假名换成纯 ASCII（英文大小写 + 数字 + _ + |）。半角片假名
//     在 Windows Terminal 默认字体无字形，回落字体会被放大成全角 → 整行字符
//     网格错位；ASCII 池每字 1 字节 1 列，跨终端零差异，单帧字节下降约 29%。
//  2. 雨帘宽度跟随终端宽度（此前硬编码 60 列）：窄终端（<60 列）会物理折行，
//     每帧变成 2 个物理行，帧数假设与退出复原全部错位。
//  3. 雨幕改为「每列一个雨头 + 拖尾」的状态机：追加式（每帧写一行、靠终端
//     滚屏）无法回改已打印行，做不出电影 Matrix 的雨头拖尾，且不限时时会以
//     每秒十几行的速度把用户的 scrollback 历史冲掉。

import "math/rand/v2"

// matrixGlyphs 是雨帘字符池：英文大小写 + 数字 + 下划线 + 竖线。
//
// 选 ASCII 而不是符号字形有两个理由：字形在所有等宽终端都存在（不依赖字体
// 回落），以及每字恒为 1 字节 1 列，雨帘宽度可精确等于终端列数。
var matrixGlyphs = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_|")

// 拖尾长度的随机区间（行）。拖尾越长雨幕越"稠"，短拖尾更像断续的雨点。
const (
	matrixTailMin = 4
	matrixTailMax = 16
)

// MatrixTier 是雨帘格子的色阶。渲染层据此选 SGR；本包不产 ESC。
//
// 只有两档：暗绿（底色与拖尾共用）与亮绿（雨头）。**空格也归入暗绿档**——
// 空格的着色在视觉上完全不可见，却能把每行的色阶跳变从几十次压到个位数：
// 若空格单独归为「不着色」，一行里随机分布的雨柱会切出几十个色段，每段两次
// SGR 跳变，单帧字节会从约 2.5 KB 抬到 10 KB 以上（12fps 即超整站 1 Mbps
// 上行的九成）。这是带宽倒推出来的设计，不是偷懒。
type MatrixTier uint8

const (
	MatrixTail MatrixTier = iota // 暗绿：底色（含空格）与拖尾
	MatrixHead                   // 亮绿：雨头
)

// MatrixCell 是雨幕的一个格子：字符 + 色阶。字符恒为 ASCII 可打印字符或空格。
type MatrixCell struct {
	Rune rune
	Tier MatrixTier
}

// MatrixRain 是雨幕状态机：每列一个雨头往下推进，落地后随机等待再从头进入；
// 雨头上方若干格是拖尾（字符在下落过程中保持稳定，形成一串而非闪烁的噪点）。
//
// **每会话一份**，由 cmd 层持有；本包只管状态与纯数据，不碰 IO。
type MatrixRain struct {
	width   int
	height  int
	chars   [][]rune // [行][列] 该格字符（生成后稳定，直到被覆盖或清空）
	born    [][]int  // [行][列] 生成时的帧号；-1 = 从未生成
	heads   []int    // [列] 雨头所在行；-1 = 该列本轮已落地、等待重启
	tails   []int    // [列] 拖尾长度
	wait    []int    // [列] 落地后还需等待的帧数
	rainCol []bool   // [列] 该列是否为"雨柱"（有雨头与拖尾）
	frame   int
}

// NewMatrixRain 造一个 width × height 的雨幕状态机。
//
// width/height 至少为 1；调用方（cmd 层）负责按终端尺寸夹取与兜底。
func NewMatrixRain(width, height int) *MatrixRain {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	r := &MatrixRain{
		width:   width,
		height:  height,
		chars:   make([][]rune, height),
		born:    make([][]int, height),
		heads:   make([]int, width),
		tails:   make([]int, width),
		wait:    make([]int, width),
		rainCol: make([]bool, width),
		frame:   0,
	}
	for y := range r.chars {
		r.chars[y] = make([]rune, width)
		r.born[y] = make([]int, width)
		for x := range r.born[y] {
			r.born[y][x] = -1
		}
	}
	for x := range r.heads {
		r.heads[x] = -1
		r.tails[x] = matrixTailMin + rand.IntN(matrixTailMax-matrixTailMin+1)
		// 雨柱隔列分布：雨帘铺满整个宽度，且每行的亮头数量约为列数的一半
		// （每行 ≈ 雨柱数 / 雨区行数），既保持"整屏都是雨"的观感，又把每帧的
		// 色阶跳变控制在个位数。
		r.rainCol[x] = x%2 == 0
	}
	return r
}

// Width 返回雨幕列数。
func (r *MatrixRain) Width() int { return r.width }

// Height 返回雨幕行数。
func (r *MatrixRain) Height() int { return r.height }

// Advance 推进一帧：雨头下移一格并写入新字符；落地后该列随机等待再从顶部重启。
//
// 重启时机的随机等待是雨幕"参差感"的来源——各列不同步落地，视觉上才是雨帘
// 而不是整齐的方阵。
func (r *MatrixRain) Advance() {
	r.frame++
	for x := 0; x < r.width; x++ {
		switch {
		case r.heads[x] >= 0:
			r.heads[x]++
			if r.heads[x] >= r.height {
				// 落地：整列（含拖尾）清空，进入随机等待。
				r.heads[x] = -1
				r.wait[x] = rand.IntN(r.height/2 + 1)
				for y := 0; y < r.height; y++ {
					r.born[y][x] = -1
				}
				continue
			}
			r.chars[r.heads[x]][x] = matrixGlyphs[rand.IntN(len(matrixGlyphs))]
			r.born[r.heads[x]][x] = r.frame
		case r.wait[x] > 0:
			r.wait[x]--
		default:
			// 等待结束：从顶部重新进入，拖尾长度重新随机。
			r.heads[x] = 0
			r.tails[x] = matrixTailMin + rand.IntN(matrixTailMax-matrixTailMin+1)
			r.chars[0][x] = matrixGlyphs[rand.IntN(len(matrixGlyphs))]
			r.born[0][x] = r.frame
		}
	}
}

// Row 返回第 y 行的格子切片。
//
// 色阶判定：雨头所在格为 MatrixHead；雨头上方 tails[x] 格内、且已生成过字符的
// 为 MatrixTail（拖尾）；其余为 MatrixBlank（空白）。
func (r *MatrixRain) Row(y int) []MatrixCell {
	out := make([]MatrixCell, r.width)
	if y < 0 || y >= r.height {
		return out
	}
	for x := 0; x < r.width; x++ {
		// 空格也归入暗绿档：视觉上无差别，却能把每行的色阶跳变压到个位数。
		cell := MatrixCell{Rune: ' ', Tier: MatrixTail}
		if r.rainCol[x] {
			h := r.heads[x]
			if h >= 0 && r.born[y][x] >= 0 {
				switch {
				case y == h:
					cell.Rune = r.chars[y][x]
					cell.Tier = MatrixHead
				case y < h && h-y <= r.tails[x]:
					cell.Rune = r.chars[y][x]
					cell.Tier = MatrixTail
				}
			}
		}
		out[x] = cell
	}
	return out
}
