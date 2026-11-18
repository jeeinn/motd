package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jeeinn/motd/internal/application/auth"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// LoginRepos 实现 P0-17 的 application/auth.LoginStore。
//
// 与 ForumRepos / DeviceAuthRepos 同一口径（ADR-010）：写一律经 Writer.Submit
// 的单事务，本包之外拿不到裸 *sql.DB。users / identities 两表均已在
// 0001_init.sql 建好，P0-17 不新增迁移。
var (
	_ auth.LoginStore   = (*LoginRepos)(nil)
	_ auth.LoginWriteTx = (*sqlLoginTx)(nil)
)

type LoginRepos struct {
	writer *Writer
}

// NewLoginRepos 装配仓储。writer 必须已 Start（由 cmd/server 负责生命周期）。
func NewLoginRepos(writer *Writer) *LoginRepos {
	return &LoginRepos{writer: writer}
}

// Write 把 §4.3 三步判定与 users+identities 插入包进同一个写事务。
// 用例闭包拿不到 *sql.Tx，事务提交成功后才返回 nil（ADR-010）。
func (r *LoginRepos) Write(ctx context.Context, fn func(auth.LoginWriteTx) error) error {
	if fn == nil {
		return errors.New("登录写入闭包为空")
	}
	return r.writer.Submit(ctx, func(_ context.Context, tx *sql.Tx) error {
		return fn(&sqlLoginTx{tx: tx})
	})
}

type sqlLoginTx struct {
	tx *sql.Tx
}

// FindIdentityByUID 是 §4.3 步骤 1：按 (provider, provider_uid) 查已绑定身份。
// 未命中返回 auth.ErrLoginNotFound；status 是脏值时直接报错，绝不静默放行。
func (w *sqlLoginTx) FindIdentityByUID(ctx context.Context, provider, providerUID string) (auth.AccountView, error) {
	var (
		view   auth.AccountView
		status string
	)
	err := w.tx.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.status
		FROM identities i JOIN users u ON u.id = i.user_id
		WHERE i.provider = ? AND i.provider_uid = ?`, provider, providerUID).
		Scan(&view.UserID, &view.Username, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.AccountView{}, auth.ErrLoginNotFound
	}
	if err != nil {
		return auth.AccountView{}, fmt.Errorf("查询身份绑定: %w", err)
	}
	st := domainuser.Status(status)
	if !st.Valid() {
		return auth.AccountView{}, fmt.Errorf("账号 %d 的状态值非法: %q", view.UserID, status)
	}
	view.Status = st
	return view, nil
}

// FindAccountByEmail 只用于 §4.3 步骤 2 生成「不自动合并」提示。
// primary_email 无唯一约束（§7 R7），因此取任意一条即可判定存在性；
// 用 lower() 做大小写不敏感比较，与登录前邮箱 normalize 配合。
func (w *sqlLoginTx) FindAccountByEmail(ctx context.Context, email string) (auth.AccountView, error) {
	var (
		view   auth.AccountView
		status string
	)
	err := w.tx.QueryRowContext(ctx, `
		SELECT id, username, status FROM users
		WHERE lower(primary_email) = lower(?) LIMIT 1`, email).
		Scan(&view.UserID, &view.Username, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.AccountView{}, auth.ErrLoginNotFound
	}
	if err != nil {
		return auth.AccountView{}, fmt.Errorf("查询邮箱归属: %w", err)
	}
	view.Status = domainuser.Status(status)
	return view, nil
}

// UsernameExists 用 COLLATE NOCASE 探测，与 users.username 的
// COLLATE NOCASE UNIQUE 保持一致。少了 NOCASE 会让大小写变体探测漏判，
// 直到 InsertUser 才被唯一约束拦下（虽然仍会重试，但白跑一个事务）。
func (w *sqlLoginTx) UsernameExists(ctx context.Context, username string) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(ctx, `
		SELECT 1 FROM users WHERE username = ? COLLATE NOCASE LIMIT 1`, username).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("探测用户名占用: %w", err)
	}
	return true, nil
}

// InsertUser 写入 users。**仓储侧再过一次 domain 校验**：ADR-005 要求任何
// 写 users.username 的路径都必须先过 ValidateUsername，DB 的 CHECK 只是
// 长度与空白兜底，字符集白名单以 domain 为准。
func (w *sqlLoginTx) InsertUser(ctx context.Context, a auth.NewAccount) (int64, error) {
	if err := domainuser.ValidateUsername(a.Username); err != nil {
		return 0, fmt.Errorf("拒绝写入非法用户名: %w", err)
	}
	if !a.Status.Valid() || a.Status != domainuser.StatusActive {
		return 0, fmt.Errorf("首登账号状态必须是 active，实际 %q", a.Status)
	}
	sec := toUnix(a.CreatedAt)
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO users
			(username, display_name, primary_email, karma, status, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?)`,
		a.Username, a.DisplayName, nullIfEmpty(a.PrimaryEmail), string(a.Status), sec, sec)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, auth.ErrUsernameConflict
		}
		return 0, fmt.Errorf("插入账号: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取账号 ID: %w", err)
	}
	return id, nil
}

// InsertIdentity 写入 identities。UNIQUE(provider, provider_uid) 是并发同 UID
// 首登的最后一道防线：撞上后映射为 auth.ErrIdentityConflict，用例整体重试，
// 重试会重新查身份并转为「登录」，绝不产生第二个账号。
func (w *sqlLoginTx) InsertIdentity(ctx context.Context, i auth.NewIdentity) error {
	verified := 0
	if i.EmailVerified {
		verified = 1
	}
	_, err := w.tx.ExecContext(ctx, `
		INSERT INTO identities
			(user_id, provider, provider_uid, email, email_verified, profile_json, created_at)
		VALUES (?, ?, ?, ?, ?, '{}', ?)`,
		i.UserID, i.Provider, i.ProviderUID, nullIfEmpty(i.Email), verified, toUnix(i.CreatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return auth.ErrIdentityConflict
		}
		return fmt.Errorf("插入身份绑定: %w", err)
	}
	return nil
}

// isUniqueViolation 判定 SQLite 的唯一约束冲突。
// 优先用驱动错误码（modernc.org/sqlite 的 Error.Code），包装错误退回字符串匹配，
// 避免错误文本随驱动版本漂移而漏判（与 write.go 的 isRetryable 同一手法）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "UNIQUE CONSTRAINT FAILED") ||
		strings.Contains(msg, "CONSTRAINT FAILED: UNIQUE") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE")
}
