package user

// 统计快照的口径回归（v0.7.1 / P4.5-M1 / ADR-064）。
// 锁定三件事：日期边界（本地日历）、峰值自洽（历史 ≥ 今日 ≥ 当前在线）、
// 以及「读失败不冒充 0」。

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStatsReader struct {
	userCount    int64
	logins       map[string]int64
	todayPeak    int
	todaySeen    bool
	allPeak      int
	allSeen      bool
	totalLogins  int64
	queriedDates []string
	err          error
}

func (f *fakeStatsReader) UserCount(ctx context.Context) (int64, error) {
	return f.userCount, f.err
}

func (f *fakeStatsReader) DailyLogins(ctx context.Context, localDate string) (int64, error) {
	f.queriedDates = append(f.queriedDates, localDate)
	if f.err != nil {
		return 0, f.err
	}
	return f.logins[localDate], nil
}

func (f *fakeStatsReader) DailyPeakOnline(ctx context.Context, localDate string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	// todayPeak 只代表「今日那一行」；昨日一律无行（除非显式 days 里配）。
	if localDate == "2026-11-22" {
		return f.todayPeak, f.todaySeen, nil
	}
	return 0, false, nil
}

func (f *fakeStatsReader) PeakOnlineAllTime(ctx context.Context) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	return f.allPeak, f.allSeen, nil
}

func (f *fakeStatsReader) TotalLogins(ctx context.Context) (int64, error) {
	return f.totalLogins, f.err
}

func newStatsFixture() (*fakeStatsReader, *StatsService, time.Time) {
	// 2026-11-22 10:00 Asia/Shanghai（UTC+8）
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, loc)
	r := &fakeStatsReader{
		userCount:   87,
		logins:      map[string]int64{"2026-11-22": 12, "2026-11-21": 9},
		todayPeak:   7,
		todaySeen:   true,
		allPeak:     15,
		allSeen:     true,
		totalLogins: 233,
	}
	return r, NewStatsService(r, func() time.Time { return now }, loc), now
}

func TestStatsSnapshotUsesLocalCalendarDates(t *testing.T) {
	r, svc, _ := newStatsFixture()
	snap, err := svc.Snapshot(context.Background(), LiveCounters{Online: 3, Active: 2})
	if err != nil {
		t.Fatalf("生成快照: %v", err)
	}
	if snap.TodayLogins != 12 || snap.YesterdayLogins != 9 {
		t.Fatalf("今日/昨日登录应为 12/9，实际 %d/%d（昨日必须走本地日历 AddDate）",
			snap.TodayLogins, snap.YesterdayLogins)
	}
	if got := r.queriedDates; len(got) < 2 || got[0] != "2026-11-22" || got[1] != "2026-11-21" {
		t.Fatalf("查询日期应为 [2026-11-22 2026-11-21]，实际 %v", got)
	}
	if snap.UserCount != 87 || snap.TotalLogins != 233 {
		t.Fatalf("注册/累计登录应为 87/233，实际 %d/%d", snap.UserCount, snap.TotalLogins)
	}
	if snap.CurrentOnline != 3 || snap.CurrentActive != 2 {
		t.Fatalf("在线/活跃应原样透传，实际 %d/%d", snap.CurrentOnline, snap.CurrentActive)
	}
}

// 跨时区日期归属：23:30Z 在 UTC+8 已是次日，日期必须按展示时区算。
func TestStatsSnapshotDateFollowsDisplayTimezone(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	now := time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC) // 本地 2026-11-23 07:30
	r := &fakeStatsReader{logins: map[string]int64{}}
	svc := NewStatsService(r, func() time.Time { return now }, loc)
	if _, err := svc.Snapshot(context.Background(), LiveCounters{}); err != nil {
		t.Fatal(err)
	}
	if got := r.queriedDates; len(got) < 2 || got[0] != "2026-11-23" || got[1] != "2026-11-22" {
		t.Fatalf("展示时区下应为 11-23 / 11-22，实际 %v", got)
	}
}

// 峰值自洽：历史 ≥ 今日 ≥ 当前在线；库值落后时用当前在线补上。
func TestStatsSnapshotPeakInvariants(t *testing.T) {
	r, svc, _ := newStatsFixture()
	r.todayPeak, r.todaySeen = 3, true
	r.allPeak, r.allSeen = 4, true

	snap, err := svc.Snapshot(context.Background(), LiveCounters{Online: 5})
	if err != nil {
		t.Fatal(err)
	}
	if snap.TodayPeak != 5 {
		t.Fatalf("今日峰值应被当前在线抬到 5，实际 %d", snap.TodayPeak)
	}
	if snap.AllTimePeak != 5 {
		t.Fatalf("历史峰值应 ≥ 今日显示值，实际 %d", snap.AllTimePeak)
	}
	if !snap.TodayPeakObserved || !snap.AllTimePeakSeen {
		t.Fatal("有实时观测时两项都必须标记为已观测")
	}
}

// `--` 的唯一判据（两条件）：当日无行 **且** 当前在线为 0。
func TestStatsSnapshotNoObservationOnlyWhenNoRowAndZeroOnline(t *testing.T) {
	r, svc, _ := newStatsFixture()
	r.todaySeen = false
	r.allSeen = false
	r.todayPeak, r.allPeak = 0, 0

	snap, err := svc.Snapshot(context.Background(), LiveCounters{Online: 0})
	if err != nil {
		t.Fatal(err)
	}
	if snap.TodayPeakObserved || snap.AllTimePeakSeen {
		t.Fatal("无行且在线为 0 时才应显示 --")
	}

	// 无行但在线 1：在线数本身构成一次观测，不得显示 --。
	snap, err = svc.Snapshot(context.Background(), LiveCounters{Online: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.TodayPeakObserved || snap.TodayPeak != 1 {
		t.Fatalf("在线 1 即构成今日观测，实际 observed=%v peak=%d", snap.TodayPeakObserved, snap.TodayPeak)
	}
}

// 读失败必须整体失败：不允许用 0 兜底把故障伪装成「没人来」。
func TestStatsSnapshotPropagatesReadError(t *testing.T) {
	r, svc, _ := newStatsFixture()
	r.err = errors.New("db is locked")
	if _, err := svc.Snapshot(context.Background(), LiveCounters{}); err == nil {
		t.Fatal("读失败时必须返回错误")
	}
}
