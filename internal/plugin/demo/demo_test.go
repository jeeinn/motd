package demo

import (
	"context"
	"errors"
	"testing"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/plugin/registry"
)

type fakeQuery struct {
	title string
	err   error
}

func (f fakeQuery) ThreadTitle(context.Context, int64) (string, error) { return f.title, f.err }

type fakeStatus struct{ status domainuser.Status }

func (f fakeStatus) Status(context.Context, int64) (domainuser.Status, error) {
	return f.status, nil
}

func demoPlugin(t *testing.T) registry.Plugin {
	t.Helper()
	for _, p := range registry.Subscribers(event.EventPostCreated) {
		if p.Name == pluginName {
			return p
		}
	}
	t.Fatal("demo 插件未通过 init() 注册或订阅了错误的事件")
	return registry.Plugin{}
}

func sampleEvent(postID int64) event.DomainEvent {
	return event.DomainEvent{
		ID:         "post:1",
		Type:       event.EventPostCreated,
		OccurredAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		ActorID:    1,
		NodeID:     1,
		ThreadID:   7,
		PostID:     postID,
		FirstPost:  postID == 1,
	}
}

func testContext(store *appuser.MemoryAchievementStore) registry.Context {
	gate := appuser.NewAchievementService(store, fakeStatus{status: domainuser.StatusActive}, time.Now)
	return registry.Context{Read: fakeQuery{title: "来自只读接口的标题"}, Achievements: gate}
}

func TestDemoGrantsAchievementAfterPostCreated(t *testing.T) {
	ctx := context.Background()
	p := demoPlugin(t)
	store := appuser.NewMemoryAchievementStore()
	c := testContext(store)

	if err := p.Handler.Handle(ctx, sampleEvent(1), c); err != nil {
		t.Fatalf("处理 PostCreated 失败: %v", err)
	}
	if store.GrantedCount() != 1 {
		t.Fatalf("发帖后应解锁 1 个成就，实际 %d", store.GrantedCount())
	}
	grants := store.Entries()
	if grants[0].UserID != 1 || grants[0].Code != domainuser.AchievementFirstPost {
		t.Fatalf("成就记录错误: %+v", grants)
	}

	// 第二次发帖：插件仍会被触发，但 application/user 判定已解锁，不重复写入。
	if err := p.Handler.Handle(ctx, sampleEvent(2), c); err != nil {
		t.Fatalf("第二次处理失败: %v", err)
	}
	if store.GrantedCount() != 1 {
		t.Fatalf("重复触发不应重复解锁，实际 %d", store.GrantedCount())
	}
}

func TestDemoRegistrationIsVisible(t *testing.T) {
	var found bool
	for _, p := range registry.All() {
		if p.Name != pluginName {
			continue
		}
		found = true
		if len(p.Commands) != 1 || p.Commands[0].Name != "/fortune" {
			t.Errorf("命令登记错误: %+v", p.Commands)
		}
		if len(p.DomainSubscriptions) != 1 || p.DomainSubscriptions[0] != event.EventPostCreated {
			t.Errorf("订阅登记错误: %+v", p.DomainSubscriptions)
		}
	}
	if !found {
		t.Fatal("demo 插件未出现在注册表中")
	}
}

func TestDemoRejectsIncompleteContext(t *testing.T) {
	ctx := context.Background()
	p := demoPlugin(t)
	e := sampleEvent(1)

	if err := p.Handler.Handle(ctx, e, registry.Context{}); err == nil {
		t.Error("缺少成就通道时应报错，不能静默跳过")
	}
	if err := p.Handler.Handle(ctx, e, registry.Context{Achievements: testContext(appuser.NewMemoryAchievementStore()).Achievements}); err == nil {
		t.Error("缺少只读通道时应报错")
	}
}

func TestDemoPropagatesReadOnlyFailure(t *testing.T) {
	ctx := context.Background()
	p := demoPlugin(t)
	store := appuser.NewMemoryAchievementStore()
	c := testContext(store)
	c.Read = fakeQuery{err: errors.New("query failed")}

	if err := p.Handler.Handle(ctx, sampleEvent(1), c); err == nil {
		t.Fatal("只读查询失败应冒泡，而不是继续申请成就")
	}
	if store.GrantedCount() != 0 {
		t.Fatal("只读查询失败时不应解锁成就")
	}
}
