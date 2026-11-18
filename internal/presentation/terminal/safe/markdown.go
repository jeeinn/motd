package safe

import (
	"strings"
	"unicode/utf8"
)

// Kind 是白名单 Markdown 解析后的行内节点类别。
type Kind uint8

const (
	KindText Kind = iota
	KindBold
	KindItalic
	KindCode
	KindLink
)

// Inline 是语义节点，不含任何 ANSI / 终端概念。
// Href 只在 KindLink 且协议白名单通过时非空。
type Inline struct {
	Kind Kind
	Text string
	Href string
}

var allowedSchemes = []string{"http://", "https://", "ssh://"}

// ParseInline 只识别 4 种白名单语法：**粗体**、*斜体*、`代码`、[文字](链接)。
// 其余 Markdown 字符（#、>、-、|、图片、HTML 标签…）一律当纯文本。
// 输入必须已经过 SanitizeText；本函数不再承担净化职责。
func ParseInline(line string) []Inline {
	runes := []rune(line)
	var out []Inline
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, Inline{Kind: KindText, Text: text.String()})
			text.Reset()
		}
	}
	for i := 0; i < len(runes); {
		rest := string(runes[i:])
		if in, n, ok := tryLink(rest); ok {
			flush()
			out = append(out, in)
			i += n
			continue
		}
		if in, n, ok := tryDelimited(rest, "**", KindBold); ok {
			flush()
			out = append(out, in)
			i += n
			continue
		}
		if in, n, ok := tryDelimited(rest, "`", KindCode); ok {
			flush()
			out = append(out, in)
			i += n
			continue
		}
		if in, n, ok := tryDelimited(rest, "*", KindItalic); ok {
			flush()
			out = append(out, in)
			i += n
			continue
		}
		text.WriteRune(runes[i])
		i++
	}
	flush()
	return out
}

// tryLink 解析 [text](url)。协议不在白名单时整段降级为纯文本，
// URL 绝不进入 Href，也不会被渲染成 OSC 8 超链接。
func tryLink(rest string) (Inline, int, bool) {
	if !strings.HasPrefix(rest, "[") {
		return Inline{}, 0, false
	}
	sep := strings.Index(rest, "](")
	if sep < 0 {
		return Inline{}, 0, false
	}
	label := rest[1:sep]
	// 关键：形如 "[2J]0;pwned **粗体** [x](...)" 的文本里，第一个 "[" 并不是链接起点。
	// 若不加约束，解析器会把到后面某个 "](" 为止的整段（含加粗）吞成纯文本。
	// 因此链接标签不得含方括号 / 换行，也不得过长；不满足就当普通文本逐字处理。
	if label == "" || strings.ContainsAny(label, "[]\n") || len([]rune(label)) > 200 {
		return Inline{}, 0, false
	}
	tail := rest[sep+2:]
	end := strings.IndexByte(tail, ')')
	if end < 0 {
		return Inline{}, 0, false
	}
	whole := rest[:sep+2+end+1]
	n := utf8.RuneCountInString(whole)
	url := tail[:end]
	if allowedURL(url) && validURLChars(url) {
		return Inline{Kind: KindLink, Text: label, Href: url}, n, true
	}
	return Inline{Kind: KindText, Text: whole}, n, true
}

func tryDelimited(rest, delim string, kind Kind) (Inline, int, bool) {
	if !strings.HasPrefix(rest, delim) {
		return Inline{}, 0, false
	}
	inner := rest[len(delim):]
	end := strings.Index(inner, delim)
	if end <= 0 {
		return Inline{}, 0, false
	}
	whole := rest[:len(delim)+end+len(delim)]
	return Inline{Kind: kind, Text: inner[:end]}, utf8.RuneCountInString(whole), true
}

func allowedURL(u string) bool {
	lower := strings.ToLower(u)
	for _, scheme := range allowedSchemes {
		if strings.HasPrefix(lower, scheme) && len(u) > len(scheme) {
			return true
		}
	}
	return false
}

func validURLChars(u string) bool {
	for _, r := range u {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
