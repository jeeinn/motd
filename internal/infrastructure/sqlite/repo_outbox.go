package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/event"
)

// OutboxStore 把 application 定义的 outbox 端口落到 SQLite。
// 所有方法都走 Writer 的单写连接：领取用 CAS 置 dispatching，
// 状态回写用带前置条件的 UPDATE，避免并发投递互相覆盖。
type OutboxStore struct {
	writer *Writer
}

var (
	_ forum.OutboxStore = (*OutboxStore)(nil)

	// errNoPending 仅用于从写事务里透出「没有可领取事件」，不向外暴露。
	errNoPending = errors.New("outbox 无待投递事件")
)

func NewOutboxStore(writer *Writer) *OutboxStore {
	return &OutboxStore{writer: writer}
}

// ClaimPending 在一个写事务里完成「SELECT 一条 pending → CAS 置 dispatching」。
// Writer 已用 _txlock=immediate 抢到写锁，因此 SELECT 与 UPDATE 之间没有竞态窗口。
func (s *OutboxStore) ClaimPending(ctx context.Context) (forum.OutboxRecord, bool, error) {
	var rec forum.OutboxRecord
	err := s.writer.Submit(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		var id int64
		var payload string
		var attempts int
		err := tx.QueryRowContext(txCtx, `
			SELECT id, payload, attempts FROM domain_event_outbox
			WHERE status = 'pending' ORDER BY id LIMIT 1`).Scan(&id, &payload, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoPending
		}
		if err != nil {
			return fmt.Errorf("查待投递事件: %w", err)
		}
		res, err := tx.ExecContext(txCtx, `
			UPDATE domain_event_outbox SET status = 'dispatching' WHERE id = ? AND status = 'pending'`, id)
		if err != nil {
			return fmt.Errorf("领取待投递事件: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("检查领取行数: %w", err)
		}
		if affected != 1 {
			return errNoPending // 被并发领取者抢走
		}
		var e event.DomainEvent
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return fmt.Errorf("反序列化事件 %d: %w", id, err)
		}
		rec = forum.OutboxRecord{ID: id, Event: e, Attempts: attempts}
		return nil
	})
	if errors.Is(err, errNoPending) {
		return forum.OutboxRecord{}, false, nil
	}
	if err != nil {
		return forum.OutboxRecord{}, false, err
	}
	return rec, true, nil
}

func (s *OutboxStore) MarkDelivered(ctx context.Context, id int64, at time.Time) error {
	return s.writer.Submit(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			UPDATE domain_event_outbox SET status = 'delivered', delivered_at = ?
			WHERE id = ? AND status = 'dispatching'`, toUnix(at), id)
		return checkDispatchingUpdate(id, res, err)
	})
}

func (s *OutboxStore) MarkFailed(ctx context.Context, id int64) error {
	return s.writer.Submit(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			UPDATE domain_event_outbox SET status = 'failed', attempts = attempts + 1
			WHERE id = ? AND status = 'dispatching'`, id)
		return checkDispatchingUpdate(id, res, err)
	})
}

// checkDispatchingUpdate 保证状态回写只影响本轮领取的那条记录。
func checkDispatchingUpdate(id int64, res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("回写 outbox 状态: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("检查 outbox 状态行数: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("outbox 事件 %d 不在 dispatching 状态，拒绝回写", id)
	}
	return nil
}

// RequeueDispatching 只在启动恢复时调用：此刻没有在途投递，
// 所有 dispatching 行都来自上次进程崩溃，安全地退回 pending 重投。
func (s *OutboxStore) RequeueDispatching(ctx context.Context) (int64, error) {
	var count int64
	err := s.writer.Submit(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(txCtx, `
			UPDATE domain_event_outbox SET status = 'pending' WHERE status = 'dispatching'`)
		if err != nil {
			return fmt.Errorf("重排 dispatching 事件: %w", err)
		}
		count, err = res.RowsAffected()
		return err
	})
	return count, err
}
