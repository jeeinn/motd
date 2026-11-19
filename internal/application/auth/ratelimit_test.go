package auth

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestRateLimitLockoutAndRecoveryWithInjectedClock(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	cfg := RateLimitConfig{MaxAttempts: 3, Window: time.Minute, Lockout: 2 * time.Minute}
	r := NewRateLimiter(WithRateLimitConfig(cfg), WithRateLimitClock(clk.now))

	for i := 0; i < 3; i++ {
		if err := r.AllowDeviceStart("ip-a"); err != nil {
			t.Fatalf("第 %d 次应放行: %v", i+1, err)
		}
	}
	var rl *RateLimitError
	err := r.AllowDeviceStart("ip-a")
	if !errors.As(err, &rl) {
		t.Fatalf("第 4 次应命中限流，实际 %v", err)
	}
	if rl.Kind != LimitDeviceStart || rl.RetryAfter != 2*time.Minute {
		t.Fatalf("锁定时长/场景错误: kind=%v retry=%v", rl.Kind, rl.RetryAfter)
	}
	if got := rl.RetryAfterSeconds(); got != 120 {
		t.Fatalf("RetryAfterSeconds=%d，期望 120", got)
	}

	// 锁定内继续请求：RetryAfter 随注入时钟递减。
	clk.advance(30 * time.Second)
	if err := r.AllowDeviceStart("ip-a"); !errors.As(err, &rl) || rl.RetryAfter != 90*time.Second {
		t.Fatalf("锁定期内 RetryAfter 应递减到 90s，实际 %v", err)
	}

	// 锁定到期恢复；窗口内的旧尝试已被清空，重新拥有 3 次额度。
	clk.advance(90 * time.Second)
	if err := r.AllowDeviceStart("ip-a"); err != nil {
		t.Fatalf("锁定到期应恢复: %v", err)
	}
}

func TestRateLimitSlidingWindowExpiresOldAttempts(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := NewRateLimiter(
		WithRateLimitConfig(RateLimitConfig{MaxAttempts: 2, Window: time.Minute, Lockout: time.Hour}),
		WithRateLimitClock(clk.now),
	)
	if err := r.AllowKeyBinding(7); err != nil {
		t.Fatal(err)
	}
	clk.advance(30 * time.Second)
	if err := r.AllowKeyBinding(7); err != nil {
		t.Fatal(err)
	}
	// 窗口内第 3 次锁定。
	if err := r.AllowKeyBinding(7); err == nil {
		t.Fatal("窗口内第 3 次应锁定")
	}
	// 直接换一个用户：不同键互不干扰。
	if err := r.AllowKeyBinding(8); err != nil {
		t.Fatalf("另一用户不应被牵连: %v", err)
	}
}

func TestRateLimitThreeScopesAreIndependent(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := NewRateLimiter(
		WithRateLimitConfig(RateLimitConfig{MaxAttempts: 1, Window: time.Hour, Lockout: time.Hour}),
		WithRateLimitClock(clk.now),
	)
	if err := r.NoteLoginFailure("ip-a", "mail-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.AllowDeviceStart("ip-a"); err != nil {
		t.Fatalf("IP 相同但场景不同，不应互相限流: %v", err)
	}
	if err := r.AllowKeyBinding(42); err != nil {
		t.Fatalf("用户键不应被 IP 失败牵连: %v", err)
	}
	if err := r.AllowVerify("mail-a"); err != nil {
		t.Fatalf("验证场景不应被登录失败牵连: %v", err)
	}
	// 同场景同键第二次才拒绝。
	if err := r.NoteLoginFailure("ip-a", "mail-a"); err == nil {
		t.Fatal("同键第二次应拒绝")
	}
	// 换邮箱或换 IP 都是新键；只有 (ip,email) 完全相同才算同一作用域。
	if err := r.NoteLoginFailure("ip-b", "mail-a"); err != nil {
		t.Fatalf("换 IP 应是新作用域: %v", err)
	}
	if err := r.NoteLoginFailure("ip-a", "mail-b"); err != nil {
		t.Fatalf("换邮箱应是新作用域: %v", err)
	}
}

func TestRateLimitScopeValidationAndReset(t *testing.T) {
	r := NewRateLimiter()
	if err := r.AllowDeviceStart(""); !errors.Is(err, ErrInvalidRateLimitScope) {
		t.Fatalf("空作用域应报错: %v", err)
	}
	if err := r.NoteLoginFailure("", ""); !errors.Is(err, ErrInvalidRateLimitScope) {
		t.Fatalf("登录失败无 IP/邮箱应报错: %v", err)
	}
	if err := r.AllowKeyBinding(0); !errors.Is(err, ErrInvalidRateLimitScope) {
		t.Fatalf("非法 userID 应报错: %v", err)
	}
	if err := r.check(LimitUnknown, "x"); !errors.Is(err, ErrInvalidRateLimitScope) {
		t.Fatalf("未登记场景应报错: %v", err)
	}
	if err := r.AllowVerify("mail-a"); err != nil {
		t.Fatal(err)
	}
	if r.Keys() != 1 {
		t.Fatalf("应只保留 1 个作用域，实际 %d", r.Keys())
	}
	r.Reset(LimitVerify, "mail-a")
	if r.Keys() != 0 {
		t.Fatalf("Reset 后作用域应清空，实际 %d", r.Keys())
	}
}

func TestRateLimitEvictsOldestWhenFull(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0).UTC()}
	r := NewRateLimiter(
		WithRateLimitConfig(RateLimitConfig{MaxAttempts: 10, Window: time.Hour, Lockout: time.Hour, MaxKeys: 2}),
		WithRateLimitClock(clk.now),
	)
	if err := r.AllowDeviceStart("ip-1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if err := r.AllowDeviceStart("ip-2"); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if err := r.AllowDeviceStart("ip-3"); err != nil {
		t.Fatal(err)
	}
	if r.Keys() != 2 {
		t.Fatalf("容量上界应为 2，实际 %d", r.Keys())
	}
	// 最旧的 ip-1 被淘汰，因此重新计数从零开始，不会立刻锁定。
	if err := r.AllowDeviceStart("ip-1"); err != nil {
		t.Fatalf("被淘汰的最旧作用域应重新计数: %v", err)
	}
}

func TestRateLimitConfigDefaultsAndNormalization(t *testing.T) {
	r := NewRateLimiter(WithRateLimitConfig(RateLimitConfig{MaxAttempts: -1, Window: 0}))
	if r.cfg != DefaultRateLimitConfig {
		t.Fatalf("非法参数应回落默认值，实际 %+v", r.cfg)
	}
	if DefaultRateLimitConfig.MaxAttempts != 5 || DefaultAuditCapacity != 512 {
		t.Fatal("默认值被意外改动")
	}
}

func TestScopeOfAvoidsConcatenationCollision(t *testing.T) {
	if scopeOf("ab", "c") == scopeOf("a", "bc") {
		t.Fatal("两个哈希拼接必须带分隔符，避免撞键")
	}
}
