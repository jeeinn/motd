package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// ADR-048 节点三不变量的迁移回归：
//  1. 0008 应用后 nodes 有 status 列，5 个 seed 自动回填 active，CHECK 拒绝非法值；
//  2. 从 v7 备份恢复能精确回到版本 7（schema 与数据双一致），再应用 0008 仍可回填。
//
// 全程 t.TempDir()，不碰 data/ 下任何真实库。

func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("读取 %s 表结构: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描表结构: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func applyThrough(ctx context.Context, t *testing.T, db *sql.DB, upTo int64) {
	t.Helper()
	all, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version <= upTo {
			if err := applyOne(ctx, db, m); err != nil {
				t.Fatalf("应用迁移 %04d: %v", m.Version, err)
			}
		}
	}
}

func TestNodesStatusBackfillsSeedRows(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	var version int64
	if err := pools.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	// fresh 库会一路应用到最新（现为 11，含 v0.7.1 的 0011）；0008 的回填断言在下方可独立核验。
	if version != 11 {
		t.Fatalf("fresh 库迁移后版本应为 11，实际 %d", version)
	}
	if !hasColumn(t, pools.writer, "nodes", "status") {
		t.Fatal("0008 应用后 nodes 应有 status 列")
	}

	var total, active, archived int64
	if err := pools.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := pools.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE status = 'active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := pools.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE status = 'archived'`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	// v0.6.0 / 0010 追加了彩蛋节点 /dev/null（archived），因此 6 总 / 5 active / 1 archived。
	if total != 6 || active != 5 || archived != 1 {
		t.Fatalf("seed 状态分布应为 6/5/1（含彩蛋隐藏节点）: total=%d active=%d archived=%d", total, active, archived)
	}

	// 归档是可逆 UPDATE，非法值必须被 CHECK 挡住。
	res, err := pools.writer.ExecContext(ctx, `UPDATE nodes SET status = 'archived' WHERE slug = '/bin/help'`)
	if err != nil {
		t.Fatalf("归档 seed 节点: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("应归档 1 行，实际 %d", n)
	}
	if _, err := pools.writer.ExecContext(ctx, `UPDATE nodes SET status = 'bogus' WHERE slug = '/bin/help'`); err == nil {
		t.Fatal("0008 的 CHECK 应拒绝非法 status")
	}
}

func TestNodesStatusRollbackToVersion7(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "motd.db")
	pools, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	applyThrough(ctx, t, pools.writer, 7)

	if hasColumn(t, pools.writer, "nodes", "status") {
		t.Fatal("v7 库不应有 nodes.status 列")
	}

	// 造一条真实业务数据，回滚后要能核对它没丢。
	if _, err := pools.writer.ExecContext(ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES ('m0_hist', 'active', 1, 1)`); err != nil {
		t.Fatalf("造历史用户: %v", err)
	}
	if _, err := pools.writer.ExecContext(ctx,
		`INSERT INTO threads (node_id, user_id, title, last_reply_at, created_at) VALUES (1, 1, 'M0-HISTORY', 1, 1)`); err != nil {
		t.Fatalf("造历史主题: %v", err)
	}

	backup, err := Backup(ctx, pools, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatalf("迁移前备份: %v", err)
	}

	// 应用 0008：历史节点自动回填。
	all, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version == 8 {
			if err := applyOne(ctx, pools.writer, m); err != nil {
				t.Fatalf("应用 0008: %v", err)
			}
		}
	}
	var v8 int64
	if err := pools.writer.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v8); err != nil {
		t.Fatal(err)
	}
	if v8 != 8 {
		t.Fatalf("迁移后版本应为 8，实际 %d", v8)
	}
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭库: %v", err)
	}

	// 回滚：恢复到迁移前备份，版本与 schema 都要精确回 v7。
	target := filepath.Join(dir, "rollback", "motd.db")
	if err := Restore(ctx, backup.Path, target); err != nil {
		t.Fatalf("回滚恢复: %v", err)
	}
	restored, err := Open(ctx, target)
	if err != nil {
		t.Fatalf("打开恢复库: %v", err)
	}
	defer func() { _ = restored.Close() }()

	var v7 int64
	if err := restored.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v7); err != nil {
		t.Fatal(err)
	}
	if v7 != 7 {
		t.Fatalf("回滚后版本应为 7，实际 %d", v7)
	}
	if hasColumn(t, restored.writer, "nodes", "status") {
		t.Fatal("回滚后 nodes.status 列应不存在")
	}
	var users, threads, nodes int64
	if err := restored.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := restored.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads`).Scan(&threads); err != nil {
		t.Fatal(err)
	}
	if err := restored.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&nodes); err != nil {
		t.Fatal(err)
	}
	if users != 1 || threads != 1 || nodes != 5 {
		t.Fatalf("回滚后数据应完整: users=%d threads=%d nodes=%d", users, threads, nodes)
	}
}
