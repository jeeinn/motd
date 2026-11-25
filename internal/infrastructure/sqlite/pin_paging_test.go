package sqlite

// R-15 / ADR-079 方案 C（分层 keyset）的分页正确性回归：
// 置顶（mark=1）是独立前缀层，必须前置且跨页不重不漏。
// 这里直接经真实 SQLite 验证仓储层 tier 分派与游标锚点。

import (
	"testing"

	"github.com/jeeinn/motd/internal/application/forum"
)

// TestPinnedThreadLeadsAndPaginatesWithoutDupes 构造一个"旧但置顶"的主题，
// 验证它排在最新普通主题之前；连续翻页覆盖所有主题恰好一次。
func TestPinnedThreadLeadsAndPaginatesWithoutDupes(t *testing.T) {
	e := newForumEnv(t)
	nodeB := e.insertNode(t, "/test/pin-b", "active")

	// pinned 是最后回复时间最旧的，但被置顶 → 必须排第一。
	pinned := e.insertThreadIn(t, e.nodeID, "PINNED", 100)
	n1 := e.insertThreadIn(t, e.nodeID, "N1", 500)
	n2 := e.insertThreadIn(t, nodeB, "N2", 400)
	n3 := e.insertThreadIn(t, e.nodeID, "N3", 300)
	n4 := e.insertThreadIn(t, nodeB, "N4", 200)
	if _, err := e.pools.writer.ExecContext(e.ctx,
		`UPDATE threads SET mark = 1 WHERE id = ?`, pinned); err != nil {
		t.Fatalf("设置置顶: %v", err)
	}

	want := map[int64]bool{pinned: true, n1: true, n2: true, n3: true, n4: true}
	seen := make(map[int64]int)
	var cursor forum.ThreadCursor
	first := true
	for page := 0; page < 10; page++ {
		rows, more, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID, nodeB}, cursor, 2))
		if err != nil {
			t.Fatalf("第 %d 页: %v", page+1, err)
		}
		for i, r := range rows {
			if !want[r.ID] {
				t.Fatalf("出现未知主题 id=%d", r.ID)
			}
			seen[r.ID]++
			if first && i == 0 && r.ID != pinned {
				t.Fatalf("置顶主题必须排首屏第一位，实际首位 id=%d（置顶 id=%d）", r.ID, pinned)
			}
		}
		first = false
		if !more {
			break
		}
		if len(rows) == 0 {
			t.Fatal("hasMore=true 但本页无行，游标无法前进")
		}
		last := rows[len(rows)-1]
		cursor = forum.ThreadCursor{
			Sort: forum.SortLatest, Mark: last.Mark,
			LastReplyAt: last.LastReplyAt.Unix(), ID: last.ID,
		}
	}
	for id := range want {
		if seen[id] != 1 {
			t.Fatalf("主题 id=%d 应恰好出现 1 次，实际 %d 次", id, seen[id])
		}
	}
}

// TestPinnedTierCursorMark 验证游标 mark 字段随 tier 正确前进：
// 第一页最后一行的 mark 应写进游标，第二页按该 mark 分派不再回置顶层。
func TestPinnedTierCursorMark(t *testing.T) {
	e := newForumEnv(t)
	// 两条置顶 + 三条普通，一页 2 条。
	p1 := e.insertThreadIn(t, e.nodeID, "P1", 100)
	p2 := e.insertThreadIn(t, e.nodeID, "P2", 90)
	e.insertThreadIn(t, e.nodeID, "N1", 500)
	e.insertThreadIn(t, e.nodeID, "N2", 400)
	e.insertThreadIn(t, e.nodeID, "N3", 300)
	for _, id := range []int64{p1, p2} {
		if _, err := e.pools.writer.ExecContext(e.ctx, `UPDATE threads SET mark = 1 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[int64]int)
	var cursor forum.ThreadCursor
	for page := 0; page < 10; page++ {
		rows, more, err := e.repos.ListThreads(e.ctx, e.crossQuery([]int64{e.nodeID}, cursor, 2))
		if err != nil {
			t.Fatalf("第 %d 页: %v", page+1, err)
		}
		for _, r := range rows {
			seen[r.ID]++
		}
		if !more {
			break
		}
		last := rows[len(rows)-1]
		if last.Mark == 1 && cursor.Mark == 1 && last.ID == cursor.ID {
			t.Fatal("游标未前进（同 mark 同 id）")
		}
		cursor = forum.ThreadCursor{
			Sort: forum.SortLatest, Mark: last.Mark,
			LastReplyAt: last.LastReplyAt.Unix(), ID: last.ID,
		}
	}
	if len(seen) != 5 {
		t.Fatalf("应覆盖 5 个主题，实际 %d 个: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("主题 id=%d 出现 %d 次（应 1 次）", id, n)
		}
	}
}
