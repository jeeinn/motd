package user

// 社区统计看板（v0.7.1 / P4.5-M1 / ADR-064）。
//
// 分层边界：
//   - 本文件只定义**读端口**与口径组装（日期边界、观测态归一），不碰 SQL、不碰终端；
//   - 实现在 infrastructure/sqlite（repo_stats.go），端口定义在使用方（ADR-001）；
//   - 文案由入口层（cmd）用固定串拼装，本层不产出任何可呈现文本。
//
// 诚实口径（用户 2026-11-22 拍板）：每一项都必须能追到数据源，且必须能表达
// 「这一项从什么时候开始有数据」。零回填、零臆造——没有观测就返回 observed=false，
// 由入口层显示 "--"，绝不把「没有数据」渲染成 0。

import (
	"context"
	"fmt"
	"time"
)

// StatsReader 是统计项的只读端口。
//
// observed 的语义（ADR-064）：
//   - 峰值类指标返回 observed=false 表示「该日/全历史都没有观测行」——
//     进程当天没运行过，或 stats_daily 尚未落第一行；
//   - 计数类指标（登录人数 / 注册数 / 累计登录）没有 observed 概念：
//     它们的表从建库起就在累积，COUNT 为 0 就是真的 0。
type StatsReader interface {
	// UserCount 是累计注册用户数：users 表 COUNT(status='active')。
	// 不含 pending（未完成注册）与 deleting/deleted（防注销虚增）。
	UserCount(ctx context.Context) (int64, error)
	// DailyLogins 是某本地日期的**去重登录人数**：daily_checkins COUNT(local_date=?)。
	DailyLogins(ctx context.Context, localDate string) (int64, error)
	// DailyPeakOnline 是某本地日期的峰值在线；无观测行时 observed=false。
	DailyPeakOnline(ctx context.Context, localDate string) (peak int, observed bool, err error)
	// PeakOnlineAllTime 是跨进程历史最高在线：MAX(peak_online)；无任何行时 observed=false。
	PeakOnlineAllTime(ctx context.Context) (peak int, observed bool, err error)
	// TotalLogins 是历史累计登录**人次**：auth_events COUNT(event='login_success')。
	TotalLogins(ctx context.Context) (int64, error)
}

// StatsSnapshot 是一屏统计的全部语义值。全部是数字与观测态，不含格式化文本。
type StatsSnapshot struct {
	TodayLogins     int64
	YesterdayLogins int64

	TodayPeak         int
	TodayPeakObserved bool
	YesterdayPeak     int
	YesterdayPeakSeen bool
	AllTimePeak       int
	AllTimePeakSeen   bool

	CurrentOnline int
	CurrentActive int

	UserCount   int64
	TotalLogins int64
}

// LiveCounters 是入口层注入的实时在线数（内存值，不落库）。
//
// 刻意**不**注入采样器的内存当日峰值：那需要「读路径持有跨日状态」，
// 而它既买不到「同日不下降」（瞬时峰值仍在两次 tick 之间涨落），
// 又会在 00:00 后的第一个 tick 之前把昨日峰值当成今日（ADR-064「不做第 3 条」）。
type LiveCounters struct {
	Online int
	Active int
}

// StatsService 组装快照：负责日期边界（本地日历）与在线数的注入。
//
// 在线数由调用方传入（与 ViewContext 同口径）：application/user 不 import presence，
// 保持「入口层组装上下文、用例层只做语义换算」的既有边界。
type StatsService struct {
	reader StatsReader
	now    func() time.Time
	loc    *time.Location
}

// NewStatsService 构造统计用例。now/loc 为空时回落到 time.Now / UTC，
// 与 NewCheckInService 的零值安全口径一致。
func NewStatsService(reader StatsReader, now func() time.Time, loc *time.Location) *StatsService {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.UTC
	}
	return &StatsService{reader: reader, now: now, loc: loc}
}

// LocalDate 返回展示时区下的本地日期（YYYY-MM-DD），与 daily_checkins / stats_daily
// 的 local_date 列同一口径（ADR-046）。
func (s *StatsService) LocalDate(t time.Time) string {
	return t.In(s.loc).Format("2006-01-02")
}

// Snapshot 读取一屏统计。任一读失败即整体失败——统计屏不显示半真半假的数字，
// 也不在失败时用 0 兜底（那会把故障伪装成「没人来」）。
//
// 昨日日期用**本地日历** AddDate(0,0,-1) 计算，绝不用 now.Add(-24h)：
// MOTD_DISPLAY_TIMEZONE 可配任意时区，在 DST 切换日 24 小时不等于「昨天」，
// 症状是「昨日数字莫名等于前日」。
func (s *StatsService) Snapshot(ctx context.Context, live LiveCounters) (StatsSnapshot, error) {
	now := s.now().In(s.loc)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	snap := StatsSnapshot{CurrentOnline: live.Online, CurrentActive: live.Active}

	var err error
	if snap.TodayLogins, err = s.reader.DailyLogins(ctx, today); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计今日登录人数: %w", err)
	}
	if snap.YesterdayLogins, err = s.reader.DailyLogins(ctx, yesterday); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计昨日登录人数: %w", err)
	}
	if snap.TodayPeak, snap.TodayPeakObserved, err = s.reader.DailyPeakOnline(ctx, today); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计今日峰值在线: %w", err)
	}
	if snap.YesterdayPeak, snap.YesterdayPeakSeen, err = s.reader.DailyPeakOnline(ctx, yesterday); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计昨日峰值在线: %w", err)
	}
	if snap.AllTimePeak, snap.AllTimePeakSeen, err = s.reader.PeakOnlineAllTime(ctx); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计历史最高在线: %w", err)
	}
	if snap.UserCount, err = s.reader.UserCount(ctx); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计注册用户数: %w", err)
	}
	if snap.TotalLogins, err = s.reader.TotalLogins(ctx); err != nil {
		return StatsSnapshot{}, fmt.Errorf("统计累计登录人次: %w", err)
	}
	applyPeakInvariants(&snap, live)
	return snap, nil
}

// applyPeakInvariants 让三个峰值在**同一屏内**自洽（ADR-064 / 用户 2026-11-22 二次拍板）：
//
//	历史在线峰值 ≥ 今日在线峰值 ≥ 当前在线。
//
// 为什么必须在展示层做：采样器把值写进库有 ≤30s 的滞后，读库值渲染会出现
// 「今日峰值 3，当前在线 5」这种自相矛盾的屏（读者只会认为统计坏了）。
// 钳制只做**向上取大**，绝不下调任何一项，因此不会制造比观测更多的信息：
// 当前在线本身就是「今天的一次观测」，历史峰值不可能小于今天已观测到的峰值。
// 「昨日峰值 ≤ 历史峰值」不在这里钳制——昨日峰值若真高于历史值，那是数据异常，
// 应当被读者的眼睛发现，而不是被展示层抹平。
//
// 只做这两支是本版刻意的边界：**不保证同一天内多次渲染的今日峰值单调不降**
// （两次 tick 之间的瞬时峰值会出现再消失）。要让显示值单调，必须在渲染路径写回
// 日内高水位——为 ≤30 秒的显示抖动引入读路径写入，收益与风险不成比例。
func applyPeakInvariants(snap *StatsSnapshot, live LiveCounters) {
	if live.Online > snap.TodayPeak {
		snap.TodayPeak = live.Online
	}
	// 有任何实时/采样观测，就说明「今天有观测」——把 -- 换成数字。
	// 否则会显示「今日在线峰值 --，当前在线 5」。
	if live.Online > 0 || snap.TodayPeak > 0 {
		snap.TodayPeakObserved = true
	}
	if snap.TodayPeak > snap.AllTimePeak {
		snap.AllTimePeak = snap.TodayPeak
		snap.AllTimePeakSeen = true
	}
}
