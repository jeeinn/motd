package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// LogLevel debug / info / warn / error。
	LogLevel string
	// LogFormat text / json。
	LogFormat string
}

// Load 读取环境变量并修正默认值。
// 约定：所有变量以 MOTD_ 前缀；空字符串视为未设置，回落到默认值。
func Load() (Config, error) {
	dataDir := env("MOTD_DATA_DIR", "./data")
	cfg := Config{
		DataDir:   dataDir,
		DBPath:    env("MOTD_DB_PATH", filepath.Join(dataDir, "motd.db")),
		HTTPAddr:  env("MOTD_HTTP_ADDR", "127.0.0.1:8080"),
		LogLevel:  strings.ToLower(env("MOTD_LOG_LEVEL", "info")),
		LogFormat: strings.ToLower(env("MOTD_LOG_FORMAT", "text")),
	}

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
