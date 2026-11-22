package main

// 当日峰值在线的采样器（v0.7.1 / P4.5-M1 / ADR-064）。
//
// 为什么节奏在 cmd 层：application 层不起 goroutine / ticker（P0-16 archtest 守卫），
// presence 包保持纯内存、不 import SQL。这里只读 presence 的两个整数、写一行 upsert，
// 与审计 1s ticker 是同一种「cmd 持有节奏」的既有模式。
//
// 诚实口径：采样必然漏掉采样间隔内的瞬时尖峰，所以对外文案是「今日在线峰值」
// 而不是「今日历史时刻的准确最大值」，并在 ADR-064 显式记录这一边界。

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// statsSampleInterval 是峰值在线的采样周期。
// 30s 是「够细（十余人量级的论坛不会漏掉整段在线期）」与「写频够低
// （每小时最多 120 次单行 upsert）」的折中，见 ADR-064。
const statsSampleInterval = 30 * time.Second

// presenceSnapshot 是采样器所需的最小在线视图（presence.Registry 满足）。
// **只取 Online()**：刻意不读 presence.Peak()——那是进程生命周期最大值、永不重置，
// 昨日 50 人同时在线会让今天首个 tick 把 50 写进今日行，且每天都重犯（QA P0-8）。
// 采样器自持「当日观测峰值」，跨天归零；瞬时尖峰漏采是已接受的采样误差（见文件头）。
type presenceSnapshot interface {
	Online() int
}

// dailyPeakWriter 是峰值写端口（sqlite.StatsStore 满足）；端口定义在使用方。
type dailyPeakWriter interface {
	UpsertDailyPeak(ctx context.Context, localDate string, peak int, seenAt time.Time) error
}

// peakSampler 采样「当日峰值在线」并写入 stats_daily。
//
// 内部状态只有三个字段，负责两件事：
//  1. **首行必写**：当天第一次采样无论峰值是几都要写一行——0 行也是观测行。
//     没有这行，「进程跑了一整天但没人来」会被显示成「没有观测」（--），
//     与事实正好相反。
//  2. **只写变高**：峰值没变高就不写，把写频从「每 30s 一次」降到
//     「每次创新高一次」；跨天不做收尾写（上一日的行早已单调写入）。
type peakSampler struct {
	logger   *slog.Logger
	presence presenceSnapshot
	store    dailyPeakWriter
	loc      *time.Location
	now      func() time.Time

	day         string // 当前观测日（本地日期）
	dayPeak     int    // 当日自持观测峰值：跨天归零，绝不继承进程峰值（仅 tick goroutine 读写）
	rowWritten  bool   // 当日的行是否已成功写入
	writtenPeak int    // 已写入的峰值，用于「只写变高」判定
	warned      bool   // 当日是否已 Warn 过写入失败（防日志刷屏）
}

func newPeakSampler(logger *slog.Logger, presence presenceSnapshot, store dailyPeakWriter, loc *time.Location, now func() time.Time) *peakSampler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if loc == nil {
		loc = time.UTC
	}
	if now == nil {
		now = time.Now
	}
	return &peakSampler{logger: logger, presence: presence, store: store, loc: loc, now: now}
}

// tick 执行一次采样。失败只跳过本轮：状态不推进，下一轮带着更大的值重试。
func (s *peakSampler) tick(ctx context.Context) {
	if s.presence == nil || s.store == nil {
		return
	}
	now := s.now().In(s.loc)
	day := now.Format("2006-01-02")
	if day != s.day {
		// 跨天：新的一天还没有观测行，当日峰值必须归零。
		// **刻意不补写昨日**：上一日的行由当日各次 tick 单调写入，00:00 之前的
		// 最后一个采样峰值不会因为跨天而丢失；而「补齐到午夜」既需要额外写，
		// 也无法观测没跑采样的时段。
		s.day, s.rowWritten, s.writtenPeak, s.warned = day, false, 0, false
		s.dayPeak = 0
	}

	// 当日峰值 = max(自持当日峰值, 当前在线)。不读 presence.Peak()：
	// 进程峰值不按天重置，会把历史最大值永久写进今天（QA P0-8 反例：
	// 昨日 50 / 今日 3 → 今日必须写 3，不能写 50）。
	if online := s.presence.Online(); online > s.dayPeak {
		s.dayPeak = online
	}
	peak := s.dayPeak
	// 当日已有行且峰值没变高：本轮不写。把写频从「每 30s 一次」降到
	// 「每次创新高一次」，把写队列留给发帖/回复等业务写（QA P1）。
	if s.rowWritten && peak <= s.writtenPeak {
		return
	}
	firstOfDay := !s.rowWritten
	if err := s.store.UpsertDailyPeak(ctx, day, peak, now); err != nil {
		// 队列满属预期背压（R3），不是故障；但持续满队列会每 30s 刷一条 Warn
		// （每小时 120 条）。当日首次失败报 Warn，其后降级 Debug（QA P1）。
		if s.warned {
			s.logger.Debug("峰值在线采样写入仍失败（跳过本轮）", "date", day, "peak", peak, "error", err)
		} else {
			s.logger.Warn("峰值在线采样写入失败（跳过本轮，下轮重试）", "date", day, "peak", peak, "error", err)
			s.warned = true
		}
		return
	}
	if s.warned {
		// 从失败恢复也要可观测：只报一次，随后清标志。
		s.logger.Info("峰值在线采样写入已恢复", "date", day, "peak", peak)
		s.warned = false
	}
	s.rowWritten, s.writtenPeak = true, peak
	if firstOfDay {
		// 每天只报一次「观测开始」，避免每 30s 一条 Info 把日志刷满。
		s.logger.Info("峰值在线观测开始", "date", day, "peak", peak)
	}
}

// runPeakSampler 启动采样 ticker，返回的 channel 在 goroutine 退出时关闭。
// 调用方（main）必须在关写队列之前等它退出（defer LIFO 已保证）。
func runPeakSampler(ctx context.Context, logger *slog.Logger,
	presence presenceSnapshot, store dailyPeakWriter,
	loc *time.Location, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	s := newPeakSampler(logger, presence, store, loc, nil)
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// 启动即采一次：进程可能正好在新一天开始时启动，等满一个周期会漏掉首段观测。
		s.tick(ctx)
		for {
			select {
			case <-ctx.Done():
				// 停机前补最后一次采样：此刻 presence 里仍是最后一批连接，
				// 不补就会把「最后一次心跳到进程退出」这段的峰值丢掉（QA P1-3）。
				// 用独立 ctx（原始 ctx 已取消，写队列尚未关闭），2s 超时保证不卡停机。
				stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				s.tick(stopCtx)
				cancel()
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
	return done
}
