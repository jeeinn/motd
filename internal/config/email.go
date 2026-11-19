package config

import (
	"fmt"
	"strconv"
	"strings"
)

// EmailConfig 是邮箱投递的显式配置。Enabled=false 时其余字段全为零值，
// 调用方据此决定不构造 Sender（不预留第二 Provider、不做静默兜底）。
type EmailConfig struct {
	Enabled  bool
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string // 授权码，不进日志、不进仓库
	From     string
}

// DefaultSMTPPort 是 163 隐式 TLS 端口（SMTPS）。
const DefaultSMTPPort = 465

// resolveEmail 显式解析邮箱配置。
//
// 语义：
//   - enabled 只认 true（大小写不敏感）；其它值与空串一律视为未启用，不校验其余项；
//   - 启用后 host/user/pass/from 缺一不可，错误信息逐个点名缺失的环境变量；
//   - port 未设置时回落到 465，设置了但非法（非数字 / 越界）直接报错，不静默吞掉。
//
// 抽成纯函数（不读 os.Getenv）是为了让「缺哪项报哪项」可被表驱动测试覆盖。
func resolveEmail(enabled, host, port, user, pass, from string) (EmailConfig, error) {
	if !strings.EqualFold(strings.TrimSpace(enabled), "true") {
		return EmailConfig{}, nil
	}

	var missing []string
	c := EmailConfig{
		Enabled:  true,
		SMTPHost: strings.TrimSpace(host),
		SMTPUser: strings.TrimSpace(user),
		SMTPPass: strings.TrimSpace(pass),
		From:     strings.TrimSpace(from),
	}
	if c.SMTPHost == "" {
		missing = append(missing, "MOTD_SMTP_HOST")
	}
	if c.SMTPUser == "" {
		missing = append(missing, "MOTD_SMTP_USER")
	}
	if c.SMTPPass == "" {
		missing = append(missing, "MOTD_SMTP_PASS")
	}
	if c.From == "" {
		missing = append(missing, "MOTD_EMAIL_FROM")
	}
	if len(missing) > 0 {
		return EmailConfig{}, fmt.Errorf("MOTD_EMAIL_ENABLED=true 但缺少配置: %s", strings.Join(missing, ", "))
	}

	rawPort := strings.TrimSpace(port)
	if rawPort == "" {
		c.SMTPPort = DefaultSMTPPort
	} else {
		n, err := strconv.Atoi(rawPort)
		if err != nil || n <= 0 || n > 65535 {
			return EmailConfig{}, fmt.Errorf("MOTD_SMTP_PORT 取值非法: %q（可选 1-65535，默认 %d）", port, DefaultSMTPPort)
		}
		c.SMTPPort = n
	}
	return c, nil
}
