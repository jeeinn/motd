package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jeeinn/motd/internal/application/auth"
)

// AuthEventStore 把 application/auth.AuditSink 落到 auth_events 表。
//
// 表与 idx_auth_events_time 已在 0001_init.sql 建好，本里程碑无新迁移。
// 所有写入走 Writer.Submit 的唯一写连接；一批事件一个事务，
// 批量 INSERT 用预编译语句逐条 Exec，避免拼接 SQL 与逐条事务提交。
type AuthEventStore struct {
	writer *Writer
}

var _ auth.AuditSink = (*AuthEventStore)(nil)

func NewAuthEventStore(writer *Writer) *AuthEventStore {
	return &AuthEventStore{writer: writer}
}

// Flush 把审计器批量导出的事件写入 auth_events。
// 调用方是审计器的定时 Tick / Stop，绝不在认证临界路径上。
func (s *AuthEventStore) Flush(ctx context.Context, events []auth.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	return s.writer.Submit(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(txCtx, `
			INSERT INTO auth_events (user_id, event, provider, ip_hash, detail_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("准备审计批量写入: %w", err)
		}
		defer func() { _ = stmt.Close() }()

		for _, ev := range events {
			var userID any
			if ev.UserID > 0 {
				userID = ev.UserID
			}
			_, err := stmt.ExecContext(txCtx,
				userID, ev.Event, nullIfEmpty(ev.Provider), nullIfEmpty(ev.IPHash),
				detailIfEmpty(ev.DetailJSON), ev.CreatedAt.Unix())
			if err != nil {
				return fmt.Errorf("写入审计事件 %s: %w", ev.Event, err)
			}
		}
		return nil
	})
}

func detailIfEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// CountAuthEvents 供集成测试与复现脚本核对「审计事件确实增长」。
// 走读池，不占用唯一写连接。
func (p *Pools) CountAuthEvents(ctx context.Context, event string) (int64, error) {
	var n int64
	q := `SELECT COUNT(*) FROM auth_events`
	var args []any
	if event != "" {
		q += ` WHERE event = ?`
		args = append(args, event)
	}
	if err := p.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("统计审计事件: %w", err)
	}
	return n, nil
}
