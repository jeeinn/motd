package sqlite

// karma 计分的 WriteTx 实现（v0.5.0 / ADR-057）。单独成文件，与 AdjustScore
// 的纪律对齐：karma 是用户声望、score 是主题分数，两者独立更新、都在同一业务事务内。

import (
	"context"
	"fmt"
)

// AdjustKarma 用原子 UPDATE 增减 users.karma，不读后写（避免并发丢更新）。
// karma 只用于派生显示等级（ADR-014），绝不参与权限判断。
func (w *sqlWriteTx) AdjustKarma(ctx context.Context, userID int64, delta int64) error {
	res, err := w.tx.ExecContext(ctx, `
		UPDATE users SET karma = karma + ? WHERE id = ?`, delta, userID)
	if err != nil {
		return fmt.Errorf("调整 karma: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查 karma 更新行数: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("用户 %d 不存在，无法调整 karma", userID)
	}
	return nil
}
