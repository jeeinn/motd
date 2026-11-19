package forum

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
)

// OutboxStore 是 outbox 的持久化端口，由 infrastructure 实现。
// 每个方法各自是一次独立写事务；领域事件 handler 的调用不在这里发生。
type OutboxStore interface {
	// ClaimPending 以 CAS 领取一条 pending 事件（status -> dispatching）。
	// 没有可领取的事件时返回 ok=false，调用方据此结束本轮投递。
	ClaimPending(ctx context.Context) (OutboxRecord, bool, error)
	// MarkDelivered 标记投递成功；仅对 dispatching 生效，防止并发覆盖。
	MarkDelivered(ctx context.Context, id int64, at time.Time) error
	// MarkFailed 标记投递失败并累加 attempts；仅对 dispatching 生效。
	MarkFailed(ctx context.Context, id int64) error
	// RequeueDispatching 把上次进程崩溃时卡在 dispatching 的事件退回 pending。
	RequeueDispatching(ctx context.Context) (int64, error)
}

// OutboxRecord 是从 outbox 领取到的一条事件及其领取时的元信息。
type OutboxRecord struct {
	ID       int64
	Event    event.DomainEvent
	Attempts int
}

// Outbox 是事务提交后的事件调度器。
//
// 时序纪律（ADR-015 / R12）：业务写 + AppendOutbox 在同一事务提交之后，
// 才由本调度器领取事件、调用 DomainEventSink.Publish、回写投递状态。
// handler 永远不在写事务里执行；投递失败也不会回滚已提交的业务数据。
type Outbox struct {
	store     OutboxStore
	sink      DomainEventSink
	onErr     PublishObserver
	delivered atomic.Int64
	failed    atomic.Int64
}

// NewOutbox 装配调度器。sink 不能为空：没有出口就没有投递语义。
func NewOutbox(store OutboxStore, sink DomainEventSink, onErr PublishObserver) (*Outbox, error) {
	if store == nil {
		return nil, errors.New("outbox store 不能为空")
	}
	if sink == nil {
		return nil, errors.New("outbox 事件出口不能为空")
	}
	return &Outbox{store: store, sink: sink, onErr: onErr}, nil
}

// DispatchPending 领取并投递当前全部 pending 事件，返回本轮成功/失败条数。
// 由写用例在事务提交后调用；正常路径下本轮只会有刚写入的那一条。
func (o *Outbox) DispatchPending(ctx context.Context) (delivered, failed int, err error) {
	for {
		rec, ok, err := o.store.ClaimPending(ctx)
		if err != nil {
			return delivered, failed, fmt.Errorf("领取 outbox 事件: %w", err)
		}
		if !ok {
			return delivered, failed, nil
		}

		// 关键：Publish 在 ClaimPending 的写事务提交之后、且不在任何写事务里。
		pubErr := o.sink.Publish(ctx, rec.Event)
		if pubErr != nil {
			failed++
			o.failed.Add(1)
			if mErr := o.store.MarkFailed(ctx, rec.ID); mErr != nil {
				return delivered, failed, fmt.Errorf("标记 outbox 失败: %w", mErr)
			}
			if o.onErr != nil {
				o.onErr(rec.Event, pubErr)
			}
			continue
		}

		delivered++
		o.delivered.Add(1)
		if mErr := o.store.MarkDelivered(ctx, rec.ID, time.Now()); mErr != nil {
			return delivered, failed, fmt.Errorf("标记 outbox 已投递: %w", mErr)
		}
	}
}

// RecoverPending 启动恢复：先把崩溃时卡在 dispatching 的事件退回 pending，
// 再投递所有残留事件。返回 (退回条数, 成功投递, 失败投递, 错误)。
func (o *Outbox) RecoverPending(ctx context.Context) (requeued int64, delivered, failed int, err error) {
	requeued, err = o.store.RequeueDispatching(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("重排 dispatching 事件: %w", err)
	}
	delivered, failed, err = o.DispatchPending(ctx)
	return requeued, delivered, failed, err
}

// Delivered / Failed 是累计投递计数，供运维观测。
func (o *Outbox) Delivered() int64 { return o.delivered.Load() }
func (o *Outbox) Failed() int64    { return o.failed.Load() }
