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
	// ErrInvalidNodeID 表示版块 ID 为负（v0.7.1 / ADR-063：NodeID=0 是跨版块哨兵，
	// 负值不是合法取值）。属于内部调用错误，不来自用户输入。
	ErrInvalidNodeID = errors.New("版块 ID 不合法")
	// ErrParentThreadMismatch 表示回复的父帖不属于同一主题（R8）。
	ErrParentThreadMismatch = errors.New("父帖与回复不在同一主题")
	// ErrInvalidVote 表示投票值不在 -1/1 的取值域内。
	ErrInvalidVote = errors.New("投票值不合法")
	// ErrInvalidVoteTarget 表示投票目标类型不是 thread/post。
	ErrInvalidVoteTarget = errors.New("投票目标类型不合法")
	// ErrInvalidQuery 表示搜索词为空或分页参数越界。
	ErrInvalidQuery = errors.New("搜索参数不合法")
	// ErrNotOwner 是删除权限的统一对外口径：非作者、访客、目标不存在或已删，
	// 一律返回它，避免通过「能否删除」探测内容的存在性与状态（v0.2.1）。
	ErrNotOwner = errors.New("仅作者可删除")
	// ErrInvalidDeleteTarget 表示删除目标类型不是 thread/post（内部调用错误，非用户文本）。
	ErrInvalidDeleteTarget = errors.New("删除目标类型不合法")
	// ErrNotEditor 是编辑权限的统一对外口径，与 ErrNotOwner 同构但动作不同：
	// 非作者、访客、目标不存在一律返回它，固定文案「仅作者可编辑」，
	// 不区分原因、不回显 ID、不泄漏目标存在性（P2-M2 / ADR-051）。
	ErrNotEditor = errors.New("仅作者可编辑")
	// ErrAdminDenied 是所有节点治理命令的统一对外口径：
	// 访客、非 active 账号、指纹不在白名单，一律返回它，不区分原因、不回显输入。
	ErrAdminDenied = errors.New("仅管理员可执行")
	// ErrInvalidSlug / ErrInvalidNodeName 是节点字段校验的哨兵，供固定文案映射。
	ErrInvalidSlug     = errors.New("slug 不合法")
	ErrInvalidNodeName = errors.New("版块名称不合法")
	// ErrNodeSlugExists 表示 slug 已被占用（含归档节点，slug 名额不释放）。
	ErrNodeSlugExists = errors.New("slug 已存在")
	// ErrSeedNodeRename 表示尝试给 5 个 seed 版块改名（用户拍板：可归档、不可改名）。
	ErrSeedNodeRename = errors.New("种子版块不可改名")
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
	case "slug":
		return ErrInvalidSlug
	case "name":
		return ErrInvalidNodeName
	default:
		return nil
	}
}
