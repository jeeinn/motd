// Package user 定义用户领域模型与用户名规则。
// 规则基线来自评审稿 v0.2 §4.5，字符集由 V 先生 2026-11-18 决策放宽：
// 允许中文 / 英文字母 / 数字 / 下划线 / 中划线，不允许任何空白字符。
//
// 本包是用户名校验的唯一事实来源：DDL 的 CHECK 只能表达长度与空白兜底，
// 字符类别白名单以这里的 ValidateUsername 为准（详见 docs/DECISIONS.md ADR-005）。
// 本包不依赖任何 transport、渲染或存储实现。
package user

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Status 对应 users.status 的取值域。
type Status string

const (
	StatusActive   Status = "active"
	StatusPending  Status = "pending"
	StatusDeleting Status = "deleting"
	StatusDeleted  Status = "deleted"
)

// Valid 报告状态是否在允许取值域内。
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusPending, StatusDeleting, StatusDeleted:
		return true
	default:
		return false
	}
}

// CanPost 表示该状态是否允许发起写操作。
// 只有 active 可以写；deleting 期间立即冻结写入，避免注销流程与内容清理互相踩踏。
func (s Status) CanPost() bool { return s == StatusActive }

// User 是用户的领域视图，不含凭证与身份源明细。
type User struct {
	ID           int64
	Username     string
	DisplayName  string
	PrimaryEmail string
	Karma        int64
	Status       Status
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// 用户名规则：3–20 个字符（按 Unicode 码点计数，中文一字算一个），
// 允许 \p{Han}（中文）、ASCII 字母、ASCII 数字、下划线、中划线；
// 其余一律拒绝——包括所有空白字符、全角空格、标点、Emoji、其他语言字母。
// 大小写不敏感唯一由 DB 的 COLLATE NOCASE 保证（中文无大小写概念，不受影响）。
var usernamePattern = regexp.MustCompile(`^[\p{Han}a-zA-Z0-9_-]{3,20}$`)

// reservedNames 为系统保留名，注册时命中则按 §4.5 追加 `_0`。
var reservedNames = map[string]struct{}{
	"root": {}, "admin": {}, "sudo": {}, "system": {}, "null": {},
	"dev": {}, "forum": {}, "help": {}, "op": {}, "ai": {},
}

// ErrInvalidUsername 表示用户名不满足命名规则。
var ErrInvalidUsername = errors.New("用户名不合法")

// ValidateUsername 校验用户名是否满足规则。
// 调用位置：注册、改名、GitHub 首登创建账号、导入数据——任何写入 users.username 的路径都必须先过这里。
func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return fmt.Errorf("%w: 需 3–20 字符，仅中文 / 英文字母 / 数字 / _ / -，且不含空白", ErrInvalidUsername)
	}
	return nil
}

// IsReserved 报告用户名是否命中系统保留名（比较时忽略大小写）。
func IsReserved(name string) bool {
	_, ok := reservedNames[strings.ToLower(name)]
	return ok
}

// ResolveUsername 返回可用用户名：命中保留名时追加 `_0`。
// 调用方仍需处理唯一约束冲突（可能已被他人占用）。
func ResolveUsername(name string) string {
	if IsReserved(name) {
		return name + "_0"
	}
	return name
}
