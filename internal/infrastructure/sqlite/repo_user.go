package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	appuser "github.com/jeeinn/motd/internal/application/user"
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

var _ appuser.ProfileReader = (*UserStatusReader)(nil)

// Karma 读取用户声望值（v0.5.0 / ADR-057），用于状态栏派生显示等级。
func (r *UserStatusReader) Karma(ctx context.Context, userID int64) (int64, error) {
	var karma int64
	if err := r.pools.QueryRowContext(ctx, `SELECT karma FROM users WHERE id = ?`, userID).Scan(&karma); err != nil {
		return 0, err
	}
	return karma, nil
}

// ProfilesByIDs 批量读取账号展示投影（ADR-065：/whoami 与 /who 共用）。
// 一次 IN 查询零 N+1；去重、跳过非正 ID；查不到的 ID 不出现在 map 中。
// 只读展示字段（username/karma/status/created_at），不碰凭证与身份源明细。
func (r *UserStatusReader) ProfilesByIDs(ctx context.Context, ids []int64) (map[int64]appuser.ProfileView, error) {
	out := make(map[int64]appuser.ProfileView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	if len(args) == 0 {
		return out, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	query := `SELECT id, username, karma, status, created_at FROM users WHERE id IN (` + placeholders + `)`
	rows, err := r.pools.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var v appuser.ProfileView
		var status string
		if err := rows.Scan(&v.UserID, &v.Username, &v.Karma, &status, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Status = domainuser.Status(status)
		out[v.UserID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
