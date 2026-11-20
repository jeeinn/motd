package config

import (
	"testing"
	"time"
)

func TestDurationEnvFallsBackOnInvalid(t *testing.T) {
	fallback := 7 * time.Second
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"150ms", 150 * time.Millisecond},
		{"2m", 2 * time.Minute},
		{"nonsense", fallback},
		{"-1s", fallback},
		{"0s", fallback},
		{"", fallback},
	}
	for _, c := range cases {
		t.Setenv("MOTD_TEST_DURATION", c.raw)
		if got := DurationEnv("MOTD_TEST_DURATION", fallback); got != c.want {
			t.Errorf("DurationEnv(%q) = %v，期望 %v", c.raw, got, c.want)
		}
	}
}

func TestLoadSSHDefaults(t *testing.T) {
	for _, k := range []string{
		"MOTD_SSH_ADDR", "MOTD_SSH_MAX_CONNS",
		"MOTD_LOG_LEVEL", "MOTD_LOG_FORMAT",
	} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHAddr != ":2222" {
		t.Errorf("SSH 默认端口应为 2222（Q4），实际 %q", cfg.SSHAddr)
	}
	if cfg.SSHMaxConns != 100 {
		t.Errorf("SSH 默认最大连接数应为 100，实际 %d", cfg.SSHMaxConns)
	}
}

func TestLoadAuthSecurityDefaults(t *testing.T) {
	for _, k := range []string{
		"MOTD_AUTH_LIMIT_MAX_FAILURES", "MOTD_AUTH_LIMIT_WINDOW",
		"MOTD_AUTH_LIMIT_LOCKOUT", "MOTD_AUTH_LIMIT_MAX_KEYS",
		"MOTD_AUDIT_CAPACITY", "MOTD_AUDIT_FLUSH_INTERVAL",
	} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthLimitMaxFailures != DefaultAuthLimitMaxFailures ||
		cfg.AuthLimitWindow != DefaultAuthLimitWindow ||
		cfg.AuthLimitLockout != DefaultAuthLimitLockout ||
		cfg.AuthLimitMaxKeys != DefaultAuthLimitMaxKeys ||
		cfg.AuditCapacity != DefaultAuditCapacity ||
		cfg.AuditFlushInterval != DefaultAuditFlushInterval {
		t.Fatalf("认证安全默认值不符: %+v", cfg)
	}
}

func TestLoadAuthSecurityOverrides(t *testing.T) {
	t.Setenv("MOTD_AUTH_LIMIT_MAX_FAILURES", "7")
	t.Setenv("MOTD_AUTH_LIMIT_WINDOW", "2m")
	t.Setenv("MOTD_AUTH_LIMIT_LOCKOUT", "30m")
	t.Setenv("MOTD_AUTH_LIMIT_MAX_KEYS", "128")
	t.Setenv("MOTD_AUDIT_CAPACITY", "64")
	t.Setenv("MOTD_AUDIT_FLUSH_INTERVAL", "250ms")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthLimitMaxFailures != 7 || cfg.AuthLimitWindow != 2*time.Minute ||
		cfg.AuthLimitLockout != 30*time.Minute || cfg.AuthLimitMaxKeys != 128 ||
		cfg.AuditCapacity != 64 || cfg.AuditFlushInterval != 250*time.Millisecond {
		t.Fatalf("环境变量未生效: %+v", cfg)
	}
}

func TestLoadGitHubClientID(t *testing.T) {
	t.Setenv("MOTD_GITHUB_CLIENT_ID", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubClientID != "" {
		t.Errorf("未配置时 client_id 应为空，实际 %q", cfg.GitHubClientID)
	}

	t.Setenv("MOTD_GITHUB_CLIENT_ID", "  Iv1.abc123  ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubClientID != "Iv1.abc123" {
		t.Errorf("client_id 应去除首尾空白，实际 %q", cfg.GitHubClientID)
	}
}
