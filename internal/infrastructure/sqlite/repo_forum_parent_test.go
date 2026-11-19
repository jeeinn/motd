package sqlite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
)

// R8 验收：父帖必须属于同一主题。
// 应用层（InsertPost 同事务先查后插）与数据库触发器（0005）两条路径都要挡住。

func TestInsertPostRejectsParentFromAnotherThread(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	threadA := e.insertThread(t, "主题A", 0, e.clock)
	threadB := e.insertThread(t, "主题B", 0, e.clock)
	parentID := e.insertPost(t, threadA, "父帖", post.StatusNormal, e.clock)
	created := time.Unix(e.clock, 0).UTC()

	// 合法：父帖与回复在同一主题。
	err := e.repos.Write(ctx, func(tx forum.WriteTx) error {
		_, err := tx.InsertPost(ctx, forum.NewPost{
			ThreadID: threadA, UserID: e.userID, BodyMD: "同主题回复",
			Status: post.StatusNormal, ParentPostID: &parentID, CreatedAt: created,
		})
		return err
	})
	if err != nil {
		t.Fatalf("同主题回复应被接受: %v", err)
	}

	// 违规：父帖在主题 A，回复却要写进主题 B。
	err = e.repos.Write(ctx, func(tx forum.WriteTx) error {
		_, err := tx.InsertPost(ctx, forum.NewPost{
			ThreadID: threadB, UserID: e.userID, BodyMD: "跨主题回复",
			Status: post.StatusNormal, ParentPostID: &parentID, CreatedAt: created,
		})
		return err
	})
	if !errors.Is(err, forum.ErrParentThreadMismatch) {
		t.Fatalf("跨主题回复应返回 ErrParentThreadMismatch，实际 %v", err)
	}
	rows, _, err := e.repos.ListPosts(ctx, forum.PostQuery{ThreadID: threadB, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("违规回复不应落库，实际 %d 条", len(rows))
	}
}

func TestParentTriggerRejectsRawCrossThreadWrite(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	threadA := e.insertThread(t, "主题A", 0, e.clock)
	threadB := e.insertThread(t, "主题B", 0, e.clock)
	parentID := e.insertPost(t, threadA, "父帖", post.StatusNormal, e.clock)

	// 绕过 WriteTx 的裸 SQL 也必须被触发器拦下。
	_, err := e.pools.writer.ExecContext(ctx, `
		INSERT INTO posts (thread_id, user_id, body_md, status, parent_post_id, created_at)
		VALUES (?, ?, '跨主题裸写', 'normal', ?, ?)`,
		threadB, e.userID, parentID, e.clock)
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "PARENT POST MUST BE IN SAME THREAD") {
		t.Fatalf("触发器应拒绝跨主题父帖，实际 %v", err)
	}

	// 合法同主题裸写不受影响。
	if _, err := e.pools.writer.ExecContext(ctx, `
		INSERT INTO posts (thread_id, user_id, body_md, status, parent_post_id, created_at)
		VALUES (?, ?, '同主题裸写', 'normal', ?, ?)`,
		threadA, e.userID, parentID, e.clock); err != nil {
		t.Fatalf("触发器不应拒绝同主题父帖: %v", err)
	}

	// UPDATE 把已有子帖改到别的主题：BEFORE UPDATE 触发器同样要拦。
	var childID int64
	if err := e.pools.reader.QueryRowContext(ctx,
		`SELECT id FROM posts WHERE body_md = '同主题裸写'`).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pools.writer.ExecContext(ctx,
		`UPDATE posts SET thread_id = ? WHERE id = ?`, threadB, childID); err == nil ||
		!strings.Contains(strings.ToUpper(err.Error()), "PARENT POST MUST BE IN SAME THREAD") {
		t.Fatalf("触发器应拒绝跨主题 UPDATE，实际 %v", err)
	}
}
