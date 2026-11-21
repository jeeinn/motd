package forum

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
)

// P0-11 回归集：ViewModel 由真实用例结果构造，仍走内存 fake，不依赖 SSH / 渲染。
// 目的是锁死「领域结果 → 语义状态」的映射，防止以后有人把格式化逻辑塞回 application。

func testViewContext() ViewContext {
	return ViewContext{
		Online:   12,
		Active:   7,
		Guest:    false,
		Username: "中文用户",
		Karma:    60, // Q5：落在 L3
		Context:  "/dev/ai",
	}
}

func TestScreenNodes(t *testing.T) {
	ctx := context.Background()
	s := NewService(newFakeRepos(), WithClock(func() time.Time { return testBase }))
	sc, err := s.ScreenNodes(ctx, testViewContext())
	if err != nil {
		t.Fatalf("构造版块屏: %v", err)
	}
	if sc.Kind != viewmodel.ScreenNodes || len(sc.Rows) != 2 {
		t.Fatalf("页面种类或行数不对: kind=%d rows=%d", sc.Kind, len(sc.Rows))
	}
	if sc.Rows[0].Index != 1 || sc.Rows[1].Index != 2 {
		t.Fatalf("序号应 1-based 且递增: %d, %d", sc.Rows[0].Index, sc.Rows[1].Index)
	}
	if sc.Rows[0].Meta.Node != "/dev/ai" || sc.Rows[0].Meta.Extra != "模型与工具链" {
		t.Fatalf("版块元信息映射错误: %+v", sc.Rows[0].Meta)
	}
	if sc.StatusBar.Level != 3 || sc.StatusBar.Karma != 60 {
		t.Fatalf("等级分档错误: level=%d karma=%d", sc.StatusBar.Level, sc.StatusBar.Karma)
	}
	if sc.StatusBar.Online != 12 || sc.StatusBar.Active != 7 || sc.StatusBar.Identity.Username != "中文用户" {
		t.Fatalf("状态栏上下文丢失: %+v", sc.StatusBar)
	}
	if sc.Selection.FocusedIndex != 1 || sc.Pagination.FirstIndex != 1 || sc.Pagination.Loaded != 2 {
		t.Fatalf("焦点或分页语义错误: %+v %+v", sc.Selection, sc.Pagination)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("版块屏未通过自洽校验: %v", err)
	}
}

func TestScreenThreads(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(thread.Thread{
		ID: 1, NodeID: 1, UserID: 1, Title: "较早", Score: 9,
		LastReplyAt: testBase.Add(10 * time.Second), CreatedAt: testBase,
	})
	repos.addThread(thread.Thread{
		ID: 2, NodeID: 1, UserID: 1, Title: "较新", Score: 3,
		LastReplyAt: testBase.Add(30 * time.Second), CreatedAt: testBase.Add(5 * time.Second),
	})
	s := NewService(repos, WithClock(func() time.Time { return testBase }))

	sc, err := s.ScreenThreads(ctx, ListThreadsQuery{
		NodeID: 1, Sort: SortLatest, Limit: 1,
	}, testViewContext())
	if err != nil {
		t.Fatalf("构造主题屏: %v", err)
	}
	if sc.Kind != viewmodel.ScreenThreads || len(sc.Rows) != 1 {
		t.Fatalf("页面种类或行数不对: %d / %d", sc.Kind, len(sc.Rows))
	}
	if sc.Rows[0].Title != "较新" || sc.Rows[0].Score != 3 || sc.Rows[0].ID != 2 {
		t.Fatalf("主题行映射错误: %+v", sc.Rows[0])
	}
	if sc.Rows[0].Meta.LastReplyAt != testBase.Add(30*time.Second).Unix() {
		t.Fatalf("时间应存 Unix 秒而非格式化文本: %+v", sc.Rows[0].Meta)
	}
	if !sc.Pagination.HasMore || sc.Pagination.NextCursor == "" {
		t.Fatal("第一页应带 hasMore 与不透明游标")
	}
	if sc.Rows[0].Body != "" {
		t.Fatal("列表行不应承载正文")
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("主题屏未通过自洽校验: %v", err)
	}

	// 翻页：序号必须接着上一页，而不是回到 1。
	v := testViewContext()
	v.RowOffset = 1
	next, err := s.ScreenThreads(ctx, ListThreadsQuery{
		NodeID: 1, Sort: SortLatest, Limit: 1, Cursor: sc.Pagination.NextCursor,
	}, v)
	if err != nil {
		t.Fatalf("翻页: %v", err)
	}
	if len(next.Rows) != 1 || next.Rows[0].Index != 2 || next.Rows[0].Title != "较早" {
		t.Fatalf("翻页序号或内容错误: %+v", next.Rows)
	}
	if next.Pagination.FirstIndex != 2 || next.Pagination.HasMore {
		t.Fatalf("第二页分页语义错误: %+v", next.Pagination)
	}
}

func TestScreenThreadDetail(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(thread.Thread{
		ID: 7, NodeID: 1, UserID: 1, Title: "标题", Score: 0,
		LastReplyAt: testBase.Add(20 * time.Second), CreatedAt: testBase,
	})
	repos.addPost(post.Post{ID: 1, ThreadID: 7, UserID: 1, BodyMD: "首帖正文", Status: post.StatusNormal, CreatedAt: testBase})
	repos.addPost(post.Post{ID: 2, ThreadID: 7, UserID: 2, BodyMD: "回复一", Status: post.StatusNormal, CreatedAt: testBase.Add(10 * time.Second)})
	repos.addPost(post.Post{ID: 3, ThreadID: 7, UserID: 1, BodyMD: "已删除", Status: post.StatusDeleted, CreatedAt: testBase.Add(20 * time.Second)})
	s := NewService(repos, WithClock(func() time.Time { return testBase }))

	sc, err := s.ScreenThreadDetail(ctx, 7, testViewContext())
	if err != nil {
		t.Fatalf("构造详情屏: %v", err)
	}
	if sc.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("页面种类错误: %d", sc.Kind)
	}
	if sc.Detail.ID != 7 || sc.Detail.Title != "标题" || sc.Detail.Body != "首帖正文" {
		t.Fatalf("主题正文应取首帖: %+v", sc.Detail)
	}
	if len(sc.Rows) != 1 || sc.Rows[0].Body != "回复一" || sc.Rows[0].Index != 1 {
		t.Fatalf("回复行应排除首帖与软删除内容: %+v", sc.Rows)
	}
	if sc.Rows[0].Meta.AuthorID != 2 {
		t.Fatalf("作者 ID 应保留语义值: %+v", sc.Rows[0].Meta)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("详情屏未通过自洽校验: %v", err)
	}
}

func TestNoticeFromError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantKind  viewmodel.NoticeKind
		wantCode  string
		wantRetry int
	}{
		{"无错误", nil, viewmodel.NoticeNone, "", 0},
		{"限流", &RateLimitError{Reason: "60 秒 1 帖", RetryAfter: 42 * time.Second},
			viewmodel.NoticeWarning, "rate_limited", 42},
		{"限流不足一秒", &RateLimitError{Reason: "x", RetryAfter: 400 * time.Millisecond},
			viewmodel.NoticeWarning, "rate_limited", 1},
		{"访客只读", ErrReadOnly, viewmodel.NoticeInfo, "read_only", 0},
		{"禁止写入", ErrForbidden, viewmodel.NoticeWarning, "forbidden", 0},
		{"对象不存在", ErrNotFound, viewmodel.NoticeWarning, "not_found", 0},
		{"标题校验", &ValidationError{Field: "title", Reason: "空白"}, viewmodel.NoticeWarning, "invalid_input", 0},
		{"游标非法", errors.Join(ErrInvalidCursor, errors.New("detail")), viewmodel.NoticeWarning, "invalid_input", 0},
		{"未知错误", errors.New("boom"), viewmodel.NoticeError, "internal", 0},
	}
	for _, c := range cases {
		n := NoticeFromError(c.err)
		if n.Kind != c.wantKind || n.Code != c.wantCode || n.RetryAfterSec != c.wantRetry {
			t.Errorf("%s: 得到 %+v，期望 kind=%d code=%q retry=%d",
				c.name, n, c.wantKind, c.wantCode, c.wantRetry)
		}
		if c.err != nil && n.Message == "" {
			t.Errorf("%s: 提示文本不应为空", c.name)
		}
	}
}

func TestWithNoticeKeepsScreen(t *testing.T) {
	ctx := context.Background()
	s := NewService(newFakeRepos(), WithClock(func() time.Time { return testBase }))
	sc, err := s.ScreenNodes(ctx, testViewContext())
	if err != nil {
		t.Fatal(err)
	}
	out := WithNotice(sc, NoticeFromError(ErrReadOnly))
	if len(out.Rows) != len(sc.Rows) || out.Kind != sc.Kind {
		t.Fatal("挂提示时不应破坏现有屏幕内容")
	}
	if out.StatusBar.Notice.Code != "read_only" {
		t.Fatalf("提示未挂上: %+v", out.StatusBar.Notice)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("带提示的屏幕应仍自洽: %v", err)
	}
}

// 作者名投影：一屏一次批量查询，id 查不到时留空交给渲染层回退。

func TestScreenThreadsProjectsAuthorsInOneBatch(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(thread.Thread{ID: 1, NodeID: 1, UserID: 1, Title: "作者 alice",
		LastReplyAt: testBase, CreatedAt: testBase})
	repos.addThread(thread.Thread{ID: 2, NodeID: 1, UserID: 2, Title: "作者 bob",
		LastReplyAt: testBase.Add(time.Second), CreatedAt: testBase})
	repos.addThread(thread.Thread{ID: 3, NodeID: 1, UserID: 99, Title: "无用户名",
		LastReplyAt: testBase.Add(2 * time.Second), CreatedAt: testBase})
	s := NewService(repos)

	sc, err := s.ScreenThreads(ctx, ListThreadsQuery{
		NodeID: 1, Sort: SortLatest, Limit: 10,
	}, testViewContext())
	if err != nil {
		t.Fatalf("构造主题屏: %v", err)
	}
	got := make(map[string]string)
	for _, r := range sc.Rows {
		got[r.Title] = r.Meta.Author
		if r.Meta.AuthorID <= 0 {
			t.Errorf("作者 ID 语义字段丢失: %+v", r.Meta)
		}
	}
	if got["作者 alice"] != "alice" || got["作者 bob"] != "bob" {
		t.Fatalf("作者 username 投影错误: %+v", got)
	}
	if name, ok := got["无用户名"]; !ok || name != "" {
		t.Fatalf("查不到的 id 应留空（渲染层回退 #ID），实际 %q ok=%v", name, ok)
	}
	if len(repos.usernameCalls) != 1 {
		t.Fatalf("一屏应恰好一次批量查询（非 N+1），实际 %d 次: %v", len(repos.usernameCalls), repos.usernameCalls)
	}
}

func TestScreenThreadDetailProjectsAuthorsInOneBatch(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(thread.Thread{ID: 7, NodeID: 1, UserID: 1, Title: "标题",
		LastReplyAt: testBase, CreatedAt: testBase})
	repos.addPost(post.Post{ID: 1, ThreadID: 7, UserID: 1, BodyMD: "首帖", Status: post.StatusNormal, CreatedAt: testBase})
	repos.addPost(post.Post{ID: 2, ThreadID: 7, UserID: 2, BodyMD: "回复 bob", Status: post.StatusNormal, CreatedAt: testBase.Add(time.Second)})
	repos.addPost(post.Post{ID: 3, ThreadID: 7, UserID: 99, BodyMD: "回复无用户名", Status: post.StatusNormal, CreatedAt: testBase.Add(2 * time.Second)})
	s := NewService(repos)

	sc, err := s.ScreenThreadDetail(ctx, 7, testViewContext())
	if err != nil {
		t.Fatalf("构造详情屏: %v", err)
	}
	if sc.Detail.AuthorID != 1 || sc.Detail.Author != "alice" {
		t.Fatalf("详情顶部作者应取首帖作者并投影 username: %+v", sc.Detail)
	}
	got := make(map[string]string)
	for _, r := range sc.Rows {
		got[r.Body] = r.Meta.Author
	}
	if got["回复 bob"] != "bob" || got["回复无用户名"] != "" {
		t.Fatalf("回复行作者投影错误: %+v", got)
	}
	if len(repos.usernameCalls) != 1 {
		t.Fatalf("详情屏应恰好一次批量查询（非 N+1），实际 %d 次", len(repos.usernameCalls))
	}
}

func TestScreenThreadsAuthorProjectionError(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	sentinel := errors.New("用户名查询失败")
	repos.failUsernames = sentinel
	repos.addThread(thread.Thread{ID: 1, NodeID: 1, UserID: 1, Title: "标题",
		LastReplyAt: testBase, CreatedAt: testBase})
	s := NewService(repos)

	if _, err := s.ScreenThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Limit: 10}, testViewContext()); !errors.Is(err, sentinel) {
		t.Fatalf("投影查询失败必须上抛，不得静默丢失作者: %v", err)
	}
}

// 回复数投影（P2-M3 / ADR-052）：一屏一次批量查询，计数按 thread ID 落到 RowMeta.Replies。

func TestScreenThreadsProjectsReplyCountsInOneBatch(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	for i, title := range []string{"无回复", "三条回复", "十二条回复"} {
		repos.addThread(thread.Thread{
			ID: int64(i + 1), NodeID: 1, UserID: 1, Title: title,
			LastReplyAt: testBase.Add(time.Duration(i) * time.Second), CreatedAt: testBase,
		})
	}
	repos.replyCounts = map[int64]int{1: 0, 2: 3, 3: 12}
	s := NewService(repos)

	sc, err := s.ScreenThreads(ctx, ListThreadsQuery{
		NodeID: 1, Sort: SortLatest, Limit: 10,
	}, testViewContext())
	if err != nil {
		t.Fatalf("构造主题屏: %v", err)
	}
	got := make(map[string]int)
	for _, r := range sc.Rows {
		got[r.Title] = r.Meta.Replies
	}
	if got["无回复"] != 0 || got["三条回复"] != 3 || got["十二条回复"] != 12 {
		t.Fatalf("回复数投影错误: %+v", got)
	}
	if len(repos.replyCountCalls) != 1 {
		t.Fatalf("一屏应恰好一次批量查询（非 N+1），实际 %d 次: %v",
			len(repos.replyCountCalls), repos.replyCountCalls)
	}
	ids := repos.replyCountCalls[0]
	if len(ids) != 3 {
		t.Fatalf("批量查询应收到本屏 3 个 thread ID，实际 %v", ids)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("主题屏未通过自洽校验: %v", err)
	}
}

func TestScreenThreadsReplyCountProjectionError(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	sentinel := errors.New("回复数查询失败")
	repos.failReplyCounts = sentinel
	repos.addThread(thread.Thread{ID: 1, NodeID: 1, UserID: 1, Title: "标题",
		LastReplyAt: testBase, CreatedAt: testBase})
	s := NewService(repos)

	if _, err := s.ScreenThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Limit: 10}, testViewContext()); !errors.Is(err, sentinel) {
		t.Fatalf("回复数投影查询失败必须上抛，不得静默显示 r0: %v", err)
	}
}

func TestScreenThreadsEmptyPageSkipsReplyCountQuery(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	s := NewService(repos)

	sc, err := s.ScreenThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest, Limit: 10}, testViewContext())
	if err != nil {
		t.Fatalf("空列表: %v", err)
	}
	if len(sc.Rows) != 0 {
		t.Fatalf("应为空屏: %+v", sc.Rows)
	}
	if len(repos.replyCountCalls) != 0 {
		t.Fatalf("空列表不应触发回复数查询，实际 %v", repos.replyCountCalls)
	}
}

func TestOnlyThreadListProjectsReplyCounts(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(thread.Thread{ID: 7, NodeID: 1, UserID: 1, Title: "标题",
		LastReplyAt: testBase, CreatedAt: testBase})
	repos.addPost(post.Post{ID: 1, ThreadID: 7, UserID: 1, BodyMD: "首帖", Status: post.StatusNormal, CreatedAt: testBase})
	repos.addPost(post.Post{ID: 2, ThreadID: 7, UserID: 2, BodyMD: "回复 关键词", Status: post.StatusNormal, CreatedAt: testBase.Add(time.Second)})
	s := NewService(repos)

	if _, err := s.ScreenThreadDetail(ctx, 7, testViewContext()); err != nil {
		t.Fatalf("详情屏: %v", err)
	}
	if _, err := s.ScreenSearchResults(ctx, SearchInput{Query: "关键词"}, testViewContext()); err != nil {
		t.Fatalf("搜索屏: %v", err)
	}
	if len(repos.replyCountCalls) != 0 {
		t.Fatalf("详情页 Rows 与搜索屏不应投影回复数，实际 %v", repos.replyCountCalls)
	}
}

// v0.6.0 / P3-M6：ViewContext 注入的已解锁隐藏节点并入列表屏，且按 ID 去重。
func TestScreenNodesWithUnlockedHiddenNode(t *testing.T) {
	ctx := context.Background()
	s := NewService(newFakeRepos(), WithClock(func() time.Time { return testBase }))
	v := testViewContext()
	v.UnlockedHiddenNodes = []node.Node{
		{ID: 99, Slug: "/dev/null", Name: "黑洞", Description: "隐藏版块"},
		{ID: 1, Slug: "/dev/ai", Name: "重复项", Description: "应被去重"}, // 与已有 ID 1 重复
	}
	sc, err := s.ScreenNodes(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Rows) != 3 { // 原有 2 个 + 解锁 1 个（重复的 ID 1 被过滤）
		t.Fatalf("应显示 3 行，实际 %d", len(sc.Rows))
	}
	last := sc.Rows[len(sc.Rows)-1]
	if last.Meta.Node != "/dev/null" || last.Index != 3 {
		t.Errorf("隐藏节点应在列表末尾且序号连续: %+v", last)
	}
	for i, r := range sc.Rows {
		if r.Index != i+1 {
			t.Errorf("行序号应唯一且递增: %+v", sc.Rows)
			break
		}
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("注入后屏幕未通过自洽校验: %v", err)
	}
}
