package main

// 峰值在线采样器的回归（v0.7.1 / P4.5-M1 / ADR-064）。
// 五条必须锁死的行为：
//  1. 当日首行必写：即使在线为 0 也要落一行，否则「跑了一整天没人来」会显示成 --；
//  2. 只写变高：同值不重复写，把写队列留给业务写；
//  3. 跨天归零：今日峰值绝不继承进程历史峰值（昨日 50 / 今日 3 必须写 3）；
//  4. 写失败不推进状态：下一轮带着同样的值重试，不丢观测；
//  5. 停止 / nil 依赖的安全边界。

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakePresence struct{ online int }

func (f *fakePresence) Online() int { return f.online }

type peakWrite struct {
	day  string
	peak int
}

type fakePeakStore struct {
	writes []peakWrite
	err    error
}

func (f *fakePeakStore) UpsertDailyPeak(ctx context.Context, localDate string, peak int, seenAt time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, peakWrite{day: localDate, peak: peak})
	return nil
}

func newTestSampler(presence *fakePresence, store *fakePeakStore, now *time.Time, loc *time.Location) *peakSampler {
	return newPeakSampler(nil, presence, store, loc, func() time.Time { return *now })
}

func TestPeakSamplerWritesFirstRowEvenWhenIdle(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, loc)
	presence := &fakePresence{online: 0}
	store := &fakePeakStore{}
	s := newTestSampler(presence, store, &now, loc)

	s.tick(context.Background())
	if len(store.writes) != 1 || store.writes[0].peak != 0 || store.writes[0].day != "2026-11-22" {
		t.Fatalf("空闲日首 tick 也必须写一行 (2026-11-22, 0)，实际 %+v", store.writes)
	}
}

func TestPeakSamplerWritesOnlyOnIncrease(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, loc)
	presence := &fakePresence{online: 0}
	store := &fakePeakStore{}
	s := newTestSampler(presence, store, &now, loc)

	s.tick(context.Background()) // 写 0
	presence.online = 3
	s.tick(context.Background()) // 写 3
	s.tick(context.Background()) // 同值，不写
	presence.online = 1          // 回落，不写
	s.tick(context.Background())
	presence.online = 8
	s.tick(context.Background()) // 写 8

	if len(store.writes) != 3 {
		t.Fatalf("应只在 0/3/8 时写 3 次，实际 %+v", store.writes)
	}
	for i, want := range []int{0, 3, 8} {
		if store.writes[i].peak != want {
			t.Fatalf("第 %d 次写入应为 %d，实际 %d", i+1, want, store.writes[i].peak)
		}
	}
}

// 跨天归零（QA P0-8）：进程峰值不按天重置，采样器必须自持当日峰值。
func TestPeakSamplerResetsDayPeakAcrossMidnight(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 22, 23, 59, 0, 0, loc)
	presence := &fakePresence{online: 50}
	store := &fakePeakStore{}
	s := newTestSampler(presence, store, &now, loc)

	s.tick(context.Background()) // 22 日写 50
	now = time.Date(2026, 11, 23, 0, 1, 0, 0, loc)
	presence.online = 3
	s.tick(context.Background()) // 23 日只能写 3，不能继承 50

	if len(store.writes) != 2 {
		t.Fatalf("跨天应各写一行，实际 %+v", store.writes)
	}
	if store.writes[1].day != "2026-11-23" || store.writes[1].peak != 3 {
		t.Fatalf("次日首行应为 (2026-11-23, 3)，实际 %+v（继承昨日峰值即 P0 级错误）", store.writes[1])
	}
}

func TestPeakSamplerRetriesAfterWriteFailure(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, loc)
	presence := &fakePresence{online: 4}
	store := &fakePeakStore{err: errors.New("队列已满")}
	s := newTestSampler(presence, store, &now, loc)

	s.tick(context.Background())
	if len(store.writes) != 0 {
		t.Fatal("失败时不应记录成功写入")
	}
	store.err = nil
	s.tick(context.Background())
	if len(store.writes) != 1 || store.writes[0].peak != 4 {
		t.Fatalf("恢复后应补写 4，实际 %+v", store.writes)
	}
}

func TestPeakSamplerNilDependenciesAreSafe(t *testing.T) {
	now := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	// presence / store 为空时 tick 直接返回，不 panic（未装配场景的零值安全）。
	s := newPeakSampler(nil, nil, nil, nil, func() time.Time { return now })
	s.tick(context.Background())
}
