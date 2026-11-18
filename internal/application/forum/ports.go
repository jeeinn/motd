// Package forum 编排论坛用例：浏览版块/主题、发帖、回复。
// 边界：事务边界与权限判断在此层；输出与 UI 无关的 ViewModel；不依赖任何 transport 或渲染实现。
//
// 本文件定义**端口（port）**：接口由使用方（application）声明，实现在 infrastructure。
// 这样 application 不 import infrastructure（§2.2 单向依赖），仓储也拿不到裸 *sql.DB——
// 写入只能通过 UnitOfWork.Write 拿到受限的 WriteTx，落实 ADR-010 的执行保障。
package forum

import (
	"context"
	"time"

	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

// ThreadSort 是主题列表的排序方式。用枚举而非字符串魔法值，非法值在用例入口即拒绝。
type ThreadSort uint8

const (
	// SortLatest 按最后回复时间倒序（同秒按 id 倒序），即「最新」。
	SortLatest ThreadSort = iota + 1
	// SortScore 按分数倒序（同分按 id 倒序），即「最高分」。
	SortScore
)

// Valid 报告排序值是否在允许取值域内。零值（未指定）不合法，调用方必须显式选择。
func (s ThreadSort) Valid() bool { return s == SortLatest || s == SortScore }

// ThreadCursor 是 keyset 分页的位置。用 (排序列, id) 复合键而不是 OFFSET：
// 深翻页代价恒定，且不会因期间有新帖而出现重复/跳行。
// 字段按 Unix 秒存，与 DDL 的 INTEGER 时间口径一致。
type ThreadCursor struct {
	Sort        ThreadSort
	LastReplyAt int64
	Score       int64
	ID          int64
}

// IsZero 表示「第一页」，即不带游标。
func (c ThreadCursor) IsZero() bool { return c.ID == 0 }

// ThreadQuery 是主题列表查询。Limit 由用例层统一裁剪，仓储会取 Limit+1 条用于判断 hasMore。
type ThreadQuery struct {
	NodeID int64
	Sort   ThreadSort
	After  ThreadCursor
	Limit  int
}

// PostQuery 是主题下内容列表查询。AfterID 为上一页最后一条的 id（升序游标）。
type PostQuery struct {
	ThreadID int64
	AfterID  int64
	Limit    int
}

// NewThread 是创建主题的写入参数。领域时间用 time.Time，由仓储负责转成 DB 的 Unix 秒。
type NewThread struct {
	NodeID    int64
	UserID    int64
	Title     string
	Score     int64
	CreatedAt time.Time
}

// NewPost 是创建内容的写入参数。Status 由用例层给出（P0 恒为 normal）。
type NewPost struct {
	ThreadID  int64
	UserID    int64
	BodyMD    string
	Status    post.Status
	CreatedAt time.Time
}

// WriteStats 是限流判定所需的近一窗口写入统计。
// LastWriteAt / OldestAt 为零值表示该窗口内没有写入。
type WriteStats struct {
	Count       int
	LastWriteAt time.Time
	OldestAt    time.Time
}

// 方法名带领域前缀，是因为 Repos 会把三个端口聚合进同一个接口：
// Go 不允许接口里出现同名不同签名的方法，统一命名也避免调用点靠参数猜语义。

// NodeReader 是版块的只读端口。
type NodeReader interface {
	ListNodes(ctx context.Context) ([]node.Node, error)
	GetNodeBySlug(ctx context.Context, slug string) (node.Node, error)
}

// ThreadReader 是主题的只读端口。
type ThreadReader interface {
	GetThread(ctx context.Context, id int64) (thread.Thread, error)
	// ListThreads 返回最多 Limit 条与 hasMore；仓储内部会多取一条来判断是否还有下一页。
	ListThreads(ctx context.Context, q ThreadQuery) ([]thread.Thread, bool, error)
}

// PostReader 是内容的只读端口。实现必须过滤软删除（status <> 'normal'）。
type PostReader interface {
	ListPosts(ctx context.Context, q PostQuery) ([]post.Post, bool, error)
}

// WriteTx 是**单个事务内**的受限写入句柄。
// 用例闭包只能拿到它，拿不到 *sql.Tx / *sql.DB；一个业务用例对应一次 UnitOfWork.Write。
// 权限与限流查询也放在这里，保证「检查 + 写入」在同一事务快照内，不留 TOCTOU 窗口。
type WriteTx interface {
	UserStatus(ctx context.Context, userID int64) (user.Status, error)
	NodeExists(ctx context.Context, nodeID int64) (bool, error)
	ThreadExists(ctx context.Context, threadID int64) (bool, error)
	RecentWriteStats(ctx context.Context, userID int64, since time.Time) (WriteStats, error)
	InsertThread(ctx context.Context, n NewThread) (int64, error)
	InsertPost(ctx context.Context, p NewPost) (int64, error)
	// TouchThread 在同一条 UPDATE 里维护 last_reply_at 与 score。
	// scoreDelta 由调用方给出：P0 无投票传 0，P1 投票可直接复用同一事务路径。
	TouchThread(ctx context.Context, threadID int64, at time.Time, scoreDelta int64) error
}

// UnitOfWork 把「一个用例一个事务」变成结构性约束。
// 实现方负责开事务、串行化、提交后才确认（ADR-010）。
type UnitOfWork interface {
	Write(ctx context.Context, fn func(WriteTx) error) error
}

// Repos 是装配时注入给 Service 的完整端口集合。
type Repos interface {
	NodeReader
	ThreadReader
	PostReader
	UnitOfWork
}
