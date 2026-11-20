package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// 迁移基线的回归测试：结构可建、可重复执行、关键约束真的生效。
// 这些断言对应 P0-05 的验收标准，改动 DDL 时应同步更新。

func openTestPools(t *testing.T) *Pools {
	t.Helper()
	path := filepath.Join(t.TempDir(), "motd.db")
	pools, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	applied, err := Migrate(ctx, pools)
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("首次迁移应至少应用一个版本")
	}

	again, err := Migrate(ctx, pools)
	if err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("重复迁移不应再应用任何版本，实际: %v", again)
	}
}

func TestMigrateCreatesExpectedTables(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	want := []string{
		"users", "identities", "local_credentials", "ssh_keys", "email_tokens",
		"oauth_states", "device_auth_states", "sessions", "auth_events",
		"nodes", "threads", "posts", "votes", "achievements",
		"domain_event_outbox", "posts_fts", "posts_fts_indexed", "schema_migrations",
	}
	for _, table := range want {
		var name string
		err := pools.reader.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("缺少表 %q: %v", table, err)
		}
	}
}

// TestThreadsStatusBackfillsHistoricalRows 复现「旧库升级」路径：
// 先在只有 0001–0003 的库上写入历史主题（当时还没有 status 列），
// 再应用 0004，确认 DEFAULT 常量把历史行回填为 active，且 CHECK 真的生效。
func TestThreadsStatusBackfillsHistoricalRows(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	all, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	// applyOne 假设 Migrate 已建好版本登记表；这里手动补建，模拟旧库。
	if _, err := pools.writer.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version <= 3 {
			if err := applyOne(ctx, pools.writer, m); err != nil {
				t.Fatalf("应用迁移 %04d: %v", m.Version, err)
			}
		}
	}

	now := time.Now().Unix()
	res, err := pools.writer.ExecContext(ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
		"old_user", now, now)
	if err != nil {
		t.Fatalf("准备历史用户: %v", err)
	}
	userID, _ := res.LastInsertId()
	res, err = pools.writer.ExecContext(ctx,
		`INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at) VALUES (1, ?, '历史主题', 0, ?, ?)`,
		userID, now, now)
	if err != nil {
		t.Fatalf("写入历史主题: %v", err)
	}
	threadID, _ := res.LastInsertId()

	var applied bool
	for _, m := range all {
		if m.Version == 4 {
			if err := applyOne(ctx, pools.writer, m); err != nil {
				t.Fatalf("应用 0004: %v", err)
			}
			applied = true
		}
	}
	if !applied {
		t.Fatal("未找到 0004_threads_status 迁移")
	}

	var status string
	if err := pools.reader.QueryRowContext(ctx, `SELECT status FROM threads WHERE id = ?`, threadID).Scan(&status); err != nil {
		t.Fatalf("读取历史主题 status: %v", err)
	}
	if status != "active" {
		t.Fatalf("历史行应回填为 active，实际 %q", status)
	}
	if _, err := pools.writer.ExecContext(ctx, `UPDATE threads SET status = 'bogus' WHERE id = ?`, threadID); err == nil {
		t.Fatal("0004 的 CHECK 应拒绝非法 status")
	}
}

// TestFreshDatabaseReachesVersion8 锁定 P2-M0 节点 status 迁移后的基线版本。
func TestFreshDatabaseReachesVersion8(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	var version int64
	if err := pools.reader.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 8 {
		t.Fatalf("fresh 库迁移后版本应为 8，实际 %d", version)
	}
}

func TestPragmaBaseline(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	var fk int
	if err := pools.reader.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err == nil {
		t.Logf("读连接 foreign_keys = %d（读连接不做写，取值不影响正确性）", fk)
	}

	// 写连接必须是 foreign_keys=ON，否则约束形同虚设。
	if err := pools.writer.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("读取 foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("写连接 foreign_keys 应为 1，实际 %d", fk)
	}

	var mode string
	if err := pools.writer.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode 应为 wal，实际 %q", mode)
	}
}

// TestUsernameConstraint 只验证 DDL 承担的两层兜底：长度、空白。
// 字符类别白名单（中文 / ASCII 字母数字 / _ / -）不由 DDL 表达，
// 事实来源是 domain/user.ValidateUsername，见 TestDDLMatchesDomainPolicy。
func TestUsernameConstraint(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	insert := func(name string) error {
		_, err := pools.writer.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', 0, 0)`, name)
		return err
	}

	// 长度按字符数计，中文一字算一个字符（SQLite length(TEXT) 的语义）。
	for _, name := range []string{"v_x", "A_B", "中文a", "中文测试", strings.Repeat("中", 20)} {
		if err := insert(name); err != nil {
			t.Fatalf("合法用户名 %q 应可插入: %v", name, err)
		}
	}

	// DDL 必须挡住的：长度越界 + 各类空白。
	bad := []string{
		"ab", "中", strings.Repeat("中", 21),
		"has space", "中 文", "中文 ", " 中文",
		"中\t文", "中\n文", "中\r文",
		"中\u3000文", // 全角空格 U+3000
	}
	for _, name := range bad {
		if err := insert(name); err == nil {
			t.Fatalf("非法用户名 %q 不应通过 CHECK 约束", name)
		}
	}

	// 其余 Unicode 空白（如 NBSP）DDL 兜底不到，必须由 domain 层拦截——这是明确的分层边界。
	if err := domainuser.ValidateUsername("中\u00a0文"); err == nil {
		t.Fatal("NBSP 等 Unicode 空白必须由 domain/user.ValidateUsername 拦截")
	}

	// NOCASE 唯一性：大小写不同视为同一个名字；中文无大小写，不受影响。
	if err := insert("V_X"); err == nil {
		t.Fatal("用户名唯一性应大小写不敏感")
	}
}

// TestDDLMatchesDomainPolicy 防止两层规则漂移：
// domain 层接受的合法用户名，DDL 一律不能拒绝。
func TestDDLMatchesDomainPolicy(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	valid := []string{
		"v_x", "A_B", "abc", "123", "a-b_c",
		"中文a", "中文测试", "中文abc", "中文_测试-1",
		strings.Repeat("中", 3), strings.Repeat("中", 20),
	}
	for _, name := range valid {
		if err := domainuser.ValidateUsername(name); err != nil {
			t.Fatalf("样本 %q 在 domain 层应合法: %v", name, err)
		}
		_, err := pools.writer.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', 0, 0)`, name)
		if err != nil {
			t.Fatalf("domain 层接受但 DDL 拒绝 %q，两层规则已漂移: %v", name, err)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	_, err := pools.writer.ExecContext(ctx,
		`INSERT INTO threads (node_id, user_id, title, last_reply_at, created_at) VALUES (999, 999, 'x', 0, 0)`)
	if err == nil {
		t.Fatal("外键约束未生效：应拒绝引用不存在的 node/user")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("期望外键错误，实际: %v", err)
	}
}
