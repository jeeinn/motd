package forum

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/domain/vote"
)

// P0-10 回归集：全部用内存 fake，不依赖 SQLite / SSH / 渲染。
// 这样业务规则可以单独跑、单独失败；真实 SQL 行为由 infrastructure/sqlite 的仓储测试覆盖。

var testBase = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// voteKey 是 UNIQUE(user_id,target_type,target_id) 的内存映射。
type voteKey struct {
	userID     int64
	targetType vote.TargetType
	targetID   int64
}

type fakeState struct {
	nodes        map[int64]node.Node
	threads      map[int64]thread.Thread
	posts        map[int64]post.Post
	users        map[int64]user.Status
	votes        map[voteKey]vote.Value
	outboxEvents []event.DomainEvent
	nextThreadID int64
	nextPostID   int64
}

func (s fakeState) clone() fakeState {
	out := fakeState{
		nodes:        make(map[int64]node.Node, len(s.nodes)),
		threads:      make(map[int64]thread.Thread, len(s.threads)),
		posts:        make(map[int64]post.Post, len(s.posts)),
		users:        make(map[int64]user.Status, len(s.users)),
		votes:        make(map[voteKey]vote.Value, len(s.votes)),
		nextThreadID: s.nextThreadID,
		nextPostID:   s.nextPostID,
		outboxEvents: append([]event.DomainEvent(nil), s.outboxEvents...),
	}
	for k, v := range s.nodes {
		out.nodes[k] = v
	}
	for k, v := range s.threads {
		out.threads[k] = v
	}
	for k, v := range s.posts {
		out.posts[k] = v
	}
	for k, v := range s.users {
		out.users[k] = v
	}
	for k, v := range s.votes {
		out.votes[k] = v
	}
	return out
}

type fakeRepos struct {
	state           fakeState
	failInsertPost  error // 注入失败，用于验证事务回滚
	failAdjustScore error // 注入失败，用于验证投票分数回滚
}

func newFakeRepos() *fakeRepos {
	return &fakeRepos{state: fakeState{
		nodes: map[int64]node.Node{
			1: {ID: 1, Slug: "/dev/ai", Name: "AI 与大模型", Description: "模型与工具链"},
			2: {ID: 2, Slug: "/dev/rust", Name: "Rust", Description: "Rust 生态"},
		},
		threads:      map[int64]thread.Thread{},
		posts:        map[int64]post.Post{},
		users:        map[int64]user.Status{1: user.StatusActive, 2: user.StatusActive},
		votes:        map[voteKey]vote.Value{},
		nextThreadID: 1,
		nextPostID:   1,
	}}
}

func (r *fakeRepos) addThread(t thread.Thread) {
	if t.ID == 0 {
		t.ID = r.state.nextThreadID
		r.state.nextThreadID++
	}
	r.state.threads[t.ID] = t
}

func (r *fakeRepos) addPost(p post.Post) {
	if p.ID == 0 {
		p.ID = r.state.nextPostID
		r.state.nextPostID++
	}
	r.state.posts[p.ID] = p
}

// ---------------------------------------------------------------------------
// 只读端口
// ---------------------------------------------------------------------------

func (r *fakeRepos) ListNodes(ctx context.Context) ([]node.Node, error) {
	out := make([]node.Node, 0, len(r.state.nodes))
	for _, n := range r.state.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *fakeRepos) GetNodeBySlug(ctx context.Context, slug string) (node.Node, error) {
	for _, n := range r.state.nodes {
		if n.Slug == slug {
			return n, nil
		}
	}
	return node.Node{}, ErrNotFound
}

func (r *fakeRepos) GetThread(ctx context.Context, id int64) (thread.Thread, error) {
	t, ok := r.state.threads[id]
	if !ok {
		return thread.Thread{}, ErrNotFound
	}
	return t, nil
}

func (r *fakeRepos) ListThreads(ctx context.Context, q ThreadQuery) ([]thread.Thread, bool, error) {
	if !q.Sort.Valid() {
		return nil, false, ErrInvalidSort
	}
	if !q.After.IsZero() && q.After.Sort != q.Sort {
		return nil, false, fmt.Errorf("%w: 游标排序与查询排序不一致", ErrInvalidCursor)
	}
	var out []thread.Thread
	for _, t := range r.state.threads {
		if t.NodeID != q.NodeID {
			continue
		}
		if !q.After.IsZero() && !beforeThreadCursor(t, q) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if q.Sort == SortLatest {
			if !a.LastReplyAt.Equal(b.LastReplyAt) {
				return a.LastReplyAt.After(b.LastReplyAt)
			}
		} else if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.ID > b.ID
	})
	limit := normalizePageSize(q.Limit)
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

func beforeThreadCursor(t thread.Thread, q ThreadQuery) bool {
	if q.Sort == SortLatest {
		sec := t.LastReplyAt.Unix()
		return sec < q.After.LastReplyAt || (sec == q.After.LastReplyAt && t.ID < q.After.ID)
	}
	return t.Score < q.After.Score || (t.Score == q.After.Score && t.ID < q.After.ID)
}

func (r *fakeRepos) ListPosts(ctx context.Context, q PostQuery) ([]post.Post, bool, error) {
	var out []post.Post
	for _, p := range r.state.posts {
		if p.ThreadID == q.ThreadID && p.Status == post.StatusNormal && p.ID > q.AfterID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	limit := normalizePageSize(q.Limit)
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// SearchPosts 是 SearchReader 的内存实现：只做子串匹配，FTS/LIKE 的真实差异
// 由 infrastructure 集成测试覆盖；这里锁定用例层的分页与摘要裁剪策略。
func (r *fakeRepos) SearchPosts(_ context.Context, q SearchQuery) ([]SearchHit, bool, error) {
	var out []SearchHit
	for _, p := range r.state.posts {
		if p.Status == post.StatusNormal && strings.Contains(p.BodyMD, q.Query) {
			out = append(out, SearchHit{ThreadID: p.ThreadID, PostID: p.ID, Excerpt: p.BodyMD})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PostID > out[j].PostID })
	limit := q.Limit
	if limit <= 0 {
		limit = SearchDefaultPageSize
	}
	if offset := q.Offset; offset >= len(out) {
		out = nil
	} else {
		out = out[offset:]
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ---------------------------------------------------------------------------
// UnitOfWork / WriteTx
// ---------------------------------------------------------------------------

// Write 在状态副本上执行闭包：返回错误则丢弃副本，成功才整体提交。
// 错误按真实 Writer 的方式包装一层，确保用例层的 errors.Is / errors.As 能穿透。
func (r *fakeRepos) Write(ctx context.Context, fn func(WriteTx) error) error {
	staged := r.state.clone()
	if err := fn(&fakeTx{
		state: &staged, failInsertPost: r.failInsertPost, failAdjustScore: r.failAdjustScore,
	}); err != nil {
		return fmt.Errorf("写入事务失败（不可重试）: %w", err)
	}
	r.state = staged
	return nil
}

type fakeTx struct {
	state           *fakeState
	failInsertPost  error
	failAdjustScore error
}

func (x *fakeTx) UserStatus(ctx context.Context, userID int64) (user.Status, error) {
	s, ok := x.state.users[userID]
	if !ok {
		return "", ErrNotFound
	}
	return s, nil
}

func (x *fakeTx) NodeExists(ctx context.Context, nodeID int64) (bool, error) {
	_, ok := x.state.nodes[nodeID]
	return ok, nil
}

func (x *fakeTx) ThreadExists(ctx context.Context, threadID int64) (bool, error) {
	_, ok := x.state.threads[threadID]
	return ok, nil
}

func (x *fakeTx) RecentWriteStats(ctx context.Context, userID int64, since time.Time) (WriteStats, error) {
	var stats WriteStats
	uniq := make(map[int64]struct{}) // 同一次发帖的 thread + 首帖共享时间戳，按动作去重
	observe := func(at time.Time) {
		uniq[at.Unix()] = struct{}{}
		accumulate(&stats, at)
	}
	for _, t := range x.state.threads {
		if t.UserID == userID && !t.CreatedAt.Before(since) {
			observe(t.CreatedAt)
		}
	}
	for _, p := range x.state.posts {
		if p.UserID == userID && !p.CreatedAt.Before(since) {
			observe(p.CreatedAt)
		}
	}
	stats.Count = len(uniq)
	return stats, nil
}

func accumulate(stats *WriteStats, at time.Time) {
	if stats.LastWriteAt.IsZero() || at.After(stats.LastWriteAt) {
		stats.LastWriteAt = at
	}
	if stats.OldestAt.IsZero() || at.Before(stats.OldestAt) {
		stats.OldestAt = at
	}
}

func (x *fakeTx) InsertThread(ctx context.Context, n NewThread) (int64, error) {
	id := x.state.nextThreadID
	x.state.nextThreadID++
	x.state.threads[id] = thread.Thread{
		ID: id, NodeID: n.NodeID, UserID: n.UserID, Title: n.Title,
		Score: n.Score, LastReplyAt: n.CreatedAt, CreatedAt: n.CreatedAt,
	}
	return id, nil
}

func (x *fakeTx) InsertPost(ctx context.Context, p NewPost) (int64, error) {
	if x.failInsertPost != nil {
		return 0, x.failInsertPost
	}
	id := x.state.nextPostID
	x.state.nextPostID++
	x.state.posts[id] = post.Post{
		ID: id, ThreadID: p.ThreadID, UserID: p.UserID,
		BodyMD: p.BodyMD, Status: p.Status, CreatedAt: p.CreatedAt,
	}
	return id, nil
}

func (x *fakeTx) AppendOutbox(ctx context.Context, e event.DomainEvent) error {
	x.state.outboxEvents = append(x.state.outboxEvents, e)
	return nil
}

func (x *fakeTx) TouchThread(ctx context.Context, threadID int64, at time.Time, scoreDelta int64) error {
	t, ok := x.state.threads[threadID]
	if !ok {
		return ErrNotFound
	}
	t.LastReplyAt = at
	t.Score += scoreDelta
	x.state.threads[threadID] = t
	return nil
}

func (x *fakeTx) GetVote(_ context.Context, userID int64, targetType vote.TargetType, targetID int64) (vote.Value, bool, error) {
	v, ok := x.state.votes[voteKey{userID, targetType, targetID}]
	return v, ok, nil
}

func (x *fakeTx) UpsertVote(_ context.Context, userID int64, targetType vote.TargetType, targetID int64, value vote.Value, _ time.Time) error {
	x.state.votes[voteKey{userID, targetType, targetID}] = value
	return nil
}

func (x *fakeTx) DeleteVote(_ context.Context, userID int64, targetType vote.TargetType, targetID int64) error {
	delete(x.state.votes, voteKey{userID, targetType, targetID})
	return nil
}

// AdjustScore 只改分数；fake 也刻意不碰 LastReplyAt，
// 这样任何误调 TouchThread 的投票实现都会在用例测试里暴露。
func (x *fakeTx) AdjustScore(_ context.Context, threadID int64, delta int64) error {
	if x.failAdjustScore != nil {
		return x.failAdjustScore
	}
	t, ok := x.state.threads[threadID]
	if !ok {
		return ErrNotFound
	}
	t.Score += delta
	x.state.threads[threadID] = t
	return nil
}

func (x *fakeTx) PostThreadID(_ context.Context, postID int64) (int64, error) {
	p, ok := x.state.posts[postID]
	if !ok || p.Status != post.StatusNormal {
		return 0, ErrNotFound
	}
	return p.ThreadID, nil
}

// 删除路径的 fake 语义：thread.Thread 没有 Status 字段（本轮不迁移），
// 因此把主题移出 map 来等价「对公开读取不可见」；posts 保留原行，只改 Status，
// 这样「回复行仍在库」在应用层测试里也能被断言。真实 SQL 行为由 sqlite 集成测试守。
func (x *fakeTx) ThreadOwner(_ context.Context, threadID int64) (int64, error) {
	t, ok := x.state.threads[threadID]
	if !ok {
		return 0, ErrNotFound
	}
	return t.UserID, nil
}

func (x *fakeTx) PostOwner(_ context.Context, postID int64) (int64, error) {
	p, ok := x.state.posts[postID]
	if !ok || p.Status != post.StatusNormal {
		return 0, ErrNotFound
	}
	return p.UserID, nil
}

func (x *fakeTx) SoftDeleteThread(_ context.Context, threadID int64) error {
	if _, ok := x.state.threads[threadID]; !ok {
		return ErrNotFound
	}
	delete(x.state.threads, threadID)
	return nil
}

func (x *fakeTx) SoftDeletePost(_ context.Context, postID int64) error {
	p, ok := x.state.posts[postID]
	if !ok || p.Status != post.StatusNormal {
		return ErrNotFound
	}
	p.Status = post.StatusDeleted
	x.state.posts[postID] = p
	return nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

type testEnv struct {
	repos *fakeRepos
	svc   *Service
	now   time.Time
}

func newTestEnv() *testEnv {
	e := &testEnv{repos: newFakeRepos(), now: testBase}
	e.svc = NewService(e.repos, WithClock(func() time.Time { return e.now }))
	return e
}

func activeActor() Actor {
	return Actor{UserID: 1, Status: user.StatusActive}
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

// ---------------------------------------------------------------------------
// 只读用例
// ---------------------------------------------------------------------------

func TestListNodes(t *testing.T) {
	e := newTestEnv()
	got, err := e.svc.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Slug != "/dev/ai" || got[1].Slug != "/dev/rust" {
		t.Fatalf("版块列表不符合预期: %+v", got)
	}
}

func TestListThreadsPagingAndCursor(t *testing.T) {
	e := newTestEnv()
	for i := 0; i < 5; i++ {
		e.repos.addThread(thread.Thread{
			NodeID: 1, UserID: 1, Title: fmt.Sprintf("t%d", i),
			Score:       int64(i * 10),
			LastReplyAt: testBase.Add(time.Duration(i) * time.Minute),
			CreatedAt:   testBase,
		})
	}

	var seen []int64
	cursor := ""
	for page := 0; page < 4; page++ {
		got, err := e.svc.ListThreads(context.Background(), ListThreadsQuery{
			NodeID: 1, Sort: SortLatest, Cursor: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("第 %d 页: %v", page+1, err)
		}
		if len(got.Rows) > 2 {
			t.Fatalf("第 %d 页返回 %d 行，超过 Limit=2", page+1, len(got.Rows))
		}
		for _, row := range got.Rows {
			seen = append(seen, row.ID)
		}
		if !got.HasMore {
			if got.NextCursor != "" {
				t.Fatal("没有下一页时 NextCursor 应为空")
			}
			break
		}
		if got.NextCursor == "" {
			t.Fatal("HasMore=true 时必须给出 NextCursor")
		}
		cursor = got.NextCursor
	}
	want := []int64{5, 4, 3, 2, 1} // last_reply_at 倒序
	if len(seen) != len(want) {
		t.Fatalf("应遍历 5 条，实际 %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("第 %d 位应为 %d，实际 %v", i+1, want[i], seen)
		}
	}

	// 最高分排序同样可用，且游标跨页不重复。
	page1, err := e.svc.ListThreads(context.Background(), ListThreadsQuery{NodeID: 1, Sort: SortScore, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page1.Rows[0].Score != 40 || page1.Rows[1].Score != 30 {
		t.Fatalf("最高分排序错误: %+v", page1.Rows)
	}
	page2, err := e.svc.ListThreads(context.Background(), ListThreadsQuery{
		NodeID: 1, Sort: SortScore, Cursor: page1.NextCursor, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Rows[0].ID == page1.Rows[1].ID {
		t.Fatal("跨页出现重复主题")
	}
}

func TestListThreadsRejectsBadQuery(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	if _, err := e.svc.ListThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: 0}); !errors.Is(err, ErrInvalidSort) {
		t.Fatalf("非法排序: %v", err)
	}
	if _, err := e.svc.ListThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Cursor: "!!!not-base64"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("乱码游标: %v", err)
	}
	cross := encodeThreadCursor(ThreadCursor{Sort: SortScore, Score: 10, ID: 1})
	if _, err := e.svc.ListThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Cursor: cross}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("排序错配的游标应被拒绝: %v", err)
	}
	// Limit 裁剪：超过上限时按 MaxPageSize 处理（不报错）。
	if _, err := e.svc.ListThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Limit: 9999}); err != nil {
		t.Fatalf("超大 Limit 应被裁剪而非报错: %v", err)
	}
}

func TestGetThreadFiltersDeletedPosts(t *testing.T) {
	e := newTestEnv()
	e.repos.addThread(thread.Thread{
		ID: 1, NodeID: 1, UserID: 1, Title: "主题", LastReplyAt: testBase, CreatedAt: testBase,
	})
	e.repos.addPost(post.Post{ThreadID: 1, BodyMD: "首帖", Status: post.StatusNormal, CreatedAt: testBase})
	e.repos.addPost(post.Post{ThreadID: 1, BodyMD: "删掉的", Status: post.StatusDeleted, CreatedAt: testBase})
	e.repos.addPost(post.Post{ThreadID: 1, BodyMD: "隐藏的", Status: post.StatusHidden, CreatedAt: testBase})

	got, err := e.svc.GetThread(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thread.Title != "主题" || len(got.Posts) != 1 || got.Posts[0].BodyMD != "首帖" {
		t.Fatalf("详情应只含 normal 内容: %+v", got)
	}
	if _, err := e.svc.GetThread(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失主题: %v", err)
	}
	if _, err := e.svc.GetThread(context.Background(), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非法 ID: %v", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := ThreadCursor{Sort: SortLatest, LastReplyAt: 1730000000, Score: 42, ID: 7}
	got, err := decodeThreadCursor(encodeThreadCursor(want))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("往返不一致: %+v vs %+v", got, want)
	}
	for _, bad := range []string{
		"YWJj", // 字段数不对
		encodeThreadCursor(ThreadCursor{Sort: 9, Score: 1, ID: 1}), // 非法排序
		encodeThreadCursor(ThreadCursor{Sort: SortLatest, ID: 0}),  // 缺锚点
	} {
		if _, err := decodeThreadCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("非法游标 %q 应被拒绝，实际 %v", bad, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 写入校验与权限
// ---------------------------------------------------------------------------

func TestCreateThreadValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		input CreateThreadInput
		want  error
	}{
		{"空标题", CreateThreadInput{NodeID: 1, Title: "", Body: "正文"}, ErrInvalidTitle},
		{"全空白标题", CreateThreadInput{NodeID: 1, Title: "   \t ", Body: "正文"}, ErrInvalidTitle},
		{"超长标题", CreateThreadInput{NodeID: 1, Title: strings.Repeat("中", 121), Body: "正文"}, ErrInvalidTitle},
		{"空正文", CreateThreadInput{NodeID: 1, Title: "标题", Body: ""}, ErrInvalidBody},
		{"全空白正文", CreateThreadInput{NodeID: 1, Title: "标题", Body: "\n \t"}, ErrInvalidBody},
		{"超 8KiB 正文", CreateThreadInput{NodeID: 1, Title: "标题", Body: strings.Repeat("x", MaxBodyBytes+1)}, ErrInvalidBody},
		{"非法版块", CreateThreadInput{NodeID: 0, Title: "标题", Body: "正文"}, ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv()
			_, err := e.svc.CreateThread(ctx, activeActor(), c.input)
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，实际 %v", c.want, err)
			}
			if len(e.repos.state.threads) != 0 || len(e.repos.state.posts) != 0 {
				t.Fatal("校验失败不应落任何数据")
			}
		})
	}

	// 边界值必须放行：120 中文标题 + 恰好 8 KiB 正文。
	e := newTestEnv()
	if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: strings.Repeat("中", MaxTitleRunes), Body: strings.Repeat("x", MaxBodyBytes),
	}); err != nil {
		t.Fatalf("边界值应通过: %v", err)
	}
}

func TestCreateThreadPermissions(t *testing.T) {
	ctx := context.Background()
	actors := []struct {
		name  string
		actor Actor
		want  error
	}{
		{"访客", Actor{Guest: true}, ErrReadOnly},
		{"零 UserID", Actor{UserID: 0, Status: user.StatusActive}, ErrReadOnly},
		{"pending", Actor{UserID: 1, Status: user.StatusPending}, ErrForbidden},
		{"deleting", Actor{UserID: 1, Status: user.StatusDeleting}, ErrForbidden},
		{"deleted", Actor{UserID: 1, Status: user.StatusDeleted}, ErrForbidden},
	}
	for _, c := range actors {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv()
			_, err := e.svc.CreateThread(ctx, c.actor, CreateThreadInput{NodeID: 1, Title: "标题", Body: "正文"})
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，实际 %v", c.want, err)
			}
		})
	}

	// Actor 快照是 active，但事务内重新读到的状态已被冻结 → 仍必须拒绝。
	t.Run("事务内状态复核", func(t *testing.T) {
		e := newTestEnv()
		e.repos.state.users[1] = user.StatusDeleting
		if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
			NodeID: 1, Title: "标题", Body: "正文",
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("事务内状态复核失效: %v", err)
		}
	})
	t.Run("用户不存在", func(t *testing.T) {
		e := newTestEnv()
		if _, err := e.svc.CreateThread(ctx, Actor{UserID: 999, Status: user.StatusActive}, CreateThreadInput{
			NodeID: 1, Title: "标题", Body: "正文",
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("不存在的用户应被拒绝: %v", err)
		}
	})
	t.Run("版块不存在", func(t *testing.T) {
		e := newTestEnv()
		if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
			NodeID: 999, Title: "标题", Body: "正文",
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("不存在的版块应被拒绝: %v", err)
		}
	})
}

func TestCreateThreadFirstPostInOneTransaction(t *testing.T) {
	e := newTestEnv()
	id, err := e.svc.CreateThread(context.Background(), activeActor(), CreateThreadInput{
		NodeID: 1, Title: "新主题", Body: "首帖正文",
	})
	if err != nil {
		t.Fatalf("创建主题失败: %v", err)
	}
	if id == 0 || len(e.repos.state.threads) != 1 || len(e.repos.state.posts) != 1 {
		t.Fatalf("主题 + 首帖应同时落地: id=%d threads=%d posts=%d",
			id, len(e.repos.state.threads), len(e.repos.state.posts))
	}
	var body string
	for _, p := range e.repos.state.posts {
		body = p.BodyMD
	}
	if body != "首帖正文" {
		t.Fatalf("首帖正文不正确: %q", body)
	}
	if !e.repos.state.threads[1].LastReplyAt.Equal(testBase) {
		t.Fatalf("主题初始 last_reply_at 应为 now，实际 %v", e.repos.state.threads[1].LastReplyAt)
	}

	// 注入 InsertPost 失败：主题不得残留（回滚语义）。
	e2 := newTestEnv()
	e2.repos.failInsertPost = errors.New("模拟首帖写入失败")
	if _, err := e2.svc.CreateThread(context.Background(), activeActor(), CreateThreadInput{
		NodeID: 1, Title: "会回滚", Body: "正文",
	}); err == nil {
		t.Fatal("首帖失败应返回错误")
	}
	if len(e2.repos.state.threads) != 0 || len(e2.repos.state.posts) != 0 {
		t.Fatalf("事务失败后不应残留数据: threads=%d posts=%d",
			len(e2.repos.state.threads), len(e2.repos.state.posts))
	}
}

func TestCreatePostUpdatesLastReplyAt(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "主题", Body: "首帖",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := e.repos.state.threads[threadID].LastReplyAt

	// 秒精度下同一秒不会推进 last_reply_at，用可控时钟跨过 1 秒验证。
	e.advance(90 * time.Second)
	postID, err := e.svc.CreatePost(ctx, activeActor(), CreatePostInput{ThreadID: threadID, Body: "回复"})
	if err != nil {
		t.Fatalf("回复失败: %v", err)
	}
	if postID == 0 || len(e.repos.state.posts) != 2 {
		t.Fatalf("应新增 1 条回复，实际 posts=%d", len(e.repos.state.posts))
	}
	after := e.repos.state.threads[threadID].LastReplyAt
	if !after.Equal(e.now) || before.Equal(after) {
		t.Fatalf("last_reply_at 必须变化且等于回复时间: before=%v after=%v now=%v", before, after, e.now)
	}
	if e.repos.state.threads[threadID].Score != 0 {
		t.Fatal("P0 无投票，score 不应被改动")
	}

	if _, err := e.svc.CreatePost(ctx, activeActor(), CreatePostInput{ThreadID: 999, Body: "回复"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主题应被拒绝: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 限流（Q6）
// ---------------------------------------------------------------------------

func TestRateLimit60SecondCooldown(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "第一帖", Body: "正文",
	}); err != nil {
		t.Fatal(err)
	}

	e.advance(10 * time.Second)
	_, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "第二帖", Body: "正文",
	})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("60 秒内第二帖应触发限流，实际 %v", err)
	}
	if rl.RetryAfter != 50*time.Second {
		t.Fatalf("剩余冷却应为 50s，实际 %v", rl.RetryAfter)
	}
	if !strings.Contains(rl.Error(), "50s") {
		t.Fatalf("错误信息应包含冷却时间: %q", rl.Error())
	}

	// 冷却结束后放行。
	e.advance(51 * time.Second)
	if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "第三帖", Body: "正文",
	}); err != nil {
		t.Fatalf("冷却结束后应可发帖: %v", err)
	}
}

// 一次 CreateThread 会写入主题 + 首帖两行，但只应消耗 1 次发帖配额。
func TestRateLimitCountsThreadPlusFirstPostAsOneEvent(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "主题", Body: "首帖",
	}); err != nil {
		t.Fatal(err)
	}
	if len(e.repos.state.threads) != 1 || len(e.repos.state.posts) != 1 {
		t.Fatal("主题与首帖应同时落地")
	}
	var stats WriteStats
	if err := e.repos.Write(ctx, func(tx WriteTx) error {
		var err error
		stats, err = tx.RecentWriteStats(ctx, 1, testBase.Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stats.Count != 1 {
		t.Fatalf("一次发帖应只计 1 次配额，实际 %d", stats.Count)
	}
}

func TestRateLimitSharedByThreadAndPost(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "主题", Body: "首帖",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(5 * time.Second)
	_, err = e.svc.CreatePost(ctx, activeActor(), CreatePostInput{ThreadID: threadID, Body: "立刻回复"})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("主题与回复共用配额，立刻回复应被限流: %v", err)
	}
	if len(e.repos.state.posts) != 1 {
		t.Fatal("被限流的回复不应落库")
	}
}

func TestRateLimitHourlyQuota(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	// 每次间隔 61s 绕过 60 秒冷却，累计 10 次写入。
	for i := 0; i < MaxWritesPerWindow; i++ {
		if i > 0 {
			e.advance(RateLimitInterval + time.Second)
		}
		if _, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
			NodeID: 1, Title: fmt.Sprintf("第%d帖", i+1), Body: "正文",
		}); err != nil {
			t.Fatalf("第 %d 帖不应被限流: %v", i+1, err)
		}
	}
	if len(e.repos.state.threads) != MaxWritesPerWindow {
		t.Fatalf("应有 %d 个主题，实际 %d", MaxWritesPerWindow, len(e.repos.state.threads))
	}

	e.advance(RateLimitInterval + time.Second)
	_, err := e.svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "第11帖", Body: "正文",
	})
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("第 11 帖应触发小时配额，实际 %v", err)
	}
	if !strings.Contains(rl.Reason, "1 小时") {
		t.Fatalf("应报告小时配额，实际原因 %q", rl.Reason)
	}
	if rl.RetryAfter <= 0 {
		t.Fatal("小时配额的剩余冷却时间必须为正")
	}
	if len(e.repos.state.threads) != MaxWritesPerWindow {
		t.Fatal("被限流的主题不应落库")
	}
}

func TestValidateHelpers(t *testing.T) {
	if err := validateTitle("   "); !errors.Is(err, ErrInvalidTitle) {
		t.Fatalf("空白标题: %v", err)
	}
	if err := validateBody(strings.Repeat("中", MaxBodyBytes)); err == nil {
		t.Fatal("8KiB 上限按字节计，中文内容必须被拒绝")
	}
	if got := normalizePageSize(0); got != DefaultPageSize {
		t.Fatalf("Limit=0 应回落默认页大小，实际 %d", got)
	}
	if got := normalizePageSize(MaxPageSize + 1); got != MaxPageSize {
		t.Fatalf("超大 Limit 应裁剪到上限，实际 %d", got)
	}
	if err := checkRate(WriteStats{}, testBase); err != nil {
		t.Fatalf("无写入记录不应限流: %v", err)
	}
}
