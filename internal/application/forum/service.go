package forum

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

// Actor 是一次调用的发起者。M2 的 auth.Principal 会映射到这里，
// 因此 P0-10 不依赖任何认证实现，也不 import transport。
type Actor struct {
	UserID int64
	Status user.Status
	Guest  bool
}

// checkWrite 是写用例的统一入口判定：访客只读，非 active 冻结。
func (a Actor) checkWrite() error {
	if a.Guest || a.UserID <= 0 {
		return ErrReadOnly
	}
	if !a.Status.CanPost() {
		return ErrForbidden
	}
	return nil
}

// Service 编排论坛用例。事务边界在这里确定：一个用例 = 一次 repos.Write。
// 事件只在事务提交后发布；投递失败不改变已提交的业务结果，只计数并上报。
type Service struct {
	repos           Repos
	now             func() time.Time
	sink            DomainEventSink
	onPublishErr    PublishObserver
	publishFailures atomic.Int64
}

// ServiceOption 用于注入时钟与事件出口；限流判定必须可测，不能靠真实等待 60 秒。
type ServiceOption func(*Service)

func WithClock(f func() time.Time) ServiceOption {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithEventSink 注入领域事件出口；不注入时用例行为完全不变（P0-10 回归集不受影响）。
func WithEventSink(sink DomainEventSink) ServiceOption {
	return func(s *Service) { s.sink = sink }
}

// WithPublishObserver 注入投递失败观察者，装配层通常接 slog。
func WithPublishObserver(obs PublishObserver) ServiceOption {
	return func(s *Service) { s.onPublishErr = obs }
}

// PublishFailures 是事件投递失败次数，供运维观测。
func (s *Service) PublishFailures() int64 { return s.publishFailures.Load() }

// publish 在 repos.Write 成功返回后调用：此刻事务已提交，订阅者看到的数据是确定的。
func (s *Service) publish(ctx context.Context, e event.DomainEvent) {
	if s.sink == nil {
		return
	}
	if err := s.sink.Publish(ctx, e); err != nil {
		s.publishFailures.Add(1)
		if s.onPublishErr != nil {
			s.onPublishErr(e, err)
		}
	}
}

func NewService(repos Repos, opts ...ServiceOption) *Service {
	s := &Service{repos: repos, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ---------------------------------------------------------------------------
// 只读用例
// ---------------------------------------------------------------------------

// ListNodes 返回全部版块（P0 只有 5 个，无需分页）。
func (s *Service) ListNodes(ctx context.Context) ([]node.Node, error) {
	return s.repos.ListNodes(ctx)
}

// ListThreadsQuery 是 transport 层传入的列表参数。Cursor 是不透明字符串。
type ListThreadsQuery struct {
	NodeID int64
	Sort   ThreadSort
	Cursor string
	Limit  int
}

// ThreadPage 是列表结果；NextCursor 为空表示没有下一页。
type ThreadPage struct {
	Rows       []thread.Thread
	NextCursor string
	HasMore    bool
}

func (s *Service) ListThreads(ctx context.Context, q ListThreadsQuery) (*ThreadPage, error) {
	if !q.Sort.Valid() {
		return nil, ErrInvalidSort
	}
	limit := normalizePageSize(q.Limit)

	after, err := decodeThreadCursor(q.Cursor)
	if err != nil {
		return nil, err
	}
	if !after.IsZero() && after.Sort != q.Sort {
		return nil, fmt.Errorf("%w: 游标排序与查询排序不一致", ErrInvalidCursor)
	}

	rows, hasMore, err := s.repos.ListThreads(ctx, ThreadQuery{
		NodeID: q.NodeID, Sort: q.Sort, After: after, Limit: limit,
	})
	if err != nil {
		return nil, err
	}

	page := &ThreadPage{Rows: rows, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = encodeThreadCursor(ThreadCursor{
			Sort:        q.Sort,
			LastReplyAt: last.LastReplyAt.Unix(),
			Score:       last.Score,
			ID:          last.ID,
		})
	}
	return page, nil
}

// ThreadDetail 是主题详情：主题本身 + 可见内容（软删除由仓储过滤）。
type ThreadDetail struct {
	Thread  thread.Thread
	Posts   []post.Post
	HasMore bool
}

func (s *Service) GetThread(ctx context.Context, threadID int64) (*ThreadDetail, error) {
	if threadID <= 0 {
		return nil, fmt.Errorf("%w: 主题 ID 非法", ErrNotFound)
	}
	t, err := s.repos.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	posts, hasMore, err := s.repos.ListPosts(ctx, PostQuery{
		ThreadID: threadID, Limit: MaxPostsPerThread,
	})
	if err != nil {
		return nil, err
	}
	return &ThreadDetail{Thread: t, Posts: posts, HasMore: hasMore}, nil
}

// ---------------------------------------------------------------------------
// 写入用例
// ---------------------------------------------------------------------------

type CreateThreadInput struct {
	NodeID int64
	Title  string
	Body   string
}

// CreateThread 在一个事务内创建主题 + 首帖（§5.2：Post 含首帖与全部回复）。
// 权限、版块有效性、限流统计、两次插入全部落在同一事务快照里。
func (s *Service) CreateThread(ctx context.Context, actor Actor, in CreateThreadInput) (int64, error) {
	if err := actor.checkWrite(); err != nil {
		return 0, err
	}
	if err := validateTitle(in.Title); err != nil {
		return 0, err
	}
	if err := validateBody(in.Body); err != nil {
		return 0, err
	}
	if in.NodeID <= 0 {
		return 0, fmt.Errorf("%w: 版块 ID 非法", ErrNotFound)
	}

	now := s.now().UTC()
	var threadID, postID int64
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		if err := s.checkWritePermission(ctx, tx, actor.UserID); err != nil {
			return err
		}
		exists, err := tx.NodeExists(ctx, in.NodeID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: 版块不存在", ErrNotFound)
		}
		stats, err := tx.RecentWriteStats(ctx, actor.UserID, now.Add(-RateLimitWindow))
		if err != nil {
			return err
		}
		if err := checkRate(stats, now); err != nil {
			return err
		}

		threadID, err = tx.InsertThread(ctx, NewThread{
			NodeID: in.NodeID, UserID: actor.UserID, Title: in.Title,
			Score: 0, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		postID, err = tx.InsertPost(ctx, NewPost{
			ThreadID: threadID, UserID: actor.UserID, BodyMD: in.Body,
			Status: post.StatusNormal, CreatedAt: now,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	// 事务已提交才发布：订阅者（插件）看到的是确定状态，不会读到未提交数据。
	s.publish(ctx, event.DomainEvent{
		ID:         fmt.Sprintf("post:%d", postID),
		Type:       event.EventPostCreated,
		OccurredAt: now,
		ActorID:    actor.UserID,
		NodeID:     in.NodeID,
		ThreadID:   threadID,
		PostID:     postID,
		FirstPost:  true,
	})
	return threadID, nil
}

type CreatePostInput struct {
	ThreadID int64
	Body     string
}

// CreatePost 在同一事务内插入回复并更新主题活跃度。
func (s *Service) CreatePost(ctx context.Context, actor Actor, in CreatePostInput) (int64, error) {
	if err := actor.checkWrite(); err != nil {
		return 0, err
	}
	if err := validateBody(in.Body); err != nil {
		return 0, err
	}
	if in.ThreadID <= 0 {
		return 0, fmt.Errorf("%w: 主题 ID 非法", ErrNotFound)
	}

	now := s.now().UTC()
	var postID int64
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		if err := s.checkWritePermission(ctx, tx, actor.UserID); err != nil {
			return err
		}
		exists, err := tx.ThreadExists(ctx, in.ThreadID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: 主题不存在", ErrNotFound)
		}
		stats, err := tx.RecentWriteStats(ctx, actor.UserID, now.Add(-RateLimitWindow))
		if err != nil {
			return err
		}
		if err := checkRate(stats, now); err != nil {
			return err
		}

		postID, err = tx.InsertPost(ctx, NewPost{
			ThreadID: in.ThreadID, UserID: actor.UserID, BodyMD: in.Body,
			Status: post.StatusNormal, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		// 一条 UPDATE 同时维护 last_reply_at 与 score。
		// P0 无投票来源，scoreDelta 恒为 0；P1 的投票可直接复用这条事务路径。
		return tx.TouchThread(ctx, in.ThreadID, now, 0)
	})
	if err != nil {
		return 0, err
	}
	s.publish(ctx, event.DomainEvent{
		ID:         fmt.Sprintf("post:%d", postID),
		Type:       event.EventPostCreated,
		OccurredAt: now,
		ActorID:    actor.UserID,
		ThreadID:   in.ThreadID,
		PostID:     postID,
		FirstPost:  false,
	})
	return postID, nil
}

// checkWritePermission 在事务内重新读取账号状态：
// Actor 携带的状态是会话建立时的快照，排队期间账号可能已被冻结。
func (s *Service) checkWritePermission(ctx context.Context, tx WriteTx, userID int64) error {
	status, err := tx.UserStatus(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: 用户不存在", ErrForbidden)
	}
	if err != nil {
		return err
	}
	if !status.CanPost() {
		return fmt.Errorf("%w: 当前状态 %q", ErrForbidden, status)
	}
	return nil
}
