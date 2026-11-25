package main

// R-15 / ADR-079 管理员白名单热重载：SIGHUP 触发重新解析 .env/env 中的
// MOTD_ADMIN_FINGERPRINTS，成功后原子换入 forum.Service（SetAdminWhitelist）。
//
// 失败关闭：解析失败或非法指纹时保留旧白名单并告警，绝不 half-apply、
// 绝不把管理员全部清空。CanAdmin/requireAdmin 单点不变（ADR-049）。
// 与 signal.NotifyContext 的 SIGINT/SIGTERM 分开：SIGHUP 不取消主 context。

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jeeinn/motd/internal/application/forum"
)

// startAdminWhitelistReload 起一个 goroutine 监听 SIGHUP，ctx 取消时退出。
// reloadFn 注入便于单测（生产传 config.ReloadAdminFingerprints）。
func startAdminWhitelistReload(ctx context.Context, svc *forum.Service, logger reloadLogger, reloadFn func() (map[string]struct{}, error)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		// signal.Stop 必须随 goroutine 生命周期，不能 defer 在外层函数：
		// 外层启动 goroutine 后立即返回，那样会立刻反注册、goroutine 永远收不到信号。
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				next, err := reloadFn()
				if err != nil {
					logger.Warn("管理员白名单重载失败，保留旧名单", "err", err)
					continue
				}
				if err := svc.SetAdminWhitelist(next); err != nil {
					logger.Warn("管理员白名单重载被拒，保留旧名单", "err", err)
					continue
				}
				logger.Info("管理员白名单已重载", "count", len(next))
			}
		}
	}()
}

// reloadLogger 只用到两个级别，避免绑定具体日志实现。
type reloadLogger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}
