package forum

import (
	"errors"
	"fmt"
	"time"
)

// 用例层的哨兵错误。仓储把 sql.ErrNoRows 映射成 ErrNotFound，
// 领域层因此不需要 import database/sql。
var (
	ErrReadOnly      = errors.New("访客只读，请先登录")
	ErrForbidden     = errors.New("账号状态不允许写入")
	ErrInvalidTitle  = errors.New("标题不合法")
	ErrInvalidBody   = errors.New("正文不合法")
	ErrNotFound      = errors.New("对象不存在")
	ErrInvalidSort   = errors.New("排序方式不合法")
	ErrInvalidCursor = errors.New("分页游标不合法")
	ErrInvalidLimit  = errors.New("分页大小不合法")
	// ErrParentThreadMismatch 表示回复的父帖不属于同一主题（R8）。
	ErrParentThreadMismatch = errors.New("父帖与回复不在同一主题")
	// ErrInvalidVote 表示投票值不在 -1/1 的取值域内。
	ErrInvalidVote = errors.New("投票值不合法")
	// ErrInvalidVoteTarget 表示投票目标类型不是 thread/post。
	ErrInvalidVoteTarget = errors.New("投票目标类型不合法")
	// ErrInvalidQuery 表示搜索词为空或分页参数越界。
	ErrInvalidQuery = errors.New("搜索参数不合法")
)

// RateLimitError 表示命中发帖限流（Q6：每人 60 秒 1 帖、每小时 10 帖）。
// RetryAfter 是剩余冷却时间，供终端显示「请 N 秒后重试」，不只是抛一句被限流。
type RateLimitError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("发帖过于频繁（%s），请 %s 后重试", e.Reason, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("发帖过于频繁（%s）", e.Reason)
}

// ValidationError 把字段级校验错误带上具体原因，便于终端直接展示。
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s不合法：%s", e.Field, e.Reason)
}

// Unwrap 让 errors.Is(err, ErrInvalidTitle/ErrInvalidBody) 成立。
func (e *ValidationError) Unwrap() error {
	switch e.Field {
	case "title":
		return ErrInvalidTitle
	case "body":
		return ErrInvalidBody
	default:
		return nil
	}
}
