package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// deviceAuthCleanupInterval 是过期设备码的清理周期。启动时先跑一次，
// 之后每小时一次；ticker 由进程持有（application / 仓储层不起 ticker，P0-16 守卫）。
const deviceAuthCleanupInterval = time.Hour

// startDeviceAuthCleanup 启动过期设备码清理循环，返回 done 供停机时等待。
//
// 停机顺序要求：调用方必须在 writer.Stop() **之前**等待 done 关闭，
// 否则 ticker goroutine 可能正卡在 Writer.Submit 上，写队列被关掉后拿到
// ErrShuttingDown（虽不影响数据，但会留下误导性的运维日志）。
func startDeviceAuthCleanup(ctx context.Context, logger *slog.Logger, m *sqlite.Maintenance, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if interval <= 0 {
			interval = deviceAuthCleanupInterval
		}
		run := func() {
			n, err := m.CleanupExpiredDeviceAuth(ctx, time.Now().UTC())
			switch {
			case err != nil:
				logger.Warn("过期设备码清理失败", "err", err)
			case n > 0:
				logger.Info("过期设备码已清理", "count", n)
			}
		}
		// 启动即跑一次：把上次进程运行期间积压的终态行清掉。
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return done
}
