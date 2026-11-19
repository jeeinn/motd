package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
)

func sampleDomainEvent() event.DomainEvent {
	return event.DomainEvent{
		ID:         "post:1",
		Type:       event.EventPostCreated,
		OccurredAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		ActorID:    1,
		ThreadID:   7,
		PostID:     1,
	}
}

func TestDomainBusPublishesToAllSubscribers(t *testing.T) {
	ctx := context.Background()
	bus := NewDomainBus()
	var got []string
	if err := bus.Subscribe(event.EventPostCreated, func(_ context.Context, e event.DomainEvent) error {
		got = append(got, "a:"+e.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe(event.EventPostCreated, func(_ context.Context, e event.DomainEvent) error {
		got = append(got, "b:"+e.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 不同种类不应被派发到。
	other := false
	if err := bus.Subscribe(event.EventVoteCast, func(context.Context, event.DomainEvent) error {
		other = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := bus.Publish(ctx, sampleDomainEvent()); err != nil {
		t.Fatalf("派发失败: %v", err)
	}
	if len(got) != 2 || got[0] != "a:post:1" || got[1] != "b:post:1" {
		t.Fatalf("订阅者未按序收到: %v", got)
	}
	if other {
		t.Fatal("不同事件种类被错误派发")
	}
	if bus.Published() != 1 || bus.Failures() != 0 || bus.SubscriberCount(event.EventPostCreated) != 2 {
		t.Fatalf("计数异常: published=%d failures=%d subs=%d",
			bus.Published(), bus.Failures(), bus.SubscriberCount(event.EventPostCreated))
	}
}

func TestDomainBusRejectsMalformedEvent(t *testing.T) {
	bus := NewDomainBus()
	base := sampleDomainEvent()
	cases := []struct {
		name   string
		mutate func(*event.DomainEvent)
	}{
		{"缺幂等键", func(e *event.DomainEvent) { e.ID = "" }},
		{"非法种类", func(e *event.DomainEvent) { e.Type = event.EventUnknown }},
		{"缺时间", func(e *event.DomainEvent) { e.OccurredAt = time.Time{} }},
		{"缺发起者", func(e *event.DomainEvent) { e.ActorID = 0 }},
	}
	for _, c := range cases {
		e := base
		c.mutate(&e)
		if err := bus.Publish(context.Background(), e); err == nil {
			t.Errorf("%s 应被拒绝", c.name)
		}
	}
	if bus.Published() != 0 || bus.Failures() != int64(len(cases)) {
		t.Fatalf("失败计数异常: published=%d failures=%d", bus.Published(), bus.Failures())
	}
}

func TestDomainBusRejectsBadSubscription(t *testing.T) {
	bus := NewDomainBus()
	if err := bus.Subscribe(event.EventUnknown, func(context.Context, event.DomainEvent) error { return nil }); err == nil {
		t.Error("非法事件种类应拒绝订阅")
	}
	if err := bus.Subscribe(event.EventPostCreated, nil); err == nil {
		t.Error("nil handler 应拒绝订阅")
	}
}

func TestDomainBusCollectsHandlerErrorsWithoutShortCircuit(t *testing.T) {
	bus := NewDomainBus()
	sentinel := errors.New("插件失败")
	called := false
	_ = bus.Subscribe(event.EventPostCreated, func(context.Context, event.DomainEvent) error { return sentinel })
	_ = bus.Subscribe(event.EventPostCreated, func(context.Context, event.DomainEvent) error {
		called = true
		return nil
	})
	err := bus.Publish(context.Background(), sampleDomainEvent())
	if !errors.Is(err, sentinel) {
		t.Fatalf("订阅者错误应返回给调用方: %v", err)
	}
	if !called {
		t.Fatal("一个订阅者失败不应阻断其他订阅者")
	}
	if bus.Failures() != 1 {
		t.Fatalf("失败应可观测: %d", bus.Failures())
	}
}

func TestInteractionBusDropsWhenFull(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	bus := NewInteractionBus(2, func(InteractionEvent) {
		once.Do(func() { <-gate }) // 卡住消费 goroutine，让队列稳定填满
	})
	total := 8
	for i := 0; i < total; i++ {
		bus.TryPublish(InteractionEvent{Kind: InteractionConnected, SessionID: "s"})
	}
	close(gate)
	bus.Close()

	if bus.Dropped() == 0 {
		t.Fatal("队列满时应丢弃，而不是阻塞或扩容")
	}
	if bus.Accepted()+bus.Dropped() != int64(total) {
		t.Fatalf("投递账不平: accepted=%d dropped=%d total=%d", bus.Accepted(), bus.Dropped(), total)
	}
	if bus.Handled() != bus.Accepted() {
		t.Fatalf("Close 应排空在途事件: handled=%d accepted=%d", bus.Handled(), bus.Accepted())
	}
}

func TestInteractionBusCloseDrainsAndIsIdempotent(t *testing.T) {
	bus := NewInteractionBus(16, nil)
	for i := 0; i < 5; i++ {
		if !bus.TryPublish(InteractionEvent{Kind: InteractionKeyPressed, SessionID: "s"}) {
			t.Fatal("未满队列不应丢弃")
		}
	}
	bus.Close()
	bus.Close() // 重复调用不应 panic
	if bus.Handled() != 5 || bus.Accepted() != 5 || bus.Dropped() != 0 {
		t.Fatalf("关闭语义异常: handled=%d accepted=%d dropped=%d", bus.Handled(), bus.Accepted(), bus.Dropped())
	}
	if bus.TryPublish(InteractionEvent{Kind: InteractionIdle}) {
		t.Fatal("关闭后不应接受新事件")
	}
	if bus.Dropped() != 1 {
		t.Fatalf("关闭后投递应计入丢弃: %d", bus.Dropped())
	}
}

func TestInteractionBusConcurrentPublish(t *testing.T) {
	bus := NewInteractionBus(64, nil)
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.TryPublish(InteractionEvent{Kind: InteractionIdle, SessionID: "s"})
		}()
	}
	wg.Wait()
	bus.Close()
	if bus.Accepted()+bus.Dropped() != n {
		t.Fatalf("并发投递账不平: %d + %d != %d", bus.Accepted(), bus.Dropped(), n)
	}
	if bus.Handled() != bus.Accepted() {
		t.Fatalf("并发下仍有在途事件未处理: %d vs %d", bus.Handled(), bus.Accepted())
	}
}

// 两条通道不共用总线：领域事件走同步 Publish，交互事件走非阻塞 TryPublish，
// 各自的计数互不影响；类型系统上二者也没有共同接口。
func TestChannelsAreIndependent(t *testing.T) {
	domain := NewDomainBus()
	interaction := NewInteractionBus(8, nil)
	_ = domain.Publish(context.Background(), sampleDomainEvent())
	_ = interaction.TryPublish(InteractionEvent{Kind: InteractionConnected})
	interaction.Close()

	if domain.Published() != 1 || interaction.Accepted() != 1 {
		t.Fatal("两条通道各自计数")
	}
	if domain.Failures() != 0 || interaction.Dropped() != 0 {
		t.Fatal("正常投递不应产生失败/丢弃")
	}
	if !InteractionConnected.Valid() || InteractionUnknown.Valid() || (InteractionKind(9)).Valid() {
		t.Fatal("交互事件种类取值域错误")
	}
}

// TestDomainBusDeduplicatesByEventID 覆盖 outbox 崩溃恢复后的重投场景：
// 同一个 DomainEvent.ID 重复投递，订阅者只应被调用一次。
func TestDomainBusDeduplicatesByEventID(t *testing.T) {
	ctx := context.Background()
	bus := NewDomainBus()
	calls := 0
	if err := bus.Subscribe(event.EventPostCreated, func(_ context.Context, _ event.DomainEvent) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	e := sampleDomainEvent()
	for i := 0; i < 3; i++ {
		if err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("重复投递不应报错: %v", err)
		}
	}
	if calls != 1 || bus.Published() != 1 {
		t.Fatalf("按 ID 去重后订阅者只应收到 1 次: calls=%d published=%d", calls, bus.Published())
	}
}
