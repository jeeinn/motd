// Command server 是 motd 的单进程入口：Go 单体 + SQLite + SSH TUI。
//
// 启动时完成配置加载、日志、SQLite 打开（双池）、迁移执行、写入队列、
// 论坛用例与插件事件链装配、身份解析、GitHub Device Flow 客户端、设备码状态机、
// 账号登录事务、公钥绑定、登录交互、终端页面与 SSH 入口，并提供 /healthz。
// P0-19 的登录交互仍是进程内最小交互层（docs/DEVIATIONS.md R18）；MVP 收尾已把
// P0-24/P0-25 的终端页面接进同一分发链，业务规则仍全部来自 application/forum。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/config"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
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

	applied, err := sqlite.Migrate(ctx, pools)
	if err != nil {
		return err
	}
	logger.Info("数据库就绪", "path", cfg.DBPath, "migrations_applied", len(applied), "applied_versions", applied)

	writer := pools.NewWriter()
	writer.Start()
	defer writer.Stop()
	logger.Info("写入队列就绪", "capacity", writer.Cap(), "timeout", "3s")

	// 装配论坛用例 + 插件事件链，并读一次 DB 做启动自检：
	// 「迁移 → 种子 → application 端口 → 读池」。/healthz 仍是纯运维探针（ADR-006）。
	rt, err := newForumRuntime(pools, writer, func(e event.DomainEvent, err error) {
		logger.Warn("领域事件投递失败（事务已提交，不回滚）",
			"event_id", e.ID, "type", e.Type, "err", err)
	})
	if err != nil {
		return fmt.Errorf("装配论坛运行时失败: %w", err)
	}
	// R12 崩溃恢复：上次进程若在领取后、回写前被 kill-9，事件会卡在 dispatching。
	// 此刻还没有在途投递，先把它们退回 pending，再重投所有残留事件。
	requeued, redelivered, redeliveryFailed, err := rt.Outbox.RecoverPending(ctx)
	if err != nil {
		return fmt.Errorf("outbox 崩溃恢复失败: %w", err)
	}
	logger.Info("outbox 恢复就绪", "requeued", requeued,
		"redelivered", redelivered, "failed", redeliveryFailed)

	nodes, err := rt.Service.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("启动自检：读取版块失败: %w", err)
	}
	logger.Info("论坛用例就绪", "nodes", len(nodes),
		"plugins", len(rt.Plugins),
		"post_created_subscribers", rt.Bus.SubscriberCount(event.EventPostCreated))

	// 心跳超时清理只在内存里跑：每 10s reap 一次超过 20s 未心跳的会话。
	// 不写库、不占用写队列（§2.6）；ctx 取消后随进程一起退出。
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if reaped := rt.Presence.Reap(); len(reaped) > 0 {
					logger.Info("清理超时会话", "count", len(reaped))
				}
			}
		}
	}()

	// SSH 入口（Q4：默认 2222）。P0-14 起用查 ssh_keys 的真实身份解析器；
	// transport/ssh 只依赖 application/auth.Resolver 端口，M2 后续换实现不改协议层（ADR-017/020）。
	resolver := auth.NewResolver(sqlite.NewSSHKeyLookup(pools))
	logger.Info("身份解析就绪", "mode", "ssh_keys")

	// GitHub Device Flow 客户端（P0-15）。缺 client_id 时客户端仍可构造，
	// 但两个需要 client_id 的 POST 端点会返回 auth.ErrClientIDMissing；
	// 日志只报「是否已配置」，绝不打印凭据值（OPS §3）。
	deviceFlow := newDeviceFlowClient(cfg)
	logger.Info("GitHub Device Flow 客户端就绪",
		"client_id_configured", cfg.GitHubClientID != "",
		"scope", auth.DeviceScope)

	// Device Flow 状态机（P0-16）：状态持久化在 device_auth_states（0001_init.sql
	// 已建表，无新迁移），读走读池、写走写入队列。状态机不 sleep、不起 ticker，
	// 轮询节奏由 P0-19 的 SSH 会话循环持有；此处只装配，/login 交互在 P0-19 接入。
	deviceAuth := newDeviceFlowService(pools, writer, deviceFlow)
	logger.Info("设备码状态机就绪", "store", "device_auth_states", "modes", "pending|authorized|denied|expired")

	// 账号登录事务（P0-17）：§4.3 三步判定与 users+identities 原子创建都在
	// 同一个写事务内；users / identities 已在 0001_init.sql 建好，无新迁移。
	// GetUser / GetEmails 仍在写事务之外（ADR-022 边界），/login 交互属 P0-19。
	login := newLoginService(writer, deviceFlow)
	logger.Info("账号登录事务就绪", "store", "users+identities", "provider", auth.ProviderGitHub)

	// P0-18 公钥绑定：Revoker 是 SSH Server（它握有全部在线 Session）。
	// Server 依赖 Handler、Handler 又要用 KeyBindingService，因此先建 handler、
	// 再建 Server，最后用 setter 把用例回填，打破装配循环。
	handler := newInputHandler().withDeviceFlow(deviceFlow).withDeviceAuth(deviceAuth).withLogin(login).
		withForum(rt.Service, sqlite.NewUserStatusReader(pools), rt.Presence)
	sshSrv, err := sshd.New(sshd.Config{
		Addr:        cfg.SSHAddr,
		DataDir:     cfg.DataDir,
		MaxConns:    cfg.SSHMaxConns,
		IdleTimeout: cfg.SSHIdleTimeout,
		Resolver:    resolver,
		Presence:    rt.Presence,
		Handler:     handler,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("初始化 SSH 服务失败: %w", err)
	}
	keyBinding := newKeyBindingService(pools, writer, sshSrv)
	handler.withKeyBinding(keyBinding)
	logger.Info("SSH host key 就绪", "fingerprint", sshSrv.HostKeyFingerprint())
	logger.Info("公钥绑定就绪", "store", "ssh_keys", "revoker", "ssh_sessions", "modes", "keys|key add|key rm")
	// P0-19：/login github 走进程内 Device Flow（Q3，不开 HTTP 端点）；
	// 呈现层是固定文本的最小交互层，P0-24 落地时替换（DEVIATIONS.md R18/R20）。
	// 终端页面已落地，但 /login 的呈现仍是 P0-19 的固定文本（DEVIATIONS.md R18/R20）。
	// 因此这里不再写 replace_at=P0-24，避免把一个已完成的编号当成未完成待办。
	logger.Info("登录交互就绪", "mode", "minimal_in_process", "presentation", "fixed_text",
		"provider", "github", "pending", "login_view_replacement")
	logger.Info("终端页面就绪", "mode", "viewmodel_terminal_renderer",
		"screens", "nodes|threads|detail|composer", "renderer", "safe_sgr_only")

	sshErr := make(chan error, 1)
	go func() {
		// 不能在 Serve 之前调 Addr()：Addr 阻塞等 ready，而 ready 由 Serve 关闭，
		// 同 goroutine 里先 Addr 后 Serve 会自锁（从零启动实测发现）。
		logger.Info("SSH 监听", "addr", cfg.SSHAddr, "max_conns", cfg.SSHMaxConns)
		sshErr <- sshSrv.Serve(ctx)
	}()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           probeMux(pools, writer, rt.Presence),
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
	case err := <-sshErr:
		if err != nil {
			return fmt.Errorf("SSH 服务: %w", err)
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
	// SSH Serve 在 ctx 取消后自行退出；等它返回，确保连接 goroutine 全部回收。
	select {
	case err := <-sshErr:
		if err != nil {
			return fmt.Errorf("SSH 服务退出: %w", err)
		}
	case <-shutdownCtx.Done():
		return fmt.Errorf("等待 SSH 服务退出超时")
	}
	logger.Info("已退出")
	return nil
}

// probeMux 只提供运维探针，不承担业务流量。
// Q3 已拍板：P0 的 Device Flow 由 SSH 会话在进程内调用 application 用例，
// 不开任何业务 HTTP 端点；HTTP 端点留到 P1 浏览器接入时再评估。
func probeMux(pools *sqlite.Pools, writer *sqlite.Writer, reg *presence.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pools.Ping(ctx); err != nil {
			http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "ok %s queue=%d/%d online=%d active=%d commits=%d\n",
			version, writer.Depth(), writer.Cap(), reg.Online(), reg.Active(), writer.Commits())
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
