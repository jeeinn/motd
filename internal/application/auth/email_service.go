package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EmailAuthService 编排注册 / 找回 / 登录 / 重发四条邮箱用例。
//
// 三条不可动摇的边界：
//  1. 发信是网络调用，永远在两个写事务之间（Writer.Submit 提交后才 Send）；
//  2. token 的抢消费、purpose/过期/次数校验、账号创建/密码重置在同一写事务；
//  3. 会话撤销是内存动作，只在提交成功后调用，失败回滚时绝不提前踢人。
//
// application 层不 import 任何 infrastructure / SQL；端口见 email_port.go。
type EmailAuthService struct {
	store   EmailStore
	sender  EmailSender
	revoker UserSessionRevoker

	audit *AuditLogger

	cooldown *RateLimiter // 60s 重发冷却
	hourly   *RateLimiter // 每小时 5 封
	attempts *RateLimiter // 5 次验证/登录尝试
	precheck *RateLimiter // 预检防爆破：独立于 attempts，避免正常 verify 被双重计数

	now      func() time.Time
	pepper   []byte
	newCode  func() (string, error)
	codeLife time.Duration
}

// 四种窗口独立实例，复用 ratelimit.go 同一套状态机（§2.9）。
var (
	DefaultEmailCooldown = RateLimitConfig{MaxAttempts: 1, Window: 60 * time.Second, Lockout: 60 * time.Second, MaxKeys: 4096}
	DefaultEmailHourly   = RateLimitConfig{MaxAttempts: 5, Window: time.Hour, Lockout: time.Hour, MaxKeys: 4096}
	DefaultEmailAttempts = RateLimitConfig{MaxAttempts: 5, Window: time.Hour, Lockout: 15 * time.Minute, MaxKeys: 4096}
	// 预检是「码是否有效」的 oracle，必须限流；但阈值比 Verify 宽，
	// 让写错一两次码的正常用户不会在收密码前就被锁住，也不与 Verify 的
	// attempts 计数串号（一次正常 verify = 预检 1 次 + Verify 1 次）。
	DefaultEmailPrecheck = RateLimitConfig{MaxAttempts: 20, Window: time.Hour, Lockout: 15 * time.Minute, MaxKeys: 4096}
)

const DefaultEmailCodeTTL = 10 * time.Minute

// ErrEmailDelivery 是投递失败的应用层哨兵，token 保留自然过期，不做补偿删除。
var ErrEmailDelivery = errors.New("验证码邮件发送失败，请稍后重试或输入 /resend")

type EmailServiceOption func(*EmailAuthService)

func WithEmailAudit(a *AuditLogger) EmailServiceOption {
	return func(s *EmailAuthService) { s.audit = a }
}
func WithEmailClock(f func() time.Time) EmailServiceOption {
	return func(s *EmailAuthService) {
		if f != nil {
			s.now = f
		}
	}
}
func WithEmailPepper(p []byte) EmailServiceOption {
	return func(s *EmailAuthService) {
		if len(p) > 0 {
			s.pepper = append([]byte(nil), p...)
		}
	}
}
func WithEmailCodeFunc(f func() (string, error)) EmailServiceOption {
	return func(s *EmailAuthService) {
		if f != nil {
			s.newCode = f
		}
	}
}
func WithEmailLimiters(cooldown, hourly, attempts *RateLimiter) EmailServiceOption {
	return func(s *EmailAuthService) {
		if cooldown != nil {
			s.cooldown = cooldown
		}
		if hourly != nil {
			s.hourly = hourly
		}
		if attempts != nil {
			s.attempts = attempts
		}
	}
}

// WithEmailPrecheckLimiter 注入预检专用限流器（测试可注入假时钟）；nil 忽略。
func WithEmailPrecheckLimiter(precheck *RateLimiter) EmailServiceOption {
	return func(s *EmailAuthService) {
		if precheck != nil {
			s.precheck = precheck
		}
	}
}
func WithEmailCodeTTL(d time.Duration) EmailServiceOption {
	return func(s *EmailAuthService) {
		if d > 0 {
			s.codeLife = d
		}
	}
}

// NewEmailAuthService 构造用例。store/sender/revoker 为空属装配错误，立即 panic。
func NewEmailAuthService(store EmailStore, sender EmailSender, revoker UserSessionRevoker, opts ...EmailServiceOption) *EmailAuthService {
	if store == nil || sender == nil || revoker == nil {
		panic("auth.NewEmailAuthService: store/sender/revoker 不能为空")
	}
	pepper, err := newPepper()
	if err != nil {
		panic("auth.NewEmailAuthService: " + err.Error()) // crypto/rand 失败属致命
	}
	s := &EmailAuthService{
		store: store, sender: sender, revoker: revoker,
		cooldown: NewRateLimiter(WithRateLimitConfig(DefaultEmailCooldown)),
		hourly:   NewRateLimiter(WithRateLimitConfig(DefaultEmailHourly)),
		attempts: NewRateLimiter(WithRateLimitConfig(DefaultEmailAttempts)),
		precheck: NewRateLimiter(WithRateLimitConfig(DefaultEmailPrecheck)),
		now:      time.Now,
		pepper:   pepper,
		newCode:  generateCode,
		codeLife: DefaultEmailCodeTTL,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// EmailRequestResult 是 /register /recover 发起阶段的结果。
// Delivered=false 且 err=nil 表示「目标不存在但对外仍回统一文案」（防枚举）。
type EmailRequestResult struct {
	Delivered bool
}

// RequestCode 写 token（事务内）→ 提交后在事务外发信。
// 发信失败保留 token 自然过期，不补偿删除；purpose 只接受 register/recover。
func (s *EmailAuthService) RequestCode(ctx context.Context, purpose, rawEmail, ipHash string) (EmailRequestResult, error) {
	email, err := s.normalizeAndValidate(rawEmail)
	if err != nil {
		return EmailRequestResult{}, err
	}
	if purpose != EmailPurposeRegister && purpose != EmailPurposeRecover {
		return EmailRequestResult{}, fmt.Errorf("%w: 未知用途", ErrInvalidCode)
	}
	if err := s.allowSend(email, ipHash); err != nil {
		return EmailRequestResult{}, err
	}

	code, err := s.newCode()
	if err != nil || !validCode(code) {
		return EmailRequestResult{}, fmt.Errorf("生成验证码失败: %v", err)
	}
	now := s.now().UTC()
	token := NewEmailToken{
		Email: email, CodeHash: hashCode(s.pepper, code), Purpose: purpose,
		ExpiresAt: now.Add(s.codeLife), CreatedAt: now,
	}

	var exists bool
	err = s.store.Write(ctx, func(tx EmailWriteTx) error {
		occupied, err := s.emailOccupied(ctx, tx, email)
		if err != nil {
			return err
		}
		if purpose == EmailPurposeRegister && occupied {
			return ErrEmailOccupied
		}
		if purpose == EmailPurposeRecover && !occupied {
			exists = false
			return nil
		}
		exists = true
		_, err = tx.InsertEmailToken(ctx, token)
		return err
	})
	if err != nil {
		return EmailRequestResult{}, err
	}
	if !exists {
		return EmailRequestResult{Delivered: false}, nil
	}

	// 事务已提交，此刻才触网；3s 写超时与 SMTP 完全解耦。
	if err := s.sender.Send(ctx, email, emailSubject(purpose), emailBody(code, purpose)); err != nil {
		return EmailRequestResult{}, ErrEmailDelivery
	}
	return EmailRequestResult{Delivered: true}, nil
}

// Resend 作废旧 token（置 consumed_at，不删行）→ 写新 token → 提交后发信。
func (s *EmailAuthService) Resend(ctx context.Context, rawEmail, ipHash string) error {
	email, err := s.normalizeAndValidate(rawEmail)
	if err != nil {
		return err
	}
	if err := s.allowSend(email, ipHash); err != nil {
		return err
	}
	code, err := s.newCode()
	if err != nil || !validCode(code) {
		return fmt.Errorf("生成验证码失败: %v", err)
	}
	now := s.now().UTC()
	var purpose string
	err = s.store.Write(ctx, func(tx EmailWriteTx) error {
		old, err := tx.FindLatestActiveToken(ctx, email)
		if err != nil {
			return err
		}
		if err := tx.ConsumeEmailToken(ctx, old.ID, now); err != nil {
			return err
		}
		purpose = old.Purpose
		_, err = tx.InsertEmailToken(ctx, NewEmailToken{
			Email: email, CodeHash: hashCode(s.pepper, code), Purpose: purpose,
			ExpiresAt: now.Add(s.codeLife), CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return err
	}
	if err := s.sender.Send(ctx, email, emailSubject(purpose), emailBody(code, purpose)); err != nil {
		return ErrEmailDelivery
	}
	return nil
}
