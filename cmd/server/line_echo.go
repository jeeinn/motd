package main

// 安全行编辑回显（ADR-026，修订 ADR-018 的「全局零回显」）。
//
// 用户 2026-11-18 决策：正常操作（斜杠命令、标题、正文）**必须有回显**，
// 只有 Secret 字段（P1 邮箱登录密码等）隐藏原文。设计稿 §3.5 本身也预留了
// 这个语义：viewmodel.Field.Secret 的注释写明「具体回显字符由 Renderer 决定」。
//
// 三条硬边界，缺一不可：
//  1. 回显的用户字符一律先经 safe.SanitizeText 净化，绝不写 KeyMsg.Raw / Text
//     原始字节；ESC / CSI / OSC / \r / 双向文本会被转成可见图示或剥离。
//  2. \b \b（退格擦除）与 \r\n（换行）是**服务端生成的编辑控制**，不是用户
//     内容，二者不共用同一条写入路径（§3.5 的信任域分离）。
//  3. 导航键 j/k/Enter 在非编辑态是「命令」不是文本，不回显；只有行编辑态回显。
//
// 因此本文件不修改 safe/ansi.go 的白名单表，既有 archtest 守卫全部保持；
// v0.3.0 的跨行退格重绘只是在服务端调用已获准的 CursorUp / ClearToEOL。

import (
	"io"
	"strings"

	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// lineEcho 是每会话一个的行编辑回显器。cols 与编辑器的 runes 一一对应，
// 记录每个输入字符实际占用的终端列数，用于宽字符（中文/Emoji）正确退格。
// width 是终端列宽（0=未知，保持旧行为）；runes 与 cols 一一对应，保存净化后的
// 可见文本，供超宽逻辑行跨物理行退格时重绘定位光标（B7，W10）。
type lineEcho struct {
	out    io.Writer
	secret bool
	cols   []int
	width  int
	runes  []string
}

func newLineEcho(out io.Writer) *lineEcho { return &lineEcho{out: out} }

// newLineEchoWithWidth 注入终端列宽。宽度未知（<=0）时退化为旧的物理行假设
// （CUU(1)、\b \b），兼容 80 列默认、直接 newLineEcho 的既有单测与无 Window 的写入器。
func newLineEchoWithWidth(out io.Writer, width int) *lineEcho {
	if width < 0 {
		width = 0
	}
	return &lineEcho{out: out, width: width}
}

// setSecret 切换敏感字段模式：true 时只回显 '*'，不回显原文。
// P1 的邮箱登录密码字段应调用 setSecret(true)；viewmodel.Field.Secret
// 是该语义在统一 ViewModel 里的正式落点。
func (e *lineEcho) setSecret(v bool) {
	e.secret = v
	e.cols = nil
	e.runes = nil
}

func (e *lineEcho) reset() { e.cols = nil; e.runes = nil }

// echoRune 只输出单个净化后的 rune，不触碰列宽栈——跨物理行重绘时复用。
// 返回净化后的可见文本，供调用方按 DisplayWidth 精确累加 cols。
func (e *lineEcho) echoRune(r rune) string {
	if e.secret {
		_, _ = io.WriteString(e.out, "*")
		return "*"
	}
	clean := safe.SanitizeText(string(r))
	if clean != "" {
		_, _ = io.WriteString(e.out, clean)
	}
	return clean
}

// rune 回显单个输入字符。返回净化结果，便于调用方判断是否保留。
func (e *lineEcho) rune(r rune) {
	clean := e.echoRune(r)
	w := 0
	for _, c := range clean {
		w += safe.DisplayWidth(c)
	}
	e.cols = append(e.cols, w)
	e.runes = append(e.runes, clean)
}

// paste 逐 rune 回显粘贴块，与编辑器按 rune 追加缓冲保持一致。
func (e *lineEcho) paste(s string) {
	for _, r := range s {
		e.rune(r)
	}
}

// pasteBlock 回显多行正文粘贴块。换行走服务端生成的 CRLF 并清空列宽栈，
// 其余字符逐 rune 经 SanitizeText 净化——与 composer 的正文行切分一一对应。
// 注意：这是服务端编辑控制与用户字节分属不同信任域（§3.5），
// 绝不把 KeyMsg.Text 的原始字节直接写回终端。
func (e *lineEcho) pasteBlock(s string) {
	for _, r := range s {
		if r == '\n' {
			e.newline()
			continue
		}
		e.rune(r)
	}
}

// backspace 只在编辑器确实删掉一个 rune 时调用；宽字符按显示列数擦除。
// 宽度已知且删除使当前物理行变空时，补一步跨行归位（B7）：
// \b \b 只清当前物理行，光标会停在空行首，与缓冲光标（上一物理行末）不符。
// 用白名单 CUU(1) 回到上一物理行，重写其已净化的可见文本把光标定位到行末。
func (e *lineEcho) backspace(deleted bool) {
	if !deleted || len(e.cols) == 0 {
		return
	}
	w := e.cols[len(e.cols)-1]
	var breaks []int
	if e.width > 0 {
		breaks = safe.MeasureLine(strings.Join(e.runes, ""), e.width).Breaks
	}
	e.cols = e.cols[:len(e.cols)-1]
	e.runes = e.runes[:len(e.runes)-1]
	for i := 0; i < w; i++ {
		_, _ = io.WriteString(e.out, "\b \b")
	}
	if e.width <= 0 || len(breaks) == 0 || len(e.runes) == 0 {
		return
	}
	// 删除前末物理行的起点 == 删除后的 rune 总数 → 被删 rune 是该物理行唯一内容。
	last := breaks[len(breaks)-1]
	if last != len(e.runes) {
		return
	}
	start := 0
	if len(breaks) >= 2 {
		start = breaks[len(breaks)-2]
	}
	rest := []rune(strings.Join(e.runes, ""))
	if start > len(rest) {
		return
	}
	_, _ = io.WriteString(e.out, safe.CursorUp(1))
	_, _ = io.WriteString(e.out, string(rest[start:])) // 已净化文本，直接回写安全
}

// mergeBackspace 回显「空行退格跨行合并」。调用时光标停在刚被删除的空行行首
// （列 0）：先用白名单 CUU 上移 n 行回到上一逻辑行的起始物理行（n=上一逻辑行
// 占用的物理行数，宽度未知时退化为 1），\r 回车首，EL 清掉起始物理行，
// 再把合并后的上一逻辑行文本逐 rune 经 SanitizeText 重写一遍——重写会覆盖
// 该逻辑行占用的全部物理行，消除超宽行的上方碎片。光标自然停在其末尾。
//
// 控制序列只由 safe 包的服务端白名单函数生成（CursorUp / ClearToEOL），
// 绝不内联 ESC 字面量、绝不发 CUP/CUD/ED/DECSET；用户文本仍走 SanitizeText，
// 与编辑控制分属两条写入路径（§3.5 信任域分离）。列宽栈重建为合并行各字符宽度。
func (e *lineEcho) mergeBackspace(merged string) {
	up := 1
	if e.width > 0 {
		up = safe.MeasureLine(merged, e.width).Rows
	}
	_, _ = io.WriteString(e.out, safe.CursorUp(up))
	_, _ = io.WriteString(e.out, "\r")
	_, _ = io.WriteString(e.out, safe.ClearToEOL())
	e.cols = nil
	e.runes = nil
	for _, r := range merged {
		e.rune(r)
	}
}

// newline 结束当前输入行并清空列宽栈。
func (e *lineEcho) newline() {
	_, _ = io.WriteString(e.out, "\r\n")
	e.reset()
}

// preloadedSlash 回显服务端预置的 '/'。必须在命令提示语输出之后调用，
// 否则提示会把用户输入行截断（本机实测发现）。
func (e *lineEcho) preloadedSlash() {
	_, _ = io.WriteString(e.out, "/")
	e.cols = append(e.cols, 1)
	e.runes = append(e.runes, "/")
}

// echoCommandKey 把斜杠命令行的按键安全地回显。
// applyKey 是纯状态机（不碰 IO），回显因此放在调用侧，用前后长度判断是否真删除。
// actionCommandStart 由 handle 单独处理（需先输出提示再回显 '/'）。
func echoCommandKey(e *lineEcho, m input.KeyMsg, wasEditing bool, beforeLen, afterLen int, action cmdAction) {
	if action == actionCommandStart || !wasEditing {
		return // 非编辑态按键是导航命令，不回显
	}
	switch m.Kind {
	case input.KeyRune:
		e.rune(m.Rune)
	case input.KeyPaste:
		e.paste(m.Text)
	case input.KeyBackspace:
		e.backspace(afterLen < beforeLen)
	case input.KeyEnter, input.KeyEsc, input.KeyCtrlC:
		e.newline()
	}
}
