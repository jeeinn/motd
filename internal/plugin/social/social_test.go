package social

// v0.5.1 / P3-M6：状态类彩蛋成就回归——夜行者时段、首次被投票给作者。

import (
	"context"
	"testing"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/plugin/registry"
)

type fakeQuery struct {
	threadOwner int64
	postOwner   int64
}

func (f fakeQuery) ThreadTitle(context.Context, int64) (string, error) { return "标题", nil }
func (f fakeQuery) ThreadOwnerID(context.Context, int64) (int64, error) {
	return f.threadOwner, nil
}
func (f fakeQuery) PostOwnerID(context.Context, int64) (int64, error) { return f.postOwner, nil }

type fakeGate struct {
	calls []appuser.AchievementRequest
}

func (g *fakeGate) Apply(_ context.Context, req appuser.AchievementRequest) (appuser.AchievementDecision, error) {
	g.calls = append(g.calls, req)
	return appuser.AchievementDecision{Granted: true}, nil
}

func socialPlugin(t *testing.T) registry.Plugin {
	t.Helper()
	for _, typ := range []event.DomainEventType{event.EventPostCreated, event.EventVoteCast} {
		for _, p := range registry.Subscribers(typ) {
			if p.Name == pluginName {
				return p
			}
		}
	}
	t.Fatal("social 插件未注册")
	return registry.Plugin{}
}

func TestNightOwlOnlyInDeepNight(t *testing.T) {
	p := socialPlugin(t)
	// UTC 时刻 → 展示时区 02:00-05:00 边界。nil Location 回落 UTC（原行为）。
	cases := []struct {
		hour     string
		want     int
		location *time.Location
	}{
		// nil Location = UTC，与改动前行为一致
		{"2026-11-20T01:30:00Z", 0, nil},
		{"2026-11-20T02:00:00Z", 1, nil},
		{"2026-11-20T04:59:00Z", 1, nil},
		{"2026-11-20T05:00:00Z", 0, nil},
		// Asia/Shanghai（UTC+8）：18:30Z→次日 02:30 本地，应解锁；
		// 而同一时刻按 UTC 判断是 18 点、不解锁——证明时区真的生效。
		{"2026-11-20T18:30:00Z", 1, mustLoadLocation(t, "Asia/Shanghai")},
		{"2026-11-20T18:30:00Z", 0, nil},
		// UTC 02:30 在上海是 10:30，不应解锁。
		{"2026-11-20T02:30:00Z", 0, mustLoadLocation(t, "Asia/Shanghai")},
	}
	for _, tc := range cases {
		at, _ := time.Parse(time.RFC3339, tc.hour)
		gate := &fakeGate{}
		c := registry.Context{Read: fakeQuery{}, Achievements: gate, Location: tc.location}
		e := event.DomainEvent{
			ID: "post:1", Type: event.EventPostCreated, OccurredAt: at,
			ActorID: 7, PostID: 1, ThreadID: 1,
		}
		if err := p.Handler.Handle(context.Background(), e, c); err != nil {
			t.Fatalf("处理事件失败: %v", err)
		}
		if len(gate.calls) != tc.want {
			t.Errorf("%s (loc=%v) 应申请成就 %d 次，实际 %d", tc.hour, tc.location, tc.want, len(gate.calls))
			continue
		}
		if tc.want == 1 && gate.calls[0].Code != domainuser.AchievementNightOwl {
			t.Errorf("深更半夜应解锁 night_owl，实际 %s", gate.calls[0].Code)
		}
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("加载时区 %q: %v", name, err)
	}
	return loc
}

func TestFirstVoteGoesToAuthorNotVoter(t *testing.T) {
	p := socialPlugin(t)
	gate := &fakeGate{}
	c := registry.Context{
		Read:         fakeQuery{threadOwner: 42, postOwner: 99},
		Achievements: gate,
	}
	// 主题投票：ActorID(投票者)=7，作者=42。
	e := event.DomainEvent{
		ID: "vote:7:thread:1", Type: event.EventVoteCast,
		OccurredAt: time.Now(), ActorID: 7, ThreadID: 1,
	}
	if err := p.Handler.Handle(context.Background(), e, c); err != nil {
		t.Fatal(err)
	}
	if len(gate.calls) != 1 {
		t.Fatalf("应申请一次成就，实际 %d", len(gate.calls))
	}
	if gate.calls[0].UserID != 42 {
		t.Errorf("成就应给作者 42，实际给 %d", gate.calls[0].UserID)
	}
	if gate.calls[0].Code != domainuser.AchievementFirstVote {
		t.Errorf("应解锁 first_vote，实际 %s", gate.calls[0].Code)
	}

	// 回复投票：作者=99。
	gate2 := &fakeGate{}
	c2 := registry.Context{Read: fakeQuery{postOwner: 99}, Achievements: gate2}
	e2 := event.DomainEvent{
		ID: "vote:7:post:5", Type: event.EventVoteCast,
		OccurredAt: time.Now(), ActorID: 7, ThreadID: 1, PostID: 5,
	}
	if err := p.Handler.Handle(context.Background(), e2, c2); err != nil {
		t.Fatal(err)
	}
	if len(gate2.calls) != 1 || gate2.calls[0].UserID != 99 {
		t.Errorf("回复投票成就应给作者 99，实际 %+v", gate2.calls)
	}
}

func TestFirstVoteSelfVoteGivesNothing(t *testing.T) {
	p := socialPlugin(t)
	gate := &fakeGate{}
	c := registry.Context{Read: fakeQuery{threadOwner: 7}, Achievements: gate}
	e := event.DomainEvent{
		ID: "vote:7:thread:1", Type: event.EventVoteCast,
		OccurredAt: time.Now(), ActorID: 7, ThreadID: 1, // 投票者就是作者
	}
	if err := p.Handler.Handle(context.Background(), e, c); err != nil {
		t.Fatal(err)
	}
	if len(gate.calls) != 0 {
		t.Errorf("自投票不应解锁 first_vote，实际 %d 次", len(gate.calls))
	}
}
