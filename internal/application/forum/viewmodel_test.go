package forum

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/viewmodel"
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
