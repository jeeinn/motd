// Package node 定义 Node（版块）领域模型。
// 术语固定为 Node → Thread → Post（评审稿 v0.2 §5.2），版块以 slug 唯一标识，形如 /dev/ai。
// 本包不依赖任何 transport、渲染或存储实现。
package node

// Node 是一个版块。Slug 是稳定标识，Name / Description 用于展示。
type Node struct {
	ID          int64
	Slug        string
	Name        string
	Description string
}
