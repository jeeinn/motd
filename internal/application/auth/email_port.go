package auth

import (
	"context"
	"errors"
	"time"
)

// ProviderEmail 是邮箱身份源的固定标识（DDL CHECK 允许 github/email）。
const ProviderEmail = "email"

// 邮箱验证码用途。DDL 的 CHECK 还包含 'verify'（绑定新邮箱到已有账号），
// 本里程碑只启用 register / recover 两类，不臆造第三类的用例。
const (
	EmailPurposeRegister = "register"
	EmailPurposeRecover  = "recover"
)

var (
	// ErrInvalidEmail 表示邮箱语法或长度不合法（调用方据此给固定提示）。
	ErrInvalidEmail = errors.New("邮箱格式不合法")
	// ErrWeakPassword 表示密码低于强度下限。
	ErrWeakPassword = errors.New("密码强度不足")
	// ErrEmailOccupied 是 R7/ADR D-c：邮箱已被别的账号占用，绝不自动合并。
	ErrEmailOccupied = errors.New("邮箱已被占用")
	// ErrInvalidCode 统一表示码错 / 已用 / 过期 / 超次 —— 对外不分流，避免探测。
	ErrInvalidCode = errors.New("验证码无效或已过期")
	// ErrEmailNotRegistered 表示找回目标不存在；对终端仍回统一文案防枚举。
	ErrEmailNotRegistered = errors.New("邮箱未注册")
	// ErrInvalidCredentials 表示邮箱或密码错误。
	ErrInvalidCredentials = errors.New("邮箱或密码错误")
	// ErrNoPendingToken 表示 /resend /verify 之前没有发起过注册或找回。
	ErrNoPendingToken = errors.New("没有待验证的邮箱流程")
	// ErrTokenConsumed 表示并发下另一个请求抢先消费了同一个 token。
	ErrTokenConsumed = errors.New("验证码已被并发消费")
)

// EmailOccupiedHint 是 R7 的固定文案：不含邮箱原文（外部数据绝不进终端拼接）。
// 当前无 /unlink 命令（解绑入口属后续阶段），因此不指向不存在的操作；
// 需要变更邮箱归属时由运维从数据层处理，避免把用户引到死路。
const EmailOccupiedHint = "该邮箱已属于其他账号，不会自动合并；如需变更归属请联系管理员处理"

// EmailSender 是投递端口（端口在 application，实现在 infrastructure/email）。
// 只有一个方法、一个生产实现；不做多 Provider 抽象。
type EmailSender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// NewEmailToken 是写 token 的最小参数。CodeHash 必须是散列值，绝不传明文码。
type NewEmailToken struct {
	Email     string
	CodeHash  string
	Purpose   string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// EmailTokenView 是事务内读到的 token 投影。Attempts 由仓储在事务内维护。
type EmailTokenView struct {
	ID        int64
	Email     string
	CodeHash  string
	Purpose   string
	ExpiresAt time.Time
	Attempts  int
	Consumed  bool
}

// CredentialView 是邮箱登录读取的最小凭证投影。
type CredentialView struct {
	UserID       int64
	Username     string
	Status       string
	PasswordHash string
}

// EmailWriteTx 是单个写事务内的受限句柄。所有 token 状态变更与账号创建 /
// 密码重置绑定同一事务快照；用例闭包拿不到 *sql.Tx。
// 前三个方法与 LoginWriteTx 同构（由同一个 sqlLoginTx 提供），避免两套口径。
type EmailWriteTx interface {
	UsernameExists(ctx context.Context, username string) (bool, error)
	InsertUser(ctx context.Context, a NewAccount) (int64, error)
	InsertIdentity(ctx context.Context, i NewIdentity) error
	// FindAccountByEmail 与 LoginWriteTx 同口径，用来探测 users.primary_email 占用。
	FindAccountByEmail(ctx context.Context, email string) (AccountView, error)

	// InsertEmailToken 写一条新 token（只存散列）。
	InsertEmailToken(ctx context.Context, t NewEmailToken) (int64, error)
	// FindLatestActiveToken 取该邮箱最近一条未消费的 register/recover token。
	// 未命中返回 ErrNoPendingToken。
	FindLatestActiveToken(ctx context.Context, email string) (EmailTokenView, error)
	// ConsumeEmailToken 以 consumed_at IS NULL 为条件抢消费；0 行返回 ErrTokenConsumed。
	ConsumeEmailToken(ctx context.Context, id int64, at time.Time) error
	// NoteEmailTokenAttempt 在事务内错码计数 +1。
	NoteEmailTokenAttempt(ctx context.Context, id int64) error

	// FindUserByEmailIdentity 按 (provider='email', provider_uid=email) 命中账号。
	// 未命中返回 ErrLoginNotFound。
	FindUserByEmailIdentity(ctx context.Context, email string) (AccountView, error)
	// UpsertLocalCredential 在账号已存在时写入 / 覆盖密码散列。
	UpsertLocalCredential(ctx context.Context, userID int64, passwordHash string, at time.Time) error
}

// EmailUnitOfWork 把「一次 token 状态变更 / 账号创建 = 一个写事务」变成结构约束。
type EmailUnitOfWork interface {
	Write(ctx context.Context, fn func(EmailWriteTx) error) error
}

// EmailStore 是装配给 EmailAuthService 的完整端口集合。
// FindCredentialByEmail 走读池，供 /login email 在事务外做 argon2 校验。
type EmailStore interface {
	EmailUnitOfWork
	FindCredentialByEmail(ctx context.Context, email string) (CredentialView, error)
}
