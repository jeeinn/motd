package sqlite

// v0.2.1 软删的 SQL 回归集：用真实 SQLite + 真实写队列 + t.TempDir()，
// 锁定「不物理删除 / 不级联 / 已从所有公开读路径消失」三条不变量。

import (
	"errors"
	"testing"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
)

func softDeleteTx(t *testing.T, e *forumEnv, fn func(forum.WriteTx) error) {
	t.Helper()
	// 闭包在写队列 goroutine 里执行，不能用 t.Fatal——错误一律带回来断言。
	if err := e.repos.Write(e.ctx, fn); err != nil {
		t.Fatalf("软删事务失败: %v", err)
	}
}

func countSoftDeleteRows(t *testing.T, e *forumEnv, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pools.QueryRowContext(e.ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	return n
}

func searchHits(t *testing.T, e *forumEnv, term string) []forum.SearchHit {
	t.Helper()
	hits, _, err := e.repos.SearchPosts(e.ctx, forum.SearchQuery{Query: term, Limit: 20})
	if err != nil {
		t.Fatalf("检索 %q: %v", term, err)
	}
	return hits
}

// 验收①：删主题后主题与下属回复对所有公开读路径不可见，但两行都还在库里。
func TestSoftDeleteThreadHidesContentButKeepsRows(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "待删主题", 0, e.clock)
	e.insertPost(t, tid, "qqqfirstbodyseed", post.StatusNormal, e.clock)
	e.insertPost(t, tid, "qqqreplybodyseed", post.StatusNormal, e.clock+1)

	// 软删前必须能搜到，否则后面的「消失」断言没有意义。
	if len(searchHits(t, e, "qqqfirstbodyseed")) == 0 || len(searchHits(t, e, "qqqreplybodyseed")) == 0 {
		t.Fatal("软删前两条内容都应可检索")
	}

	var ownerID int64
	softDeleteTx(t, e, func(tx forum.WriteTx) error {
		var err error
		ownerID, err = tx.ThreadOwner(e.ctx, tid)
		if err != nil {
			return err
		}
		return tx.SoftDeleteThread(e.ctx, tid)
	})
	if ownerID != e.userID {
		t.Fatalf("作者 ID 不正确: %d，期望 %d", ownerID, e.userID)
	}

	var status string
	if err := e.pools.QueryRowContext(e.ctx, `SELECT status FROM threads WHERE id = ?`, tid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("主题应软删为 deleted，实际 %q", status)
	}
	if got := countSoftDeleteRows(t, e, `SELECT COUNT(*) FROM posts WHERE thread_id = ?`, tid); got != 2 {
		t.Fatalf("软删主题不得级联删除回复，期望 2 行，实际 %d", got)
	}
	if _, err := e.repos.GetThread(e.ctx, tid); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("GetThread 应拒绝已删主题: %v", err)
	}
	rows, _, err := e.repos.ListThreads(e.ctx, forum.ThreadQuery{NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == tid {
			t.Fatal("已删主题不应出现在列表")
		}
	}
	// 关键：回复行虽在，但随主题一并从 /search 消失。
	if hits := searchHits(t, e, "qqqfirstbodyseed"); len(hits) != 0 {
		t.Fatalf("已删主题的首帖不得被检索到: %+v", hits)
	}
	if hits := searchHits(t, e, "qqqreplybodyseed"); len(hits) != 0 {
		t.Fatalf("已删主题的回复不得被检索到: %+v", hits)
	}
}

// 删回复：行保留、状态置 deleted、列表与 FTS 都不可见；主题与兄弟回复不受影响。
func TestSoftDeletePostKeepsRowRemovesFromFTSAndLists(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "主题", 0, e.clock)
	pid := e.insertPost(t, tid, "qqqdeleteme1234", post.StatusNormal, e.clock)
	other := e.insertPost(t, tid, "qqqkeepme123456", post.StatusNormal, e.clock+1)
	if len(searchHits(t, e, "qqqdeleteme1234")) == 0 {
		t.Fatal("软删前应能检索到该回复")
	}

	var owner int64
	softDeleteTx(t, e, func(tx forum.WriteTx) error {
		var err error
		owner, err = tx.PostOwner(e.ctx, pid)
		if err != nil {
			return err
		}
		return tx.SoftDeletePost(e.ctx, pid)
	})
	if owner != e.userID {
		t.Fatalf("PostOwner 应返回作者 ID: %d", owner)
	}

	var status string
	if err := e.pools.QueryRowContext(e.ctx, `SELECT status FROM posts WHERE id = ?`, pid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("回复应软删为 deleted，实际 %q", status)
	}
	if got := countSoftDeleteRows(t, e, `SELECT COUNT(*) FROM posts WHERE thread_id = ?`, tid); got != 2 {
		t.Fatalf("软删回复不得物理删除行，期望 2 行，实际 %d", got)
	}
	visible, _, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: tid, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != other {
		t.Fatalf("已删回复不应出现在可见列表: %+v", visible)
	}
	if hits := searchHits(t, e, "qqqdeleteme1234"); len(hits) != 0 {
		t.Fatalf("已删回复不得被检索到: %+v", hits)
	}
	if hits := searchHits(t, e, "qqqkeepme123456"); len(hits) != 1 {
		t.Fatalf("兄弟回复不得被牵连: %+v", hits)
	}
}

// 判权读取与条件更新：已删目标对判权也不可见，重复软删返回 ErrNotFound。
func TestSoftDeleteOwnerReadsAndConditionalUpdate(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "主题", 0, e.clock)
	pid := e.insertPost(t, tid, "qqqownerprobe123", post.StatusNormal, e.clock)

	var owner int64
	softDeleteTx(t, e, func(tx forum.WriteTx) error {
		var err error
		owner, err = tx.PostOwner(e.ctx, pid)
		return err
	})
	if owner != e.userID {
		t.Fatalf("PostOwner 应返回作者 ID: %d", owner)
	}

	softDeleteTx(t, e, func(tx forum.WriteTx) error { return tx.SoftDeletePost(e.ctx, pid) })

	var errPost, errThread error
	var gotPost int64
	softDeleteTx(t, e, func(tx forum.WriteTx) error {
		gotPost, errPost = tx.PostOwner(e.ctx, pid)
		_, errThread = tx.ThreadOwner(e.ctx, 999999)
		err2 := tx.SoftDeletePost(e.ctx, pid) // 重复软删：条件更新 affected=0
		if !errors.Is(err2, forum.ErrNotFound) {
			t.Errorf("重复软删应返回 ErrNotFound，实际 %v", err2)
		}
		return nil
	})
	if !errors.Is(errPost, forum.ErrNotFound) || gotPost != 0 {
		t.Fatalf("已删回复对判权应视为不存在: owner=%d err=%v", gotPost, errPost)
	}
	if !errors.Is(errThread, forum.ErrNotFound) {
		t.Fatalf("不存在主题对判权应视为不存在: %v", errThread)
	}

	softDeleteTx(t, e, func(tx forum.WriteTx) error { return tx.SoftDeleteThread(e.ctx, tid) })
	var afterThread int64
	var errAfter error
	softDeleteTx(t, e, func(tx forum.WriteTx) error {
		afterThread, errAfter = tx.ThreadOwner(e.ctx, tid)
		return nil
	})
	if !errors.Is(errAfter, forum.ErrNotFound) || afterThread != 0 {
		t.Fatalf("已删主题对判权应视为不存在: owner=%d err=%v", afterThread, errAfter)
	}
}
