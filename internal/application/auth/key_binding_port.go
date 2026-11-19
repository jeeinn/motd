package auth

import (
	"context"
	"time"
)

// 公钥绑定端口（评审稿 §2.2 / ADR-001「端口定义在使用方」、ADR-010 UoW）。
//
// 与 forum.Repos / DeviceAuthStore / LoginStore 同构：application 只声明接口，
// SQL 实现落在 infrastructure/sqlite；读走只读门面，写一律经 UnitOfWork.Write
// 拿到受限 WriteTx，拿不到 *sql.Tx。

// KeyBindingView 是单个写事务内读到的绑定状态。
// 刻意**包含已吊销行**：ssh_keys.fingerprint 是 UNIQUE，吊销只是打 revoked_at
// 标记，行仍然存在并继续占位，否则「一钥只属一号」会被重新绑定绕过。
type KeyBindingView struct {
	Found   bool
	UserID  int64
	Revoked bool
}

// NewKeyBinding 是插入 ssh_keys 的参数。PublicKey 由用例先过
// ValidatePublicKeyText，仓储再兜一层。
type NewKeyBinding struct {
	UserID      int64
	Fingerprint string
	PublicKey   string
	CreatedAt   time.Time
}

// KeyBindingWriteTx 是单个写事务内的受限句柄。
// 判定（FindByFingerprint）与写入（Insert / Revoke）绑定同一事务快照，
// 避免并发绑定出现「两个事务都判定未命中，然后都插入」的 TOCTOU。
type KeyBindingWriteTx interface {
	// FindByFingerprint 按指纹精确查一行（含已吊销）；不存在返回空 Found=false。
	FindByFingerprint(ctx context.Context, fingerprint string) (KeyBindingView, error)

	// Insert 新建绑定。fingerprint UNIQUE 冲突必须映射为 ErrKeyConflict，
	// 由用例整体重试（事务已回滚、无半成品）。
	Insert(ctx context.Context, b NewKeyBinding) error

	// Revoke 软吊销：UPDATE ... SET revoked_at=? WHERE fingerprint=?
	//   AND user_id=? AND revoked_at IS NULL，返回受影响行数。
	// 必须用 user_id 钉住归属（只能解绑自己的钥匙），且**禁止物理 DELETE** ——
	// 删掉行会让该指纹从「命中已吊销 → 拒绝认证」退化成「未命中 → 访客放行」。
	Revoke(ctx context.Context, userID int64, fingerprint string, at time.Time) (int64, error)
}

// KeyBindingUnitOfWork 把「一次绑定变更 = 一个写事务」变成结构性约束。
type KeyBindingUnitOfWork interface {
	Write(ctx context.Context, fn func(KeyBindingWriteTx) error) error
}

// KeyBindingReader 是 /keys 的只读端口，走读池，不占唯一写连接。
type KeyBindingReader interface {
	ListActive(ctx context.Context, userID int64) ([]KeyBinding, error)

	// GetByFingerprint 查询当前账号的一把未吊销公钥详情（P1-M4）。
	// userID 必须写进查询条件：查不到（含属于别人的钥匙）一律返回
	// ErrKeyNotFound，不区分「不存在」与「跨用户」，避免指纹枚举泄露。
	GetByFingerprint(ctx context.Context, userID int64, fingerprint string) (KeyBinding, error)
}

// KeyBindingStore 是装配时注入给 KeyBindingService 的完整端口集合。
type KeyBindingStore interface {
	KeyBindingReader
	KeyBindingUnitOfWork
}

// SessionRevoker 在解绑事务**提交成功后**撤销仍在用该指纹的在线会话。
//
// 端口定义在使用方（application），实现落在 transport/ssh：只有它握有全部
// *Session 且能关闭底层 channel。这是纯内存动作、不会失败，因此不返回 error；
// 也正因为无法回滚，它绝不能被放进写事务闭包 —— 一旦事务回滚，会话已经被
// 踢掉且无法复原。archtest 会检查调用点在 Write 之后。
type SessionRevoker interface {
	RevokeSessions(ctx context.Context, fingerprint string) int
}

// UserSessionRevoker 是注销边界的会话撤销端口（P1-M2）。
//
// 与 SessionRevoker 同构：端口在 application，实现在 transport/ssh。
// 按 userID 关闭该账号的**所有**在线会话，包括触发注销的当前会话。
// 同样是纯内存动作、不可回滚，因此绝不能放进写事务闭包。
type UserSessionRevoker interface {
	RevokeUserSessions(ctx context.Context, userID int64) int
}
