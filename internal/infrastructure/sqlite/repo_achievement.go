package sqlite

// 成就持久化：实现 application/user 定义的 AchievementStore 端口（接口在使用方）。
// 与 P0 的内存占位不同，这里写 achievements 表，进程重启后成就不丢。
// Grant 走唯一写队列（Writer.Submit），读走读池；幂等由 UNIQUE(user_id, code) 保证。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

var _ appuser.AchievementStore = (*AchievementStore)(nil)

// AchievementStore 是 achievements 表的持久化实现。
// 端口仍留在 application 层（ADR-015），本文件只做实现，cmd 负责装配。
type AchievementStore struct {
	pools  *Pools
	writer *Writer
}

func NewAchievementStore(pools *Pools, writer *Writer) *AchievementStore {
	return &AchievementStore{pools: pools, writer: writer}
}

func (s *AchievementStore) Has(ctx context.Context, userID int64, code domainuser.AchievementCode) (bool, error) {
	var one int
	err := s.pools.QueryRowContext(ctx, `
		SELECT 1 FROM achievements WHERE user_id = ? AND code = ?`, userID, string(code)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询成就: %w", err)
	}
	return true, nil
}

// Grant 经唯一写连接插入；ON CONFLICT DO NOTHING 使重复授予幂等（与内存实现同口径）。
// 注意：本方法在插件事件回调里调用，是独立事务，不与发帖/投票的写事务嵌套。
func (s *AchievementStore) Grant(ctx context.Context, userID int64, code domainuser.AchievementCode, at time.Time) error {
	return s.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO achievements (user_id, code, unlocked_at)
			VALUES (?, ?, ?)
			ON CONFLICT(user_id, code) DO NOTHING`,
			userID, string(code), toUnix(at.UTC()))
		if err != nil {
			return fmt.Errorf("写入成就: %w", err)
		}
		return nil
	})
}

// GrantedCount 是全表解锁计数，仅运维/测试观测使用，不参与任何业务判定。
func (s *AchievementStore) GrantedCount() int {
	var n int
	if err := s.pools.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM achievements`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// Entries 返回全部解锁记录快照，供现有端到端测试观测（不是端口方法）。
// 顺序稳定按 (user_id, code)，便于断言。
func (s *AchievementStore) Entries() []appuser.Grant {
	rows, err := s.pools.QueryContext(context.Background(), `
		SELECT user_id, code, unlocked_at FROM achievements ORDER BY user_id, code`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]appuser.Grant, 0)
	for rows.Next() {
		var g appuser.Grant
		var code string
		var unlocked int64
		if err := rows.Scan(&g.UserID, &code, &unlocked); err != nil {
			return nil
		}
		g.Code = domainuser.AchievementCode(code)
		g.UnlockedAt = time.Unix(unlocked, 0).UTC()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}
