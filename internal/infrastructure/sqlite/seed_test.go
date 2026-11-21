package sqlite

import (
	"context"
	"slices"
	"testing"
)

// P0-09 回归集：空库迁移后版块齐全、重复迁移不重复插入、手工重放不覆盖已有数据。

var seedNodes = map[string]string{
	"/dev/ai":   "AI 与大模型",
	"/dev/rust": "Rust",
	"/dev/jobs": "工作与招聘",
	"/etc/rant": "随便聊聊",
	"/bin/help": "帮助与支持",
	// v0.6.0 / 0010：彩蛋隐藏节点（status=archived，默认不在 active 列表）。
	"/dev/null": "黑洞",
}

// 复用 write_test.go 里同包的 countNodes(ctx, pools)，避免重复定义。
func TestSeedNodes(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	applied, err := Migrate(ctx, pools)
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if !slices.Contains(applied, int64(2)) {
		t.Fatalf("0002 应被应用，实际 applied=%v", applied)
	}

	if got := countNodes(ctx, pools); got != len(seedNodes) {
		t.Fatalf("空库迁移后应有 %d 个版块，实际 %d", len(seedNodes), got)
	}

	rows, err := pools.reader.QueryContext(ctx, `SELECT slug, name, description FROM nodes`)
	if err != nil {
		t.Fatalf("查询版块: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]string)
	for rows.Next() {
		var slug, name, desc string
		if err := rows.Scan(&slug, &name, &desc); err != nil {
			t.Fatalf("扫描版块: %v", err)
		}
		if desc == "" {
			t.Errorf("版块 %q 的 description 不应为空", slug)
		}
		seen[slug] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历版块: %v", err)
	}
	for slug, name := range seedNodes {
		if seen[slug] != name {
			t.Errorf("版块 %q 应为 %q，实际 %q", slug, name, seen[slug])
		}
	}

	// 版本登记必须只有一行，否则「重复迁移不重放」的保证就不可靠。
	var versions int
	if err := pools.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations 中 version=2 应恰有 1 行，实际 %d", versions)
	}

	// 重复迁移：版本登记生效，不应再插入。
	again, err := Migrate(ctx, pools)
	if err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("重复迁移不应再应用版本，实际 %v", again)
	}
	if got := countNodes(ctx, pools); got != len(seedNodes) {
		t.Fatalf("重复迁移后版块数应不变，实际 %d", got)
	}
}

func TestSeedNodesReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 模拟运维手工重放迁移文件（或版本登记丢失后重建）：
	// INSERT OR IGNORE 必须既不增行，也不覆盖已有名称。
	if _, err := pools.writer.ExecContext(ctx, `UPDATE nodes SET name = '被改过的名字' WHERE slug = '/dev/ai'`); err != nil {
		t.Fatal(err)
	}
	body, err := migrationFiles.ReadFile("migrations/0002_seed_nodes.sql")
	if err != nil {
		t.Fatalf("读取 0002 迁移: %v", err)
	}
	if _, err := pools.writer.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("重放 0002 失败: %v", err)
	}

	if got := countNodes(ctx, pools); got != len(seedNodes) {
		t.Fatalf("重放后版块数应为 %d，实际 %d", len(seedNodes), got)
	}
	var name string
	if err := pools.reader.QueryRowContext(ctx,
		`SELECT name FROM nodes WHERE slug = '/dev/ai'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "被改过的名字" {
		t.Fatalf("重放不应覆盖已有版块名称，实际 %q", name)
	}
}
