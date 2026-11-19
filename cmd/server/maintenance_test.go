package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func insertExpiredAuthRow(t *testing.T, writer *sqlite.Writer, code string) {
	t.Helper()
	ctx := context.Background()
	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO device_auth_states
				(provider, device_code, user_code, polling_interval, expires_at, status, created_at)
			VALUES ('github', ?, ?, 5, ?, 'denied', ?)`,
			code, "USER-"+code, time.Now().Add(-time.Minute).Unix(), time.Now().Unix())
		return err
	})
	if err != nil {
		t.Fatalf("插入过期状态: %v", err)
	}
}

func authRowExists(t *testing.T, pools *sqlite.Pools, code string) bool {
	t.Helper()
	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM device_auth_states WHERE device_code = ?`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// 启动即跑一次 + 之后按周期跑；ctx 取消后循环退出，done 关闭。
func TestDeviceAuthCleanupRunsAtStartupAndPeriodically(t *testing.T) {
	ctx, _, _, pools, writer := setupMVPRuntime(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 启动前先放一条过期行，验证「启动即跑一次」。
	insertExpiredAuthRow(t, writer, "startup-stale")
	runCtx, cancel := context.WithCancel(ctx)
	done := startDeviceAuthCleanup(runCtx, logger, sqlite.NewMaintenance(writer), 25*time.Millisecond)
	waitUntil(t, "启动清理", func() bool { return !authRowExists(t, pools, "startup-stale") })

	// 新产生一条过期行，等待下一个周期清理。
	insertExpiredAuthRow(t, writer, "tick-stale")
	waitUntil(t, "周期清理", func() bool { return !authRowExists(t, pools, "tick-stale") })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后清理循环未退出")
	}
}
