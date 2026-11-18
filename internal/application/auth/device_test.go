package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 8 个官方错误码 + token_expired 别名（官方文档表格与正文不一致，双兼容）。
func TestClassifyDeviceErrorCoversAllCodes(t *testing.T) {
	cases := []struct {
		code string
		want PollStatus
	}{
		{"authorization_pending", PollPending},
		{"slow_down", PollSlowDown},
		{"expired_token", PollExpired},
		{"token_expired", PollExpired},
		{"access_denied", PollTerminal},
		{"device_flow_disabled", PollTerminal},
		{"incorrect_client_credentials", PollTerminal},
		{"incorrect_device_code", PollTerminal},
		{"unsupported_grant_type", PollTerminal},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			pe := ClassifyDeviceError(c.code, "access_token", 0)
			if pe.Status != c.want {
				t.Fatalf("错误码 %q 分类 = %s，期望 %s", c.code, pe.Status, c.want)
			}
			if pe.Code != c.code || pe.Endpoint != "access_token" {
				t.Errorf("错误码与端点应原样保留，实际 %q / %q", pe.Code, pe.Endpoint)
			}
			if pe.Message == "" {
				t.Error("每个错误码都必须映射为可读中文提示")
			}
			var target *PollError
			if !errors.As(error(pe), &target) {
				t.Error("PollError 必须能被 errors.As 取回，供 P0-16 状态机决策")
			}
			// Error() 只能给可读提示，绝不携带原始响应体 / 凭据。
			if pe.Error() != pe.Message {
				t.Error("Error() 必须等于 Message")
			}
		})
	}
}

func TestClassifyDeviceErrorUnknownIsSanitized(t *testing.T) {
	pe := ClassifyDeviceError("\x1b[31mbad\x1b]0;pwned", "access_token", 0)
	if pe.Status != PollUnknown {
		t.Fatalf("未知错误码应为 PollUnknown，实际 %s", pe.Status)
	}
	// 必须按 rune 检查：C1 的 0x9B 会命中中文「回」的 UTF-8 次字节，
	// 用 strings.Contains 做字节级匹配会误报（与 P0-23 的 C1 CSI 同类陷阱）。
	if strings.ContainsRune(pe.Message, 0x1b) || strings.ContainsRune(pe.Message, 0x9b) {
		t.Errorf("错误消息不得携带 ESC / C1 控制字符，实际 %q", pe.Message)
	}
	if !strings.Contains(pe.Message, "未识别") {
		t.Errorf("未知错误必须明确提示已终止，实际 %q", pe.Message)
	}
}

func TestSafeCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"authorization_pending", "authorization_pending"},
		{"a-b_9", "a-b_9"},
		{"\x1b[31m", "31m"},
		{"", "unknown"},
		{"\x00\x01", "unknown"},
	}
	for _, c := range cases {
		if got := safeCode(c.in); got != c.want {
			t.Errorf("safeCode(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	if got := safeCode(strings.Repeat("A", 200)); len(got) != 64 {
		t.Errorf("错误码应截断到 64 字符，实际 %d", len(got))
	}
}

func TestDeviceCodePollIntervalFallback(t *testing.T) {
	for _, bad := range []int{0, -1, -999} {
		if got := (DeviceCode{Interval: bad}).PollInterval(); got != DefaultPollInterval {
			t.Errorf("非法 interval %d 应回落到 %d，实际 %d", bad, DefaultPollInterval, got)
		}
	}
	if got := (DeviceCode{Interval: 12}).PollInterval(); got != 12 {
		t.Errorf("合法 interval 应原样返回，实际 %d", got)
	}
}

func TestAccessTokenValid(t *testing.T) {
	if (AccessToken{}).Valid() {
		t.Error("空令牌不应有效")
	}
	if !(AccessToken{Token: "gho_abc", TokenType: "bearer"}).Valid() {
		t.Error("非空令牌应有效")
	}
}

func TestNextInterval(t *testing.T) {
	pending := ClassifyDeviceError("authorization_pending", "access_token", 0)
	expired := ClassifyDeviceError("expired_token", "access_token", 0)
	denied := ClassifyDeviceError("access_denied", "access_token", 0)

	cases := []struct {
		name string
		last int
		err  error
		want int
	}{
		{"nil 错误保持", 7, nil, 7},
		{"pending 保持", 5, pending, 5},
		{"expired 保持", 5, expired, 5},
		{"terminal 保持", 5, denied, 5},
		{"slow_down 无新值加 5 秒", 5, ClassifyDeviceError("slow_down", "access_token", 0), 10},
		{"slow_down 取响应新值", 5, ClassifyDeviceError("slow_down", "access_token", 20), 20},
		{"slow_down 新值更小不缩小", 10, ClassifyDeviceError("slow_down", "access_token", 3), 10},
		{"last 为零抬到默认", 0, nil, DefaultPollInterval},
		{"last 为负抬到默认", -3, nil, DefaultPollInterval},
		{"包装过的错误也能识别", 5, fmt.Errorf("轮询: %w", ClassifyDeviceError("slow_down", "access_token", 0)), 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextInterval(c.last, c.err); got != c.want {
				t.Fatalf("NextInterval(%d, %v) = %d，期望 %d", c.last, c.err, got, c.want)
			}
		})
	}
}

// 验收项「轮询间隔不得小于 interval」：把一次真实序列（pending →
// slow_down(interval=20) → pending）走一遍，间隔只增不减。
func TestNextIntervalNeverShrinksAcrossSequence(t *testing.T) {
	interval := (DeviceCode{Interval: 5}).PollInterval()
	results := []error{
		ClassifyDeviceError("authorization_pending", "access_token", 0),
		ClassifyDeviceError("slow_down", "access_token", 20),
		ClassifyDeviceError("authorization_pending", "access_token", 0),
	}
	for i, err := range results {
		next := NextInterval(interval, err)
		if next < interval {
			t.Fatalf("第 %d 轮间隔缩小：%d → %d", i+1, interval, next)
		}
		if next < DefaultPollInterval {
			t.Fatalf("第 %d 轮间隔低于官方最小 %d 秒", i+1, DefaultPollInterval)
		}
		interval = next
	}
	if interval != 20 {
		t.Fatalf("slow_down 返回 20 秒后应一直保持，实际 %d", interval)
	}
}
