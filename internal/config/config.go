package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // 内嵌 IANA 时区库：默认 Asia/Shanghai 在 distroless/scratch 镜像里也能解析
)

// Config 是进程启动所需的全部外部输入。
// 原则：只放运行期真正读取的字段；未落地的配置项等到对应用例实现时再加，
// 避免出现「配置存在但没人读」的假接口。
type Config struct {
	// DataDir 运行期数据根目录：SQLite 库、SSH host key、备份产物都落在这里。
	DataDir string
	// DBPath SQLite 数据库文件路径。
	DBPath string
	// HTTPAddr 运维探针监听地址（/healthz），不承担业务流量。
	HTTPAddr string
	// SSHAddr SSH 入口监听地址（Q4 已拍板：默认 :2222）。
	SSHAddr string
	// SSHMaxConns SSH 最大并发连接数（§1.2 P0 容量基线 100）。
	SSHMaxConns int
	// SSHIdleTimeout SSH 会话空闲超时（v0.4.1 起语义为"连接无心跳"，非"用户无按键"）。
	// 连接级心跳持续运行即重置该计时器：用户停止浏览但连接仍在时不会被断开。
	// 客户端静默消失由 transport 层的绝对上限（6h）兜底，不受此值影响。
	SSHIdleTimeout time.Duration
	// GitHubClientID 是 Q7 的 OAuth App client_id，只从 MOTD_GITHUB_CLIENT_ID 读取。
	// Device Flow 不需要 client_secret（§7 R1）；值不进仓库、不写 systemd unit（OPS §3）。
	GitHubClientID string
	// Email 是邮箱投递配置（P1-M5）。Enabled=false 时为零值，进程不构造 Sender。
	// 授权码只在进程内存中流转，绝不进日志与任何持久化结构。
	Email EmailConfig
	// AuthLimitMaxFailures 是同一作用域在窗口内允许的最大认证尝试/失败次数。
	AuthLimitMaxFailures int
	// AuthLimitWindow 是认证限流的滑动窗口长度。
	AuthLimitWindow time.Duration
	// AuthLimitLockout 是超限后的锁定时长；锁定期内所有认证动作直接拒绝。
	AuthLimitLockout time.Duration
	// AuthLimitMaxKeys 是内存限流表保留的最大作用域数量，防止按随机 IP 打爆内存。
	AuthLimitMaxKeys int
	// AuditCapacity 是审计环形缓冲容量；写满后认证动作 fail-closed。
	AuditCapacity int
	// AuditFlushInterval 是异步批量落库周期。
	AuditFlushInterval time.Duration
	// AdminFingerprints 是节点治理命令的公钥指纹白名单（键为服务端算出的指纹）。
	// 空 map 表示无人可管；判定统一走 application/admin.CanAdmin。
	AdminFingerprints map[string]struct{}
	// LogLevel debug / info / warn / error。
	LogLevel string
	// LogFormat text / json。
	LogFormat string
	// DisplayTimezone 是展示时区的原始配置名（IANA 名 / UTC / Local），供日志与运维核对。
	DisplayTimezone string
	// DisplayLocation 是 DisplayTimezone 在 Load 阶段解析出的 *time.Location。
	// 展示层直接复用这一份解析结果，避免每屏重复解析、重复报错。
	DisplayLocation *time.Location
	// DotEnvDisabled 表示启动时是否关闭 .env 自动加载（逃生舱）。
	// 只读展示用：真正的开关在进程入口 LoadDotEnv 里、在 Load 之前生效。
	DotEnvDisabled bool
}

// 认证安全默认值的唯一事实来源。application/auth 侧有一份等价兜底，
// 这里显式给出环境变量默认值，便于 OPS 直接抄进 systemd unit。
const (
	DefaultAuthLimitMaxFailures = 5
	DefaultAuthLimitWindow      = 5 * time.Minute
	DefaultAuthLimitLockout     = 15 * time.Minute
	DefaultAuthLimitMaxKeys     = 4096
	DefaultAuditCapacity        = 512
	DefaultAuditFlushInterval   = time.Second
	// DefaultDisplayTimezone 是中国用户默认的展示时区；只影响呈现层，
	// 存储层仍以 Unix 秒归一化（application/domain 的 .UTC() 不动）。
	DefaultDisplayTimezone = "Asia/Shanghai"
)

// Load 读取环境变量并修正默认值。
// 约定：所有变量以 MOTD_ 前缀；空字符串视为未设置，回落到默认值。
func Load() (Config, error) {
	dataDir := env("MOTD_DATA_DIR", "./data")
	cfg := Config{
		DataDir:              dataDir,
		DBPath:               env("MOTD_DB_PATH", filepath.Join(dataDir, "motd.db")),
		HTTPAddr:             env("MOTD_HTTP_ADDR", "127.0.0.1:8080"),
		SSHAddr:              env("MOTD_SSH_ADDR", ":2222"),
		SSHMaxConns:          IntEnv("MOTD_SSH_MAX_CONNS", 100),
		SSHIdleTimeout:       DurationEnv("MOTD_SSH_IDLE_TIMEOUT", 10*time.Minute),
		GitHubClientID:       env("MOTD_GITHUB_CLIENT_ID", ""),
		AuthLimitMaxFailures: IntEnv("MOTD_AUTH_LIMIT_MAX_FAILURES", DefaultAuthLimitMaxFailures),
		AuthLimitWindow:      DurationEnv("MOTD_AUTH_LIMIT_WINDOW", DefaultAuthLimitWindow),
		AuthLimitLockout:     DurationEnv("MOTD_AUTH_LIMIT_LOCKOUT", DefaultAuthLimitLockout),
		AuthLimitMaxKeys:     IntEnv("MOTD_AUTH_LIMIT_MAX_KEYS", DefaultAuthLimitMaxKeys),
		AuditCapacity:        IntEnv("MOTD_AUDIT_CAPACITY", DefaultAuditCapacity),
		AuditFlushInterval:   DurationEnv("MOTD_AUDIT_FLUSH_INTERVAL", DefaultAuditFlushInterval),
		LogLevel:             strings.ToLower(env("MOTD_LOG_LEVEL", "info")),
		LogFormat:            strings.ToLower(env("MOTD_LOG_FORMAT", "text")),
		DotEnvDisabled:       dotEnvDisabled(),
	}

	// 展示时区：启动期解析一次。env() 已经把空串/纯空白当作未设置并回落到默认；
	// 非法 IANA 名在这里直接失败关闭，绝不静默回落成 UTC（那正是本次要修的体验 bug）。
	// 错误信息只含配置名与标准库解析错误，不含任何凭据。
	tzRaw := env("MOTD_DISPLAY_TIMEZONE", DefaultDisplayTimezone)
	loc, err := time.LoadLocation(tzRaw)
	if err != nil {
		return Config{}, fmt.Errorf("MOTD_DISPLAY_TIMEZONE 取值非法: %q（需为 IANA 时区名、UTC 或 Local）: %w", tzRaw, err)
	}
	cfg.DisplayTimezone = tzRaw
	cfg.DisplayLocation = loc

	// 邮箱配置显式 resolve：启用时逐项校验、缺哪项报哪项，不做静默兜底；
	// 未启用时零影响（不触碰任何 Sender）。授权码只在此处从环境读入内存。
	emailCfg, err := resolveEmail(
		os.Getenv("MOTD_EMAIL_ENABLED"),
		os.Getenv("MOTD_SMTP_HOST"),
		os.Getenv("MOTD_SMTP_PORT"),
		os.Getenv("MOTD_SMTP_USER"),
		os.Getenv("MOTD_SMTP_PASS"),
		os.Getenv("MOTD_EMAIL_FROM"),
	)
	if err != nil {
		return Config{}, err
	}
	cfg.Email = emailCfg

	// 管理员白名单在启动期解析：非法指纹直接失败关闭，不留「配了却没人能管」的静默状态。
	adminFPs, err := ParseAdminFingerprints(env("MOTD_ADMIN_FINGERPRINTS", ""))
	if err != nil {
		return Config{}, err
	}
	cfg.AdminFingerprints = adminFPs

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("MOTD_LOG_LEVEL 取值非法: %q（可选 debug/info/warn/error）", cfg.LogLevel)
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return Config{}, fmt.Errorf("MOTD_LOG_FORMAT 取值非法: %q（可选 text/json）", cfg.LogFormat)
	}

	return cfg, nil
}

// Ensure 创建运行期依赖的目录。
func (c Config) Ensure() error {
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("创建数据目录 %s: %w", c.DataDir, err)
	}
	if dir := filepath.Dir(c.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建数据库目录 %s: %w", dir, err)
		}
	}
	return nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return fallback
}

// IntEnv 供后续模块（如写入队列容量、SSH 最大连接数）复用。
func IntEnv(key string, fallback int) int {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return v
}

// DurationEnv 解析 Go duration 字符串；非法或 <=0 一律回落默认值，
// 避免一个拼写错误把空闲超时设成 0 导致所有会话立刻断开。
func DurationEnv(key string, fallback time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}
