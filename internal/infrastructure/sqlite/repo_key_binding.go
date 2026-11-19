package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

// KeyBindingRepos 实现 P0-18 的 application/auth.KeyBindingStore。
//
// 与 ForumRepos / DeviceAuthRepos / LoginRepos 同一口径（ADR-010）：读走 Pools
// 只读门面，写经 Writer.Submit 的单事务；本包之外拿不到裸 *sql.DB。
// ssh_keys 表已在 0001_init.sql 建好，P0-18 **不新增迁移**。
var (
	_ auth.KeyBindingStore   = (*KeyBindingRepos)(nil)
	_ auth.KeyBindingWriteTx = (*sqlKeyBindingTx)(nil)
)

type KeyBindingRepos struct {
	pools  *Pools
	writer *Writer
}

// NewKeyBindingRepos 装配仓储。writer 必须已 Start（由 cmd/server 负责生命周期）。
func NewKeyBindingRepos(pools *Pools, writer *Writer) *KeyBindingRepos {
	return &KeyBindingRepos{pools: pools, writer: writer}
}

// ListActive 是 /keys 的只读查询：只列未吊销、且属于该账号的钥匙，按创建顺序。
// label 恒为服务端默认值（P0 不提供设置入口），last_used_at 可能为空。
func (r *KeyBindingRepos) ListActive(ctx context.Context, userID int64) ([]auth.KeyBinding, error) {
	rows, err := r.pools.QueryContext(ctx, `
		SELECT fingerprint, label, created_at, last_used_at
		FROM ssh_keys
		WHERE user_id = ? AND revoked_at IS NULL
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("查询公钥列表: %w", err)
	}
	defer rows.Close()

	var out []auth.KeyBinding
	for rows.Next() {
		var (
			b     auth.KeyBinding
			last  sql.NullInt64
			label sql.NullString
		)
		if err := rows.Scan(&b.Fingerprint, &label, &b.CreatedAtUnix, &last); err != nil {
			return nil, fmt.Errorf("扫描公钥列表: %w", err)
		}
		if label.Valid {
			b.Label = label.String
		}
		if last.Valid {
			b.LastUsedUnix = last.Int64
			b.HasLastUsed = true
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历公钥列表: %w", err)
	}
	return out, nil
}

// GetByFingerprint 是 /keys <fingerprint> 的单行只读查询（P1-M4，ADR-037）。
// userID 写进 WHERE 是安全边界：别人的钥匙与本账号不存在的钥匙返回同一个
// ErrKeyNotFound，调用方无法借此枚举他人指纹。
// label 目前恒为空（P0 起就没有设置入口），仅作为展示字段读出来备用。
func (r *KeyBindingRepos) GetByFingerprint(ctx context.Context, userID int64, fingerprint string) (auth.KeyBinding, error) {
	var (
		b     auth.KeyBinding
		last  sql.NullInt64
		label sql.NullString
	)
	err := r.pools.QueryRowContext(ctx, `
		SELECT fingerprint, label, created_at, last_used_at
		FROM ssh_keys
		WHERE user_id = ? AND fingerprint = ? AND revoked_at IS NULL`,
		userID, fingerprint).Scan(&b.Fingerprint, &label, &b.CreatedAtUnix, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.KeyBinding{}, auth.ErrKeyNotFound
	}
	if err != nil {
		return auth.KeyBinding{}, fmt.Errorf("查询公钥详情: %w", err)
	}
	if label.Valid {
		b.Label = label.String
	}
	if last.Valid {
		b.LastUsedUnix = last.Int64
		b.HasLastUsed = true
	}
	return b, nil
}

// TouchLastUsed 记录一次成功认证的使用时间（P1-M4）。只改 last_used_at，
// 且用 fingerprint + revoked_at IS NULL 钉住目标：已吊销的钥匙不再更新。
// 0 行是合法结果（钥匙恰好被解绑），不当成错误——它只是运维痕迹，不影响认证。
// 节流与异步由调用方 KeyUsageRecorder 负责，这里只做一次受限的写事务。
func (r *KeyBindingRepos) TouchLastUsed(ctx context.Context, fingerprint string, at time.Time) error {
	return r.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE ssh_keys SET last_used_at = ?
			WHERE fingerprint = ? AND revoked_at IS NULL`, toUnix(at), fingerprint)
		if err != nil {
			return fmt.Errorf("更新公钥使用时间: %w", err)
		}
		return nil
	})
}

// Write 把用例闭包包进 Writer.Submit 的单事务（ADR-010）。
func (r *KeyBindingRepos) Write(ctx context.Context, fn func(auth.KeyBindingWriteTx) error) error {
	if fn == nil {
		return errors.New("公钥绑定写入闭包为空")
	}
	return r.writer.Submit(ctx, func(_ context.Context, tx *sql.Tx) error {
		return fn(&sqlKeyBindingTx{tx: tx})
	})
}

// sqlKeyBindingTx 是事务内的受限句柄，所有语句绑定同一个 *sql.Tx。
type sqlKeyBindingTx struct {
	tx *sql.Tx
}

// FindByFingerprint 含已吊销行：吊销只打 revoked_at 标记，行仍占位。
// 少了这一步，重新绑定会绕过「一钥只属一号」。
func (w *sqlKeyBindingTx) FindByFingerprint(ctx context.Context, fingerprint string) (auth.KeyBindingView, error) {
	var (
		userID  int64
		revoked sql.NullInt64
	)
	err := w.tx.QueryRowContext(ctx,
		`SELECT user_id, revoked_at FROM ssh_keys WHERE fingerprint = ?`, fingerprint).
		Scan(&userID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.KeyBindingView{}, nil
	}
	if err != nil {
		return auth.KeyBindingView{}, fmt.Errorf("查询公钥绑定: %w", err)
	}
	return auth.KeyBindingView{
		Found:   true,
		UserID:  userID,
		Revoked: revoked.Valid,
	}, nil
}

// Insert 写入 ssh_keys。fingerprint 的 UNIQUE 是「一钥只属一号」的最后防线：
// 并发撞上后映射成 auth.ErrKeyConflict，用例整体重试（首轮事务已回滚）。
// 仓储侧再过一次 ValidatePublicKeyText，防止任何绕过用例的直写路径落脏数据。
func (w *sqlKeyBindingTx) Insert(ctx context.Context, b auth.NewKeyBinding) error {
	if b.UserID <= 0 {
		return fmt.Errorf("拒绝写入非法用户 ID %d", b.UserID)
	}
	if !auth.ValidFingerprint(b.Fingerprint) {
		return fmt.Errorf("拒绝写入非法指纹格式")
	}
	if err := auth.ValidatePublicKeyText(b.PublicKey); err != nil {
		return fmt.Errorf("拒绝写入非法公钥文本: %w", err)
	}
	_, err := w.tx.ExecContext(ctx, `
		INSERT INTO ssh_keys
			(user_id, fingerprint, public_key, label, last_used_at, revoked_at, created_at)
		VALUES (?, ?, ?, '', NULL, NULL, ?)`,
		b.UserID, b.Fingerprint, b.PublicKey, toUnix(b.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrKeyConflict
		}
		return fmt.Errorf("插入公钥绑定: %w", err)
	}
	return nil
}

// Revoke 是**软吊销**：只打 revoked_at 标记，绝不 DELETE。
//
// 物理删除会让该指纹从 P0-14 的「命中已吊销 → 拒绝认证」退化成
// 「未命中 → 访客放行」，等于把吊销动作反过来放宽权限。WHERE 里同时钉住
// user_id（只能解绑自己的钥匙）与 revoked_at IS NULL（幂等）。
func (w *sqlKeyBindingTx) Revoke(ctx context.Context, userID int64, fingerprint string, at time.Time) (int64, error) {
	res, err := w.tx.ExecContext(ctx, `
		UPDATE ssh_keys SET revoked_at = ?
		WHERE fingerprint = ? AND user_id = ? AND revoked_at IS NULL`,
		toUnix(at), fingerprint, userID)
	if err != nil {
		return 0, fmt.Errorf("吊销公钥: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("检查吊销行数: %w", err)
	}
	return n, nil
}
