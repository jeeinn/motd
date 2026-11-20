package sqlite

// 作者编辑的写入实现（P2-M2 / ADR-051）：只改 body_md 与 edited_at。
//
// 绝不 UPDATE status / created_at / last_reply_at：
//   - status 决定可见性，编辑不改变可见性（软删口径 ADR-038）；
//   - created_at 是发帖时间，编辑不伪造新帖；
//   - last_reply_at 属于 threads，编辑绝不推进——否则 SortLatest 会把旧主题顶到最新
//     （同投票 AdjustScore 的教训 ADR-029）。
//
// 条件更新 WHERE status = 'normal' 是第二道防线：即使 PostOwner 判权后目标被并发软删 /
// 隐藏，affected=0 也会让编辑失败。0007 的 posts_fts_update 触发器会随本次 UPDATE
// 同步重建正文索引，检索立即可见新正文，无需额外写库。

import (
	"context"
	"fmt"
)

// EditPost 只更新本行正文与编辑时间，绑定在调用方的同一个 *sql.Tx 上。
func (w *sqlWriteTx) EditPost(ctx context.Context, postID int64, newBody string, editedAt int64) error {
	res, err := w.tx.ExecContext(ctx,
		`UPDATE posts SET body_md = ?, edited_at = ? WHERE id = ? AND status = 'normal'`,
		newBody, editedAt, postID)
	if err != nil {
		return fmt.Errorf("编辑内容: %w", err)
	}
	return requireOneRow(res, "内容编辑")
}
