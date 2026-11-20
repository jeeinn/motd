package sqlite

// P2-M2 / ADR-051 的迁移回归（0009_edited_at）：
//  1. fresh 库应用后 posts 有 edited_at 列，且为可空、无 DEFAULT（NULL = 从未编辑）；
//  2. 历史行（迁移前写入）保持 NULL，scanPost 的 COALESCE 把它读成 0；
//  3. v8 备份恢复精确回 v8（无 edited_at 列），再应用 0009 可正常加列。
//
// 全程 t.TempDir()，不碰 data/ 下任何真实库。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func editedAtColumnNullable(t *testing.T, db *sql.DB) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(posts)`)
	if err != nil {
		t.Fatalf("读取 posts 表结构: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描表结构: %v", err)
		}
		if name == "edited_at" {
			// 可空 + 无默认值：NULL 本身就是「未编辑」，历史行无需回填。
			return notnull == 0 && dflt == nil
		}
	}
	t.Fatal("posts 表缺少 edited_at 列")
	return false
}

func TestEditedAtColumnExistsAndNullable(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !hasColumn(t, pools.writer, "posts", "edited_at") {
		t.Fatal("应用 0009 后 posts 应有 edited_at 列")
	}
	if !editedAtColumnNullable(t, pools.writer) {
		t.Fatal("edited_at 必须可空且无 DEFAULT（NULL = 从未编辑）")
	}
	// threads 不该有此列：正文在首帖，标记读首帖。
	if hasColumn(t, pools.writer, "threads", "edited_at") {
		t.Fatal("threads 不应有 edited_at：正文存在首帖")
	}
}

func TestEditedAtBackfillsHistoricalRowsAsNull(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	// 只用 0001–0008 建库并写一条历史 post（当时还没有 edited_at 列）。
	applyThrough(ctx, t, pools.writer, 8)
	res, err := pools.writer.ExecContext(ctx, `
		INSERT INTO users (username, status, created_at, updated_at) VALUES ('hist_edit', 'active', 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	res, err = pools.writer.ExecContext(ctx, `
		INSERT INTO threads (node_id, user_id, title, last_reply_at, created_at) VALUES (1, ?, '历史主题', 1, 1)`, uid)
	if err != nil {
		t.Fatal(err)
	}
	tid, _ := res.LastInsertId()
	res, err = pools.writer.ExecContext(ctx, `
		INSERT INTO posts (thread_id, user_id, body_md, status, created_at) VALUES (?, ?, '历史正文', 'normal', 1)`, tid, uid)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := res.LastInsertId()

	// 应用 0009。
	all, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version == 9 {
			if err := applyOne(ctx, pools.writer, m); err != nil {
				t.Fatalf("应用 0009: %v", err)
			}
		}
	}

	var edited sql.NullInt64
	if err := pools.reader.QueryRowContext(ctx, `SELECT edited_at FROM posts WHERE id = ?`, pid).Scan(&edited); err != nil {
		t.Fatalf("读取历史行 edited_at: %v", err)
	}
	if edited.Valid {
		t.Fatalf("历史行 edited_at 应保持 NULL（未编辑），实际 %d", edited.Int64)
	}
	// scanPost 的 COALESCE 口径：NULL 读成 0。
	var coalesced int64
	if err := pools.reader.QueryRowContext(ctx, `SELECT COALESCE(edited_at, 0) FROM posts WHERE id = ?`, pid).Scan(&coalesced); err != nil {
		t.Fatal(err)
	}
	if coalesced != 0 {
		t.Fatalf("COALESCE(NULL,0) 应为 0，实际 %d", coalesced)
	}
}

func TestEditedAtRollbackToVersion8(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "motd.db")
	pools, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}

	applyThrough(ctx, t, pools.writer, 8)
	if hasColumn(t, pools.writer, "posts", "edited_at") {
		t.Fatal("v8 库不应有 posts.edited_at 列")
	}

	backup, err := Backup(ctx, pools, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatalf("迁移前备份: %v", err)
	}
	all, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version == 9 {
			if err := applyOne(ctx, pools.writer, m); err != nil {
				t.Fatalf("应用 0009: %v", err)
			}
		}
	}
	if !hasColumn(t, pools.writer, "posts", "edited_at") {
		t.Fatal("应用 0009 后应有 edited_at 列")
	}
	if err := pools.Close(); err != nil {
		t.Fatal(err)
	}

	// 回滚到 v8 备份：版本与 schema 都精确回退。
	target := filepath.Join(dir, "rollback", "motd.db")
	if err := Restore(ctx, backup.Path, target); err != nil {
		t.Fatalf("回滚恢复: %v", err)
	}
	restored, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("打开恢复库: %v", err)
	}
	defer func() { _ = restored.Close() }()

	var v int64
	if err := restored.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 8 {
		t.Fatalf("回滚后版本应为 8，实际 %d", v)
	}
	if hasColumn(t, restored.writer, "posts", "edited_at") {
		t.Fatal("回滚后 posts.edited_at 列应不存在")
	}
}
