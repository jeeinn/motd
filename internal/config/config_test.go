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
		"MOTD_SSH_ADDR", "MOTD_SSH_MAX_CONNS", "MOTD_SSH_IDLE_TIMEOUT",
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
	if cfg.SSHIdleTimeout != 10*time.Minute {
		t.Errorf("SSH 默认空闲超时应为 10m，实际 %v", cfg.SSHIdleTimeout)
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
