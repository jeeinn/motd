package i18n

// i18n 骨架（DESIGN-v090 §1 D3）：本期只收服务端固定文案——10 个 hint key
// （hintPrefix + 4 条引导 + 5 条草稿状态）× 2 locale，以及 notice 前缀。
// 不做全量 UI 翻译；表内只存 string 常量与 %s 槽位，不含任何 ANSI/控制字节。
//
// 信任域不变：文案由调用方经 safe.RenderSystem 渲染（服务端常量路径），
// 用户文本继续走 RenderUser/SanitizeText；本包不碰光标控制、不碰 ANSI 白名单。

import "fmt"

const (
	LocaleZH = "zh" // 默认，行为与 v0.8.4 逐字节一致
	LocaleEN = "en"
)

// hint key：调用方只传 key，不在调用点散落文案字面量。
const (
	KeyHintPrefix             = "hintPrefix"
	KeyComposerTitle          = "composerTitle"
	KeyComposerBody           = "composerBody"
	KeyComposerBodyAfterTitle = "composerBodyAfterTitle"
	KeyComposerEdit           = "composerEdit"
	KeyDraftRetainedRate      = "draftRetainedRate"
	KeyDraftSuspended         = "draftSuspended"
	KeyDraftRetained          = "draftRetained"
	KeyDraftRestored          = "draftRestored"
	KeyDraftDiscarded         = "draftDiscarded"
)

// hintCatalog：zh 为现状原文（逐字保留），en 为译文（短句优先、关键信息前置、
// 每条分句 ≤80 列；保留 %s 槽位供限流倒计时注入）。
var hintCatalog = map[string]map[string]string{
	LocaleZH: {
		KeyHintPrefix:             "[提示] ",
		KeyComposerTitle:          "新建主题：输入标题后按 Enter 确认（Esc 取消）",
		KeyComposerBody:           "回复：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyComposerBodyAfterTitle: "标题已接收：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyComposerEdit:           "编辑：重新输入完整新正文（覆盖原文），Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyDraftRetainedRate:      "内容已保留为临时草稿（仅本次连接）：请约 %s 后按 Ctrl-D（或单独一行 .）重发；Esc 挂起草稿回浏览屏，回到原帖按 r 恢复",
		KeyDraftSuspended:         "草稿已保留，回到原帖按 r 即可恢复",
		KeyDraftRetained:          "（输入内容已保留为临时草稿，仅本次连接有效；处理完成后重新 /new 或 /reply 即可恢复）",
		KeyDraftRestored:          "草稿已恢复：内容仍在，直接按 Ctrl-D（或单独一行 .）提交",
		KeyDraftDiscarded:         "（此前的临时草稿已不适用，重新开始输入）",
	},
	LocaleEN: {
		KeyHintPrefix:             "[Info] ",
		KeyComposerTitle:          "New topic: type a title, Enter to confirm (Esc to cancel)",
		KeyComposerBody:           "Reply: type body; Enter = new line, Ctrl-D or line . = submit (Esc to cancel)",
		KeyComposerBodyAfterTitle: "Title saved. Type body; Enter = new line, Ctrl-D or line . = submit (Esc cancels)",
		KeyComposerEdit:           "Edit: retype full body (replaces original); Enter = new line, Ctrl-D/. = submit",
		KeyDraftRetainedRate:      "Draft kept for this connection. Retry in %s: Ctrl-D or line .; Esc suspends, press r in thread to restore",
		KeyDraftSuspended:         "Draft saved. Press r in the thread to restore",
		KeyDraftRetained:          "Input kept as temporary draft (this connection only); start /new or /reply again to restore",
		KeyDraftRestored:          "Draft restored. Submit with Ctrl-D (or a line with .)",
		KeyDraftDiscarded:         "Previous temporary draft no longer applies; start over",
	},
}

// Lookup 按 locale/key 取文案；未知 locale 或 key 回退 zh（默认语言永不缺文案）。
// args 用于 %s 槽位（当前仅 draftRetainedRate 含一个槽位）。
func Lookup(locale, key string, args ...any) string {
	m, ok := hintCatalog[locale]
	if !ok {
		m = hintCatalog[LocaleZH]
	}
	text, ok := m[key]
	if !ok {
		text = hintCatalog[LocaleZH][key]
	}
	if len(args) > 0 && text != "" {
		text = fmt.Sprintf(text, args...)
	}
	return text
}

// NoticePrefix 返回 notice 前缀（服务端常量）。Error 与 Warning/default 各自
// 两种 locale 共 4 组，仍由 view.go 交给 safe.RenderSystem，消息正文另走 RenderUser。
func NoticePrefix(locale string, isError bool) string {
	if locale == LocaleEN {
		if isError {
			return "[Error] "
		}
		return "[Info] "
	}
	if isError {
		return "[错误] "
	}
	return "[提示] "
}

// Keys 返回全部 hint key（供测试遍历完整性，顺序固定便于断言）。
func Keys() []string {
	return []string{
		KeyHintPrefix, KeyComposerTitle, KeyComposerBody, KeyComposerBodyAfterTitle,
		KeyComposerEdit, KeyDraftRetainedRate, KeyDraftSuspended, KeyDraftRetained,
		KeyDraftRestored, KeyDraftDiscarded,
	}
}
