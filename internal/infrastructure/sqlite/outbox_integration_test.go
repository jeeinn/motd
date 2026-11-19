package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/user"
)

// R12 集成测试：真实 SQLite + 真实 Writer + 真实事务边界。
// 覆盖同事务写入/回滚、event_id 去重、CAS 领取、状态回写、崩溃恢复与提交后投递。

func outboxTestEvent(id string) event.DomainEvent {
	return event.DomainEvent{
		ID:         id,
		Type:       event.EventPostCreated,
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
		ActorID:    1,
		ThreadID:   1,
		PostID:     1,
	}
}

func outboxCount(t *testing.T, e *forumEnv) int {
	t.Helper()
	var n int
	if err := e.pools.reader.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM domain_event_outbox`).Scan(&n); err != nil {
		t.Fatalf("统计 outbox: %v", err)
	}
	return n
}

func outboxStatus(t *testing.T, e *forumEnv, id string) (status string, attempts int) {
	t.Helper()
	if err := e.pools.reader.QueryRowContext(e.ctx,
		`SELECT status, attempts FROM domain_event_outbox WHERE event_id = ?`, id).Scan(&status, &attempts); err != nil {
		t.Fatalf("读取 outbox 状态 %q: %v", id, err)
	}
	return
}

func TestAppendOutboxSharesBusinessTransaction(t *testing.T) {
	e := newForumEnv(t)
	ev := outboxTestEvent("post:commit")

	err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		if err := tx.AppendOutbox(e.ctx, ev); err != nil {
			return err
		}
		_, err := tx.InsertThread(e.ctx, forum.NewThread{
			NodeID: e.nodeID, UserID: e.userID, Title: "outbox", CreatedAt: time.Unix(e.clock, 0).UTC(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("同事务写 outbox + 业务写应成功: %v", err)
	}
	if n := outboxCount(t, e); n != 1 {
		t.Fatalf("提交后应有 1 条 outbox，实际 %d", n)
	}
	if status, _ := outboxStatus(t, e, ev.ID); status != "pending" {
		t.Fatalf("新事件应为 pending，实际 %q", status)
	}

	// 写入失败时 outbox 行必须随事务一起回滚。
	wantErr := errors.New("注入失败")
	err = e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		if err := tx.AppendOutbox(e.ctx, outboxTestEvent("post:rollback")); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("注入失败应透传，实际 %v", err)
	}
	if n := outboxCount(t, e); n != 1 {
		t.Fatalf("回滚不应留下 outbox 行，实际 %d", n)
	}
}

func TestAppendOutboxDeduplicatesByEventID(t *testing.T) {
	e := newForumEnv(t)
	ev := outboxTestEvent("post:dup")
	for i := 0; i < 3; i++ {
		if err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
			return tx.AppendOutbox(e.ctx, ev)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := outboxCount(t, e); n != 1 {
		t.Fatalf("同一 event_id 反复写入应只留 1 条，实际 %d", n)
	}
}

func TestOutboxStoreClaimAndStatusTransitions(t *testing.T) {
	e := newForumEnv(t)
	store := NewOutboxStore(e.writer)
	first, second := outboxTestEvent("post:first"), outboxTestEvent("post:second")
	for _, ev := range []event.DomainEvent{first, second} {
		if err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
			return tx.AppendOutbox(e.ctx, ev)
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec, ok, err := store.ClaimPending(e.ctx)
	if err != nil || !ok || rec.Event.ID != first.ID {
		t.Fatalf("应先领取第一条: rec=%+v ok=%v err=%v", rec, ok, err)
	}
	if status, _ := outboxStatus(t, e, first.ID); status != "dispatching" {
		t.Fatalf("领取后应为 dispatching，实际 %q", status)
	}
	if err := store.MarkDelivered(e.ctx, rec.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if status, _ := outboxStatus(t, e, first.ID); status != "delivered" {
		t.Fatalf("应为 delivered，实际 %q", status)
	}

	rec, ok, err = store.ClaimPending(e.ctx)
	if err != nil || !ok || rec.Event.ID != second.ID {
		t.Fatalf("应领取第二条: rec=%+v ok=%v err=%v", rec, ok, err)
	}
	if err := store.MarkFailed(e.ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if status, attempts := outboxStatus(t, e, second.ID); status != "failed" || attempts != 1 {
		t.Fatalf("失败应标记 failed 且 attempts=1，实际 %q/%d", status, attempts)
	}

	if _, ok, err := store.ClaimPending(e.ctx); err != nil || ok {
		t.Fatalf("无 pending 时应返回 ok=false，实际 ok=%v err=%v", ok, err)
	}
}

func TestOutboxStoreRequeueDispatchingForCrashRecovery(t *testing.T) {
	e := newForumEnv(t)
	store := NewOutboxStore(e.writer)
	ev := outboxTestEvent("post:crash")
	if err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		return tx.AppendOutbox(e.ctx, ev)
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.ClaimPending(e.ctx); !ok {
		t.Fatal("应能领取事件")
	}
	// 模拟进程在投递前崩溃：事件永久停在 dispatching。
	if _, ok, _ := store.ClaimPending(e.ctx); ok {
		t.Fatal("dispatching 事件不应被重复领取")
	}
	n, err := store.RequeueDispatching(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("应退回 1 条 dispatching，实际 n=%d err=%v", n, err)
	}
	rec, ok, err := store.ClaimPending(e.ctx)
	if err != nil || !ok || rec.Event.ID != ev.ID {
		t.Fatalf("重扫后应能重新领取: rec=%+v ok=%v err=%v", rec, ok, err)
	}
}

type captureSink struct{ events []event.DomainEvent }

func (s *captureSink) Publish(_ context.Context, e event.DomainEvent) error {
	s.events = append(s.events, e)
	return nil
}

// TestOutboxEndToEndDelivery 把真实 Service + 真实 Repos + 真实 OutboxStore 串起来，
// 证明事件在写事务内落库、提交后才投递、投递后状态为 delivered。
func TestOutboxEndToEndDelivery(t *testing.T) {
	e := newForumEnv(t)
	store := NewOutboxStore(e.writer)
	sink := &captureSink{}
	o, err := forum.NewOutbox(store, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := forum.NewService(e.repos,
		forum.WithClock(func() time.Time { return time.Unix(e.clock, 0).UTC() }),
		forum.WithOutbox(o),
	)

	threadID, err := svc.CreateThread(e.ctx, forum.Actor{UserID: e.userID, Status: user.StatusActive},
		forum.CreateThreadInput{NodeID: e.nodeID, Title: "outbox e2e", Body: "正文"})
	if err != nil {
		t.Fatalf("发帖失败: %v", err)
	}
	if len(sink.events) != 1 || sink.events[0].ThreadID != threadID {
		t.Fatalf("提交后应投递 1 条事件: %+v", sink.events)
	}
	if n := outboxCount(t, e); n != 1 {
		t.Fatalf("应落库 1 条 outbox，实际 %d", n)
	}
	if status, _ := outboxStatus(t, e, sink.events[0].ID); status != "delivered" {
		t.Fatalf("投递成功后状态应为 delivered，实际 %q", status)
	}
	if o.Delivered() != 1 {
		t.Fatalf("调度器成功计数应为 1，实际 %d", o.Delivered())
	}
}
