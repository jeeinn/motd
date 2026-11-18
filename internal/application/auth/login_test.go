package auth

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// P0-17 纯函数回归：登录输入归一化与用户名派生 / 分配。
// 这些是「写事务之前」就能判定完的逻辑，用穷举式表驱动测死，
// 避免把脏数据带进写事务后才靠 DB 约束发现。

// ---------------------------------------------------------------------------
// NewIdentityProfile
// ---------------------------------------------------------------------------

func TestNewIdentityProfileRejectsNonPositiveID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := NewIdentityProfile(GitHubUser{ID: id, Login: "alice"}, nil); err == nil {
			t.Fatalf("id=%d 应被拒绝", id)
		}
	}
}

func TestNewIdentityProfileUsesNumericIDAsProviderUID(t *testing.T) {
	p, err := NewIdentityProfile(GitHubUser{ID: 12345678, Login: "alice"}, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if p.Provider != ProviderGitHub {
		t.Fatalf("provider 应为 %q，实际 %q", ProviderGitHub, p.Provider)
	}
	if p.ProviderUID != "12345678" {
		t.Fatalf("provider_uid 必须是数字 id 的字符串，实际 %q", p.ProviderUID)
	}
	if p.PreferredUsername != "alice" {
		t.Fatalf("用户名应保留 alice，实际 %q", p.PreferredUsername)
	}
	if p.Email != "" || p.EmailVerified {
		t.Fatalf("无邮箱时不应伪造，实际 %q / %v", p.Email, p.EmailVerified)
	}
}

func TestNewIdentityProfilePicksPrimaryVerifiedEmail(t *testing.T) {
	p, err := NewIdentityProfile(GitHubUser{ID: 42, Login: "bob"}, []GitHubEmail{
		{Email: "old@example.com", Primary: false, Verified: true},
		{Email: "Not-Verified@Example.com", Primary: true, Verified: false},
		{Email: "  Bob@Example.COM  ", Primary: true, Verified: true},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if !p.EmailVerified {
		t.Fatal("应选中 primary+verified 邮箱")
	}
	if p.Email != "bob@example.com" {
		t.Fatalf("邮箱应 trim + 小写，实际 %q", p.Email)
	}
}

func TestNewIdentityProfileValidatesDerivedUsername(t *testing.T) {
	// 超长 login 必须截断到 20 且仍然合法。
	long := strings.Repeat("a", 60)
	p, err := NewIdentityProfile(GitHubUser{ID: 7, Login: long}, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if utf8.RuneCountInString(p.PreferredUsername) != maxUsernameRunes {
		t.Fatalf("应截断到 %d 码点，实际 %d", maxUsernameRunes, utf8.RuneCountInString(p.PreferredUsername))
	}
	if err := domainuser.ValidateUsername(p.PreferredUsername); err != nil {
		t.Fatalf("派生用户名必须合法: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SelectPrimaryVerifiedEmail
// ---------------------------------------------------------------------------

func TestSelectPrimaryVerifiedEmail(t *testing.T) {
	cases := []struct {
		name   string
		in     []GitHubEmail
		want   string
		wantOK bool
	}{
		{"空列表", nil, "", false},
		{"只有非 primary", []GitHubEmail{{Email: "a@b.com", Verified: true}}, "", false},
		{"只有未验证", []GitHubEmail{{Email: "a@b.com", Primary: true}}, "", false},
		{"primary 但邮箱为空", []GitHubEmail{{Email: "  ", Primary: true, Verified: true}}, "", false},
		{"无 @ 非法", []GitHubEmail{{Email: "not-an-email", Primary: true, Verified: true}}, "", false},
		{"多个 @ 非法", []GitHubEmail{{Email: "a@b@c.com", Primary: true, Verified: true}}, "", false},
		{"含空白", []GitHubEmail{{Email: "a b@c.com", Primary: true, Verified: true}}, "", false},
		{"合法", []GitHubEmail{{Email: "A@B.com", Primary: true, Verified: true}}, "a@b.com", true},
		{"跳过非法取后一个", []GitHubEmail{
			{Email: "bad", Primary: true, Verified: true},
			{Email: "ok@example.com", Primary: true, Verified: true},
		}, "ok@example.com", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SelectPrimaryVerifiedEmail(c.in)
			if got != c.want || ok != c.wantOK {
				t.Fatalf("得到 (%q, %v)，期望 (%q, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestValidEmailRejectsControlCharacters(t *testing.T) {
	for _, s := range []string{"a\x1b@b.com", "a@b\n.com", "a@b\x00.com", "a@b\x7f.com"} {
		if validEmail(s) {
			t.Fatalf("%q 含控制字符，应判非法", s)
		}
	}
	if !validEmail("user.name+tag@example.co") {
		t.Fatal("常见合法邮箱应通过")
	}
}

// ---------------------------------------------------------------------------
// DeriveUsername
// ---------------------------------------------------------------------------

func TestDeriveUsername(t *testing.T) {
	cases := []struct {
		name  string
		login string
		uid   string
		want  string
	}{
		{"普通 login", "alice", "1", "alice"},
		{"带中划线", "alice-01", "1", "alice-01"},
		{"两字中文补下划线", "张三", "1", "张三_"},
		{"非法字符剔除", "a!b@c#", "1", "abc"},
		{"超长截断", strings.Repeat("x", 50), "9", strings.Repeat("x", 20)},
		{"过短补下划线", "ab", "987", "ab_"},
		{"空 login 用 uid 兜底", "", "12345", "gh12345"},
		{"保留名追后缀", "root", "1", "root_0"},
		{"保留名大小写不敏感", "Admin", "1", "Admin_0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DeriveUsername(c.login, c.uid)
			if err != nil {
				t.Fatalf("派生失败: %v", err)
			}
			if got != c.want {
				t.Fatalf("得到 %q，期望 %q", got, c.want)
			}
			if err := domainuser.ValidateUsername(got); err != nil {
				t.Fatalf("派生结果必须通过 domain 校验: %v", err)
			}
		})
	}
}

func TestDeriveUsernameAlwaysProducesLegalName(t *testing.T) {
	// 恶意 / 极端输入：全部必须产出合法用户名，绝不返回错误让登录失败。
	inputs := []struct{ login, uid string }{
		{"", "1"},
		{"!!!", "2"},
		{"\x1b[2J", "3"},
		{"  ", "4"},
		{strings.Repeat("中", 30), "5"},
		{"a", "6"},
		{"__", "7"},
		{"---", "8"},
		{"root", "9"},
	}
	for _, in := range inputs {
		got, err := DeriveUsername(in.login, in.uid)
		if err != nil {
			t.Fatalf("login=%q uid=%q 派生失败: %v", in.login, in.uid, err)
		}
		if err := domainuser.ValidateUsername(got); err != nil {
			t.Fatalf("login=%q 产出非法用户名 %q: %v", in.login, got, err)
		}
	}
}

func TestTruncateRunesCountsCodePointsNotBytes(t *testing.T) {
	s := "中文用户名测试"
	got := truncateRunes(s, 3)
	if got != "中文用" {
		t.Fatalf("应按码点截断为 中文用，实际 %q", got)
	}
	if truncateRunes(s, 0) != "" {
		t.Fatal("n=0 应返回空串")
	}
	if truncateRunes(s, 100) != s {
		t.Fatal("n 大于长度时应原样返回")
	}
}

// ---------------------------------------------------------------------------
// AllocateUsername
// ---------------------------------------------------------------------------

func TestAllocateUsernameReturnsBaseWhenFree(t *testing.T) {
	got, err := AllocateUsername("alice", func(string) (bool, error) { return false, nil }, 3)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if got != "alice" {
		t.Fatalf("应直接返回 alice，实际 %q", got)
	}
}

func TestAllocateUsernameAppendsNumericSuffix(t *testing.T) {
	taken := map[string]bool{"alice": true, "alice2": true}
	got, err := AllocateUsername("alice", func(n string) (bool, error) { return taken[n], nil }, 5)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if got != "alice3" {
		t.Fatalf("应跳过 alice/alice2 得到 alice3，实际 %q", got)
	}
}

func TestAllocateUsernameKeepsSuffixWithinRuneLimit(t *testing.T) {
	base := strings.Repeat("长", 20) // 正好 20 码点
	taken := map[string]bool{base: true}
	got, err := AllocateUsername(base, func(n string) (bool, error) { return taken[n], nil }, 3)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if n := utf8.RuneCountInString(got); n > maxUsernameRunes {
		t.Fatalf("加后缀后不得超过 %d 码点，实际 %d（%q）", maxUsernameRunes, n, got)
	}
	if !strings.HasSuffix(got, "2") {
		t.Fatalf("应带数字后缀，实际 %q", got)
	}
	if err := domainuser.ValidateUsername(got); err != nil {
		t.Fatalf("候选必须合法: %v", err)
	}
}

func TestAllocateUsernameExhausted(t *testing.T) {
	_, err := AllocateUsername("a", func(string) (bool, error) { return true, nil }, 3)
	if err == nil {
		t.Fatal("候选全被占用时应报错")
	}
	if !strings.Contains(err.Error(), "候选全部被占用") {
		t.Fatalf("错误应可读，实际 %v", err)
	}
}

func TestAllocateUsernamePropagatesProbeError(t *testing.T) {
	boom := context.Canceled
	_, err := AllocateUsername("a", func(string) (bool, error) { return false, boom }, 3)
	if err == nil || err != boom {
		t.Fatalf("探测错误应原样透传，实际 %v", err)
	}
}

func TestAllocateUsernameDefaultsAttempts(t *testing.T) {
	// attempts<=0 应回落到 DefaultUsernameAttempts，而不是一次都不试。
	calls := 0
	got, err := AllocateUsername("alice", func(string) (bool, error) {
		calls++
		return false, nil
	}, 0)
	if err != nil || got != "alice" {
		t.Fatalf("默认 attempts 应生效，得到 (%q, %v)", got, err)
	}
	if calls != 1 {
		t.Fatalf("首个候选可用时只应探测 1 次，实际 %d", calls)
	}
}

func TestUsernameCandidate(t *testing.T) {
	if got := usernameCandidate("alice", 0); got != "alice" {
		t.Fatalf("i=0 应返回基准名，实际 %q", got)
	}
	if got := usernameCandidate("alice", 1); got != "alice2" {
		t.Fatalf("i=1 应返回 alice2，实际 %q", got)
	}
	if got := usernameCandidate(strings.Repeat("x", 20), 9); utf8.RuneCountInString(got) != 20 {
		t.Fatalf("超长基准名加后缀应保持 20 码点，实际 %d（%q）", utf8.RuneCountInString(got), got)
	}
}

// ---------------------------------------------------------------------------
// 哨兵与固定文案
// ---------------------------------------------------------------------------

func TestEmailOccupiedNoticeCarriesNoExternalData(t *testing.T) {
	// Notice 是固定文案：不得含邮箱 / login / 任何控制字符。
	if strings.ContainsAny(EmailOccupiedNotice, "\x1b\n\r\t") {
		t.Fatalf("Notice 不得含控制字符: %q", EmailOccupiedNotice)
	}
	if !strings.Contains(EmailOccupiedNotice, "/link") {
		t.Fatal("Notice 应提示 /link 的显式关联路径（§4.3 步骤 2）")
	}
}

func TestLoginSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidIdentityProfile, ErrEmptyToken, ErrLoginNotFound,
		ErrAccountInactive, ErrIdentityConflict, ErrUsernameConflict,
		ErrUsernameExhausted, ErrLoginRetriesExhausted,
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i] == all[j] {
				t.Fatalf("哨兵重复: %v", all[i])
			}
		}
	}
}

func TestLoginResultCreatedSemantics(t *testing.T) {
	// Created 是「本次新建」的语义，命中已有身份时必须为 false，
	// 否则 P0-19 会误报「欢迎注册」。
	var r LoginResult
	if r.Created {
		t.Fatal("零值 LoginResult 的 Created 应为 false")
	}
}
