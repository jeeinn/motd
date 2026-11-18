// Package thread 定义 Thread（讨论主题）领域模型。
// 术语固定为 Node → Thread → Post（评审稿 v0.2 §5.2），不再使用「帖子/评论」混称。
package thread

import "time"

// Thread 是一个讨论主题，持有标题与聚合分。
type Thread struct {
	ID          int64
	NodeID      int64
	UserID      int64
	Title       string
	Score       int64
	LastReplyAt time.Time
	CreatedAt   time.Time
}
