package sqlite

import (
	"context"
	"testing"
)

// 作者名投影（v0.3.3）：一次批量查 id → username，非 N+1；
// deleted 用户仍是历史帖作者，用户名照常可读。

func insertNamedUser(t *testing.T, e *forumEnv, username, status string) int64 {
	t.Helper()
	res, err := e.pools.writer.ExecContext(e.ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		username, status, e.clock, e.clock)
	if err != nil {
		t.Fatalf("插入用户 %q: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取用户 ID: %v", err)
	}
	return id
}

func TestUsernamesByIDsBatchProjection(t *testing.T) {
	e := newForumEnv(t)
	bob := insertNamedUser(t, e, "bob", "deleted")
	carol := insertNamedUser(t, e, "carol", "active")

	got, err := e.repos.UsernamesByIDs(e.ctx, []int64{e.userID, bob, 987654321, e.userID, carol})
	if err != nil {
		t.Fatalf("批量查询用户名: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应只返回 3 个存在的用户（重复 id 不重复计数）: %+v", got)
	}
	if got[e.userID] != "tester" {
		t.Errorf("tester 用户名错误: %q", got[e.userID])
	}
	if got[bob] != "bob" {
		t.Errorf("deleted 用户仍应返回用户名，实际 %q", got[bob])
	}
	if got[carol] != "carol" {
		t.Errorf("carol 用户名错误: %q", got[carol])
	}
	if _, ok := got[987654321]; ok {
		t.Error("不存在的 id 不应出现在结果里")
	}
}

func TestUsernamesByIDsEmptyInput(t *testing.T) {
	e := newForumEnv(t)
	for _, ids := range [][]int64{nil, {}, {0, -1}} {
		got, err := e.repos.UsernamesByIDs(context.Background(), ids)
		if err != nil {
			t.Fatalf("ids=%v 不应报错: %v", ids, err)
		}
		if len(got) != 0 {
			t.Fatalf("ids=%v 应返回空 map，实际 %+v", ids, got)
		}
	}
}
