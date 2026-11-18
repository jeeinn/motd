// Command server 是 motd 的单进程入口：Go 单体 + SQLite + SSH TUI。
//
// 当前阶段：骨架。启动时完成配置加载、日志、SQLite 打开（双池）、迁移执行，
// 并提供运维探针 /healthz。SSH 入口、认证与论坛用例按 docs/P0-PLAN.md 逐项接入。
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"motd/internal/config"
	"motd/internal/infrastructure/sqlite"
)

// version 由构建时注入（-ldflags "-X main.version=..."）。
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cfg.Ensure(); err != nil {
		return err
	}

	pools, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := pools.Close(); cerr != nil {
			logger.Error("关闭数据库失败", "err", cerr)
		}
	}()

	applied, err := sqlite.Migrate(ctx, pools.Writer)
	if err != nil {
		return err
	}
	logger.Info("数据库就绪", "path", cfg.DBPath, "migrations_applied", len(applied), "applied_versions", applied)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           probeMux(pools.Reader),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("探针监听", "addr", cfg.HTTPAddr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("探针服务: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("收到退出信号，开始停服")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("停服: %w", err)
	}
	logger.Info("已退出")
	return nil
}

// probeMux 只提供运维探针，不承担业务流量。
// 业务端点（OAuth 回调、Device Flow）在 P0 认证阶段按 §4.8 接入。
func probeMux(reader *sql.DB) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := reader.QueryRowContext(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "ok %s\n", version)
	})
	return mux
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
