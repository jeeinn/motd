package sqlite

// 数据运维：device_auth_states 过期行清理（P1-M4）。
//
// 表已在 0001_init.sql 建好（expires_at + idx_device_auth_states_expiry），
// 本轮无 schema 迁移。所有删除走 Writer.Submit 的单事务、参数化，不拼 SQL。
//
// 安全口径（OPS / ADR-035）：
//   - 只删**自身 expires_at 已过**的行。pending 是「在途登录」，未过期时
//     绝不删除——删掉会让正在轮询的客户端突然拿到「码不存在」；
//   - 终态（authorized / denied / expired）同样要等自身过期才清，
//     避免刚完成的登录历史被立刻抹掉；
//   - 状态值用 IN 枚举写死，不用「NOT pending」之类的反向条件，防止
//     将来新增状态值被误当成可清理对象。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Maintenance 持有写入门面，只暴露运维动作，不暴露裸 *sql.DB。
type Maintenance struct {
	writer *Writer
}

func NewMaintenance(writer *Writer) *Maintenance {
	return &Maintenance{writer: writer}
}

// CleanupExpiredDeviceAuth 删除所有「自身已过期」的设备码状态行，返回删除行数。
// now 由调用方注入（进程侧 ticker），便于用固定时钟做确定性回归。
//
// 条件刻意只认 expires_at：四态枚举里的 pending 与其它三态共用同一条
// 「未过期不动」的安全线，在途登录因此天然被保护。
func (m *Maintenance) CleanupExpiredDeviceAuth(ctx context.Context, now time.Time) (int64, error) {
	var removed int64
	err := m.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM device_auth_states
			WHERE expires_at < ?
			  AND status IN ('pending','authorized','denied','expired')`,
			toUnix(now))
		if err != nil {
			return fmt.Errorf("清理过期设备码状态: %w", err)
		}
		removed, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("检查清理行数: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
