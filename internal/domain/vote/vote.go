// Package vote 定义 Vote（对 Thread 或 Post 的投票）领域模型。
// votes 为多态关联，目标存在性由 Application 层在事务内校验。
package vote

import "time"

// TargetType 是投票目标类型。
type TargetType string

const (
	TargetThread TargetType = "thread"
	TargetPost   TargetType = "post"
)

// Valid 报告目标类型是否合法。
func (t TargetType) Valid() bool { return t == TargetThread || t == TargetPost }

// Value 是投票方向：仅允许 +1 / -1。
type Value int

const (
	Up   Value = 1
	Down Value = -1
)

// Valid 报告投票值是否合法。
func (v Value) Valid() bool { return v == Up || v == Down }

// Vote 是一条投票记录。
type Vote struct {
	ID         int64
	UserID     int64
	TargetType TargetType
	TargetID   int64
	Value      Value
	CreatedAt  time.Time
}
