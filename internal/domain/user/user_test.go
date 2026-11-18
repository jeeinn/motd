package user

import (
	"errors"
	"strings"
	"testing"
)

// 用户名规则是注册链路的第一道门，且 DDL 无法完整表达字符类别白名单，
// 因此这里就是规则的事实来源。改动规则时同步改 0001_init.sql 的兜底约束。

func TestValidateUsername_Accept(t *testing.T) {
	valid := []string{
		"v_x", "A_B", "abc", "123", "a-b_c",
		"中文a", "中文测试", "中文abc", "中文_测试-1",
		strings.Repeat("中", 3),  // 下界：3 个汉字
		strings.Repeat("中", 20), // 上界：20 个汉字
		strings.Repeat("a", 20),
	}
	for _, name := range valid {
		if err := ValidateUsername(name); err != nil {
			t.Errorf("应接受 %q: %v", name, err)
		}
	}
}

func TestValidateUsername_Reject(t *testing.T) {
	invalid := []string{
		"", "ab", "a", "中文", // 2 个汉字，长度不足
		strings.Repeat("中", 21), strings.Repeat("a", 21),
		"has space", "中文 空格", " 前后", "中\t文", "中\n文", "中\r文",
		"中\u00a0文", // NBSP
		"中\u3000文", // 全角空格
		"x!y", "a.b", "a/b", "a@b", "a+b", "a=b",
		"😀", "abc😀",
		"Привет", // 非中文非 ASCII 字母
		"é",      // 拉丁扩展字母
	}
	for _, name := range invalid {
		if err := ValidateUsername(name); err == nil {
			t.Errorf("应拒绝 %q", name)
		} else if !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("拒绝 %q 时应返回 ErrInvalidUsername，实际 %v", name, err)
		}
	}
}

func TestValidateUsername_AllowsNoWhitespace(t *testing.T) {
	// 单独锁死「不允许任何空白字符」这条硬约束，避免以后放宽字符集时误放开空白。
	whitespaces := []string{" ", "\t", "\n", "\r", "\v", "\f", "\u00a0", "\u3000", "\u2003", "\u2028"}
	for _, ws := range whitespaces {
		name := "中" + ws + "文"
		if err := ValidateUsername(name); err == nil {
			t.Errorf("用户名含空白 U+%04X 时应被拒绝: %q", []rune(ws)[0], name)
		}
	}
}

func TestReservedNames(t *testing.T) {
	for _, name := range []string{"root", "ROOT", "Admin", "dev", "DEV"} {
		if !IsReserved(name) {
			t.Errorf("%q 应命中保留名（大小写不敏感）", name)
		}
	}
	if IsReserved("中文") {
		t.Error("中文用户名不应命中保留名")
	}

	if got := ResolveUsername("ROOT"); got != "ROOT_0" {
		t.Errorf("保留名应追加 _0，实际 %q", got)
	}
	if got := ResolveUsername("中文"); got != "中文" {
		t.Errorf("非保留名应原样返回，实际 %q", got)
	}
}

func TestStatus(t *testing.T) {
	if !StatusActive.Valid() || Status("bogus").Valid() {
		t.Error("状态取值域判定错误")
	}
	if !StatusActive.CanPost() || StatusDeleting.CanPost() || StatusDeleted.CanPost() {
		t.Error("仅 active 状态应允许写入")
	}
}
