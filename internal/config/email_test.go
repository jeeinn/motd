package config

import (
	"strings"
	"testing"
)

// 邮箱配置必须显式 resolve：默认关闭、开启逐项校验、缺哪项报哪项、不静默兜底。
// 这些用例只用占位字符串，绝不写入真实凭据。

func TestResolveEmailDisabledByDefault(t *testing.T) {
	for _, enabled := range []string{"", "false", "FALSE", "0", "yes", " true-ish "} {
		cfg, err := resolveEmail(enabled, "", "", "", "", "")
		if err != nil {
			t.Fatalf("enabled=%q 未启用时不应校验其余项: %v", enabled, err)
		}
		if cfg.Enabled {
			t.Fatalf("enabled=%q 不应解析为启用", enabled)
		}
	}
}

func TestResolveEmailEnabledAcceptsTrueAnyCase(t *testing.T) {
	for _, enabled := range []string{"true", "TRUE", " True "} {
		cfg, err := resolveEmail(enabled, "smtp.example.com", "", "u@example.com", "placeholder", "u@example.com")
		if err != nil {
			t.Fatalf("enabled=%q: %v", enabled, err)
		}
		if !cfg.Enabled || cfg.SMTPPort != DefaultSMTPPort {
			t.Fatalf("enabled=%q 应启用且端口回落 465: %+v", enabled, cfg)
		}
	}
}

func TestResolveEmailReportsEveryMissingField(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		user    string
		pass    string
		from    string
		missing []string
	}{
		{"缺 host", "", "u@example.com", "placeholder", "u@example.com", []string{"MOTD_SMTP_HOST"}},
		{"缺 user", "smtp.example.com", "", "placeholder", "u@example.com", []string{"MOTD_SMTP_USER"}},
		{"缺 pass", "smtp.example.com", "u@example.com", "", "u@example.com", []string{"MOTD_SMTP_PASS"}},
		{"缺 from", "smtp.example.com", "u@example.com", "placeholder", "", []string{"MOTD_EMAIL_FROM"}},
		{"多项缺失", "", "", "", "", []string{"MOTD_SMTP_HOST", "MOTD_SMTP_USER", "MOTD_SMTP_PASS", "MOTD_EMAIL_FROM"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveEmail("true", c.host, "", c.user, c.pass, c.from)
			if err == nil {
				t.Fatal("缺失必填项必须报错，不得静默兜底")
			}
			msg := err.Error()
			for _, field := range c.missing {
				if !strings.Contains(msg, field) {
					t.Errorf("错误信息必须点名 %s，实际 %q", field, msg)
				}
			}
		})
	}
}

func TestResolveEmailRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "-1", "70000"} {
		_, err := resolveEmail("true", "smtp.example.com", port, "u@example.com", "placeholder", "u@example.com")
		if err == nil || !strings.Contains(err.Error(), "MOTD_SMTP_PORT") {
			t.Errorf("非法端口 %q 必须报 MOTD_SMTP_PORT，实际 %v", port, err)
		}
	}
}

func TestResolveEmailTrimsWhitespace(t *testing.T) {
	cfg, err := resolveEmail("true", "  smtp.example.com  ", " 587 ", "  u@example.com  ", "  placeholder  ", "  u@example.com  ")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPUser != "u@example.com" ||
		cfg.SMTPPass != "placeholder" || cfg.From != "u@example.com" || cfg.SMTPPort != 587 {
		t.Fatalf("应去除首尾空白: %+v", cfg)
	}
}

func TestLoadReadsEmailFromEnvironment(t *testing.T) {
	for _, k := range []string{
		"MOTD_EMAIL_ENABLED", "MOTD_SMTP_HOST", "MOTD_SMTP_PORT",
		"MOTD_SMTP_USER", "MOTD_SMTP_PASS", "MOTD_EMAIL_FROM",
	} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil || cfg.Email.Enabled {
		t.Fatalf("默认应关闭邮箱且 Load 成功: cfg=%+v err=%v", cfg.Email, err)
	}

	t.Setenv("MOTD_EMAIL_ENABLED", "true")
	t.Setenv("MOTD_SMTP_HOST", "smtp.example.com")
	t.Setenv("MOTD_SMTP_USER", "u@example.com")
	t.Setenv("MOTD_SMTP_PASS", "placeholder")
	t.Setenv("MOTD_EMAIL_FROM", "u@example.com")
	cfg, err = Load()
	if err != nil || !cfg.Email.Enabled || cfg.Email.SMTPHost != "smtp.example.com" || cfg.Email.SMTPPass != "placeholder" {
		t.Fatalf("环境变量未生效: cfg=%+v err=%v", cfg.Email, err)
	}
}
