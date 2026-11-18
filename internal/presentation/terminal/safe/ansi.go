package safe

// Style 是服务端允许生成的样式白名单。用户内容无法选择样式，
// 只能由白名单 Markdown 解析器映射到这里的枚举。
type Style uint8

const (
	StylePlain Style = iota
	StyleBold
	StyleDim
	StyleItalic
	StyleCode
	StyleLink
	StyleHeader
)

// 下面这张表是渲染输出中**唯一**允许出现的 ESC 序列来源。
// 只允许 SGR（\x1b[...m）：颜色与字重，不动光标、不清屏、不改标题、不写剪贴板。
// archtest 守卫会扫描本包其余文件：出现任何 \x1b 字面量即测试失败；
// 本文件里的每个 \x1b 字面量也必须是纯 SGR 形式。
const (
	resetANSI  = "\x1b[0m"
	boldANSI   = "\x1b[1m"
	dimANSI    = "\x1b[2m"
	italicANSI = "\x1b[3m"
	codeANSI   = "\x1b[36m"
	linkANSI   = "\x1b[4m"
	headerANSI = "\x1b[1;4m"
)

func openANSI(s Style) string {
	switch s {
	case StyleBold:
		return boldANSI
	case StyleDim:
		return dimANSI
	case StyleItalic:
		return italicANSI
	case StyleCode:
		return codeANSI
	case StyleLink:
		return linkANSI
	case StyleHeader:
		return headerANSI
	default:
		return ""
	}
}

func closeANSI(s Style) string {
	if s == StylePlain {
		return ""
	}
	return resetANSI
}
