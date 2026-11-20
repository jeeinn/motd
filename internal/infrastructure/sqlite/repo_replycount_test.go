package sqlite

// P2-M3 / ADR-052：回复数读时聚合的仓储回归 + 真实 SQLite 端到端。
// 本文件只碰 newForumEnv 的临时库，生产 data/ 全程不触碰。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/user"
)

func TestReplyCountsByThreadIDsBatchProjection(t *testing.T) {
	e := newForumEnv(t)
	t1 := e.insertThread(t, "主题一", 0, e.clock)
	t2 := e.insertThread(t, "主题二", 0, e.clock)
	t3 := e.insertThread(t, "主题三", 0, e.clock)

	// 主题一：首帖 + 2 条回复 + 1 条软删回复 → 2。
	e.insertPost(t, t1, "首帖", post.StatusNormal, e.clock)
	e.insertPost(t, t1, "回复A", post.StatusNormal, e.clock+1)
	e.insertPost(t, t1, "回复B", post.StatusNormal, e.clock+2)
	e.insertPost(t, t1, "已删回复", post.StatusDeleted, e.clock+3)
	// 主题二：首帖 + 1 normal + 1 hidden → 1（hidden 不计）。
	e.insertPost(t, t2, "首帖", post.StatusNormal, e.clock)
	e.insertPost(t, t2, "回复", post.StatusNormal, e.clock+1)
	e.insertPost(t, t2, "隐藏回复", post.StatusHidden, e.clock+2)
	// 主题三：只有首帖 → 0（正文不算回复）。
	e.insertPost(t, t3, "首帖", post.StatusNormal, e.clock)

	got, err := e.repos.ReplyCountsByThreadIDs(e.ctx, []int64{t1, t2, t3, t1, 987654321, 0, -1})
	if err != nil {
		t.Fatalf("批量查询回复数: %v", err)
	}
	if got[t1] != 2 || got[t2] != 1 || got[t3] != 0 {
		t.Fatalf("回复数错误（口径=normal 数-首帖）: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("重复 id 不应重复计数、无效 id 不应入结果: %+v", got)
	}
	if _, ok := got[987654321]; ok {
		t.Fatalf("无帖主题不应出现在结果里: %+v", got)
	}
}

func TestReplyCountsByThreadIDsEmptyInput(t *testing.T) {
	e := newForumEnv(t)
	for _, ids := range [][]int64{nil, {}, {0, -1}} {
		got, err := e.repos.ReplyCountsByThreadIDs(context.Background(), ids)
		if err != nil {
			t.Fatalf("ids=%v 不应报错: %v", ids, err)
		}
		if len(got) != 0 {
			t.Fatalf("ids=%v 应返回空 map，实际 %+v", ids, got)
		}
	}
}

// EXPLAIN 断言验的是 replyCountsQuery 产出的生产 SQL 本身（与仓储共用构造器），
// 因此任何把 INDEXED BY 去掉的改动都会在这里变红。小表上纯 IN 查询会选 SCAN posts，
// 显式锁定索引后各规模都稳定 SEARCH posts USING INDEX idx_posts_thread。
func TestReplyCountsQueryUsesThreadIndex(t *testing.T) {
	e := newForumEnv(t)
	q := replyCountsQuery(20)
	args := make([]any, 20)
	for i := range args {
		args[i] = int64(i + 1) // EXPLAIN 也要求占位符有实参；取值不影响计划。
	}
	rows, err := e.pools.reader.QueryContext(e.ctx, `EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatalf("查看执行计划: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("扫描执行计划: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := plan.String()
	t.Logf("回复数批量查询执行计划:\n%s", text)
	if !strings.Contains(text, "SEARCH posts USING INDEX idx_posts_thread") {
		t.Errorf("执行计划未走 idx_posts_thread:\n%s", text)
	}
	if strings.Contains(text, "SCAN posts") {
		t.Errorf("执行计划出现 posts 全表扫描:\n%s", text)
	}
}

// 端到端：真实 SQLite + Service.ScreenThreads，走完整的 发帖 → 回复 → 软删 → 显示链路。
// 证明 rN 的数据来自生产路径而非测试手工 set，且三条主题计数互不串。
func TestReplyCountScreenRealSQLite(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	viewer := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}
	listQuery := forum.ListThreadsQuery{NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 20}

	// 60 秒 1 帖的限流按 created_at 去重，每次写把时钟推进 61 秒，模拟真实用户逐次操作。
	now := testBaseTime
	writer := func() *forum.Service {
		svc := forum.NewService(e.repos, forum.WithClock(func() time.Time { return now }))
		now = now.Add(61 * time.Second)
		return svc
	}

	repliesOf := func(title string) int {
		t.Helper()
		sc, err := viewer.ScreenThreads(ctx, listQuery, forum.ViewContext{})
		if err != nil {
			t.Fatalf("构造主题屏: %v", err)
		}
		for _, r := range sc.Rows {
			if r.Title == title {
				return r.Meta.Replies
			}
		}
		t.Fatalf("列表里找不到主题 %q", title)
		return -1
	}

	// 建三条主题：A 用于 0 → r1 → 软删回 0；B、C 用于验证计数不串。
	create := func(title string) int64 {
		t.Helper()
		id, err := writer().CreateThread(ctx, actor, forum.CreateThreadInput{
			NodeID: e.nodeID, Title: title, Body: "首帖正文",
		})
		if err != nil {
			t.Fatalf("建主题 %q: %v", title, err)
		}
		return id
	}
	idA := create("计数A")
	idB := create("计数B")
	_ = create("计数C")

	// ① 只有首帖：0（view 层 N>0 才显示 r，因此此时不显示）。
	if n := repliesOf("计数A"); n != 0 {
		t.Fatalf("只有正文时回复数应为 0，实际 %d", n)
	}

	// ② 回复一条：1（r1）。
	replyID, err := writer().CreatePost(ctx, actor, forum.CreatePostInput{
		ThreadID: idA, Body: "第一条回复",
	})
	if err != nil {
		t.Fatalf("回复: %v", err)
	}
	if n := repliesOf("计数A"); n != 1 {
		t.Fatalf("回复一条后应为 1，实际 %d", n)
	}
	if b := repliesOf("计数B"); b != 0 || repliesOf("计数C") != 0 {
		t.Fatalf("主题间回复数串了: B=%d C=%d", b, repliesOf("计数C"))
	}

	// ③ 作者软删该回复：回到 0（r 消失）。
	if _, err := writer().Delete(ctx, actor, forum.DeletePost, replyID); err != nil {
		t.Fatalf("软删回复: %v", err)
	}
	if n := repliesOf("计数A"); n != 0 {
		t.Fatalf("软删回复后应回到 0，实际 %d", n)
	}

	// ④ 给 B 加两条回复：只有 B 变成 2，A/C 仍为 0。
	for i := 0; i < 2; i++ {
		if _, err := writer().CreatePost(ctx, actor, forum.CreatePostInput{
			ThreadID: idB, Body: "B 的回复",
		}); err != nil {
			t.Fatalf("给 B 回复: %v", err)
		}
	}
	if n := repliesOf("计数B"); n != 2 {
		t.Fatalf("B 应有 2 条回复，实际 %d", n)
	}
	if n := repliesOf("计数A"); n != 0 || repliesOf("计数C") != 0 {
		t.Fatalf("B 的回复串到了 A/C: A=%d C=%d", n, repliesOf("计数C"))
	}
}
