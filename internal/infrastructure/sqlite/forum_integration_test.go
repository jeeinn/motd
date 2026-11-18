package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/user"
)

// 集成测试：application/forum.Service 直接驱动真实的 SQLite 仓储。
// 用例层测试用内存 fake，仓储层测试用裸 SQL，这里补上两者之间的契约：
// 端口签名、时间转换、status 映射、事务边界、限流在真实链路上是否一致。
func TestForumServiceWithRealSQLite(t *testing.T) {
	e := newForumEnv(t)
	ctx := context.Background()
	now := testBaseTime
	svc := forum.NewService(e.repos, forum.WithClock(func() time.Time { return now }))
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}

	// 版块：种子数据 + 测试自建节点都可见。
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出真实版块: %v", err)
	}
	if len(nodes) < 5 {
		t.Fatalf("至少应有 5 个种子版块，实际 %d", len(nodes))
	}

	// 发帖：主题 + 首帖同事务落地。
	threadID, err := svc.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: e.nodeID, Title: "真实链路主题", Body: "首帖正文",
	})
	if err != nil {
		t.Fatalf("真实创建主题失败: %v", err)
	}
	if threadID <= 0 {
		t.Fatal("应返回自增主题 ID")
	}

	page, err := svc.ListThreads(ctx, forum.ListThreadsQuery{
		NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 10,
	})
	if err != nil {
		t.Fatalf("列出真实主题: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Title != "真实链路主题" {
		t.Fatalf("主题列表不正确: %+v", page.Rows)
	}
	if page.HasMore || page.NextCursor != "" {
		t.Fatal("只有 1 条时不应有下一页")
	}
	if !page.Rows[0].LastReplyAt.Equal(testBaseTime) || !page.Rows[0].CreatedAt.Equal(testBaseTime) {
		t.Fatalf("DB Unix 秒 → time.Time 转换错误: %+v", page.Rows[0])
	}

	detail, err := svc.GetThread(ctx, threadID)
	if err != nil {
		t.Fatalf("读取真实详情: %v", err)
	}
	if len(detail.Posts) != 1 || detail.Posts[0].BodyMD != "首帖正文" {
		t.Fatalf("首帖未随主题落地: %+v", detail.Posts)
	}

	// 回复：跨过 60 秒冷却，last_reply_at 必须推进。
	before := detail.Thread.LastReplyAt
	now = testBaseTime.Add(90 * time.Second)
	postID, err := svc.CreatePost(ctx, actor, forum.CreatePostInput{
		ThreadID: threadID, Body: "真实回复",
	})
	if err != nil {
		t.Fatalf("真实回复失败: %v", err)
	}
	if postID <= 0 {
		t.Fatal("应返回自增内容 ID")
	}
	after, err := svc.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Posts) != 2 || after.Posts[1].BodyMD != "真实回复" {
		t.Fatalf("回复未落地: %+v", after.Posts)
	}
	if !after.Thread.LastReplyAt.Equal(now) || before.Equal(after.Thread.LastReplyAt) {
		t.Fatalf("last_reply_at 未推进: before=%v after=%v now=%v", before, after.Thread.LastReplyAt, now)
	}
	if after.Thread.Score != 0 {
		t.Fatal("P0 无投票，score 应保持 0")
	}

	// 限流：同一时刻连发第二条回复必须被拒，且不落库。
	_, err = svc.CreatePost(ctx, actor, forum.CreatePostInput{ThreadID: threadID, Body: "刷屏"})
	var rl *forum.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("真实链路应触发 60 秒冷却，实际 %v", err)
	}
	if rl.RetryAfter <= 0 {
		t.Fatal("剩余冷却时间必须为正")
	}
	if !strings.Contains(rl.Error(), "60 秒") {
		t.Fatalf("限流原因应说明 60 秒冷却: %q", rl.Error())
	}
	afterRate, err := svc.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRate.Posts) != 2 {
		t.Fatalf("被限流的内容不应落库，实际 %d 条", len(afterRate.Posts))
	}

	// 访客只读：真实链路上也不能写。
	if _, err := svc.CreatePost(ctx, forum.Actor{Guest: true}, forum.CreatePostInput{
		ThreadID: threadID, Body: "访客",
	}); !errors.Is(err, forum.ErrReadOnly) {
		t.Fatalf("访客写入应被拒绝: %v", err)
	}
}
