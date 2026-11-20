package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/user"
)

// P0-08 仓储回归集：分页、排序、软删除过滤、事务原子性。
// 用真实 SQLite + 真实写入队列，但只依赖 0001 建表（种子数据由 P0-09 单独验证），
// 因此本测试与种子任务解耦。

var testBaseTime = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

type forumEnv struct {
	ctx    context.Context
	repos  *ForumRepos
	pools  *Pools
	writer *Writer
	nodeID int64
	userID int64
	clock  int64
}

func newForumEnv(t *testing.T) *forumEnv {
	t.Helper()
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(writer.Stop)

	base := testBaseTime.Unix()
	res, err := pools.writer.ExecContext(ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
		"tester", base, base)
	if err != nil {
		t.Fatalf("插入测试用户: %v", err)
	}
	userID, _ := res.LastInsertId()

	res, err = pools.writer.ExecContext(ctx,
		`INSERT INTO nodes (slug, name, description) VALUES (?, ?, ?)`,
		"/test/node", "测试版块", "用于仓储回归")
	if err != nil {
		t.Fatalf("插入测试版块: %v", err)
	}
	nodeID, _ := res.LastInsertId()

	return &forumEnv{
		ctx: ctx, pools: pools, writer: writer, repos: NewForumRepos(pools, writer),
		nodeID: nodeID, userID: userID, clock: base,
	}
}

func (e *forumEnv) insertThread(t *testing.T, title string, score int64, lastReply int64) int64 {
	t.Helper()
	res, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.nodeID, e.userID, title, score, lastReply, e.clock)
	if err != nil {
		t.Fatalf("插入主题 %q: %v", title, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *forumEnv) insertPost(t *testing.T, threadID int64, body string, status post.Status, created int64) int64 {
	t.Helper()
	res, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO posts (thread_id, user_id, body_md, status, created_at) VALUES (?, ?, ?, ?, ?)`,
		threadID, e.userID, body, string(status), created)
	if err != nil {
		t.Fatalf("插入内容 %q: %v", body, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestForumReposNodes(t *testing.T) {
	e := newForumEnv(t)
	if _, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO nodes (slug, name, description) VALUES (?, ?, '')`, "/test/aaa", "AAA"); err != nil {
		t.Fatal(err)
	}

	list, err := e.repos.ListNodes(e.ctx)
	if err != nil {
		t.Fatalf("列出版块: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("至少应包含 2 个版块，实际 %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("版块应按 id 升序，实际 %d 在 %d 之后", list[i-1].ID, list[i].ID)
		}
	}
	got, err := e.repos.GetNodeBySlug(e.ctx, "/test/aaa")
	if err != nil {
		t.Fatalf("按 slug 查询: %v", err)
	}
	if got.Name != "AAA" || got.ID == 0 {
		t.Fatalf("版块字段不正确: %+v", got)
	}
	if _, err := e.repos.GetNodeBySlug(e.ctx, "/not/exist"); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("缺失版块应返回 ErrNotFound，实际 %v", err)
	}
}

// TestNodeStatusFiltering 锁定 ADR-048「归档不物理删」的读口径：
// 归档节点从公开列表消失，但仍可经 slug 取到（历史帖详情页要显示版块名），
// 且归档是可逆的 UPDATE，不是 DELETE FROM nodes。
func TestNodeStatusFiltering(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx

	if _, err := e.pools.writer.ExecContext(ctx,
		`UPDATE nodes SET status = 'archived' WHERE id = ?`, e.nodeID); err != nil {
		t.Fatalf("归档测试版块: %v", err)
	}

	list, err := e.repos.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出版块: %v", err)
	}
	for _, n := range list {
		if n.ID == e.nodeID {
			t.Fatalf("归档节点 id=%d 不应出现在公开列表", e.nodeID)
		}
	}
	if len(list) != 5 {
		t.Fatalf("归档 1 个自建节点后应只剩 5 个 seed 版块，实际 %d", len(list))
	}

	got, err := e.repos.GetNodeBySlug(ctx, "/test/node")
	if err != nil || got.ID != e.nodeID {
		t.Fatalf("历史帖详情页仍需能按 slug 取到归档版块: %+v, %v", got, err)
	}

	if _, err := e.pools.writer.ExecContext(ctx,
		`UPDATE nodes SET status = 'active' WHERE id = ?`, e.nodeID); err != nil {
		t.Fatalf("恢复版块: %v", err)
	}
	list, err = e.repos.ListNodes(ctx)
	if err != nil || len(list) != 6 {
		t.Fatalf("恢复 active 后应重新出现在列表，实际 %d, %v", len(list), err)
	}
}

// TestThreadStatusFiltering 锁定 0004 引入的公开过滤口径：
// GetThread / ListThreads 只读 active，deleted / hidden 主题不得泄漏。
func TestThreadStatusFiltering(t *testing.T) {
	e := newForumEnv(t)
	activeID := e.insertThread(t, "可见", 0, e.clock)
	deletedID := e.insertThread(t, "已删除", 0, e.clock)
	hiddenID := e.insertThread(t, "已隐藏", 0, e.clock)
	for _, id := range []int64{deletedID, hiddenID} {
		status := "deleted"
		if id == hiddenID {
			status = "hidden"
		}
		if _, err := e.pools.writer.ExecContext(e.ctx,
			`UPDATE threads SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := e.repos.GetThread(e.ctx, activeID); err != nil {
		t.Fatalf("active 主题应可读: %v", err)
	}
	for name, id := range map[string]int64{"deleted": deletedID, "hidden": hiddenID} {
		if _, err := e.repos.GetThread(e.ctx, id); !errors.Is(err, forum.ErrNotFound) {
			t.Fatalf("%s 主题应对公开读取不可见，实际 %v", name, err)
		}
	}

	for _, sort := range []forum.ThreadSort{forum.SortLatest, forum.SortScore} {
		rows, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{
			NodeID: e.nodeID, Sort: sort, Limit: 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != activeID {
			t.Fatalf("排序 %d 的列表应只含 active 主题，实际 %+v", sort, rows)
		}
	}
}

func TestThreadListLatestPaging(t *testing.T) {
	e := newForumEnv(t)
	// 故意乱序插入，验证排序只依赖 last_reply_at / score，不依赖插入顺序。
	ids := []int64{
		e.insertThread(t, "t1", 0, e.clock+10),
		e.insertThread(t, "t2", 0, e.clock+50),
		e.insertThread(t, "t3", 0, e.clock+30),
		e.insertThread(t, "t4", 0, e.clock+30), // 与 t3 同秒，按 id 倒序
		e.insertThread(t, "t5", 0, e.clock+20),
	}
	wantOrder := []int64{ids[1], ids[3], ids[2], ids[4], ids[0]}

	var seen []int64
	var cursor forum.ThreadCursor
	for page := 0; page < 4; page++ {
		rows, hasMore, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{
			NodeID: e.nodeID, Sort: forum.SortLatest, After: cursor, Limit: 2,
		})
		if err != nil {
			t.Fatalf("第 %d 页查询失败: %v", page+1, err)
		}
		if len(rows) > 2 {
			t.Fatalf("仓储返回行数 %d 超过 Limit=2（hasMore 应靠多取一条判断）", len(rows))
		}
		for _, row := range rows {
			seen = append(seen, row.ID)
		}
		if !hasMore {
			break
		}
		last := rows[len(rows)-1]
		cursor = forum.ThreadCursor{
			Sort: forum.SortLatest, LastReplyAt: last.LastReplyAt.Unix(), ID: last.ID,
		}
	}
	if len(seen) != len(wantOrder) {
		t.Fatalf("分页应遍历全部 %d 条，实际 %d 条: %v", len(wantOrder), len(seen), seen)
	}
	for i := range wantOrder {
		if seen[i] != wantOrder[i] {
			t.Fatalf("最新排序第 %d 位应为 %d，实际 %d（完整序列 %v）", i+1, wantOrder[i], seen[i], seen)
		}
	}
}

func TestThreadListScoreSort(t *testing.T) {
	e := newForumEnv(t)
	ids := []int64{
		e.insertThread(t, "low", 1, e.clock),
		e.insertThread(t, "high", 99, e.clock),
		e.insertThread(t, "midA", 10, e.clock),
		e.insertThread(t, "midB", 10, e.clock), // 同分，按 id 倒序
	}
	rows, hasMore, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{
		NodeID: e.nodeID, Sort: forum.SortScore, Limit: 10,
	})
	if err != nil {
		t.Fatalf("最高分排序失败: %v", err)
	}
	if hasMore {
		t.Fatal("仅 4 条却报告还有下一页")
	}
	want := []int64{ids[1], ids[3], ids[2], ids[0]}
	for i := range want {
		if rows[i].ID != want[i] {
			t.Fatalf("最高分排序第 %d 位应为 %d，实际 %d", i+1, want[i], rows[i].ID)
		}
	}

	// 游标翻页：从第一条之后继续，得分必须单调不增。
	cursor := forum.ThreadCursor{Sort: forum.SortScore, Score: rows[0].Score, ID: rows[0].ID}
	next, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{
		NodeID: e.nodeID, Sort: forum.SortScore, After: cursor, Limit: 10,
	})
	if err != nil {
		t.Fatalf("最高分游标翻页失败: %v", err)
	}
	if len(next) != 3 || next[0].ID != want[1] {
		t.Fatalf("游标应跳过首条并返回剩余 3 条，实际 %d 条，首条 ID=%d", len(next), next[0].ID)
	}
}

func TestThreadListRejectsBadQuery(t *testing.T) {
	e := newForumEnv(t)
	e.insertThread(t, "x", 0, e.clock)

	if _, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{NodeID: e.nodeID, Sort: 0, Limit: 10}); !errors.Is(err, forum.ErrInvalidSort) {
		t.Fatalf("非法排序应返回 ErrInvalidSort，实际 %v", err)
	}
	_, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{
		NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 10,
		After: forum.ThreadCursor{Sort: forum.SortScore, Score: 5, ID: 1},
	})
	if !errors.Is(err, forum.ErrInvalidCursor) {
		t.Fatalf("游标排序与查询排序不一致应拒绝，实际 %v", err)
	}
}

func TestPostListFiltersSoftDeleted(t *testing.T) {
	e := newForumEnv(t)
	threadID := e.insertThread(t, "t", 0, e.clock)
	e.insertPost(t, threadID, "正常首帖", post.StatusNormal, e.clock)
	e.insertPost(t, threadID, "已删除", post.StatusDeleted, e.clock+1)
	e.insertPost(t, threadID, "已隐藏", post.StatusHidden, e.clock+2)
	e.insertPost(t, threadID, "正常回复", post.StatusNormal, e.clock+3)

	rows, hasMore, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: threadID, Limit: 10})
	if err != nil {
		t.Fatalf("列出内容: %v", err)
	}
	if hasMore || len(rows) != 2 {
		t.Fatalf("软删除内容必须被过滤，期望 2 条，实际 %d 条 hasMore=%v", len(rows), hasMore)
	}
	if rows[0].BodyMD != "正常首帖" || rows[1].BodyMD != "正常回复" {
		t.Fatalf("内容应按 id 升序且只含 normal: %+v", rows)
	}

	// AfterID 游标：只返回更晚的内容。
	page, _, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: threadID, AfterID: rows[0].ID, Limit: 10})
	if err != nil {
		t.Fatalf("内容游标翻页: %v", err)
	}
	if len(page) != 1 || page[0].BodyMD != "正常回复" {
		t.Fatalf("AfterID 应跳过首帖，实际 %+v", page)
	}
}

func TestWriteTxAtomicityAndFirstPost(t *testing.T) {
	e := newForumEnv(t)
	sentinel := errors.New("用例主动失败")

	// 失败：主题已插入但闭包返回错误，提交不得发生。
	err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		if _, err := tx.InsertThread(e.ctx, forum.NewThread{
			NodeID: e.nodeID, UserID: e.userID, Title: "会回滚的主题", CreatedAt: testBaseTime,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("用例错误应原样传出（可 errors.Is 识别），实际 %v", err)
	}
	var count int
	if err := e.pools.reader.QueryRowContext(e.ctx,
		`SELECT COUNT(*) FROM threads WHERE title = '会回滚的主题'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("闭包失败后主题仍存在，事务未回滚")
	}

	// 成功：主题 + 首帖必须在同一事务里一起落地（§5.2 Post 含首帖）。
	var threadID, postID int64
	err = e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		var err error
		threadID, err = tx.InsertThread(e.ctx, forum.NewThread{
			NodeID: e.nodeID, UserID: e.userID, Title: "新主题", CreatedAt: testBaseTime,
		})
		if err != nil {
			return err
		}
		postID, err = tx.InsertPost(e.ctx, forum.NewPost{
			ThreadID: threadID, UserID: e.userID, BodyMD: "首帖正文",
			Status: post.StatusNormal, CreatedAt: testBaseTime,
		})
		return err
	})
	if err != nil {
		t.Fatalf("成功提交: %v", err)
	}
	if threadID == 0 || postID == 0 {
		t.Fatal("主题与首帖都应拿到自增 ID")
	}
	var threads, posts int
	if err := e.pools.reader.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if err := e.pools.reader.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM posts`).Scan(&posts); err != nil {
		t.Fatal(err)
	}
	if threads != 1 || posts != 1 {
		t.Fatalf("提交后应有 1 主题 1 首帖，实际 %d / %d", threads, posts)
	}
}

func TestTouchThreadUpdatesLastReplyAndScore(t *testing.T) {
	e := newForumEnv(t)
	threadID := e.insertThread(t, "t", 5, e.clock)

	before, err := e.repos.GetThread(e.ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	at := testBaseTime.Add(90 * time.Second)
	err = e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		return tx.TouchThread(e.ctx, threadID, at, 3)
	})
	if err != nil {
		t.Fatalf("更新主题活跃度: %v", err)
	}
	after, err := e.repos.GetThread(e.ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastReplyAt.Equal(at) {
		t.Fatalf("last_reply_at 应变为 %v，实际 %v", at, after.LastReplyAt)
	}
	if before.LastReplyAt.Equal(after.LastReplyAt) {
		t.Fatal("回复后 last_reply_at 必须变化")
	}
	if after.Score != before.Score+3 {
		t.Fatalf("score 应在同一条 UPDATE 里 +3，实际 %d → %d", before.Score, after.Score)
	}
	if err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		return tx.TouchThread(e.ctx, 999999, at, 0)
	}); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("不存在的主题应返回 ErrNotFound，实际 %v", err)
	}
}

func TestRecentWriteStatsInTransaction(t *testing.T) {
	e := newForumEnv(t)
	threadID := e.insertThread(t, "t", 0, e.clock)
	// 首帖与主题共享 created_at，按「发帖动作」去重后不应额外占一份配额。
	e.insertPost(t, threadID, "首帖", post.StatusNormal, e.clock)
	e.insertPost(t, threadID, "p1", post.StatusNormal, e.clock+10)
	e.insertPost(t, threadID, "p2", post.StatusNormal, e.clock+20)
	// 窗口外的旧记录不应计入。
	e.insertPost(t, threadID, "old", post.StatusNormal, e.clock-7200)

	var stats forum.WriteStats
	err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		var err error
		stats, err = tx.RecentWriteStats(e.ctx, e.userID, testBaseTime.Add(-time.Hour))
		return err
	})
	if err != nil {
		t.Fatalf("统计近期写入: %v", err)
	}
	if stats.Count != 3 {
		t.Fatalf("窗口内应有 3 次发帖动作（主题+首帖算 1 次，另 2 次回复），实际 %d", stats.Count)
	}
	if !stats.LastWriteAt.Equal(testBaseTime.Add(20 * time.Second)) {
		t.Fatalf("LastWriteAt 应为最近一次，实际 %v", stats.LastWriteAt)
	}
	if !stats.OldestAt.Equal(testBaseTime) {
		t.Fatalf("OldestAt 应为窗口内最早一次，实际 %v", stats.OldestAt)
	}

	var empty forum.WriteStats
	err = e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		var err error
		empty, err = tx.RecentWriteStats(e.ctx, e.userID, testBaseTime.Add(24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Count != 0 || !empty.LastWriteAt.IsZero() {
		t.Fatalf("窗口内无写入应返回零值，实际 %+v", empty)
	}
}

func TestThreadGetAndUserStatusNotFound(t *testing.T) {
	e := newForumEnv(t)
	if _, err := e.repos.GetThread(e.ctx, 999999); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("缺失主题应返回 ErrNotFound，实际 %v", err)
	}
	err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		status, err := tx.UserStatus(e.ctx, e.userID)
		if err != nil {
			return err
		}
		if status != user.StatusActive {
			return fmt.Errorf("期望 active，实际 %q", status)
		}
		exists, err := tx.NodeExists(e.ctx, e.nodeID)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("版块存在性判断错误")
		}
		if exists, err = tx.ThreadExists(e.ctx, 999999); err != nil {
			return err
		}
		if exists {
			return errors.New("不存在的主题被判为存在")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("事务内状态查询: %v", err)
	}
	if _, _, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: 999999}); err != nil {
		t.Fatalf("空列表不应报错: %v", err)
	}
}
