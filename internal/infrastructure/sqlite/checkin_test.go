package sqlite

// v0.6.0：签到与隐藏节点解锁的 SQLite 回归。

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func setupCheckInEnv(t *testing.T) (*CheckInStore, context.Context, int64) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, t.TempDir()+"/motd.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(writer.Stop)
	now := time.Now().Unix()
	var uid int64
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`, "tester", now, now)
		if err != nil {
			return err
		}
		uid, _ = res.LastInsertId()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return NewCheckInStore(pools, writer), ctx, uid
}

func TestCheckInRecordAndStreak(t *testing.T) {
	store, ctx, uid := setupCheckInEnv(t)
	now := time.Now()
	for i := 0; i < 7; i++ {
		date := now.AddDate(0, 0, -6+i).Format("2006-01-02")
		if err := store.Record(ctx, uid, date, now); err != nil {
			t.Fatal(err)
		}
	}
	// 重复同一天幂等（UNIQUE 约束不报错）。
	if err := store.Record(ctx, uid, now.Format("2006-01-02"), now); err != nil {
		t.Fatalf("重复签到应幂等: %v", err)
	}
	var count int
	if err := store.pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM daily_checkins WHERE user_id = ?`, uid).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 7 {
		t.Errorf("7 天签到应只有 7 条记录，实际 %d", count)
	}
	streak, err := store.Streak(ctx, uid, now.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if streak != 7 {
		t.Errorf("连续天数应为 7，实际 %d", streak)
	}
}

func TestHiddenNodeUnlock(t *testing.T) {
	store, ctx, uid := setupCheckInEnv(t)
	// 种子里 /dev/null 是 hidden，默认不在 active 列表。
	var count int
	if err := store.pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE slug = '/dev/null' AND status = 'archived'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("应有默认不可见的 /dev/null 种子节点，实际 %d", count)
	}
	// 未解锁时 UnlockedNodes 为空。
	nodes, err := store.UnlockedNodes(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("未解锁应返回空，实际 %d", len(nodes))
	}
	// 解锁后可见，且字段完整。
	if err := store.UnlockSlug(ctx, uid, "/dev/null", time.Now()); err != nil {
		t.Fatal(err)
	}
	nodes, err = store.UnlockedNodes(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Slug != "/dev/null" {
		t.Fatalf("解锁后应返回 /dev/null，实际 %+v", nodes)
	}
	if nodes[0].ID <= 0 || nodes[0].Name == "" {
		t.Errorf("解锁节点字段不完整: %+v", nodes[0])
	}
	// 重复解锁幂等。
	if err := store.UnlockSlug(ctx, uid, "/dev/null", time.Now()); err != nil {
		t.Fatal(err)
	}
	nodes, _ = store.UnlockedNodes(ctx, uid)
	if len(nodes) != 1 {
		t.Errorf("重复解锁应幂等，实际 %d 条", len(nodes))
	}
	// 非法 slug 拒绝。
	if err := store.UnlockSlug(ctx, uid, "/not/hidden", time.Now()); err == nil {
		t.Error("不存在的隐藏节点应报错")
	}
}
