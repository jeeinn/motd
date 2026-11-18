package auth

import (
	"context"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// 登录事务端口（评审稿 §2.2 / ADR-001「端口定义在使用方」、ADR-010 UoW）。
//
// 与 forum.Repos / DeviceAuthStore 同构：application 只声明接口，SQL 实现落在
// infrastructure/sqlite。此处**不设独立读端口**：§4.3 的三步判定（查身份 / 查邮箱 /
// 探用户名）必须与 users+identities 的插入处在同一个写事务快照内，否则并发首登
// 会留下 TOCTOU 窗口，只能靠唯一约束兜底。网络调用（GetUser/GetEmails）由用例
// 在开写事务之前完成，绝不塞进 Write 闭包（ADR-022 的同一条边界）。

// AccountView 是登录事务内读到的账号投影，只含判定所需的最小字段。
type AccountView struct {
	UserID   int64
	Username string
	Status   domainuser.Status
}

// NewAccount 是写入 users 的参数。PrimaryEmail 为空时落 NULL（无验证邮箱）。
type NewAccount struct {
	Username     string
	DisplayName  string
	PrimaryEmail string
	Status       domainuser.Status
	CreatedAt    time.Time
}

// NewIdentity 是写入 identities 的参数。Email 为空时落 NULL。
type NewIdentity struct {
	UserID        int64
	Provider      string
	ProviderUID   string
	Email         string
	EmailVerified bool
	CreatedAt     time.Time
}

// LoginWriteTx 是单个写事务内的受限句柄。所有查询与两张表的插入绑定同一个
// 事务快照；用例闭包拿不到 *sql.Tx，也就无法绕过事务边界（ADR-010）。
type LoginWriteTx interface {
	// FindIdentityByUID 按 (provider, provider_uid) 查已绑定身份，命中返回账号视图，
	// 未命中返回 ErrLoginNotFound。这是 §4.3 步骤 1 的唯一依据。
	FindIdentityByUID(ctx context.Context, provider, providerUID string) (AccountView, error)

	// FindAccountByEmail 只用于 §4.3 步骤 2 生成「不自动合并」提示，**不复用账号**。
	// 未命中返回 ErrLoginNotFound。命中多个时由实现取一条即可（判定只需存在性）。
	FindAccountByEmail(ctx context.Context, email string) (AccountView, error)

	// UsernameExists 用 NOCASE 语义探测用户名占用，供 AllocateUsername 后缀重试。
	UsernameExists(ctx context.Context, username string) (bool, error)

	// InsertUser / InsertIdentity 必须在同一事务内连续调用；任一失败整体回滚，
	// 绝不允许出现「有 users 无 identities」的半成品账号。
	// 唯一约束冲突由实现映射为 ErrUsernameConflict / ErrIdentityConflict。
	InsertUser(ctx context.Context, a NewAccount) (int64, error)
	InsertIdentity(ctx context.Context, i NewIdentity) error
}

// LoginUnitOfWork 把「一次登录 = 一个写事务」变成结构性约束。
type LoginUnitOfWork interface {
	Write(ctx context.Context, fn func(LoginWriteTx) error) error
}

// LoginStore 是装配时注入给 LoginService 的完整端口集合。
type LoginStore interface {
	LoginUnitOfWork
}
