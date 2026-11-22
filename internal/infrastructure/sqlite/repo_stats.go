package sqlite

// 社区统计看板的只读实现（v0.7.1 / P4.5-M1 / ADR-064）。
// 端口 forum/user 侧定义，实现在本包：application 不 import database/sql（ADR-001）。
// 全部查询走读池，不占用唯一写连接；峰值写入口只有 UpsertDailyPeak（cmd 采样器调）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
)

// 编译期确认端口契约。
var _ appuser.StatsReader = (*StatsStore)(nil)

type StatsStore struct {
	pools  *Pools
	writer *Writer
}

func NewStatsStore(pools *Pools, writer *Writer) *StatsStore {
	return &StatsStore{pools: pools, writer: writer}
}

// UserCount 是累计注册用户数：只算 status='active'。
// pending（邮箱注册未完成）、deleting/deleted（注销中/已注销）都不计——
// 否则会注销一次涨一次，把「注册数」变成「注册过的账号数」。
func (s *StatsStore) UserCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE status = 'active'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计注册用户数: %w", err)
	}
	return n, nil
}

// DailyLogins 是某本地日期的去重登录人数。daily_checkins 的 UNIQUE(user_id, local_date)
// 保证「一人一天一行」，COUNT 即去重人数，无需 DISTINCT。
func (s *StatsStore) DailyLogins(ctx context.Context, localDate string) (int64, error) {
	var n int64
	if err := s.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM daily_checkins WHERE local_date = ?`, localDate).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计当日登录人数: %w", err)
	}
	return n, nil
}

// DailyPeakOnline 读某日峰值。无行 = 当日无观测（进程没运行过），返回 observed=false，
// 与「观测到的峰值就是 0」严格区分——这是 ADR-064 的诚实口径。
func (s *StatsStore) DailyPeakOnline(ctx context.Context, localDate string) (int, bool, error) {
	var peak int
	err := s.pools.QueryRowContext(ctx,
		`SELECT peak_online FROM stats_daily WHERE local_date = ?`, localDate).Scan(&peak)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("统计当日峰值在线: %w", err)
	}
	return peak, true, nil
}

// PeakOnlineAllTime 读历史最高在线。空表时 MAX 返回 NULL，用 COALESCE 归一到 0，
// 并由 observed 区分「从未观测」与「峰值为 0」。
func (s *StatsStore) PeakOnlineAllTime(ctx context.Context) (int, bool, error) {
	var peak int
	var seen int
	if err := s.pools.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(peak_online), 0), COUNT(*) FROM stats_daily`).Scan(&peak, &seen); err != nil {
		return 0, false, fmt.Errorf("统计历史最高在线: %w", err)
	}
	return peak, seen > 0, nil
}

// TotalLogins 是累计登录人次：只数 login_success（v0.7.1 起累积）。
// 走 idx_auth_events_event（迁移 0011），不按 created_at 过滤——历史成功登录
// 早于事件上线，表中本来就没有那些行，这是「起点」而非「漏数」。
func (s *StatsStore) TotalLogins(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_events WHERE event = 'login_success'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计累计登录人次: %w", err)
	}
	return n, nil
}

// UpsertDailyPeak 写入某日本地峰值：同日只升不降（MAX），无行则插入。
//
// 语义要点（ADR-064）：
//   - 单条幂等 upsert：参数里没有「旧值」，并发两个进程写同一天也只会取大者，
//     不会因为先写 0 再写 7 而反复重置（跨天不重置也靠这条 MAX）；
//   - 写失败由调用方（cmd 采样器）记日志并跳过本轮，**不重试、不堆积**——
//     峰值只增不减，下一轮还会带着更大的值再来。
func (s *StatsStore) UpsertDailyPeak(ctx context.Context, localDate string, peak int, seenAt time.Time) error {
	return s.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO stats_daily (local_date, peak_online, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(local_date) DO UPDATE SET
				peak_online = MAX(stats_daily.peak_online, excluded.peak_online),
				-- 只在新值更大时推进 updated_at：否则「每 30s 写一次同值」
				-- 会让 updated_at 失去「最后一次真的变高」的含义（v3 注释口径）。
				updated_at  = CASE WHEN excluded.peak_online > stats_daily.peak_online
				                   THEN excluded.updated_at ELSE stats_daily.updated_at END`,
			localDate, peak, seenAt.UTC().Unix())
		if err != nil {
			return fmt.Errorf("写入当日峰值在线: %w", err)
		}
		return nil
	})
}
