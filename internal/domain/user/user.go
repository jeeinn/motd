// Package user 定义用户领域模型与用户名规则（评审稿 v0.2 §4.5、§5.1）。
// 本包不依赖任何 transport、渲染或存储实现。
package user

import (
	"errors"
	"fmt"
	"regexp"
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

// 用户名规则：3–20 字符，仅 [a-z0-9_-]，大小写不敏感唯一（唯一性由 DB 的 NOCASE 约束保证）。
var usernamePattern = regexp.MustCompile(`^[a-z0-9_-]{3,20}$`)

// reservedNames 为系统保留名，注册时命中则按 §4.5 追加 `_0`。
var reservedNames = map[string]struct{}{
	"root": {}, "admin": {}, "sudo": {}, "system": {}, "null": {},
	"dev": {}, "forum": {}, "help": {}, "op": {}, "ai": {},
}

// ErrInvalidUsername 表示用户名不满足命名规则。
var ErrInvalidUsername = errors.New("用户名不合法")

// ValidateUsername 校验用户名是否满足规则。
func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return fmt.Errorf("%w: 需 3–20 字符，仅 a-z 0-9 _ -", ErrInvalidUsername)
	}
	return nil
}

// IsReserved 报告用户名是否命中系统保留名。
func IsReserved(name string) bool {
	_, ok := reservedNames[name]
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
