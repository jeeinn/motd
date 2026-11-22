package sqlite

// 跨版块最新列表（/latest）的仓储回归（v0.7.1 / P4.5-M2 / ADR-063）。
//
// 覆盖三类「最容易悄悄坏掉」的行为：
//  1. 可见性：allow-list 是唯一判据（归档版块不进列表，但已解锁的彩蛋版块可以进）；
//  2. 分页：同秒多帖时 keyset 靠 id 兜底，翻页不重不漏；
//  3. 执行计划：跨版块 SQL 必须走强制索引，且不出现 TEMP B-TREE。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/forum"
)

func (e *forumEnv) insertNode(t *testing.T, slug, status string) int64 {
	t.Helper()
	res, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO nodes (slug, name, description, status) VALUES (?, ?, '', ?)`,
		slug, slug, status)
	if err != nil {
		t.Fatalf("插入版块 %q（%s）: %v", slug, status, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *forumEnv) insertThreadIn(t *testing.T, nodeID int64, title string, lastReply int64) int64 {
	t.Helper()
	res, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		nodeID, e.userID, title, lastReply, e.clock)
	if err != nil {
		t.Fatalf("插入主题 %q: %v", title, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// allowAll 返回「两个公开版块」的白名单，是跨版块查询的正常入参。
func (e *forumEnv) crossQuery(ids []int64, after forum.ThreadCursor, limit int) forum.ThreadQuery {
	return forum.ThreadQuery{
		NodeID: 0, AllowedNodeIDs: ids, Sort: forum.SortLatest, After: after, Limit: limit,
	}
}

func TestListThreadsCrossNodeLatest(t *testing.T) {
	e := newForumEnv(t)
	nodeB := e.insertNode(t, "/test/second", "active")
	nodeArch := e.insertNode(t, "/test/archived", "archived")

	aOld := e.insertThreadIn(t, e.nodeID, "A 旧", 100)
	bMid := e.insertThreadIn(t, nodeB, "B 中", 200)
	aNew := e.insertThreadIn(t, e.nodeID, "A 新", 300)
	e.insertThreadIn(t, nodeArch, "归档版块的主题", 400) // 必须不可见
	deleted := e.insertThreadIn(t, e.nodeID, "已删除", 500)
	if _, err := e.pools.writer.ExecContext(e.ctx,
		`UPDATE threads SET status = 'deleted' WHERE id = ?`, deleted); err != nil {
		t.Fatal(err)
	}

	rows, hasMore, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID, nodeB}, forum.ThreadCursor{}, 20))
	if err != nil {
		t.Fatalf("跨版块查询: %v", err)
	}
	if hasMore {
		t.Fatal("4 条可见主题在 limit=20 下不应有下一页")
	}
	want := []int64{aNew, bMid, aOld}
	if len(rows) != len(want) {
		t.Fatalf("应返回 %d 条，实际 %d 条: %+v", len(want), len(rows), rows)
	}
	for i, id := range want {
		if rows[i].ID != id {
			t.Errorf("第 %d 条应为 id=%d，实际 %d（应按 last_reply_at DESC, id DESC）", i, id, rows[i].ID)
		}
	}
	for _, r := range rows {
		if r.Title == "已删除" || r.Title == "归档版块的主题" {
			t.Fatalf("不可见主题泄漏到跨版块列表: %+v", r)
		}
	}
}

// allow-list 是**放行**语义而非排除语义：调用方把已解锁的隐藏版块传进来，
// 其主题就必须出现（否则「Konami 解锁了 /dev/null 却看不到新帖」）。
// 反过来，没传进来的归档版块一律不出现——判据只有白名单，没有第二套规则。
func TestListThreadsCrossNodeAllowListIsAuthoritative(t *testing.T) {
	e := newForumEnv(t)
	nodeHidden := e.insertNode(t, "/test/hidden", "archived")
	inHidden := e.insertThreadIn(t, nodeHidden, "隐藏版块的主题", 100)
	inPublic := e.insertThreadIn(t, e.nodeID, "公开版块的主题", 200)

	rows, _, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID, nodeHidden}, forum.ThreadCursor{}, 20))
	if err != nil {
		t.Fatalf("跨版块查询: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != inPublic || rows[1].ID != inHidden {
		t.Fatalf("白名单内的两个版块主题都应出现，实际 %+v", rows)
	}

	rows, _, err = e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID}, forum.ThreadCursor{}, 20))
	if err != nil {
		t.Fatalf("跨版块查询: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != inPublic {
		t.Fatalf("未放行的归档版块主题不应出现，实际 %+v", rows)
	}
}

// Fail-closed：没有白名单就不查，返回空页而不是「全站可见」。
func TestListThreadsCrossNodeEmptyAllowListIsEmpty(t *testing.T) {
	e := newForumEnv(t)
	e.insertThreadIn(t, e.nodeID, "有主题", 100)

	rows, hasMore, err := e.repos.ListThreads(e.ctx, e.crossQuery(nil, forum.ThreadCursor{}, 20))
	if err != nil {
		t.Fatalf("空白名单不应报错: %v", err)
	}
	if len(rows) != 0 || hasMore {
		t.Fatalf("空白名单必须 fail-closed 返回空页，实际 %d 条 hasMore=%v", len(rows), hasMore)
	}
}

// 跨版块热度榜不在 v0.7.1 范围：必须显式报错，不能静默落到 node_id=0 返回空列表。
func TestListThreadsCrossNodeRejectsScoreSort(t *testing.T) {
	e := newForumEnv(t)
	q := e.crossQuery([]int64{e.nodeID}, forum.ThreadCursor{}, 20)
	q.Sort = forum.SortScore
	if _, _, err := e.repos.ListThreads(e.ctx, q); err == nil {
		t.Fatal("跨版块 + SortScore 应被拒绝")
	}
}

func TestListThreadsRejectsNegativeNodeID(t *testing.T) {
	e := newForumEnv(t)
	_, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{NodeID: -1, Sort: forum.SortLatest, Limit: 20})
	if err == nil || !strings.Contains(err.Error(), "版块 ID 不能为负") {
		t.Fatalf("负版块 ID 应被拒绝，实际 %v", err)
	}
}

// 同秒多帖：keyset 的次级键 id 必须保证翻页不重不漏（QA P0-4）。
func TestListThreadsCrossNodePagingWithIdenticalTimestamps(t *testing.T) {
	e := newForumEnv(t)
	nodeB := e.insertNode(t, "/test/second", "active")
	// 5 条主题全部落在同一秒，且跨两个版块交替出现。
	const ts = int64(1_700_000_000)
	want := map[int64]bool{}
	for i := 0; i < 5; i++ {
		node := e.nodeID
		if i%2 == 1 {
			node = nodeB
		}
		want[e.insertThreadIn(t, node, fmt.Sprintf("同秒 #%d", i), ts)] = true
	}

	seen := map[int64]bool{}
	var cursor forum.ThreadCursor
	for page := 0; page < 10; page++ {
		rows, hasMore, err := e.repos.ListThreads(e.ctx,
			e.crossQuery([]int64{e.nodeID, nodeB}, cursor, 2))
		if err != nil {
			t.Fatalf("第 %d 页: %v", page+1, err)
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("翻页重复了 id=%d", r.ID)
			}
			seen[r.ID] = true
			cursor = forum.ThreadCursor{Sort: forum.SortLatest, LastReplyAt: r.LastReplyAt.Unix(), ID: r.ID}
		}
		if !hasMore {
			break
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("应翻出 %d 条，实际 %d 条（漏行说明游标比较写错）", len(want), len(seen))
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("漏掉 id=%d", id)
		}
	}
}

// 行值游标 (a,b) < (?,?) 与历史 OR 形态必须语义等价（QA P0-4）：
// 本测试锁定「换写法不改结果」；执行计划测试锁定「换写法真的变快」。
func TestLatestCursorFormsAgree(t *testing.T) {
	e := newForumEnv(t)
	for i := 0; i < 6; i++ {
		e.insertThreadIn(t, e.nodeID, fmt.Sprintf("T%d", i), int64(100+i/2))
	}
	const afterReply, afterID = int64(101), int64(4)

	rowsNew, _, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID},
		forum.ThreadCursor{Sort: forum.SortLatest, LastReplyAt: afterReply, ID: afterID}, 20))
	if err != nil {
		t.Fatalf("行值游标查询: %v", err)
	}

	legacy := selectThreadsScoped + ` WHERE t.status = 'active' AND t.node_id IN (?)` +
		` AND (t.last_reply_at < ? OR (t.last_reply_at = ? AND t.id < ?))` +
		` ORDER BY t.last_reply_at DESC, t.id DESC LIMIT ?`
	lrows, err := e.pools.QueryContext(e.ctx, legacy, e.nodeID, afterReply, afterReply, afterID, 21)
	if err != nil {
		t.Fatalf("OR 形态查询: %v", err)
	}
	defer lrows.Close()
	var legacyIDs []int64
	for lrows.Next() {
		var th threadRowScan
		if err := lrows.Scan(&th.id, &th.nodeID, &th.userID, &th.title, &th.score, &th.lastReply, &th.created); err != nil {
			t.Fatalf("扫描 OR 形态结果: %v", err)
		}
		legacyIDs = append(legacyIDs, th.id)
	}
	if err := lrows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(rowsNew) != len(legacyIDs) {
		t.Fatalf("两种游标形态行数不同：新 %d / 旧 %d", len(rowsNew), len(legacyIDs))
	}
	for i := range rowsNew {
		if rowsNew[i].ID != legacyIDs[i] {
			t.Fatalf("第 %d 行不同：新 %d / 旧 %d", i, rowsNew[i].ID, legacyIDs[i])
		}
	}
}

// threadRowScan 是只在本测试用的扫描中转结构（复用生产列序，不引依赖）。
type threadRowScan struct {
	id, nodeID, userID, score, lastReply, created int64
	title                                         string
}

// 执行计划：必须命中强制索引，且不出现 TEMP B-TREE 排序（DoD §3.2 #6）。
func TestLatestThreadsQueryPlanUsesForcedIndex(t *testing.T) {
	e := newForumEnv(t)
	nodeB := e.insertNode(t, "/test/second", "active")
	for i := 0; i < 4; i++ {
		e.insertThreadIn(t, e.nodeID, fmt.Sprintf("A%d", i), int64(100+i))
		e.insertThreadIn(t, nodeB, fmt.Sprintf("B%d", i), int64(200+i))
	}

	plan := explainPlan(t, e.pools, latestThreadsQuery(2, true),
		e.nodeID, nodeB, int64(1000), int64(1000), 10)
	t.Logf("跨版块执行计划: %s", plan)

	if !strings.Contains(plan, "idx_threads_last_reply") {
		t.Fatalf("执行计划未使用 idx_threads_last_reply: %s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("执行计划出现 TEMP B-TREE 排序: %s", plan)
	}
}

// 强制索引是 fail-fast 契约：索引缺失时必须报错，而不是静默退化成全表扫。
// 这条测试同时保护「上线前疏漏」——有人手工 DROP 索引或改名，/latest 会立刻暴露。
func TestLatestThreadsQueryFailsFastWithoutIndex(t *testing.T) {
	e := newForumEnv(t)
	if _, err := e.pools.writer.ExecContext(e.ctx, `DROP INDEX idx_threads_last_reply`); err != nil {
		t.Fatalf("删除索引: %v", err)
	}
	if _, _, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID}, forum.ThreadCursor{}, 20)); err == nil {
		t.Fatal("索引缺失时应 fail-fast 报错")
	}
}

// explainPlan 把 EXPLAIN QUERY PLAN 的所有 detail 行拼成一段文本，供断言使用。
func explainPlan(t *testing.T, pools *Pools, query string, args ...any) string {
	t.Helper()
	rows, err := pools.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("查看执行计划: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("扫描执行计划: %v", err)
		}
		b.WriteString(detail)
		b.WriteString(" | ")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历执行计划: %v", err)
	}
	return b.String()
}
