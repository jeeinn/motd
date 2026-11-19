package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const testEmail = "alice@example.com"

func newTestService(t *testing.T) (*EmailAuthService, *fakeEmailStore, *fakeEmailSender, *fakeEmailRevoker) {
	t.Helper()
	store := newFakeEmailStore()
	sender := &fakeEmailSender{store: store}
	revoker := &fakeEmailRevoker{closed: 2}
	svc := NewEmailAuthService(store, sender, revoker,
		WithEmailPepper([]byte("fixed-test-pepper-not-a-secret")),
		WithEmailCodeFunc(func() (string, error) { return "123456", nil }),
		WithEmailClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }),
	)
	return svc, store, sender, revoker
}

func TestRequestCodeWritesTokenThenSendsOutsideTransaction(t *testing.T) {
	svc, store, sender, _ := newTestService(t)
	res, err := svc.RequestCode(context.Background(), EmailPurposeRegister, "  Alice@Example.com ", "iphash")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Delivered || sender.calls != 1 || sender.lastTo != testEmail {
		t.Fatalf("应发送一封到归一化邮箱: %+v calls=%d to=%q", res, sender.calls, sender.lastTo)
	}
	if !strings.Contains(sender.lastBody, "123456") {
		t.Fatalf("邮件正文应含验证码: %q", sender.lastBody)
	}
	if !store.lastWriteCommitted || store.writes != 1 {
		t.Fatal("发信前必须先提交写事务")
	}
	if len(store.tokens) != 1 || strings.Contains(store.tokens[0].codeHash, "123456") {
		t.Fatal("库里只能存散列，不得存明文码")
	}
}

func TestRequestCodeSendFailureKeepsTokenForExpiry(t *testing.T) {
	svc, store, sender, _ := newTestService(t)
	sender.fail = true
	_, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip")
	if !errors.Is(err, ErrEmailDelivery) {
		t.Fatalf("投递失败应回发送失败，实际 %v", err)
	}
	if len(store.tokens) != 1 || store.tokens[0].consumed {
		t.Fatal("发信失败不补偿删除，token 保留自然过期")
	}
}

func TestRequestCodeOccupiedEmailGetsFixedHint(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	store.seedIdentity(testEmail, 7, "old-hash")
	_, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip")
	if !errors.Is(err, ErrEmailOccupied) {
		t.Fatalf("占用应报 ErrEmailOccupied，实际 %v", err)
	}
	if !strings.Contains(EmailOccupiedHint, "不会自动合并") || strings.Contains(EmailOccupiedHint, "解绑") {
		t.Fatal("固定提示须说明不自动合并，且不指向当前不存在的解绑命令")
	}
}

func TestRequestCodeRecoverUnknownEmailIsUniform(t *testing.T) {
	svc, _, sender, _ := newTestService(t)
	res, err := svc.RequestCode(context.Background(), EmailPurposeRecover, testEmail, "ip")
	if err != nil || res.Delivered {
		t.Fatalf("未注册邮箱找回应静默成功但不发信: %+v %v", res, err)
	}
	if sender.calls != 0 {
		t.Fatal("未注册邮箱不得实际发信")
	}
}

func TestVerifyRegisterCreatesAccountAtomically(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Verify(context.Background(), testEmail, "123456", "password123", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.UserID <= 0 || res.Username == "" || res.Purpose != EmailPurposeRegister {
		t.Fatalf("注册结果不对: %+v", res)
	}
	if !store.tokens[0].consumed {
		t.Fatal("验证码应已消费")
	}
	if !verifyPassword(store.creds[res.UserID], "password123") {
		t.Fatal("argon2 密码未正确写入")
	}
	if store.identities[testEmail] != res.UserID {
		t.Fatal("email 身份未绑定到新账号")
	}
}

func TestVerifyWrongCodeIncrementsAttemptsInTransaction(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Verify(context.Background(), testEmail, "654321", "password123", "ip")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("错码应报 ErrInvalidCode，实际 %v", err)
	}
	if store.tokens[0].attempts != 1 {
		t.Fatalf("错码应在事务内 attempts++，实际 %d", store.tokens[0].attempts)
	}
	if store.tokens[0].consumed {
		t.Fatal("错码不应消费验证码")
	}
	if len(store.users) != 0 {
		t.Fatal("错码不得建号")
	}
}

func TestVerifyConcurrentOnlyOneClaimWins(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	// 用不同限流作用域，避免「尝试次数」先于并发抢消费触发拒绝。
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = svc.Verify(context.Background(), testEmail, "123456", "password123", "")
		}(i)
	}
	wg.Wait()
	ok, bad := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInvalidCode):
			bad++
		default:
			t.Fatalf("意外错误: %v", err)
		}
	}
	if ok != 1 || bad != 1 {
		t.Fatalf("并发消费必须一成一败，实际 成功=%d 失败=%d", ok, bad)
	}
}

func TestVerifyRecoverResetsPasswordAndRevokesSessions(t *testing.T) {
	svc, store, sender, revoker := newTestService(t)
	oldHash, _ := hashPassword("oldpassword123")
	store.seedIdentity(testEmail, 42, oldHash)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRecover, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatal("已注册邮箱找回应发信")
	}
	res, err := svc.Verify(context.Background(), testEmail, "123456", "newpassword123", "ip")
	if err != nil {
		t.Fatal(err)
	}
	if res.UserID != 42 || res.Purpose != EmailPurposeRecover {
		t.Fatalf("找回结果不对: %+v", res)
	}
	if verifyPassword(store.creds[42], "oldpassword123") || !verifyPassword(store.creds[42], "newpassword123") {
		t.Fatal("密码未正确重置")
	}
	calls, uid := revoker.stats()
	if calls != 1 || uid != 42 || res.SessionsClosed != 2 {
		t.Fatalf("找回后应撤销该用户全部会话: calls=%d uid=%d closed=%d", calls, uid, res.SessionsClosed)
	}
}

func TestVerifyExpiredCodeRejected(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	store.tokens[0].expiresAt = time.Unix(1_699_999_999, 0).UTC()
	_, err := svc.Verify(context.Background(), testEmail, "123456", "password123", "ip")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("过期码应拒绝，实际 %v", err)
	}
}

func TestLoginEmailSuccessAndFailure(t *testing.T) {
	svc, store, _, _ := newTestService(t)
	hash, _ := hashPassword("correct1234")
	store.seedIdentity(testEmail, 9, hash)

	got, err := svc.Login(context.Background(), testEmail, "correct1234", "ip")
	if err != nil || got.UserID != 9 || got.Username == "" {
		t.Fatalf("正确密码应登录成功: %+v %v", got, err)
	}
	_, err = svc.Login(context.Background(), testEmail, "wrong1234", "ip")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误密码应拒绝，实际 %v", err)
	}
}

func TestResendConsumesOldAndSendsNew(t *testing.T) {
	svc, store, sender, _ := newTestService(t)
	if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	// 冷却是 60s：把时钟推进后再重发。
	now := time.Unix(1_700_000_061, 0).UTC()
	svc.cooldown = NewRateLimiter(WithRateLimitConfig(DefaultEmailCooldown), WithRateLimitClock(func() time.Time { return now }))
	svc.hourly = NewRateLimiter(WithRateLimitConfig(DefaultEmailHourly), WithRateLimitClock(func() time.Time { return now }))
	if err := svc.Resend(context.Background(), testEmail, "ip"); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 {
		t.Fatalf("重发应再发一封，实际 %d", sender.calls)
	}
	active := 0
	for _, tok := range store.tokens {
		if !tok.consumed {
			active++
		}
	}
	if active != 1 || len(store.tokens) != 2 {
		t.Fatalf("/resend 应作废旧 token 并新增一条，不删行: total=%d active=%d", len(store.tokens), active)
	}
}

// 三类限流：60s 冷却、每小时 5 封、5 次尝试。
func TestEmailRateLimits(t *testing.T) {
	t.Run("60s resend cooldown", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
			t.Fatal(err)
		}
		_, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip")
		var rl *RateLimitError
		if !errors.As(err, &rl) || rl.Kind != LimitVerify {
			t.Fatalf("60s 内重发应被限流，实际 %v", err)
		}
	})

	t.Run("5 emails per hour", func(t *testing.T) {
		base := time.Unix(1_700_000_000, 0).UTC()
		now := base
		svc, store, _, _ := newTestService(t)
		svc.cooldown = NewRateLimiter(WithRateLimitConfig(RateLimitConfig{MaxAttempts: 100, Window: time.Hour, Lockout: time.Hour}), WithRateLimitClock(func() time.Time { return now }))
		svc.hourly = NewRateLimiter(WithRateLimitConfig(DefaultEmailHourly), WithRateLimitClock(func() time.Time { return now }))
		for i := 0; i < 5; i++ {
			now = base.Add(time.Duration(i) * 61 * time.Second)
			for j := range store.tokens {
				store.tokens[j].consumed = true
			}
			if _, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip"); err != nil {
				t.Fatalf("第 %d 封应成功: %v", i+1, err)
			}
		}
		now = base.Add(5 * 61 * time.Second)
		_, err := svc.RequestCode(context.Background(), EmailPurposeRegister, testEmail, "ip")
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("每小时第 6 封应被限流，实际 %v", err)
		}
	})

	t.Run("5 verify attempts", func(t *testing.T) {
		svc, _, _, _ := newTestService(t)
		for i := 0; i < 5; i++ {
			_, _ = svc.Verify(context.Background(), testEmail, "111111", "password123", "ip")
		}
		_, err := svc.Verify(context.Background(), testEmail, "111111", "password123", "ip")
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("第 6 次尝试应被限流，实际 %v", err)
		}
	})
}
