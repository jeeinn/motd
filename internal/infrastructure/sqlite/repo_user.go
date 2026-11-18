package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// UserStatusReader 实现 application/user.StatusReader（结构化接口匹配）。
// 只读账号状态，走读池；不提供任何写入方法。
type UserStatusReader struct {
	pools *Pools
}

func NewUserStatusReader(pools *Pools) *UserStatusReader {
	return &UserStatusReader{pools: pools}
}

func (r *UserStatusReader) Status(ctx context.Context, userID int64) (domainuser.Status, error) {
	var raw string
	err := r.pools.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("用户 %d 不存在", userID)
	}
	if err != nil {
		return "", err
	}
	status := domainuser.Status(raw)
	if !status.Valid() {
		return "", fmt.Errorf("用户 %d 的状态值非法: %q", userID, raw)
	}
	return status, nil
}
