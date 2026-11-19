package sqlite

// 作者删除的写入实现（v0.2.1）：只改 status，不物理删除、不级联。
// 每条语句都绑定在调用方的同一个 *sql.Tx 上，与判权读取处于同一事务快照。
//
// 条件更新（WHERE status = 'active' / 'normal'）是第二道防线：
// 即使判权后目标被并发改态，affected=0 也会让删除失败，绝不重复写入。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jeeinn/motd/internal/application/forum"
)

// ThreadOwner 只认 active 主题：已删 / 隐藏主题对判权也视为「不存在」。
func (w *sqlWriteTx) ThreadOwner(ctx context.Context, threadID int64) (int64, error) {
	var owner int64
	err := w.tx.QueryRowContext(ctx,
		`SELECT user_id FROM threads WHERE id = ? AND status = 'active'`, threadID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, forum.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查询主题作者: %w", err)
	}
	return owner, nil
}

// PostOwner 只认 normal 内容：已软删 / 隐藏的回复不再可删。
func (w *sqlWriteTx) PostOwner(ctx context.Context, postID int64) (int64, error) {
	var owner int64
	err := w.tx.QueryRowContext(ctx,
		`SELECT user_id FROM posts WHERE id = ? AND status = 'normal'`, postID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, forum.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查询内容作者: %w", err)
	}
	return owner, nil
}

// SoftDeleteThread 不碰 posts 表：下属回复行保留，随主题一并不可见。
func (w *sqlWriteTx) SoftDeleteThread(ctx context.Context, threadID int64) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE threads SET status = 'deleted' WHERE id = ? AND status = 'active'`, threadID)
	if err != nil {
		return fmt.Errorf("软删主题: %w", err)
	}
	return requireOneRow(res, "主题软删")
}

// SoftDeletePost 只改本行 status；0007 的 posts_fts_update 触发器会同步摘除索引。
func (w *sqlWriteTx) SoftDeletePost(ctx context.Context, postID int64) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE posts SET status = 'deleted' WHERE id = ? AND status = 'normal'`, postID)
	if err != nil {
		return fmt.Errorf("软删内容: %w", err)
	}
	return requireOneRow(res, "内容软删")
}

func requireOneRow(res sql.Result, what string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查%s行数: %w", what, err)
	}
	if affected == 0 {
		return forum.ErrNotFound
	}
	return nil
}
