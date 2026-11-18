package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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

	applied, err := Migrate(ctx, pools.Writer)
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("首次迁移应至少应用一个版本")
	}

	again, err := Migrate(ctx, pools.Writer)
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
	if _, err := Migrate(ctx, pools.Writer); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	want := []string{
		"users", "identities", "local_credentials", "ssh_keys", "email_tokens",
		"oauth_states", "device_auth_states", "sessions", "auth_events",
		"nodes", "threads", "posts", "votes", "achievements",
		"schema_migrations",
	}
	for _, table := range want {
		var name string
		err := pools.Reader.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("缺少表 %q: %v", table, err)
		}
	}
}

func TestPragmaBaseline(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	var fk int
	if err := pools.Reader.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err == nil {
		t.Logf("读连接 foreign_keys = %d（读连接不做写，取值不影响正确性）", fk)
	}

	// 写连接必须是 foreign_keys=ON，否则约束形同虚设。
	if err := pools.Writer.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("读取 foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("写连接 foreign_keys 应为 1，实际 %d", fk)
	}

	var mode string
	if err := pools.Writer.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
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
	if _, err := Migrate(ctx, pools.Writer); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	insert := func(name string) error {
		_, err := pools.Writer.ExecContext(ctx,
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
	if _, err := Migrate(ctx, pools.Writer); err != nil {
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
		_, err := pools.Writer.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', 0, 0)`, name)
		if err != nil {
			t.Fatalf("domain 层接受但 DDL 拒绝 %q，两层规则已漂移: %v", name, err)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools.Writer); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	_, err := pools.Writer.ExecContext(ctx,
		`INSERT INTO threads (node_id, user_id, title, last_reply_at, created_at) VALUES (999, 999, 'x', 0, 0)`)
	if err == nil {
		t.Fatal("外键约束未生效：应拒绝引用不存在的 node/user")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("期望外键错误，实际: %v", err)
	}
}
