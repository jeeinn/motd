package forum

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSlugAcceptsADR048Shapes(t *testing.T) {
	for _, slug := range []string{"/dev/ai", "/help", "/bin/help", "/a", "/123/456", "/dev/a1"} {
		if err := ValidateSlug(slug); err != nil {
			t.Errorf("合法 slug %q 被拒绝: %v", slug, err)
		}
	}
}

func TestValidateSlugRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"":                                   "空串",
		"dev/ai":                             "缺前导斜杠",
		"/dev/ai/":                           "结尾斜杠",
		"/dev//ai":                           "连续斜杠",
		"/dev/ai/x":                          "三段",
		"/Dev/ai":                            "大写",
		"/dev-ai":                            "连字符",
		"/dev_ai":                            "下划线",
		"/dev/ai中文":                          "非 ASCII",
		"/dev/\x1b[2J":                       "控制字符",
		"/" + strings.Repeat("a", 33):        "段超长",
		"/" + strings.Repeat("a", 63) + "/b": "整体超长",
	}
	for slug, name := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateSlug(slug)
			if err == nil {
				t.Fatalf("非法 slug 被接受: %q", slug)
			}
			if !errors.Is(err, ErrInvalidSlug) {
				t.Fatalf("错误应可 errors.Is 到 ErrInvalidSlug，实际 %v", err)
			}
			if strings.Contains(err.Error(), slug) && slug != "" {
				t.Fatalf("错误文案不得回显用户原文 %q: %v", slug, err)
			}
		})
	}
}

func TestValidateNodeName(t *testing.T) {
	if err := validateNodeName("   "); !errors.Is(err, ErrInvalidNodeName) {
		t.Fatalf("空白名称应被拒绝: %v", err)
	}
	if err := validateNodeName("含\x1b[31m控制符"); !errors.Is(err, ErrInvalidNodeName) {
		t.Fatalf("控制字符应被拒绝: %v", err)
	}
	if err := validateNodeName(strings.Repeat("名", 65)); !errors.Is(err, ErrInvalidNodeName) {
		t.Fatalf("超长名称应被拒绝: %v", err)
	}
	if err := validateNodeName("正常中文名称"); err != nil {
		t.Fatalf("正常名称被拒绝: %v", err)
	}
}
