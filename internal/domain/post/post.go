// Package post 定义 Post（主题下的内容条目，含首帖与回复）领域模型。
package post

import "time"

// Status 对应 posts.status 的取值域；删除只改状态，不物理删除记录。
type Status string

const (
	StatusNormal  Status = "normal"
	StatusDeleted Status = "deleted"
	StatusHidden  Status = "hidden"
)

// Valid 报告状态是否在允许取值域内。
func (s Status) Valid() bool {
	switch s {
	case StatusNormal, StatusDeleted, StatusHidden:
		return true
	default:
		return false
	}
}

// Visible 表示该内容是否对外可见。
func (s Status) Visible() bool { return s == StatusNormal }

// Post 是主题下的一条内容。ParentID 为预留字段，当前不启用（扁平回复）。
type Post struct {
	ID        int64
	ThreadID  int64
	UserID    int64
	BodyMD    string
	Status    Status
	ParentID  *int64
	CreatedAt time.Time
}
