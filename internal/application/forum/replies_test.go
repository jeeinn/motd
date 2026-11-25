package forum

import (
	"context"
	"errors"
	"testing"

	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
)

// R-06 查询链：正文与回复拆开后，三条语义必须成立——
// ThreadHead 只取正文；ListReplies 按 keyset 续页且不含正文；ListRepliesTail 取末页。

func seedThreadWithReplies(e *testEnv, replies int) (threadID, opID int64) {
	e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "分页主题", CreatedAt: testBase})
	e.repos.addPost(post.Post{
		ID: 100, ThreadID: 10, UserID: 7, BodyMD: "主题正文",
		Status: post.StatusNormal, CreatedAt: testBase,
	})
	for i := 0; i < replies; i++ {
		e.repos.addPost(post.Post{
			ID: int64(101 + i), ThreadID: 10, UserID: 8,
			BodyMD: "回复", Status: post.StatusNormal, CreatedAt: testBase,
		})
	}
	return 10, 100
}

func TestThreadHeadReturnsBodyOnly(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, opID := seedThreadWithReplies(e, 3)

	head, err := e.svc.ThreadHead(ctx, threadID)
	if err != nil {
		t.Fatalf("取主题正文失败: %v", err)
	}
	if head.Post.ID != opID {
		t.Fatalf("正文应是首帖 %d，实际 %d", opID, head.Post.ID)
	}
	if head.Post.BodyMD != "主题正文" {
		t.Fatalf("正文内容错误: %q", head.Post.BodyMD)
	}
	if !head.HasMore {
		t.Fatal("有回复时 HasMore 应为 true")
	}
	if head.Thread.ID != threadID {
		t.Fatalf("主题 id 错误: %d", head.Thread.ID)
	}
}

func TestThreadHeadWithoutReplies(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, _ := seedThreadWithReplies(e, 0)

	head, err := e.svc.ThreadHead(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if head.HasMore {
		t.Fatal("无回复时 HasMore 应为 false")
	}
}

func TestThreadHeadRejectsInvalidID(t *testing.T) {
	e := newTestEnv()
	if _, err := e.svc.ThreadHead(context.Background(), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非法 id 应返回 ErrNotFound，实际 %v", err)
	}
}

func TestListRepliesSkipsBodyAndPages(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, _ := seedThreadWithReplies(e, 3)

	first, err := e.svc.ListReplies(ctx, threadID, 0, 2)
	if err != nil {
		t.Fatalf("取第一页失败: %v", err)
	}
	if len(first.Replies) != 2 {
		t.Fatalf("第一页应有 2 条回复，实际 %d", len(first.Replies))
	}
	if first.Replies[0].ID != 101 {
		t.Fatalf("第一页应从首帖之后开始（101），实际 %d", first.Replies[0].ID)
	}
	for _, p := range first.Replies {
		if p.ID == 100 {
			t.Fatal("回复页不得包含正文")
		}
	}
	if !first.HasMore {
		t.Fatal("还有第 3 条时应 HasMore=true")
	}
	if first.NextCursor != first.Replies[len(first.Replies)-1].ID {
		t.Fatalf("NextCursor 应为末条 id，实际 %d", first.NextCursor)
	}

	second, err := e.svc.ListReplies(ctx, threadID, first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Replies) != 1 {
		t.Fatalf("第二页应只剩 1 条，实际 %d", len(second.Replies))
	}
	if second.HasMore {
		t.Fatal("末尾页应 HasMore=false")
	}
	if second.Replies[0].ID != second.NextCursor {
		t.Fatalf("末条应等于 NextCursor，实际 %d vs %d", second.Replies[0].ID, second.NextCursor)
	}
}

func TestListRepliesOnlyBody(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, _ := seedThreadWithReplies(e, 0)

	page, err := e.svc.ListReplies(ctx, threadID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Replies) != 0 || page.HasMore || page.NextCursor != 0 {
		t.Fatalf("无回复时应返回空页，实际 %+v", page)
	}
}

func TestListRepliesTailReturnsLastPage(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	threadID, _ := seedThreadWithReplies(e, 5)

	page, err := e.svc.ListRepliesTail(ctx, threadID, 2)
	if err != nil {
		t.Fatalf("取末页失败: %v", err)
	}
	if len(page.Replies) != 2 {
		t.Fatalf("末页应有 2 条，实际 %d", len(page.Replies))
	}
	// 种子回复 id 为 101..105，末页应是 104、105 且升序。
	if page.Replies[0].ID != 104 || page.Replies[1].ID != 105 {
		t.Fatalf("末页应为 [104 105]，实际 [%d %d]", page.Replies[0].ID, page.Replies[1].ID)
	}
	for _, p := range page.Replies {
		if p.ID == 100 {
			t.Fatal("末页不得包含正文")
		}
	}
}

func TestListRepliesTailOnlyBody(t *testing.T) {
	e := newTestEnv()
	threadID, _ := seedThreadWithReplies(e, 0)

	page, err := e.svc.ListRepliesTail(context.Background(), threadID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Replies) != 0 {
		t.Fatalf("无回复时末页应为空，实际 %d 条", len(page.Replies))
	}
}
