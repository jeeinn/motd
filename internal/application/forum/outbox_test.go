package forum

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
)

// 调度器单测：用内存 fake 锁定「提交后投递、状态回写、失败可观测、崩溃恢复」四件事。
// 真实 SQLite 的 CAS 与事务边界由 infrastructure 的集成测试覆盖。

type fakeOutboxStore struct {
	records  []OutboxRecord
	status   map[int64]string
	claimErr error
	requeued int64
}

func newFakeOutboxStore(n int) *fakeOutboxStore {
	s := &fakeOutboxStore{status: make(map[int64]string)}
	for i := 0; i < n; i++ {
		id := int64(i + 1)
		s.records = append(s.records, OutboxRecord{
			ID: id,
			Event: event.DomainEvent{
				ID:         "post:" + string(rune('a'+i)),
				Type:       event.EventPostCreated,
				OccurredAt: time.Now().UTC(),
				ActorID:    1,
			},
		})
		s.status[id] = "pending"
	}
	return s
}

func (s *fakeOutboxStore) ClaimPending(context.Context) (OutboxRecord, bool, error) {
	if s.claimErr != nil {
		return OutboxRecord{}, false, s.claimErr
	}
	for _, r := range s.records {
		if s.status[r.ID] == "pending" {
			s.status[r.ID] = "dispatching"
			return r, true, nil
		}
	}
	return OutboxRecord{}, false, nil
}

func (s *fakeOutboxStore) MarkDelivered(_ context.Context, id int64, _ time.Time) error {
	s.status[id] = "delivered"
	return nil
}

func (s *fakeOutboxStore) MarkFailed(_ context.Context, id int64) error {
	s.status[id] = "failed"
	return nil
}

func (s *fakeOutboxStore) RequeueDispatching(context.Context) (int64, error) {
	var n int64
	for id, st := range s.status {
		if st == "dispatching" {
			s.status[id] = "pending"
			n++
		}
	}
	s.requeued = n
	return n, nil
}

func TestOutboxDispatchMarksDelivered(t *testing.T) {
	store := newFakeOutboxStore(2)
	sink := &recordingSink{}
	o, err := NewOutbox(store, sink, nil)
	if err != nil {
		t.Fatal(err)
	}

	delivered, failed, err := o.DispatchPending(context.Background())
	if err != nil || delivered != 2 || failed != 0 {
		t.Fatalf("投递结果异常: delivered=%d failed=%d err=%v", delivered, failed, err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("订阅者应收到 2 条事件，实际 %d", len(sink.events))
	}
	for _, r := range store.records {
		if store.status[r.ID] != "delivered" {
			t.Fatalf("事件 %d 应标记 delivered，实际 %q", r.ID, store.status[r.ID])
		}
	}
	if o.Delivered() != 2 || o.Failed() != 0 {
		t.Fatalf("观测计数异常: delivered=%d failed=%d", o.Delivered(), o.Failed())
	}
}

func TestOutboxPublishFailureMarksFailedAndNotifies(t *testing.T) {
	store := newFakeOutboxStore(1)
	pubErr := errors.New("插件投递失败")
	sink := &recordingSink{fail: pubErr}
	obs := &observerLog{}
	o, err := NewOutbox(store, sink, obs.observe)
	if err != nil {
		t.Fatal(err)
	}

	delivered, failed, err := o.DispatchPending(context.Background())
	if err != nil || delivered != 0 || failed != 1 {
		t.Fatalf("投递结果异常: delivered=%d failed=%d err=%v", delivered, failed, err)
	}
	if store.status[1] != "failed" {
		t.Fatalf("失败事件应标记 failed，实际 %q", store.status[1])
	}
	if len(obs.failures) != 1 || !errors.Is(obs.failures[0].err, pubErr) {
		t.Fatalf("失败必须可观测: %+v", obs.failures)
	}
	if o.Failed() != 1 {
		t.Fatal("失败计数应为 1")
	}
}

func TestOutboxRecoverPendingRequeuesDispatching(t *testing.T) {
	store := newFakeOutboxStore(1)
	// 模拟上次进程在领取后、投递前崩溃：卡在 dispatching。
	store.status[1] = "dispatching"
	sink := &recordingSink{}
	o, err := NewOutbox(store, sink, nil)
	if err != nil {
		t.Fatal(err)
	}

	requeued, delivered, failed, err := o.RecoverPending(context.Background())
	if err != nil || requeued != 1 || delivered != 1 || failed != 0 {
		t.Fatalf("恢复结果异常: requeued=%d delivered=%d failed=%d err=%v", requeued, delivered, failed, err)
	}
	if len(sink.events) != 1 || store.status[1] != "delivered" {
		t.Fatalf("残留事件应被重投并落地: events=%d status=%q", len(sink.events), store.status[1])
	}
}

func TestOutboxClaimErrorStopsDispatch(t *testing.T) {
	store := newFakeOutboxStore(0)
	store.claimErr = errors.New("领取失败")
	o, err := NewOutbox(store, &recordingSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := o.DispatchPending(context.Background()); err == nil {
		t.Fatal("领取失败应向上冒泡，由调用方计数并上报")
	}
}

// 写用例必须把事件在 Write 闭包内交给 AppendOutbox（同事务），
// 而不是提交后才补写——后者会在崩溃时丢事件。
func TestCreateThreadAppendsOutboxInsideWrite(t *testing.T) {
	repos := newFakeRepos()
	o, err := NewOutbox(newFakeOutboxStore(0), &recordingSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repos, WithClock(func() time.Time { return testBase }), WithOutbox(o))

	if _, err := svc.CreateThread(context.Background(), activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	}); err != nil {
		t.Fatalf("发帖失败: %v", err)
	}
	if len(repos.state.outboxEvents) != 1 {
		t.Fatalf("事件应在写事务内追加到 outbox，实际 %d 条", len(repos.state.outboxEvents))
	}
	ev := repos.state.outboxEvents[0]
	if ev.Type != event.EventPostCreated || ev.PostID <= 0 || ev.ID == "" {
		t.Fatalf("追加的事件字段错误: %+v", ev)
	}
}

func TestCreateThreadRollbackDropsOutboxEvent(t *testing.T) {
	repos := newFakeRepos()
	repos.failInsertPost = errors.New("注入插入失败")
	o, err := NewOutbox(newFakeOutboxStore(0), &recordingSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(repos, WithClock(func() time.Time { return testBase }), WithOutbox(o))

	if _, err := svc.CreateThread(context.Background(), activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	}); err == nil {
		t.Fatal("注入的写入失败应让用例失败")
	}
	if len(repos.state.outboxEvents) != 0 {
		t.Fatal("事务回滚时 outbox 事件必须一并丢弃")
	}
}

func TestNewOutboxRejectsNilDependencies(t *testing.T) {
	if _, err := NewOutbox(nil, &recordingSink{}, nil); err == nil {
		t.Fatal("nil store 应被拒绝")
	}
	if _, err := NewOutbox(newFakeOutboxStore(0), nil, nil); err == nil {
		t.Fatal("nil sink 应被拒绝")
	}
}
