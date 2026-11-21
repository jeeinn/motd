package user

// v0.6.0：签到与连续天数回归。

import (
	"context"
	"testing"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

type fakeCheckInStore struct {
	dates   map[string]bool // "2026-11-20" -> true
	records int
}

func newFakeCheckInStore() *fakeCheckInStore {
	return &fakeCheckInStore{dates: make(map[string]bool)}
}

func (s *fakeCheckInStore) Record(_ context.Context, _ int64, date string, _ time.Time) error {
	s.records++
	s.dates[date] = true
	return nil
}

func (s *fakeCheckInStore) Streak(_ context.Context, _ int64, date string) (int, error) {
	cur, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, err
	}
	n := 0
	for {
		if !s.dates[cur.Format("2006-01-02")] {
			break
		}
		n++
		cur = cur.AddDate(0, 0, -1)
	}
	return n, nil
}

type fakeGate struct{ calls []AchievementRequest }

func (g *fakeGate) Apply(_ context.Context, req AchievementRequest) (AchievementDecision, error) {
	g.calls = append(g.calls, req)
	return AchievementDecision{Granted: true}, nil
}

func TestCheckInStreakAndAchievement(t *testing.T) {
	ctx := context.Background()
	store := newFakeCheckInStore()
	gate := &fakeGate{}
	fixed := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC) // 展示时区 03:00
	svc := NewCheckInService(store, gate, func() time.Time { return fixed }, time.UTC)

	// 连续 7 天签到（每天推进时钟）。
	for i := 0; i < 6; i++ {
		res, err := svc.CheckIn(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if res.Unlocked {
			t.Fatalf("第 %d 天不应解锁", i+1)
		}
		fixed = fixed.AddDate(0, 0, 1)
		svc = NewCheckInService(store, gate, func() time.Time { return fixed }, time.UTC)
	}
	res, err := svc.CheckIn(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unlocked || res.Streak != 7 {
		t.Errorf("第 7 天应解锁 7 连续成就，实际 streak=%d unlocked=%v", res.Streak, res.Unlocked)
	}
	if len(gate.calls) != 1 || gate.calls[0].Code != domainuser.AchievementStreak7 {
		t.Errorf("应申请一次 streak_7，实际 %+v", gate.calls)
	}
}

func TestCheckInIdempotentSameDay(t *testing.T) {
	ctx := context.Background()
	store := newFakeCheckInStore()
	gate := &fakeGate{}
	fixed := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	svc := NewCheckInService(store, gate, func() time.Time { return fixed }, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := svc.CheckIn(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if store.records != 3 {
		t.Errorf("fake 层记录 3 次（真实仓储 UNIQUE 幂等由 SQLite 测试保证），实际 %d", store.records)
	}
}

func TestCheckInBrokenStreakResets(t *testing.T) {
	ctx := context.Background()
	store := newFakeCheckInStore()
	gate := &fakeGate{}
	// 预置 11-18、11-20（缺 11-19），连续天数应为 1。
	store.dates["2026-11-18"] = true
	store.dates["2026-11-20"] = true
	fixed := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	svc := NewCheckInService(store, gate, func() time.Time { return fixed }, time.UTC)
	res, err := svc.CheckIn(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Streak != 1 {
		t.Errorf("断签应重置，实际连续 %d 天", res.Streak)
	}
}

func TestProgressBar(t *testing.T) {
	svc := NewCheckInService(newFakeCheckInStore(), &fakeGate{}, time.Now, time.UTC)
	got := svc.ProgressBar(3)
	if want := "签到 ███░░░░ 3/7 天"; got != want {
		t.Errorf("进度条=%q 期望 %q", got, want)
	}
}
