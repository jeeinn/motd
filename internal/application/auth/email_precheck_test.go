package auth

// /verify 收密码前只读预检的回归（v0.3.1 体验项）。
// 全程进程内 fake：不触网、不碰 SQLite、不碰生产 data/。

import (
	"context"
	"errors"
	"testing"
	"time"
)

const precheckFixedNow = 1_700_000_000

func newPrecheckTestService(t *testing.T, precheck *RateLimiter) (*EmailAuthService, *fakeEmailStore) {
	t.Helper()
	store := newFakeEmailStore()
	opts := []EmailServiceOption{
		WithEmailPepper([]byte("fixed-test-pepper-not-a-secret")),
		WithEmailCodeFunc(func() (string, error) { return "123456", nil }),
		WithEmailClock(func() time.Time { return time.Unix(precheckFixedNow, 0).UTC() }),
	}
	if precheck != nil {
		opts = append(opts, WithEmailPrecheckLimiter(precheck))
	}
	svc := NewEmailAuthService(store, &fakeEmailSender{store: store}, &fakeEmailRevoker{}, opts...)
	return svc, store
}

func requestPrecheckToken(t *testing.T, svc *EmailAuthService, email string) {
	t.Helper()
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, email, "iphash"); err != nil {
		t.Fatal(err)
	}
}

// 有效码放行，且全程只读：不写库、不消费、不计尝试。
func TestPreCheckCodeValidIsReadOnly(t *testing.T) {
	svc, store := newPrecheckTestService(t, nil)
	requestPrecheckToken(t, svc, testEmail)
	writesBefore := store.writes

	if err := svc.PreCheckCode(context.Background(), "  Alice@Example.com  ", "123456", "iphash"); err != nil {
		t.Fatalf("有效码预检应放行: %v", err)
	}
	if store.writes != writesBefore {
		t.Fatalf("预检绝不写库: writes %d → %d", writesBefore, store.writes)
	}
	if store.reads != 1 {
		t.Fatalf("预检应且只应走一次只读端口，实际 reads=%d", store.reads)
	}
	if len(store.tokens) != 1 || store.tokens[0].consumed || store.tokens[0].attempts != 0 {
		t.Fatalf("预检不得消费 token 或累计尝试: %+v", store.tokens)
	}
}

// 无效码（格式/无 pending/错码/过期/已消费/尝试用尽）统一 ErrInvalidCode，且全部只读。
func TestPreCheckCodeRejectsBadCodesWithoutWriting(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		mutate func(*fakeEmailStore)
	}{
		{name: "格式非法", code: "12a456"},
		{name: "长度不足", code: "12345"},
		{name: "错码", code: "654321"},
		{name: "无待验证流程", code: "123456", mutate: func(s *fakeEmailStore) { s.tokens = nil }},
		{name: "已过期", code: "123456", mutate: func(s *fakeEmailStore) {
			s.tokens[0].expiresAt = time.Unix(precheckFixedNow-1, 0).UTC()
		}},
		{name: "已消费", code: "123456", mutate: func(s *fakeEmailStore) { s.tokens[0].consumed = true }},
		{name: "尝试用尽", code: "123456", mutate: func(s *fakeEmailStore) { s.tokens[0].attempts = 5 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store := newPrecheckTestService(t, nil)
			requestPrecheckToken(t, svc, testEmail)
			if c.mutate != nil {
				c.mutate(store)
			}
			writesBefore := store.writes
			err := svc.PreCheckCode(context.Background(), testEmail, c.code, "iphash")
			if !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("应统一回 ErrInvalidCode，实际 %v", err)
			}
			if store.writes != writesBefore {
				t.Fatalf("无效码预检也不得写库: %d → %d", writesBefore, store.writes)
			}
			if store.reads != 1 && c.code != "12a456" && c.code != "12345" {
				t.Fatalf("格式合法时应查一次只读端口，实际 reads=%d", store.reads)
			}
			// 只有「已消费 / 无 pending / 尝试用尽」三例预先改过 token 状态，
			// 其余案例断言预检没有偷偷消费或计数。
			if len(store.tokens) == 1 && c.name != "已消费" && c.name != "无待验证流程" && c.name != "尝试用尽" {
				if store.tokens[0].consumed || store.tokens[0].attempts != 0 {
					t.Fatalf("预检不得改 token 状态: %+v", store.tokens[0])
				}
			}
		})
	}
}

// 预检不能成为验证码爆破 oracle：轻量限流命中后拒绝，且不写库。
func TestPreCheckCodeRateLimitsOracle(t *testing.T) {
	clock := time.Unix(precheckFixedNow, 0).UTC()
	limiter := NewRateLimiter(
		WithRateLimitClock(func() time.Time { return clock }),
		WithRateLimitConfig(RateLimitConfig{MaxAttempts: 2, Window: time.Hour, Lockout: 15 * time.Minute, MaxKeys: 16}),
	)
	svc, store := newPrecheckTestService(t, limiter)
	requestPrecheckToken(t, svc, testEmail)

	for i := 0; i < 2; i++ {
		if err := svc.PreCheckCode(context.Background(), testEmail, "654321", "iphash"); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("第 %d 次错码应回码无效，实际 %v", i+1, err)
		}
	}
	writesBefore := store.writes
	err := svc.PreCheckCode(context.Background(), testEmail, "654321", "iphash")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("第 3 次应命中预检限流，实际 %v", err)
	}
	if rl.RetryAfterSeconds() <= 0 {
		t.Fatal("限流错误必须带可展示的重试秒数")
	}
	if store.writes != writesBefore {
		t.Fatal("限流拒绝也不得写库")
	}
}

// TOCTOU：预检通过不代表可信任。预检后 token 失效，正式 Verify 必须独立拒绝。
func TestPreCheckPassedThenVerifyStillRejectsStaleToken(t *testing.T) {
	svc, store := newPrecheckTestService(t, nil)
	requestPrecheckToken(t, svc, testEmail)
	if err := svc.PreCheckCode(context.Background(), testEmail, "123456", "iphash"); err != nil {
		t.Fatal(err)
	}
	// 预检通过后，另一个并发请求抢先消费了同一个码。
	store.tokens[0].consumed = true
	if _, err := svc.Verify(context.Background(), testEmail, "123456", "password123", "iphash"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Verify 必须独立重校验，实际 %v", err)
	}
	if len(store.users) != 0 {
		t.Fatal("陈旧的预检结果不得导致建号")
	}
}
