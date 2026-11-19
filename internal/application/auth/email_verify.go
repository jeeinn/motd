package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// EmailVerifyResult 是一次成功消费的确定结果。
type EmailVerifyResult struct {
	UserID         int64
	Username       string
	Purpose        string
	Created        bool
	SessionsClosed int // 仅 recover：重置后撤销的旧会话数
}

// Verify 单事务原子消费验证码；register 建号、recover 重置密码。
//
// 顺序：入参收口 → argon2 散列密码（事务外，19MiB 不占写连接）→
// 限流 → 单写事务（比码/查过期/查次数 → 抢消费 → 建号或改密）→
// 提交后按 recover 撤销全部会话（内存动作）。
func (s *EmailAuthService) Verify(ctx context.Context, rawEmail, code, password, ipHash string) (EmailVerifyResult, error) {
	email, err := s.normalizeAndValidate(rawEmail)
	if err != nil {
		return EmailVerifyResult{}, err
	}
	if !validCode(code) {
		return EmailVerifyResult{}, ErrInvalidCode
	}
	passwordHash, err := hashPassword(password) // 19MiB，必须在写事务之外
	if err != nil {
		return EmailVerifyResult{}, err
	}
	if err := s.noteAttempt(email, ipHash); err != nil {
		return EmailVerifyResult{}, err
	}

	now := s.now().UTC()
	var out EmailVerifyResult
	err = s.store.Write(ctx, func(tx EmailWriteTx) error {
		token, err := tx.FindLatestActiveToken(ctx, email)
		if err != nil {
			return ErrInvalidCode // 没有待验证流程 → 统一回「码无效」
		}
		if !codeMatches(s.pepper, code, token.CodeHash) {
			if err := s.noteBadCode(ctx, tx, token.ID, ipHash); err != nil {
				return err
			}
			return ErrInvalidCode
		}
		if token.Consumed || !now.Before(token.ExpiresAt) || token.Attempts >= 5 {
			return ErrInvalidCode
		}
		// 条件更新抢消费：RowsAffected==1 才继续，两个并发请求只有一个成功。
		if err := tx.ConsumeEmailToken(ctx, token.ID, now); err != nil {
			return ErrInvalidCode
		}
		out.Purpose = token.Purpose
		switch token.Purpose {
		case EmailPurposeRegister:
			userID, username, err := s.createAccount(ctx, tx, email, passwordHash, now)
			if err != nil {
				return err
			}
			out.UserID, out.Username, out.Created = userID, username, true
		case EmailPurposeRecover:
			acct, err := tx.FindUserByEmailIdentity(ctx, email)
			if err != nil {
				return ErrInvalidCode // 不暴露「邮箱未注册」差异
			}
			if err := tx.UpsertLocalCredential(ctx, acct.UserID, passwordHash, now); err != nil {
				return err
			}
			out.UserID, out.Username, out.Purpose = acct.UserID, acct.Username, EmailPurposeRecover
		default:
			return ErrInvalidCode
		}
		return nil
	})
	if err != nil {
		return EmailVerifyResult{}, err
	}

	// 找回成功后旧会话必须失效；撤销是内存动作，事务提交后才调用。
	if out.Purpose == EmailPurposeRecover {
		out.SessionsClosed = s.revoker.RevokeUserSessions(ctx, out.UserID)
		if auditErr := s.noteEvent(AuditSessionRevoked, out.UserID, ipHash, out.SessionsClosed); auditErr != nil {
			return out, auditErr // 撤销已生效，错误只用于上报
		}
	}
	return out, nil
}

// createAccount 在事务内建 users + identities + local_credentials。
// 用户名派生完全复用 GitHub 规则：DeriveUsername（两字中文补下划线 /
// 超长截断 / 保留名）→ AllocateUsername（冲突加数字），不新造一套。
func (s *EmailAuthService) createAccount(ctx context.Context, tx EmailWriteTx, email, passwordHash string, now time.Time) (int64, string, error) {
	occupied, err := s.emailOccupied(ctx, tx, email)
	if err != nil {
		return 0, "", err
	}
	if occupied {
		return 0, "", ErrEmailOccupied // R7：命中占用只回固定提示，绝不合并
	}
	base, err := DeriveUsername(emailLocalPart(email), emailUID(email))
	if err != nil {
		return 0, "", err
	}
	username, err := AllocateUsername(base, func(name string) (bool, error) {
		return tx.UsernameExists(ctx, name)
	}, DefaultUsernameAttempts)
	if err != nil {
		return 0, "", err
	}
	userID, err := tx.InsertUser(ctx, NewAccount{
		Username: username, DisplayName: username, PrimaryEmail: email,
		Status: domainuser.StatusActive, CreatedAt: now,
	})
	if err != nil {
		return 0, "", err
	}
	if err := tx.InsertIdentity(ctx, NewIdentity{
		UserID: userID, Provider: ProviderEmail, ProviderUID: email,
		Email: email, EmailVerified: true, CreatedAt: now,
	}); err != nil {
		return 0, "", err
	}
	if err := tx.UpsertLocalCredential(ctx, userID, passwordHash, now); err != nil {
		return 0, "", err
	}
	return userID, username, nil
}

// EmailLoginResult 是邮箱密码认证结果，只含升级会话所需的最小字段。
type EmailLoginResult struct {
	UserID   int64
	Username string
}

// Login 用邮箱+密码认证。argon2 校验在事务外，只读一次凭证；认证失败
// 写审计并计入限流，成功由调用方复用 guest→user 升级通道。
func (s *EmailAuthService) Login(ctx context.Context, rawEmail, password, ipHash string) (EmailLoginResult, error) {
	email, err := s.normalizeAndValidate(rawEmail)
	if err != nil {
		return EmailLoginResult{}, err
	}
	if password == "" {
		return EmailLoginResult{}, ErrInvalidCredentials
	}
	if err := s.noteAttempt(email, ipHash); err != nil {
		return EmailLoginResult{}, err
	}
	cred, err := s.store.FindCredentialByEmail(ctx, email)
	if err != nil || !verifyPassword(cred.PasswordHash, password) || cred.Status != string(domainuser.StatusActive) {
		if auditErr := s.noteLoginFailure(ipHash); auditErr != nil {
			return EmailLoginResult{}, auditErr
		}
		return EmailLoginResult{}, ErrInvalidCredentials
	}
	return EmailLoginResult{UserID: cred.UserID, Username: cred.Username}, nil
}

// -------------------------------------------------------------------------
// 内部工具
// -------------------------------------------------------------------------

func (s *EmailAuthService) normalizeAndValidate(raw string) (string, error) {
	email := normalizeEmail(raw)
	if !validEmailAddress(email) {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// emailOccupied 同时看 identities(provider=email) 与 users.primary_email：
// 二者任一命中即视为「旧号占用」，按 D-c 不自动合并。
func (s *EmailAuthService) emailOccupied(ctx context.Context, tx EmailWriteTx, email string) (bool, error) {
	_, err := tx.FindUserByEmailIdentity(ctx, email)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrLoginNotFound) {
		return false, err
	}
	// primary_email 无 UNIQUE，但任何一条命中都表示这个邮箱已经属于某账号。
	_, err = tx.FindAccountByEmail(ctx, email)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, ErrLoginNotFound) {
		return false, err
	}
	return false, nil
}

func (s *EmailAuthService) allowSend(email, ipHash string) error {
	e := "e:" + emailScopeHash(email)
	if err := s.cooldown.AllowScope("resend:" + e); err != nil {
		return err
	}
	if err := s.hourly.AllowScope("hourly:" + e); err != nil {
		return err
	}
	if ipHash != "" {
		if err := s.cooldown.AllowScope("resend:i:" + ipHash); err != nil {
			return err
		}
		if err := s.hourly.AllowScope("hourly:i:" + ipHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *EmailAuthService) noteAttempt(email, ipHash string) error {
	if err := s.attempts.AllowScope("attempts:e:" + emailScopeHash(email)); err != nil {
		return err
	}
	if ipHash != "" {
		return s.attempts.AllowScope("attempts:i:" + ipHash)
	}
	return nil
}

func (s *EmailAuthService) noteBadCode(ctx context.Context, tx EmailWriteTx, tokenID int64, ipHash string) error {
	if err := tx.NoteEmailTokenAttempt(ctx, tokenID); err != nil {
		return err
	}
	return s.noteEvent(AuditVerificationFailure, 0, ipHash, 0)
}

func (s *EmailAuthService) noteLoginFailure(ipHash string) error {
	return s.noteEvent(AuditLoginFailure, 0, ipHash, 0)
}

func (s *EmailAuthService) noteEvent(event string, userID int64, ipHash string, closed int) error {
	if s.audit == nil {
		return nil
	}
	detail := `{}`
	if closed > 0 {
		detail = fmt.Sprintf(`{"sessions_closed":%d}`, closed)
	}
	return s.audit.Record(event, userID, ProviderEmail, ipHash, detail)
}

func emailScopeHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

// emailUID 给 DeriveUsername 的兜底 uid：短 hex，绝不含邮箱原文里的 @/点。
func emailUID(email string) string {
	return emailScopeHash(email)[:12]
}

func emailSubject(purpose string) string {
	if purpose == EmailPurposeRecover {
		return "motd 密码找回验证码"
	}
	return "motd 邮箱注册验证码"
}

func emailBody(code, purpose string) string {
	if purpose == EmailPurposeRecover {
		return "您的 motd 密码找回验证码是 " + code + "，10 分钟内有效。请在终端输入 /verify " + code + " 后按提示设置新密码。如非本人操作请忽略。"
	}
	return "您的 motd 注册验证码是 " + code + "，10 分钟内有效。请在终端输入 /verify " + code + " 后按提示设置密码。如非本人操作请忽略。"
}
