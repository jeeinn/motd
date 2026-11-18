package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jeeinn/motd/internal/application/auth"
)

// SSHKeyLookup 实现 application/auth.KeyLookup：只读 ssh_keys 表，走读池。
// 不提供任何写入方法 —— 公钥绑定 / 解绑属于 P0-18，必须经写入队列。
type SSHKeyLookup struct {
	pools *Pools
}

var _ auth.KeyLookup = (*SSHKeyLookup)(nil)

func NewSSHKeyLookup(pools *Pools) *SSHKeyLookup {
	return &SSHKeyLookup{pools: pools}
}

// Lookup 只返回三状态所需的最小快照。
// 指纹用参数化查询传入，不接受拼接；SQL 注入串最多查不到记录（→ 访客），
// 不会改变查询结构。
func (r *SSHKeyLookup) Lookup(ctx context.Context, fingerprint string) (auth.KeyStatus, error) {
	var (
		userID  int64
		revoked sql.NullInt64
	)
	err := r.pools.QueryRowContext(ctx,
		`SELECT user_id, revoked_at FROM ssh_keys WHERE fingerprint = ?`,
		fingerprint,
	).Scan(&userID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.KeyStatus{}, nil // 未绑定 → 访客
	}
	if err != nil {
		return auth.KeyStatus{}, fmt.Errorf("查询 ssh_keys: %w", err)
	}
	return auth.KeyStatus{
		Found:   true,
		Revoked: revoked.Valid,
		UserID:  userID,
	}, nil
}
