package sqlite

// 统计看板的仓储回归（v0.7.1 / P4.5-M1 / ADR-064）。
// 锁定四件事：COUNT 口径（active 过滤 / 去重登录 / login_success）、
// 峰值「只升不降」、观测态三态区分、日期列约束。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStatsEnv(t *testing.T) (context.Context, *StatsStore, *Pools, *Writer) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(func() {
		writer.Stop()
		_ = pools.Close()
	})
	return ctx, NewStatsStore(pools, writer), pools, writer
}

func TestStatsStorePeakUpsertMonotonic(t *testing.T) {
	ctx, store, _, _ := newStatsEnv(t)
	const day = "2026-11-22"
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	if peak, observed, err := store.DailyPeakOnline(ctx, day); err != nil || observed || peak != 0 {
		t.Fatalf("空表应为 (0,false)：peak=%d observed=%v err=%v", peak, observed, err)
	}
	if err := store.UpsertDailyPeak(ctx, day, 7, at); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if peak, observed, _ := store.DailyPeakOnline(ctx, day); !observed || peak != 7 {
		t.Fatalf("首次写入后应为 (7,true)，实际 (%d,%v)", peak, observed)
	}
	// 更低的值不得覆盖（MAX 语义）：这是「只升不降」的核心。
	if err := store.UpsertDailyPeak(ctx, day, 3, at.Add(time.Minute)); err != nil {
		t.Fatalf("写入更低峰值: %v", err)
	}
	if peak, _, _ := store.DailyPeakOnline(ctx, day); peak != 7 {
		t.Fatalf("写入 3 后应仍为 7，实际 %d", peak)
	}
	// 0 也不得把已有峰值压下去（跨天后首个 tick 写 0 的历史坑）。
	if err := store.UpsertDailyPeak(ctx, day, 0, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("写入 0: %v", err)
	}
	if peak, _, _ := store.DailyPeakOnline(ctx, day); peak != 7 {
		t.Fatalf("写入 0 后应仍为 7，实际 %d", peak)
	}
	// 更高的值必须覆盖。
	if err := store.UpsertDailyPeak(ctx, day, 12, at.Add(3*time.Minute)); err != nil {
		t.Fatalf("写入更高峰值: %v", err)
	}
	if peak, _, _ := store.DailyPeakOnline(ctx, day); peak != 12 {
		t.Fatalf("写入 12 后应为 12，实际 %d", peak)
	}
}

// updated_at 只在「真的变高」时推进：否则运维无法用它判断最后观测时刻。
func TestStatsStoreUpdatedAtOnlyAdvancesOnIncrease(t *testing.T) {
	ctx, store, pools, _ := newStatsEnv(t)
	const day = "2026-11-22"
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	if err := store.UpsertDailyPeak(ctx, day, 5, base); err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := pools.QueryRowContext(ctx,
		`SELECT updated_at FROM stats_daily WHERE local_date = ?`, day).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != base.Unix() {
		t.Fatalf("updated_at 应为 %d，实际 %d", base.Unix(), first)
	}

	// 同值 / 更低值：updated_at 不动。
	if err := store.UpsertDailyPeak(ctx, day, 5, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var same int64
	if err := pools.QueryRowContext(ctx,
		`SELECT updated_at FROM stats_daily WHERE local_date = ?`, day).Scan(&same); err != nil {
		t.Fatal(err)
	}
	if same != first {
		t.Fatalf("峰值未变高时 updated_at 不应推进：%d → %d", first, same)
	}
}

// 跨天隔离：写今天不影响昨天；历史峰值取全表 MAX。
func TestStatsStoreCrossDayIsolation(t *testing.T) {
	ctx, store, _, _ := newStatsEnv(t)
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	if err := store.UpsertDailyPeak(ctx, "2026-11-21", 9, at); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDailyPeak(ctx, "2026-11-22", 3, at); err != nil {
		t.Fatal(err)
	}
	if peak, _, _ := store.DailyPeakOnline(ctx, "2026-11-22"); peak != 3 {
		t.Fatalf("今日峰值应为 3（昨日 9 不得泄漏），实际 %d", peak)
	}
	peak, observed, err := store.PeakOnlineAllTime(ctx)
	if err != nil || !observed || peak != 9 {
		t.Fatalf("历史峰值应为 9，实际 (%d,%v,%v)", peak, observed, err)
	}
}

func TestStatsStoreRejectsInvalidDates(t *testing.T) {
	ctx, store, _, _ := newStatsEnv(t)
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	for _, day := range []string{"", "2026-11-2", "2026/11/22"} {
		if err := store.UpsertDailyPeak(ctx, day, 1, at); err == nil {
			t.Fatalf("非法日期 %q 应被 CHECK 拒绝", day)
		}
	}
}

// 注册数只算 active：pending / deleting / deleted 都不计（防注销虚增）。
func TestStatsStoreUserCountOnlyActive(t *testing.T) {
	ctx, store, pools, _ := newStatsEnv(t)
	now := time.Now().Unix()
	for i, st := range []string{"active", "active", "pending", "deleting", "deleted"} {
		if _, err := pools.writer.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("user-%s-%d", st, i), st, now, now); err != nil {
			t.Fatalf("插入用户 %s: %v", st, err)
		}
	}
	n, err := store.UserCount(ctx)
	if err != nil {
		t.Fatalf("统计注册用户数: %v", err)
	}
	if n != 2 {
		t.Fatalf("只应统计 2 个 active 用户，实际 %d", n)
	}
}

func TestStatsStoreDailyLoginsCountsDistinctRows(t *testing.T) {
	ctx, store, pools, _ := newStatsEnv(t)
	now := time.Now().Unix()
	const day, other = "2026-11-22", "2026-11-21"
	res, err := pools.writer.ExecContext(ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES ('alice', 'active', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()

	// 同一用户同一天只能有一行（UNIQUE），不同天各一行。
	for _, d := range []string{day, day, other} {
		if _, err := pools.writer.ExecContext(ctx,
			`INSERT INTO daily_checkins (user_id, local_date, created_at) VALUES (?, ?, ?)
			 ON CONFLICT(user_id, local_date) DO NOTHING`, uid, d, now); err != nil {
			t.Fatalf("插入签到 %s: %v", d, err)
		}
	}
	if n, err := store.DailyLogins(ctx, day); err != nil || n != 1 {
		t.Fatalf("今日登录人数应为 1，实际 %d（err=%v）", n, err)
	}
	if n, err := store.DailyLogins(ctx, other); err != nil || n != 1 {
		t.Fatalf("昨日登录人数应为 1，实际 %d（err=%v）", n, err)
	}
	if n, err := store.DailyLogins(ctx, "2026-11-20"); err != nil || n != 0 {
		t.Fatalf("无记录日期应为 0，实际 %d（err=%v）", n, err)
	}
}

func TestStatsStoreTotalLoginsCountsOnlySuccess(t *testing.T) {
	ctx, store, pools, _ := newStatsEnv(t)
	now := time.Now().Unix()
	for _, ev := range []string{"login_success", "login_success", "login_failure", "logout"} {
		if _, err := pools.writer.ExecContext(ctx,
			`INSERT INTO auth_events (user_id, event, provider, ip_hash, detail_json, created_at)
			 VALUES (1, ?, 'github', '', '{}', ?)`, ev, now); err != nil {
			t.Fatalf("插入审计事件 %s: %v", ev, err)
		}
	}
	n, err := store.TotalLogins(ctx)
	if err != nil || n != 2 {
		t.Fatalf("累计登录人次应为 2（只数 login_success），实际 %d（err=%v）", n, err)
	}
}

// 0011 迁移的对象必须真的存在：索引 ×2 + stats_daily 表。
func TestMigration0011Objects(t *testing.T) {
	ctx, _, pools, _ := newStatsEnv(t)
	for _, name := range []string{"idx_threads_last_reply", "idx_auth_events_event"} {
		var got string
		if err := pools.reader.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("缺少索引 %q: %v", name, err)
		}
	}
	var ddl string
	if err := pools.reader.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'stats_daily'`).Scan(&ddl); err != nil {
		t.Fatalf("缺少 stats_daily 表: %v", err)
	}
	if !strings.Contains(ddl, "NOT NULL") || !strings.Contains(ddl, "local_date") {
		t.Fatalf("stats_daily 的 local_date 必须 NOT NULL: %s", ddl)
	}
}
