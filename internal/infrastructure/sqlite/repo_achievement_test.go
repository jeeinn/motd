package sqlite

// v0.5.0 / P3-M1：成就落 SQLite 的持久化回归。
// 覆盖：Has/Grant 幂等、计数、重启后不丢（内存占位实现做不到的核心差异）、Entries 稳定顺序。

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// newAchievementStore 打开库、迁移、按需种用户（achievements.user_id 有外键），
// 返回仓储与关闭函数。userIDs 为空则不种用户。
func newAchievementStore(t *testing.T, path string, userIDs ...int64) (*AchievementStore, func()) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	w := pools.NewWriter()
	w.Start()
	for _, id := range userIDs {
		uid := id
		if err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO users (id, username, status, created_at, updated_at)
				VALUES (?, ?, 'active', 0, 0)`, uid, fmt.Sprintf("ach_user_%d", uid))
			return err
		}); err != nil {
			t.Fatalf("种用户 %d: %v", id, err)
		}
	}
	stop := func() {
		w.Stop()
		_ = pools.Close()
	}
	return NewAchievementStore(pools, w), stop
}

func TestAchievementStoreGrantIsIdempotentAndCounted(t *testing.T) {
	ctx := context.Background()
	store, stop := newAchievementStore(t, filepath.Join(t.TempDir(), "motd.db"), 1, 2)
	defer stop()

	code := domainuser.AchievementFirstPost
	has, err := store.Has(ctx, 1, code)
	if err != nil || has {
		t.Fatalf("初始应未解锁: has=%v err=%v", has, err)
	}

	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := store.Grant(ctx, 1, code, at); err != nil {
		t.Fatalf("首次授予: %v", err)
	}
	// 重复授予必须幂等（UNIQUE(user_id, code) + ON CONFLICT DO NOTHING）。
	if err := store.Grant(ctx, 1, code, at.Add(time.Hour)); err != nil {
		t.Fatalf("重复授予: %v", err)
	}
	has, err = store.Has(ctx, 1, code)
	if err != nil || !has {
		t.Fatalf("授予后应已解锁: has=%v err=%v", has, err)
	}
	if got := store.GrantedCount(); got != 1 {
		t.Fatalf("重复授予不应产生第二条记录，计数应为 1，实际 %d", got)
	}
	entries := store.Entries()
	if len(entries) != 1 || entries[0].Code != code || entries[0].UserID != 1 {
		t.Fatalf("Entries 快照不正确: %+v", entries)
	}
	// 不同用户同代码应各自独立。
	if err := store.Grant(ctx, 2, code, at); err != nil {
		t.Fatalf("另一用户授予: %v", err)
	}
	if got := store.GrantedCount(); got != 2 {
		t.Fatalf("两个用户解锁同一成就，计数应为 2，实际 %d", got)
	}
}

func TestAchievementStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "motd.db")
	code := domainuser.AchievementFirstPost
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	store, stop := newAchievementStore(t, path, 7)
	if err := store.Grant(ctx, 7, code, at); err != nil {
		t.Fatalf("授予: %v", err)
	}
	stop() // 模拟进程重启：关闭 writer 与连接池

	reopened, stop2 := newAchievementStore(t, path)
	defer stop2()
	has, err := reopened.Has(ctx, 7, code)
	if err != nil || !has {
		t.Fatalf("重启后成就应仍在: has=%v err=%v", has, err)
	}
	if got := reopened.GrantedCount(); got != 1 {
		t.Fatalf("重启后计数应为 1，实际 %d", got)
	}
}
