package config

import (
	"strings"
	"testing"
	"time"
)

// patch「展示时区可配置」：只校验展示层配置，不触碰任何存储归一化逻辑。

func TestLoadDisplayTimezoneDefaultsToShanghai(t *testing.T) {
	t.Setenv("MOTD_DISPLAY_TIMEZONE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DisplayTimezone != DefaultDisplayTimezone {
		t.Errorf("默认展示时区应为 %q，实际 %q", DefaultDisplayTimezone, cfg.DisplayTimezone)
	}
	if cfg.DisplayLocation == nil || cfg.DisplayLocation.String() != "Asia/Shanghai" {
		t.Fatalf("默认展示时区应解析为 Asia/Shanghai，实际 %v", cfg.DisplayLocation)
	}
	if got := time.Unix(1_700_000_000, 0).In(cfg.DisplayLocation).Format("01-02 15:04"); got != "11-15 06:13" {
		t.Errorf("默认时区下时间格式错误: %q", got)
	}
}

func TestLoadDisplayTimezoneAcceptedValues(t *testing.T) {
	cases := []struct {
		raw      string
		wantName string
	}{
		{"Asia/Shanghai", "Asia/Shanghai"},
		{"America/New_York", "America/New_York"},
		{"UTC", "UTC"},
		{"Local", time.Local.String()},
		{"  Asia/Shanghai  ", "Asia/Shanghai"}, // env() 去首尾空白
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			t.Setenv("MOTD_DISPLAY_TIMEZONE", c.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(%q): %v", c.raw, err)
			}
			if cfg.DisplayLocation == nil || cfg.DisplayLocation.String() != c.wantName {
				t.Fatalf("Load(%q) location = %v，期望 %q", c.raw, cfg.DisplayLocation, c.wantName)
			}
		})
	}
}

func TestLoadDisplayTimezoneInvalidFailsFast(t *testing.T) {
	// 凭据只作为「不应出现在错误里」的探针；非法时区错误绝不能带出任何配置值。
	const secret = "SUPER_SECRET_SMTP_PASS_SHOULD_NOT_LEAK"
	t.Setenv("MOTD_SMTP_PASS", secret)
	for _, raw := range []string{"Mars/Olympus", "Not/AZone", "UTC+8", "Asia/上海"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("MOTD_DISPLAY_TIMEZONE", raw)
			cfg, err := Load()
			if err == nil {
				t.Fatalf("非法时区 %q 必须启动期失败", raw)
			}
			if !strings.Contains(err.Error(), "MOTD_DISPLAY_TIMEZONE") || !strings.Contains(err.Error(), raw) {
				t.Fatalf("错误应含变量名与非法值，实际: %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("时区错误泄露了无关的 SMTP 凭据")
			}
			if cfg.DisplayLocation != nil {
				t.Fatal("失败时不得返回半初始化的 location")
			}
		})
	}
}

func TestLoadDisplayTimezoneProcessEnvWinsOverDotEnv(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	t.Setenv("MOTD_DISPLAY_TIMEZONE", "Asia/Shanghai")
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_DISPLAY_TIMEZONE=UTC\n")

	if _, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe}); err != nil {
		t.Fatalf("applyDotEnv: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DisplayTimezone != "Asia/Shanghai" {
		t.Fatalf("进程环境变量必须优先于 .env，实际 %q", cfg.DisplayTimezone)
	}
}

func TestLoadDisplayTimezoneDotEnvFillsMissing(t *testing.T) {
	t.Setenv(DotEnvDisabledKey, "")
	t.Setenv("MOTD_DISPLAY_TIMEZONE", "")
	cwd, _, exe := dotEnvTestDirs(t)
	writeFakeDotEnv(t, cwd, "MOTD_DISPLAY_TIMEZONE=America/New_York\n")

	if _, err := applyDotEnv(dotEnvLookup{cwdDir: cwd, exePath: exe}); err != nil {
		t.Fatalf("applyDotEnv: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DisplayTimezone != "America/New_York" {
		t.Fatalf(".env 应填空缺，实际 %q", cfg.DisplayTimezone)
	}
}
