package sqlite

// P2-M1b 内容治理写入实现：只改 status='hidden'，不物理删、不级联。
// 每条语句绑定调用方同一个 *sql.Tx，与审计事件同提交、同回滚。
//
// 条件更新用 IN ('normal','hidden') / IN ('active','hidden') 实现幂等：
// 已隐藏的目标再隐藏一次 affected=1，不报错；作者已删除（deleted）或
// 根本不存在的目标 affected=0 → forum.ErrNotFound，由用例层收敛为固定文案。
// 0001 的 posts.status CHECK 与 0004 的 threads.status CHECK 已含 hidden，无需迁移。

import (
	"context"
	"fmt"
)

// HidePost 只把 posts.status 置为 hidden；0007 的 posts_fts_update 触发器
// 会在同一事务里摘除 FTS 索引，检索路径不会命中隐藏内容。
func (w *sqlWriteTx) HidePost(ctx context.Context, postID int64) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE posts SET status = 'hidden' WHERE id = ? AND status IN ('normal', 'hidden')`, postID)
	if err != nil {
		return fmt.Errorf("隐藏内容: %w", err)
	}
	return requireOneRow(res, "内容隐藏")
}

// HideThread 只把 threads.status 置为 hidden；下属回复行保留，随主题一并不可见。
func (w *sqlWriteTx) HideThread(ctx context.Context, threadID int64) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE threads SET status = 'hidden' WHERE id = ? AND status IN ('active', 'hidden')`, threadID)
	if err != nil {
		return fmt.Errorf("隐藏主题: %w", err)
	}
	return requireOneRow(res, "主题隐藏")
}
