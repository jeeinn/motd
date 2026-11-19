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

	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// lineEcho 是每会话一个的行编辑回显器。cols 与编辑器的 runes 一一对应，
// 记录每个输入字符实际占用的终端列数，用于宽字符（中文/Emoji）正确退格。
type lineEcho struct {
	out    io.Writer
	secret bool
	cols   []int
}

func newLineEcho(out io.Writer) *lineEcho { return &lineEcho{out: out} }

// setSecret 切换敏感字段模式：true 时只回显 '*'，不回显原文。
// P1 的邮箱登录密码字段应调用 setSecret(true)；viewmodel.Field.Secret
// 是该语义在统一 ViewModel 里的正式落点。
func (e *lineEcho) setSecret(v bool) {
	e.secret = v
	e.cols = nil
}

func (e *lineEcho) reset() { e.cols = nil }

// rune 回显单个输入字符。返回净化结果，便于调用方判断是否保留。
func (e *lineEcho) rune(r rune) {
	if e.secret {
		_, _ = io.WriteString(e.out, "*")
		e.cols = append(e.cols, 1)
		return
	}
	clean := safe.SanitizeText(string(r))
	if clean != "" {
		_, _ = io.WriteString(e.out, clean)
	}
	w := 0
	for _, c := range clean {
		w += safe.DisplayWidth(c)
	}
	e.cols = append(e.cols, w)
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
func (e *lineEcho) backspace(deleted bool) {
	if !deleted || len(e.cols) == 0 {
		return
	}
	w := e.cols[len(e.cols)-1]
	e.cols = e.cols[:len(e.cols)-1]
	for i := 0; i < w; i++ {
		_, _ = io.WriteString(e.out, "\b \b")
	}
}

// mergeBackspace 回显"空行退格跨行合并"。调用时光标停在刚被删除的空行行首
// （列 0）：先用白名单 CUU 上移一行回到上一行，\r 回车首，EL 清掉上一行旧内容，
// 再把合并后的上一行文本逐 rune 经 SanitizeText 重写一遍。这样光标自然停在
// 上一行末尾（两行拼接处），用户后续输入接在那里。
//
// 控制序列只由 safe 包的服务端白名单函数生成（CursorUp / ClearToEOL），
// 绝不内联 ESC 字面量、绝不发 CUP/CUD/ED/DECSET；用户文本仍走 SanitizeText，
// 与编辑控制分属两条写入路径（§3.5 信任域分离）。列宽栈重建为合并行各字符宽度。
func (e *lineEcho) mergeBackspace(merged string) {
	_, _ = io.WriteString(e.out, safe.CursorUp(1))
	_, _ = io.WriteString(e.out, "\r")
	_, _ = io.WriteString(e.out, safe.ClearToEOL())
	e.cols = nil
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
