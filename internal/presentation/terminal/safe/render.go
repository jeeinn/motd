package safe

import (
	"errors"
	"strings"
)

var ErrUnsafeSystemText = errors.New("服务端文本含控制字符，拒绝渲染")

// Options 由调用方（页面层）提供渲染参数；Width <= 0 表示不折行。
type Options struct {
	Width int
}

// RenderUser 是**用户提交内容**的唯一渲染入口：净化 → 折行 → 白名单解析 → 受控 ANSI。
//
// 输出里的 ESC 序列只可能来自 ansi.go 的 SGR 常量表；任何来自用户原文的
// ESC 都已在第一段被转成可见字符，不可能重新变成控制序列。
func RenderUser(raw string, opts Options) string {
	clean := SanitizeText(raw)
	if opts.Width > 0 {
		clean = WrapText(clean, opts.Width)
	}
	var b strings.Builder
	for i, line := range strings.Split(clean, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		for _, in := range ParseInline(line) {
			b.WriteString(renderInline(in))
		}
	}
	return b.String()
}

func renderInline(in Inline) string {
	style := styleFor(in.Kind)
	text := in.Text
	if in.Kind == KindLink && in.Href != "" {
		// P0 不生成 OSC 8 超链接（那会打开另一条终端控制面），
		// 只把已通过白名单的 URL 以纯文本附在标签后。
		text += " <" + in.Href + ">"
	}
	return openANSI(style) + text + closeANSI(style)
}

func styleFor(k Kind) Style {
	switch k {
	case KindBold:
		return StyleBold
	case KindItalic:
		return StyleItalic
	case KindCode:
		return StyleCode
	case KindLink:
		return StyleLink
	default:
		return StylePlain
	}
}

// RenderSystem 是**服务端可信文本**专用路径：不调用 SanitizeText。
//
// 这不是「跳过净化」，而是另一条更严格的校验：服务端文本本不该含控制字符，
// 一旦出现就是调用方 bug，直接报错而不是替它擦屁股。§3.5 要求两条路径
// 不共用同一个 sanitize，archtest 会静态证明 RenderSystem 体内没有 SanitizeText。
func RenderSystem(text string, style Style) (string, error) {
	if err := validateSystemText(text); err != nil {
		return "", err
	}
	return openANSI(style) + text + closeANSI(style), nil
}

func validateSystemText(s string) error {
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return ErrUnsafeSystemText
		}
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ErrUnsafeSystemText
		}
		if isBidiControl(r) || isInvisibleFormat(r) {
			return ErrUnsafeSystemText
		}
	}
	return nil
}
