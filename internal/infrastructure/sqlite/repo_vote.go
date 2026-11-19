package sqlite

// 投票相关的 WriteTx 实现。单独成文件，避免继续膨胀 repo_forum.go，
// 也把「投票只调 AdjustScore、绝不调 TouchThread」的纪律落在文件边界上。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/vote"
)

func (w *sqlWriteTx) GetVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64) (vote.Value, bool, error) {
	var v int
	err := w.tx.QueryRowContext(ctx, `
		SELECT value FROM votes
		WHERE user_id = ? AND target_type = ? AND target_id = ?`,
		userID, string(targetType), targetID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询已有投票: %w", err)
	}
	return vote.Value(v), true, nil
}

func (w *sqlWriteTx) UpsertVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64, value vote.Value, at time.Time) error {
	_, err := w.tx.ExecContext(ctx, `
		INSERT INTO votes (user_id, target_type, target_id, value, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, target_type, target_id)
		DO UPDATE SET value = excluded.value, created_at = excluded.created_at`,
		userID, string(targetType), targetID, int(value), toUnix(at))
	if err != nil {
		return fmt.Errorf("写入投票: %w", err)
	}
	return nil
}

func (w *sqlWriteTx) DeleteVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64) error {
	if _, err := w.tx.ExecContext(ctx, `
		DELETE FROM votes
		WHERE user_id = ? AND target_type = ? AND target_id = ?`,
		userID, string(targetType), targetID); err != nil {
		return fmt.Errorf("删除投票: %w", err)
	}
	return nil
}

// AdjustScore 只更新 threads.score。这里刻意不出现 last_reply_at：
// 投票是「评价」而非「回复」，不能污染按 last_reply_at 排序的 SortLatest。
func (w *sqlWriteTx) AdjustScore(ctx context.Context, threadID int64, delta int64) error {
	res, err := w.tx.ExecContext(ctx, `
		UPDATE threads SET score = score + ? WHERE id = ?`, delta, threadID)
	if err != nil {
		return fmt.Errorf("调整主题分数: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查分数更新行数: %w", err)
	}
	if affected == 0 {
		return forum.ErrNotFound
	}
	return nil
}

// PostThreadID 只认 status='normal' 的内容：软删除 / 隐藏的内容不可投票，
// 与 PostReader 的可见性口径保持一致。
func (w *sqlWriteTx) PostThreadID(ctx context.Context, postID int64) (int64, error) {
	var threadID int64
	err := w.tx.QueryRowContext(ctx, `
		SELECT thread_id FROM posts WHERE id = ? AND status = 'normal'`, postID).Scan(&threadID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, forum.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查询内容所属主题: %w", err)
	}
	return threadID, nil
}
