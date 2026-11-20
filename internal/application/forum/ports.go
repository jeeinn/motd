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

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/domain/vote"
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
// ParentPostID 非空表示这是一条回复：仓储必须在同一事务内确认父帖属于
// 同一 thread_id，否则拒绝写入（0005_post_parent_thread 触发器为第二道防线）。
type NewPost struct {
	ThreadID     int64
	UserID       int64
	BodyMD       string
	Status       post.Status
	ParentPostID *int64
	CreatedAt    time.Time
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

// NodeAdminView 是管理员列表的一行：公开列表只给 node.Node，治理屏还需要状态。
// Status 只在这里出现，不进公开 ViewModel，避免归档节点泄漏到普通用户视图。
type NodeAdminView struct {
	ID     int64
	Slug   string
	Name   string
	Status string
}

// NewNode 是创建版块的写入参数。Slug 由用例层的 ValidateSlug 收口。
// nodes 表没有 created_at 列，事件时间由用例层的 now 提供。
type NewNode struct {
	Slug        string
	Name        string
	Description string
}

// NodeReader 是版块的只读端口。
type NodeReader interface {
	ListNodes(ctx context.Context) ([]node.Node, error)
	// ListAllNodes 是管理员专用只读变体：含 active/archived，按 id 升序。
	// 公开用例不得调用它；准入由 Service.ListAdminNodes 单点把守。
	ListAllNodes(ctx context.Context) ([]NodeAdminView, error)
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
	// PostAuthor 返回单条可见内容的作者 ID，不存在或 status<>'normal' 返回 ErrNotFound。
	// 供编辑入口在进入 composer **之前**做只读判权（CanEditPost），复用 canModify 单一谓词；
	// 真正的判权仍在 EditPost 的写事务内复核，防 TOCTOU。
	PostAuthor(ctx context.Context, postID int64) (int64, error)
}

// SearchQuery 是正文检索的归一化参数：Query 已经过清洗，
// Limit/Offset 已由用例层裁剪；仓储不得再放宽页长。
type SearchQuery struct {
	Query  string
	Limit  int
	Offset int
}

// SearchHit 是一条正文命中。Excerpt 是摘要，不含任何标记与渲染指令。
type SearchHit struct {
	ThreadID int64
	PostID   int64
	Excerpt  string
}

// SearchReader 是全文检索的只读端口。实现负责 FTS5/LIKE 的选择与 SQL 转义，
// application 只决定「查询词是否合法」与「摘要/分页边界」。
type SearchReader interface {
	SearchPosts(ctx context.Context, q SearchQuery) ([]SearchHit, bool, error)
}

// UsernameReader 是用户名投影的只读端口（ADR-014 / ADR-019 的边界补充）。
// 领域对象 Thread/Post 只持有 UserID；展示层需要的 @username 是**外部文本**，
// 因此在组装 Screen 时按 ID 批量投影进 RowMeta.Author，绝不污染领域模型。
type UsernameReader interface {
	// UsernamesByIDs 返回 id → username。ids 为空时必须返回空 map 且不查库；
	// 查不到的 id 不出现在结果里（由渲染层回退到 #ID）。不按 status 过滤：
	// 历史帖的作者即使已注销，用户名仍应可读。
	UsernamesByIDs(ctx context.Context, ids []int64) (map[int64]string, error)
}

// WriteTx 是**单个事务内**的受限写入句柄。
// 用例闭包只能拿到它，拿不到 *sql.Tx / *sql.DB；一个业务用例对应一次 UnitOfWork.Write。
// 权限与限流查询也放在这里，保证「检查 + 写入」在同一事务快照内，不留 TOCTOU 窗口。
type WriteTx interface {
	UserStatus(ctx context.Context, userID int64) (user.Status, error)
	NodeExists(ctx context.Context, nodeID int64) (bool, error)
	// NodeIDBySlug 在写事务内解析用户给的 slug；不存在返回 ErrNotFound。
	// 目标解析与写入同快照，避免「检查后改名」的 TOCTOU 窗口。
	NodeIDBySlug(ctx context.Context, slug string) (int64, error)
	// CreateNode 插入一个 active 节点；slug 冲突必须返回 ErrNodeSlugExists。
	CreateNode(ctx context.Context, n NewNode) (int64, error)
	// RenameNode 只改 name，绝不改 slug（slug 是稳定标识）。
	RenameNode(ctx context.Context, id int64, newName string) error
	// ArchiveNode 只把 nodes.status 置为 archived；threads 一行不动，重复归档不报错。
	ArchiveNode(ctx context.Context, id int64) error
	ThreadExists(ctx context.Context, threadID int64) (bool, error)
	RecentWriteStats(ctx context.Context, userID int64, since time.Time) (WriteStats, error)
	InsertThread(ctx context.Context, n NewThread) (int64, error)
	InsertPost(ctx context.Context, p NewPost) (int64, error)
	// TouchThread 在同一条 UPDATE 里维护 last_reply_at 与 score，**仅供发帖/回复**。
	// 投票绝不能调用它：投票改分数但不推进 last_reply_at，否则 SortLatest 会把旧主题
	// 顶到最新。投票走下面的 AdjustScore。
	TouchThread(ctx context.Context, threadID int64, at time.Time, scoreDelta int64) error
	// GetVote 返回 (用户, 目标) 的当前投票值与是否存在；未投票返回 (0, false, nil)。
	GetVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64) (vote.Value, bool, error)
	// UpsertVote 写入或覆盖投票（按 UNIQUE(user_id,target_type,target_id) 冲突更新）。
	UpsertVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64, value vote.Value, at time.Time) error
	// DeleteVote 删除一条投票，供 Unvote 使用。投票值只允许 ±1，不能用 UpsertVote(0) 代替删除。
	DeleteVote(ctx context.Context, userID int64, targetType vote.TargetType, targetID int64) error
	// AdjustScore 只改 threads.score，**绝不触碰 last_reply_at**。
	// delta 是新旧投票值之差；保证与 UpsertVote 在同一事务内提交。
	AdjustScore(ctx context.Context, threadID int64, delta int64) error
	// PostThreadID 返回内容所属主题；内容不存在或不可见时返回 ErrNotFound。
	// 投票目标是 post 时用它确定分数记到哪个 thread。
	PostThreadID(ctx context.Context, postID int64) (int64, error)
	// ThreadOwner 返回主题作者 ID。主题不存在或 status<>'active' 时返回 ErrNotFound，
	// 让「取目标 + 判权 + 软删」落在同一事务快照内，不留 TOCTOU 窗口。
	ThreadOwner(ctx context.Context, threadID int64) (int64, error)
	// PostOwner 返回内容作者 ID。内容不存在或 status<>'normal' 时返回 ErrNotFound。
	PostOwner(ctx context.Context, postID int64) (int64, error)
	// SoftDeleteThread 只把 threads.status 置为 deleted：不物理删除、不级联回复。
	SoftDeleteThread(ctx context.Context, threadID int64) error
	// SoftDeletePost 只把 posts.status 置为 deleted：不物理删除、不级联。
	SoftDeletePost(ctx context.Context, postID int64) error
	// EditPost 只 UPDATE posts 的 body_md 与 edited_at，**绝不碰 status / created_at /
	// last_reply_at**：编辑不推进主题活跃度（同投票 AdjustScore 的教训 ADR-029），
	// 也不改变可见性。editedAt 是 Unix 秒（由用例的 now 提供）。历史行 edited_at IS NULL。
	EditPost(ctx context.Context, postID int64, newBody string, editedAt int64) error
	// HideThread / HidePost 只把 status 置为 hidden（治理动作，区别于作者自删的 deleted）：
	// 不物理删、不级联；已 hidden 重复调用是合法 no-op（幂等便于重试）；
	// 目标不存在或已被作者删除返回 ErrNotFound。与判权一起落在同一事务快照内。
	HideThread(ctx context.Context, threadID int64) error
	HidePost(ctx context.Context, postID int64) error
	// AppendOutbox 在业务写事务内追加一条领域事件（R12）。
	// 必须与业务写同提交、同回滚；event_id 唯一，重复事件静默去重。
	// 投递绝不在这里做——handler 只能在事务提交后由 Outbox 调度器调用。
	AppendOutbox(ctx context.Context, e event.DomainEvent) error
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
	SearchReader
	UsernameReader
	UnitOfWork
}
