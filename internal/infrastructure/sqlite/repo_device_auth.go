package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jeeinn/motd/internal/application/auth"
)

// DeviceAuthRepos 实现 P0-16 的 application/auth.DeviceAuthStore。
//
// 与 ForumRepos 同一口径（ADR-010）：读走 Pools 只读门面，写经 Writer.Submit
// 的单事务；本包之外拿不到裸 *sql.DB。表 device_auth_states 已在 0001_init.sql
// 建好，P0-16 不新增迁移。
var (
	_ auth.DeviceAuthStore   = (*DeviceAuthRepos)(nil)
	_ auth.DeviceAuthWriteTx = (*sqlDeviceAuthTx)(nil)
)

type DeviceAuthRepos struct {
	pools  *Pools
	writer *Writer
}

// NewDeviceAuthRepos 装配仓储。writer 必须已 Start（由 cmd/server 负责生命周期）。
func NewDeviceAuthRepos(pools *Pools, writer *Writer) *DeviceAuthRepos {
	return &DeviceAuthRepos{pools: pools, writer: writer}
}

const selectDeviceAuth = `
	SELECT id, provider, device_code, user_code, polling_interval,
	       expires_at, status, session_ref, created_at
	FROM device_auth_states`

type deviceAuthScanner interface {
	Scan(dest ...any) error
}

// scanDeviceAuth 把一行映射成领域状态。status 用 ParseXxx 思路校验，
// 遇到 DDL 之外的脏值直接报错，绝不静默当成终态放行。
func scanDeviceAuth(s deviceAuthScanner) (auth.DeviceAuthState, error) {
	var (
		st      auth.DeviceAuthState
		status  string
		expSec  int64
		creaSec int64
		sessRef sql.NullString
	)
	if err := s.Scan(
		&st.ID, &st.Provider, &st.DeviceCode, &st.UserCode, &st.PollingInterval,
		&expSec, &status, &sessRef, &creaSec,
	); err != nil {
		return auth.DeviceAuthState{}, err
	}
	parsed := auth.DeviceAuthStatus(status)
	if !parsed.Valid() {
		return auth.DeviceAuthState{}, fmt.Errorf("%w: device_auth_states.id=%d status=%q",
			auth.ErrInvalidStatus, st.ID, status)
	}
	st.Status = parsed
	st.ExpiresAt = fromUnix(expSec)
	st.CreatedAt = fromUnix(creaSec)
	if sessRef.Valid {
		st.SessionRef = sessRef.String
	}
	return st, nil
}

// GetByDeviceCode 是只读端口，走读池，不占唯一写连接。
func (r *DeviceAuthRepos) GetByDeviceCode(ctx context.Context, deviceCode string) (auth.DeviceAuthState, error) {
	st, err := scanDeviceAuth(r.pools.QueryRowContext(ctx, selectDeviceAuth+` WHERE device_code = ?`, deviceCode))
	if errors.Is(err, sql.ErrNoRows) {
		return auth.DeviceAuthState{}, auth.ErrDeviceAuthNotFound
	}
	if err != nil {
		return auth.DeviceAuthState{}, fmt.Errorf("查询设备码状态: %w", err)
	}
	return st, nil
}

// Write 把用例闭包包进 Writer.Submit 的单事务（ADR-010）。
func (r *DeviceAuthRepos) Write(ctx context.Context, fn func(auth.DeviceAuthWriteTx) error) error {
	if fn == nil {
		return errors.New("写入闭包为空")
	}
	return r.writer.Submit(ctx, func(_ context.Context, tx *sql.Tx) error {
		return fn(&sqlDeviceAuthTx{tx: tx})
	})
}

// sqlDeviceAuthTx 是事务内的受限句柄，所有语句绑定同一个 *sql.Tx。
type sqlDeviceAuthTx struct {
	tx *sql.Tx
}

func (w *sqlDeviceAuthTx) Insert(ctx context.Context, n auth.NewDeviceAuthState) (int64, error) {
	if !n.Status.Valid() {
		return 0, fmt.Errorf("%w: 插入状态 %q", auth.ErrInvalidStatus, n.Status)
	}
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO device_auth_states
			(provider, device_code, user_code, polling_interval, expires_at, status, session_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Provider, n.DeviceCode, n.UserCode, n.PollingInterval,
		toUnix(n.ExpiresAt), n.Status.String(), nullIfEmpty(n.SessionRef), toUnix(n.CreatedAt),
	)
	if err != nil {
		return 0, fmt.Errorf("插入设备码状态: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取设备码状态 ID: %w", err)
	}
	return id, nil
}

// SupersedeSessionPending 把同一 session_ref 下所有 pending 记录批量作废。
// 0 行是合法结果（该会话此前没有待处理码），不报错。
func (w *sqlDeviceAuthTx) SupersedeSessionPending(ctx context.Context, sessionRef string, to auth.DeviceAuthStatus) (int64, error) {
	if err := auth.CanTransition(auth.StatusPending, to); err != nil {
		return 0, err
	}
	res, err := w.tx.ExecContext(ctx, `
		UPDATE device_auth_states SET status = ?
		WHERE session_ref = ? AND status = ?`,
		to.String(), sessionRef, auth.StatusPending.String(),
	)
	if err != nil {
		return 0, fmt.Errorf("作废旧设备码: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("检查作废旧码行数: %w", err)
	}
	return n, nil
}

// Transition 是 CAS 迁移：WHERE 同时钉住 id 与期望旧状态。
// RowsAffected == 0 时再查一次区分「行不存在」与「状态已被并发修改」，
// 让迟到的轮询结果不会把终态覆盖回去。
func (w *sqlDeviceAuthTx) Transition(ctx context.Context, id int64, expectFrom, to auth.DeviceAuthStatus) error {
	if err := auth.CanTransition(expectFrom, to); err != nil {
		return err
	}
	res, err := w.tx.ExecContext(ctx, `
		UPDATE device_auth_states SET status = ? WHERE id = ? AND status = ?`,
		to.String(), id, expectFrom.String(),
	)
	if err != nil {
		return fmt.Errorf("迁移设备码状态: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查迁移行数: %w", err)
	}
	if n == 1 {
		return nil
	}
	return classifyCASMiss(ctx, w.tx, id)
}

// UpdateInterval 持久化 slow_down 后的退避间隔，同样带 CAS。
func (w *sqlDeviceAuthTx) UpdateInterval(ctx context.Context, id int64, expectFrom auth.DeviceAuthStatus, interval int) error {
	if interval <= 0 {
		return auth.ErrInvalidInterval
	}
	res, err := w.tx.ExecContext(ctx, `
		UPDATE device_auth_states SET polling_interval = ? WHERE id = ? AND status = ?`,
		interval, id, expectFrom.String(),
	)
	if err != nil {
		return fmt.Errorf("更新轮询间隔: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查间隔更新行数: %w", err)
	}
	if n == 1 {
		return nil
	}
	return classifyCASMiss(ctx, w.tx, id)
}

// classifyCASMiss 在写事务内判定 CAS 未命中的原因。
func classifyCASMiss(ctx context.Context, tx *sql.Tx, id int64) error {
	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM device_auth_states WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrDeviceAuthNotFound
	}
	if err != nil {
		return fmt.Errorf("复查设备码状态: %w", err)
	}
	return auth.ErrStateConflict
}

// nullIfEmpty 让空 session_ref 落成 NULL，与 DDL 的可空列一致。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
