package command

// /matrix 滚动雨帘（v0.6.0）。用 SGR 绿色 + 固定字符集逐行输出，
// 不使用被 ADR-027 禁止的 ED 清屏 / CUP 光标定位 / 备用屏序列。
// 这里只生成「一帧的字符内容」（纯字符串，无 ESC），帧率与输出由 cmd 层控制。
// 所有字符来自固定白名单，不含任何用户数据或控制字符。

import (
	"math/rand/v2"
	"strings"
)

const matrixWidth = 60 // 每行字符数（固定，不接收用户输入）

var matrixGlyphs = []rune("01ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜｦﾝ")

// MatrixFrame 生成一行随机字符雨（不含 ESC、不含用户数据）。
func MatrixFrame() string {
	var b strings.Builder
	b.Grow(matrixWidth)
	for i := 0; i < matrixWidth; i++ {
		// 大部分位置是空隙，少部分是亮字符——形成稀疏雨帘而非实心块。
		if rand.IntN(4) == 0 {
			b.WriteRune(matrixGlyphs[rand.IntN(len(matrixGlyphs))])
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}
