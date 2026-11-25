package sqlite

// R-15 / ADR-079 主题置顶的写实现：只改 threads.mark，不碰 status /
// last_reply_at / score。置顶与清同版块其它置顶在同一写事务内完成，
// 保证"每版块 1 条"原子替换。

import (
	"context"
	"fmt"
)

// SetThreadMark 把主题 mark 置为 0 或 1。条件 status='active'：
// 已删除/已隐藏的主题不能被置顶（治理顺序：先恢复再置顶）。
// 重复设置同一值是合法 no-op（SQLite UPDATE affected=1 即使值没变）。
func (w *sqlWriteTx) SetThreadMark(ctx context.Context, threadID int64, mark int8) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE threads SET mark = ? WHERE id = ? AND status = 'active'`, mark, threadID)
	if err != nil {
		return fmt.Errorf("设置主题置顶标记: %w", err)
	}
	return requireOneRow(res, "主题置顶")
}

// ClearPinnedInNode 把同版块除 exceptThreadID 外的置顶主题清为普通。
// 不影响其它版块、不碰 status，未命中任何行是合法 no-op（返回 nil）。
func (w *sqlWriteTx) ClearPinnedInNode(ctx context.Context, nodeID, exceptThreadID int64) error {
	_, err := w.tx.ExecContext(ctx,
		`UPDATE threads SET mark = 0 WHERE node_id = ? AND mark = 1 AND id <> ?`, nodeID, exceptThreadID)
	if err != nil {
		return fmt.Errorf("清除同版块置顶: %w", err)
	}
	return nil
}
