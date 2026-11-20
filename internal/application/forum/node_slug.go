package forum

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ADR-048 的 slug 规则在这里第一次有了调用者：M1 的节点创建写路径。
// 规则：ASCII [a-z0-9/]，以 / 开头，1–2 段，禁结尾 / 与连续 //，
// 每段 1–32 字符，整体 ≤64 字符。
const (
	MaxSlugRunes     = 64
	MaxSlugSegRunes  = 32
	MaxNodeNameRunes = 64
)

// ValidateSlug 校验版块 slug。错误文案是固定模板，不含用户原文：
// 这段文本可能被写回终端，回显原文会给 ANSI/CSI 注入留下旁路。
func ValidateSlug(slug string) error {
	reason := func(s string) error { return &ValidationError{Field: "slug", Reason: s} }
	if slug == "" || slug[0] != '/' {
		return reason("必须以 / 开头")
	}
	if len(slug) > MaxSlugRunes {
		return reason(fmt.Sprintf("最多 %d 字符", MaxSlugRunes))
	}
	if strings.HasSuffix(slug, "/") {
		return reason("不能以 / 结尾")
	}
	if strings.Contains(slug, "//") {
		return reason("不允许连续斜杠")
	}
	parts := strings.Split(slug[1:], "/")
	if len(parts) < 1 || len(parts) > 2 {
		return reason("只允许 1–2 段")
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > MaxSlugSegRunes {
			return reason(fmt.Sprintf("每段需 1–%d 字符", MaxSlugSegRunes))
		}
		for _, c := range part {
			if !isSlugChar(byte(c)) || c > 0x7f {
				return reason("仅允许小写字母、数字与 /")
			}
		}
	}
	return nil
}

func isSlugChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

// validateNodeName 与标题同口径：非空白、≤64 字符、拒绝控制字符。
// 名称来自命令参数，控制字符必须挡在创建之前，不能等渲染层兜底。
func validateNodeName(name string) error {
	reason := func(s string) error { return &ValidationError{Field: "name", Reason: s} }
	if strings.TrimSpace(name) == "" {
		return reason("不能为空白")
	}
	if utf8.RuneCountInString(name) > MaxNodeNameRunes {
		return reason(fmt.Sprintf("最多 %d 字符", MaxNodeNameRunes))
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return reason("不能包含控制字符")
		}
	}
	return nil
}
