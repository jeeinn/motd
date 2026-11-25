package forum

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// P0 的输入边界与限流参数。
// 依据：P0-PLAN.md P0-10 规则 + Q6 已拍板结论（每人 60 秒 1 帖、每小时 10 帖）。
const (
	MaxTitleRunes = 120
	MaxBodyBytes  = 8 << 10 // 8 KiB，防止一条内容打满带宽（按字节而非字符计）

	DefaultPageSize = 20
	MaxPageSize     = 100
	// MaxPostsPerThread 是 GetThread / ScreenThreadDetail 的**单次批量**上限。
	// R-06 起详情屏走分页（每批 detailReplyPageSize 条），本常量只服务于
	// 仍一次性取满的旧调用方与测试，不再代表"可见内容上限"。
	MaxPostsPerThread = 200

	RateLimitInterval  = 60 * time.Second
	RateLimitWindow    = time.Hour
	MaxWritesPerWindow = 10
)

func normalizePageSize(n int) int {
	if n <= 0 {
		return DefaultPageSize
	}
	if n > MaxPageSize {
		return MaxPageSize
	}
	return n
}

// validateTitle：1–120 字符且非空白。长度按 Unicode 码点计，中文一字算一个。
// 存储时保留原文（不 Trim），避免丢掉用户有意的首尾格式。
func validateTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return &ValidationError{Field: "title", Reason: "不能为空白"}
	}
	n := utf8.RuneCountInString(title)
	if n > MaxTitleRunes {
		return &ValidationError{
			Field:  "title",
			Reason: fmt.Sprintf("最多 %d 字符，实际 %d", MaxTitleRunes, n),
		}
	}
	return nil
}

// validateBody：非空（去空白后）且不超过 8 KiB。上限按字节，因为带宽是字节。
func validateBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return &ValidationError{Field: "body", Reason: "不能为空白"}
	}
	if len(body) > MaxBodyBytes {
		return &ValidationError{
			Field:  "body",
			Reason: fmt.Sprintf("最多 %d 字节，实际 %d", MaxBodyBytes, len(body)),
		}
	}
	return nil
}

// checkRate 在同一事务快照内判定限流。
// 优先级：先 60 秒冷却（更常见的误触），再 1 小时配额；两者都带剩余冷却时间。
func checkRate(stats WriteStats, now time.Time) error {
	if stats.Count == 0 {
		return nil
	}
	elapsed := now.Sub(stats.LastWriteAt)
	if elapsed < 0 {
		elapsed = 0 // 时钟回拨时按「刚刚写过」处理，不把冷却时间放大
	}
	if elapsed < RateLimitInterval {
		return &RateLimitError{
			Reason:     "60 秒内只能发 1 帖",
			RetryAfter: RateLimitInterval - elapsed,
		}
	}
	if stats.Count >= MaxWritesPerWindow {
		retry := stats.OldestAt.Add(RateLimitWindow).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return &RateLimitError{
			Reason:     "1 小时内最多 10 帖",
			RetryAfter: retry,
		}
	}
	return nil
}
