//go:build linux

package main

// 回归测试：startAdminWhitelistReload 必须真的接到 SIGHUP。
// 曾有 P0 bug：signal.Stop 误 defer 在外层函数，启动 goroutine 后外层立即
// 返回 → 立刻反注册 → goroutine 永远收不到信号，热重载整体失效。
// 纯测 SetAdminWhitelist 的用例抓不到（绕过 signal.Notify 路径），
// 这里发真实进程信号验证接线。CI 在 ubuntu-latest 跑，本文件仅 Linux 编译。

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
)

type reloadTestLogger struct{}

func (reloadTestLogger) Info(string, ...any) {}
func (reloadTestLogger) Warn(string, ...any) {}

func TestStartAdminWhitelistReloadReceivesSIGHUP(t *testing.T) {
	svc := forum.NewService(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := make(chan struct{}, 4)
	fn := func() (map[string]struct{}, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return map[string]struct{}{}, nil
	}
	startAdminWhitelistReload(ctx, svc, reloadTestLogger{}, fn)
	// 给 goroutine 时间完成 signal.Notify 注册。
	time.Sleep(100 * time.Millisecond)

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("发送 SIGHUP: %v", err)
	}
	select {
	case <-called:
		// PASS：回调被真实信号触发。
	case <-time.After(2 * time.Second):
		t.Fatal("SIGHUP 未触发重载回调（signal.Stop 可能被提前调用）")
	}
}
