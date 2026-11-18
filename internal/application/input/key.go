// Package input 定义「归一化后的输入事件」。
//
// 边界：类型定义在 application（使用方），transport 只负责把字节流解码成
// KeyMsg；命令注册表也读同一套类型，避免出现第二条业务路径（评审稿 §3.2）。
// 本包不含任何终端 / 渲染概念，也不 import transport。
package input

// Kind 是一次按键的语义类别。
type Kind uint8

const (
	KeyUnknown Kind = iota
	KeyRune         // 可打印字符，含中文等 UTF-8 多字节
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyTab
	KeyArrowUp
	KeyArrowDown
	KeyArrowLeft
	KeyArrowRight
	KeyCtrlC
	KeyCtrlD
	KeyPaste // 一次 bracketed paste 块
)

// KeyMsg 是解码后的单个输入事件。
//
// Rune 只对 KeyRune 有意义；Text 只对 KeyPaste 有意义。
// Raw 保留原始字节仅供诊断与回放，**绝不直接写回终端**（P0-23 安全渲染约束）。
type KeyMsg struct {
	Kind Kind
	Rune rune
	Text string
	Raw  []byte
}
