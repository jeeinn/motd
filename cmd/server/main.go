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
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/config"
	"github.com/jeeinn/motd/internal/domain/event"
	emailinfra "github.com/jeeinn/motd/internal/infrastructure/email"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	"github.com/jeeinn/motd/internal/plugin/registry"
	"github.com/jeeinn/motd/internal/presentation/terminal"
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
	// .env 只填空缺，必须早于 config.Load()；返回值只有文件路径，不含任何键值。
	dotenvSource, err := config.LoadDotEnv()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)
	// 只打来源路径或 env_only，绝不打印键、值或授权码（ADR-021）。
	if dotenvSource == "" {
		logger.Info("配置来源 env_only")
	} else {
		logger.Info("配置来源", "file", dotenvSource)
	}

	// 展示时区：启动期一次性注入 presentation 层，之后只读。
	// 论坛列表/详情与 /keys 公钥详情共用这份解析结果，不会两处各读各的。
	// 非法值已在 config.Load 阶段失败关闭，进不到这里。
	terminal.SetDisplayLocation(cfg.DisplayLocation)
	logger.Info("展示时区就绪", "timezone", cfg.DisplayTimezone)

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

	// P1-M4 数据运维：device_auth_states 只清「自身已过期」的行（pending 未过期
	// 绝不动，保护在途登录）。启动跑一次 + 每小时一次；停机时先等循环退出，
	// 再停写队列（defer LIFO），避免 ticker 正卡在 Submit 上被关队列。
	cleanupDone := startDeviceAuthCleanup(ctx, logger, sqlite.NewMaintenance(writer), deviceAuthCleanupInterval)
	defer func() { <-cleanupDone }()
	logger.Info("设备码清理就绪", "interval", deviceAuthCleanupInterval.String(), "scope", "expired_only")

	// P1-M2 认证安全：限流器与审计器都是纯内存组件；落库端口由 SQLite 实现。
	// 审计落库周期由本进程持有（application 层不起 ticker，P0-16 archtest 守卫）。
	limiter := auth.NewRateLimiter(auth.WithRateLimitConfig(auth.RateLimitConfig{
		MaxAttempts: cfg.AuthLimitMaxFailures,
		Window:      cfg.AuthLimitWindow,
		Lockout:     cfg.AuthLimitLockout,
		MaxKeys:     cfg.AuthLimitMaxKeys,
	}))
	auditLogger := auth.NewAuditLogger(sqlite.NewAuthEventStore(writer),
		auth.WithAuditCapacity(cfg.AuditCapacity),
		auth.WithAuditFlushInterval(cfg.AuditFlushInterval))
	auditDone := make(chan struct{})
	go func() {
		defer close(auditDone)
		ticker := time.NewTicker(auditLogger.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				auditLogger.Tick(ctx)
			}
		}
	}()
	// 停机顺序：先等 ticker goroutine 退出，再排空最后一批，最后才停写队列。
	defer func() {
		<-auditDone
		auditLogger.Stop()
	}()
	gate := auth.NewSecurityGate(limiter, auditLogger, auth.ProviderGitHub)
	logger.Info("认证安全就绪", "audit_capacity", cfg.AuditCapacity,
		"audit_flush", cfg.AuditFlushInterval.String(),
		"limit_max_failures", cfg.AuthLimitMaxFailures, "limit_window", cfg.AuthLimitWindow.String())

	// 装配论坛用例 + 插件事件链，并读一次 DB 做启动自检：
	// 「迁移 → 种子 → application 端口 → 读池」。/healthz 仍是纯运维探针（ADR-006）。
	rt, err := newForumRuntime(pools, writer, func(e event.DomainEvent, err error) {
		logger.Warn("领域事件投递失败（事务已提交，不回滚）",
			"event_id", e.ID, "type", e.Type, "err", err)
	}, cfg.AdminFingerprints, auditLogger)
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
	// P1-M4：认证命中已绑定钥匙后，异步节流更新 ssh_keys.last_used_at。
	// 装饰器只在解析成功且得到用户身份时打标记，节流 / 写库全在 sqlite 实现里。
	usageRecorder := sqlite.NewKeyUsageRecorder(sqlite.NewKeyBindingRepos(pools, writer), sqlite.DefaultKeyUsageInterval)
	defer usageRecorder.Wait() // 先等在途使用记录写完，再停写队列（defer LIFO）
	resolver := auth.NewUsageRecordingResolver(auth.NewResolver(sqlite.NewSSHKeyLookup(pools)), usageRecorder)
	logger.Info("身份解析就绪", "mode", "ssh_keys", "last_used_interval", sqlite.DefaultKeyUsageInterval.String())

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
	// v0.5.1 / P3-M6：把彩蛋命令并入命令注册表（插件命令带 /，需转换后合并）。
	eggReg, err := eggRegistry(registry.All())
	if err != nil {
		return fmt.Errorf("装配彩蛋命令: %w", err)
	}
	// v0.6.0：签到 service（登录成功后的副作用）。
	checkInStore := sqlite.NewCheckInStore(pools, writer)
	checkInSvc := appuser.NewCheckInService(checkInStore, rt.Gate, time.Now, cfg.DisplayLocation)
	onLogin := func(ctx context.Context, userID int64) {
		res, err := checkInSvc.CheckIn(ctx, userID)
		if err != nil {
			logger.Warn("签到写入失败", "user_id", userID, "error", err)
			return
		}
		if res.Unlocked {
			logger.Info("用户达成连续签到成就", "user_id", userID, "streak", res.Streak)
		}
	}
	// v0.6.0：Konami 码解锁 /dev/null（按用户持久化，服务端固定 slug 不接受用户输入）。
	onUnlock := func(ctx context.Context, userID int64) error {
		return checkInStore.UnlockSlug(ctx, userID, "/dev/null", time.Now())
	}
	handler := newInputHandler().withDeviceFlow(deviceFlow).withDeviceAuth(deviceAuth).withLogin(login).
		withForum(rt.Service, sqlite.NewUserStatusReader(pools), rt.Presence).
		withSecurity(gate).withCommands(eggReg).withLoginHook(onLogin).
		withUnlockHooks(onUnlock, checkInStore.UnlockedNodes)
	sshSrv, err := sshd.New(sshd.Config{
		Addr:     cfg.SSHAddr,
		DataDir:  cfg.DataDir,
		MaxConns: cfg.SSHMaxConns,
		Resolver: resolver,
		Presence: rt.Presence,
		Handler:  handler,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("初始化 SSH 服务失败: %w", err)
	}
	keyBinding := newKeyBindingService(pools, writer, sshSrv, gate)
	handler.withKeyBinding(keyBinding)
	// 注销边界复用同一 SSH Server 作 UserSessionRevoker：按 userID 关掉全部在线会话。
	logoutSvc := auth.NewLogoutService(sshSrv, gate)
	handler.withLogout(logoutSvc)
	logger.Info("SSH host key 就绪", "fingerprint", sshSrv.HostKeyFingerprint())
	logger.Info("公钥绑定就绪", "store", "ssh_keys", "revoker", "ssh_sessions", "modes", "keys|key add|key rm")
	logger.Info("注销边界就绪", "command", "/logout", "revoker", "ssh_sessions_by_user")

	// P1-M5 邮箱认证：只有显式 MOTD_EMAIL_ENABLED=true 才构造投递通道。
	// 日志只报「是否启用 / 主机 / 端口」，绝不打印账号与授权码（OPS §3）。
	if cfg.Email.Enabled {
		sender := emailinfra.NewSMTPSender(emailinfra.Config{
			Host: cfg.Email.SMTPHost, Port: cfg.Email.SMTPPort,
			User: cfg.Email.SMTPUser, Pass: cfg.Email.SMTPPass, From: cfg.Email.From,
		})
		defer sender.Close()
		emailSvc := auth.NewEmailAuthService(
			sqlite.NewEmailRepos(pools, writer), sender, sshSrv,
			auth.WithEmailAudit(auditLogger),
		)
		handler.withEmail(emailSvc)
		logger.Info("邮箱认证就绪",
			"enabled", true, "smtp_host", cfg.Email.SMTPHost, "smtp_port", cfg.Email.SMTPPort,
			"commands", "register|verify|resend|recover|login email")
	} else {
		logger.Info("邮箱认证未启用", "enabled", false)
	}

	// P0-19：/login github 走进程内 Device Flow（Q3，不开 HTTP 端点）。
	// P1-M2：发起屏已替换为论坛同款 Screen（ScreenLogin + 共享 Renderer +
	// 同源状态栏）；轮询中的 waiting/retry/终态仍是逐行固定文本（进度反馈，
	// 不做整屏重绘）。device_code / access_token 始终不进 view（ADR-021）。
	logger.Info("登录交互就绪", "mode", "minimal_in_process", "presentation", "forum_screen",
		"progress", "fixed_text", "provider", "github")
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
		Handler:           probeMux(pools, writer, rt.Presence, auditLogger, limiter, sshSrv.Bandwidth()),
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
// probeMux 只提供运维探针，不承担业务流量。审计器 / 限流器可传 nil，
// 此时 /healthz 只输出原有指标，便于不装配安全组件的本机开发场景。
// bw 是可省略的进程级带宽计数器；不传时用 ssh 包的兜底计数器，
// 保持旧的五参数调用方源码兼容。
func probeMux(pools *sqlite.Pools, writer *sqlite.Writer, reg *presence.Registry,
	audit *auth.AuditLogger, limiter *auth.RateLimiter, bw ...*sshd.Bandwidth) http.Handler {
	bandwidth := sshd.ProcessBandwidth()
	if len(bw) > 0 && bw[0] != nil {
		bandwidth = bw[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pools.Ping(ctx); err != nil {
			http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// tx_bytes / rx_bytes 是进程级纯内存计数：与 commits 无关，
		// 字节累加不写库、不进领域模型、不在终端呈现暴露。
		_, _ = fmt.Fprintf(w, "ok %s queue=%d/%d online=%d active=%d commits=%d tx_bytes=%d rx_bytes=%d",
			version, writer.Depth(), writer.Cap(), reg.Online(), reg.Active(), writer.Commits(),
			bandwidth.Tx(), bandwidth.Rx())
		if audit != nil {
			depth, dropped, flushed, failed := audit.Stats()
			_, _ = fmt.Fprintf(w, " audit_depth=%d audit_cap=%d audit_dropped=%d audit_flushed=%d audit_failed=%d",
				depth, audit.Capacity(), dropped, flushed, failed)
		}
		if limiter != nil {
			_, _ = fmt.Fprintf(w, " limit_keys=%d", limiter.Keys())
		}
		_, _ = fmt.Fprint(w, "\n")
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
