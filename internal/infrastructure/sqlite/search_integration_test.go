package sqlite

// P1-M3 检索真实链路测试：FTS5 trigram 命中、短词 LIKE 回退、注入字符、分页、
// 软删除可见性、摘要体积。每条都走 0007 建好的索引与真实触发器和写连接。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
)

func searchService(t *testing.T, e *forumEnv) *forum.Service {
	t.Helper()
	return forum.NewService(e.repos,
		forum.WithClock(func() time.Time { return time.Unix(e.clock, 0).UTC() }))
}

func searchTexts(t *testing.T, res *forum.SearchResult) []string {
	t.Helper()
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.Excerpt)
	}
	return out
}

func TestFTSSearchFindsChinesePhrase(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "检索主题", 0, e.clock)
	e.insertPost(t, tid, "你好世界，这是 trigram 检索", post.StatusNormal, e.clock)
	e.insertPost(t, tid, "完全不相关的内容", post.StatusNormal, e.clock)

	res, err := searchService(t, e).Search(e.ctx, forum.SearchInput{Query: "你好世", Limit: 20})
	if err != nil {
		t.Fatalf("FTS 检索失败: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("3 字中文应命中 1 条，实际 %d: %+v", len(res.Hits), res.Hits)
	}
	if res.Hits[0].PostID <= 0 || res.Hits[0].ThreadID != tid {
		t.Fatalf("结果应带 postID 与所属 threadID: %+v", res.Hits[0])
	}
}

func TestShortQueryFallsBackToLIKE(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "短词主题", 0, e.clock)
	e.insertPost(t, tid, "你好世界", post.StatusNormal, e.clock)

	res, err := searchService(t, e).Search(e.ctx, forum.SearchInput{Query: "你好", Limit: 20})
	if err != nil {
		t.Fatalf("短词检索失败: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("2 字中文应经 LIKE 回退命中 1 条，实际 %d", len(res.Hits))
	}
}

func TestSearchEscapesWildcardsAndQuote(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "注入主题", 0, e.clock)
	e.insertPost(t, tid, "alpha beta", post.StatusNormal, e.clock)
	e.insertPost(t, tid, "gamma delta", post.StatusNormal, e.clock)
	e.insertPost(t, tid, "折扣 100%_ 特价", post.StatusNormal, e.clock)
	e.insertPost(t, tid, `他说 "hi" 然后离开`, post.StatusNormal, e.clock)
	e.insertPost(t, tid, "star * marker", post.StatusNormal, e.clock)

	svc := searchService(t, e)

	// "%_" 只有 2 码点，走 LIKE；未转义会把全部 5 条都搜出来。
	res, err := svc.Search(e.ctx, forum.SearchInput{Query: "%_", Limit: 20})
	if err != nil {
		t.Fatalf("通配符检索失败: %v", err)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Excerpt, "100%_") {
		t.Fatalf("通配符必须按字面量匹配，实际 %d 条: %v", len(res.Hits), searchTexts(t, res))
	}

	// 单个 * 走 LIKE，只能命中含 * 的那条，不能触发 FTS 语法错误。
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: "*", Limit: 20})
	if err != nil {
		t.Fatalf("星号检索失败: %v", err)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Excerpt, "star *") {
		t.Fatalf("星号应按字面量匹配，实际 %d 条: %v", len(res.Hits), searchTexts(t, res))
	}

	// 带引号的 4 码点查询走 FTS，内部引号必须翻倍后作为字面量。
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: `"hi"`, Limit: 20})
	if err != nil {
		t.Fatalf("引号检索失败: %v", err)
	}
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Excerpt, "hi") {
		t.Fatalf("引号应按字面量匹配，实际 %d 条: %v", len(res.Hits), searchTexts(t, res))
	}
}

func TestSearchPaginationIsStable(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "分页主题", 0, e.clock)
	for i := 0; i < 5; i++ {
		e.insertPost(t, tid, "共同词 内容"+string(rune('a'+i)), post.StatusNormal, e.clock)
	}
	svc := searchService(t, e)
	seen := map[int64]bool{}
	for page := 1; page <= 3; page++ {
		res, err := svc.Search(e.ctx, forum.SearchInput{Query: "共同词", Page: page, Limit: 2})
		if err != nil {
			t.Fatalf("第 %d 页检索失败: %v", page, err)
		}
		if res.Page != page {
			t.Fatalf("结果页号应为 %d，实际 %d", page, res.Page)
		}
		want := 2
		if page == 3 {
			want = 1
		}
		if len(res.Hits) != want {
			t.Fatalf("第 %d 页应有 %d 条，实际 %d", page, want, len(res.Hits))
		}
		if res.HasMore != (page < 3) {
			t.Fatalf("第 %d 页 hasMore 应为 %v", page, page < 3)
		}
		for _, h := range res.Hits {
			if seen[h.PostID] {
				t.Fatalf("分页出现重复 postID=%d", h.PostID)
			}
			seen[h.PostID] = true
		}
	}
}

func TestSearchHonorsPostStatusAndTriggers(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "状态主题", 0, e.clock)
	visible := e.insertPost(t, tid, "隐身关键词 可见", post.StatusNormal, e.clock)
	removed := e.insertPost(t, tid, "消失关键词 待删", post.StatusNormal, e.clock)
	hidden := e.insertPost(t, tid, "隐藏关键词 初始", post.StatusDeleted, e.clock)
	_ = hidden

	svc := searchService(t, e)
	res, err := svc.Search(e.ctx, forum.SearchInput{Query: "关键词"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("初始应命中 2 条 normal，实际 %d", len(res.Hits))
	}

	// UPDATE 触发器：改为 deleted 后必须从索引移除。
	if _, err := e.pools.writer.ExecContext(e.ctx, `UPDATE posts SET status = 'deleted' WHERE id = ?`, removed); err != nil {
		t.Fatal(err)
	}
	// UPDATE 触发器：从 deleted 恢复为 normal 后必须重新进索引。
	if _, err := e.pools.writer.ExecContext(e.ctx, `UPDATE posts SET status = 'normal' WHERE id = ?`, hidden); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: "关键词"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, h := range res.Hits {
		ids[h.PostID] = true
	}
	if !ids[visible] || ids[removed] || !ids[hidden] {
		t.Fatalf("触发器同步不符合预期: %+v", ids)
	}

	// UPDATE 触发器：正文改写后，旧词必须消失、新词必须可查。
	if _, err := e.pools.writer.ExecContext(e.ctx,
		`UPDATE posts SET body_md = '全新内容 改写词' WHERE id = ?`, hidden); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: "全新内容"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].PostID != hidden {
		t.Fatalf("改写后的正文应可检索: %+v", res.Hits)
	}
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: "隐藏关键词"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("旧正文不应残留索引: %+v", res.Hits)
	}

	// DELETE 触发器：物理删除后不应残留索引行。
	if _, err := e.pools.writer.ExecContext(e.ctx, `DELETE FROM posts WHERE id = ?`, visible); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Search(e.ctx, forum.SearchInput{Query: "关键词"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.PostID == visible {
			t.Fatal("物理删除的内容不应仍可检索")
		}
	}
}

func TestSearchExcerptFits1MbpsBudget(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "摘要主题", 0, e.clock)
	big := "带宽关键词 " + strings.Repeat("很长的内容", 2000)
	e.insertPost(t, tid, big, post.StatusNormal, e.clock)

	res, err := searchService(t, e).Search(e.ctx, forum.SearchInput{Query: "带宽", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("应命中 1 条，实际 %d", len(res.Hits))
	}
	if n := len(res.Hits[0].Excerpt); n > forum.SearchMaxExcerptBytes {
		t.Fatalf("摘要 %d 字节超过上限 %d", n, forum.SearchMaxExcerptBytes)
	}
	// 20 条满页时体积仍远小于 1 Mbps 链路上的常见单帧预算。
	if budget := forum.SearchMaxPageSize * forum.SearchMaxExcerptBytes; budget > 8<<10 {
		t.Fatalf("满页响应预算 %d 字节过大", budget)
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	e := newForumEnv(t)
	if _, err := searchService(t, e).Search(e.ctx, forum.SearchInput{Query: "   "}); !errors.Is(err, forum.ErrInvalidQuery) {
		t.Fatalf("空白查询应拒绝: %v", err)
	}
}

// TestSearchDoesNotRequireLogin 锁定只读语义：搜索不需要 Actor，访客同样可用。
func TestSearchDoesNotRequireLogin(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "公开检索", 0, e.clock)
	e.insertPost(t, tid, "访客可见词", post.StatusNormal, e.clock)
	res, err := searchService(t, e).Search(e.ctx, forum.SearchInput{Query: "访客可见"})
	if err != nil || len(res.Hits) != 1 {
		t.Fatalf("访客应能检索公开内容: res=%+v err=%v", res, err)
	}
}
