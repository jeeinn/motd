package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// EmailRepos 实现 application/auth.EmailStore（ADR-010：端口在使用方）。
//
// 写一律经 Writer.Submit 的单事务；读凭证走读池。users / identities /
// local_credentials / email_tokens 四张表均已在 0001_init.sql 建好，无新迁移。
// 本文件绝不触发网络发信——那是 application 在事务提交后的职责。
var (
	_ auth.EmailStore   = (*EmailRepos)(nil)
	_ auth.EmailWriteTx = (*sqlEmailTx)(nil)
)

type EmailRepos struct {
	writer *Writer
	pools  *Pools
}

func NewEmailRepos(pools *Pools, writer *Writer) *EmailRepos {
	return &EmailRepos{writer: writer, pools: pools}
}

// Write 把邮箱 token 状态变更与账号创建/密码重置包进同一个写事务。
func (r *EmailRepos) Write(ctx context.Context, fn func(auth.EmailWriteTx) error) error {
	if fn == nil {
		return errors.New("邮箱写入闭包为空")
	}
	return r.writer.Submit(ctx, func(_ context.Context, tx *sql.Tx) error {
		login := &sqlLoginTx{tx: tx}
		return fn(&sqlEmailTx{sqlLoginTx: login, tx: tx})
	})
}

// FindCredentialByEmail 走读池，供 /login email 在事务外做 argon2 校验。
func (r *EmailRepos) FindCredentialByEmail(ctx context.Context, email string) (auth.CredentialView, error) {
	var view auth.CredentialView
	err := r.pools.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.status, lc.password_hash
		FROM identities i
		JOIN users u ON u.id = i.user_id
		JOIN local_credentials lc ON lc.user_id = u.id
		WHERE i.provider = 'email' AND lower(i.provider_uid) = lower(?) LIMIT 1`, email).
		Scan(&view.UserID, &view.Username, &view.Status, &view.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.CredentialView{}, auth.ErrLoginNotFound
	}
	if err != nil {
		return auth.CredentialView{}, fmt.Errorf("查询邮箱凭证: %w", err)
	}
	return view, nil
}

// sqlEmailTx 复用 sqlLoginTx 的 FindAccountByEmail / UsernameExists /
// InsertUser / InsertIdentity，保证用户名探测与 GitHub 首登完全同口径。
type sqlEmailTx struct {
	*sqlLoginTx
	tx *sql.Tx
}

func (w *sqlEmailTx) InsertEmailToken(ctx context.Context, t auth.NewEmailToken) (int64, error) {
	if t.CodeHash == "" || t.Purpose == "" {
		return 0, errors.New("token 缺少散列或用途")
	}
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO email_tokens
			(email, code_hash, purpose, expires_at, attempts, consumed_at, created_at)
		VALUES (?, ?, ?, ?, 0, NULL, ?)`,
		t.Email, t.CodeHash, t.Purpose, toUnix(t.ExpiresAt), toUnix(t.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("写入邮箱验证码: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取验证码 ID: %w", err)
	}
	return id, nil
}

// FindLatestActiveToken 取最近一条未消费的 register/recover token。
// verify 用途（绑定新邮箱）本里程碑未启用，不进入候选集。
func (w *sqlEmailTx) FindLatestActiveToken(ctx context.Context, email string) (auth.EmailTokenView, error) {
	var (
		view      auth.EmailTokenView
		expiresAt int64
	)
	err := w.tx.QueryRowContext(ctx, `
		SELECT id, email, code_hash, purpose, expires_at, attempts
		FROM email_tokens
		WHERE lower(email) = lower(?) AND consumed_at IS NULL
		  AND purpose IN ('register','recover')
		ORDER BY id DESC LIMIT 1`, email).
		Scan(&view.ID, &view.Email, &view.CodeHash, &view.Purpose, &expiresAt, &view.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.EmailTokenView{}, auth.ErrNoPendingToken
	}
	if err != nil {
		return auth.EmailTokenView{}, fmt.Errorf("查询邮箱验证码: %w", err)
	}
	view.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return view, nil
}

// ConsumeEmailToken 条件更新抢消费：consumed_at IS NULL 保证并发只有一个成功。
func (w *sqlEmailTx) ConsumeEmailToken(ctx context.Context, id int64, at time.Time) error {
	res, err := w.tx.ExecContext(ctx, `
		UPDATE email_tokens SET consumed_at = ?
		WHERE id = ? AND consumed_at IS NULL`, toUnix(at), id)
	if err != nil {
		return fmt.Errorf("消费邮箱验证码: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("取验证码消费结果: %w", err)
	}
	if n != 1 {
		return auth.ErrTokenConsumed
	}
	return nil
}

func (w *sqlEmailTx) NoteEmailTokenAttempt(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, `
		UPDATE email_tokens SET attempts = attempts + 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("累计验证码尝试次数: %w", err)
	}
	return nil
}

func (w *sqlEmailTx) FindUserByEmailIdentity(ctx context.Context, email string) (auth.AccountView, error) {
	var (
		view   auth.AccountView
		status string
	)
	err := w.tx.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.status
		FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = 'email' AND lower(i.provider_uid) = lower(?) LIMIT 1`, email).
		Scan(&view.UserID, &view.Username, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.AccountView{}, auth.ErrLoginNotFound
	}
	if err != nil {
		return auth.AccountView{}, fmt.Errorf("查询邮箱身份: %w", err)
	}
	st := domainuser.Status(status)
	if !st.Valid() {
		return auth.AccountView{}, fmt.Errorf("账号 %d 的状态值非法: %q", view.UserID, status)
	}
	view.Status = st
	return view, nil
}

// UpsertLocalCredential 首注写入、找回覆盖。ON CONFLICT(user_id) 让两条路径
// 用同一条语句，避免「先查后写」在并发找回时留下 TOCTOU。
func (w *sqlEmailTx) UpsertLocalCredential(ctx context.Context, userID int64, passwordHash string, at time.Time) error {
	if userID <= 0 || passwordHash == "" {
		return errors.New("凭证缺少用户 ID 或散列")
	}
	_, err := w.tx.ExecContext(ctx, `
		INSERT INTO local_credentials (user_id, password_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			password_hash = excluded.password_hash,
			updated_at = excluded.updated_at`,
		userID, passwordHash, toUnix(at), toUnix(at))
	if err != nil {
		return fmt.Errorf("写入本地凭证: %w", err)
	}
	return nil
}
