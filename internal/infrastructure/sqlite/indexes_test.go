package sqlite

import (
	"context"
	"strings"
	"testing"
)

// P0-10 回归：限流查询必须走覆盖索引，而不是每次写入都全表扫描。
func TestRateLimitIndexes(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	applied, err := Migrate(ctx, pools)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !containsVersion(applied, 3) {
		t.Fatalf("0003 应被应用，实际 %v", applied)
	}

	for _, name := range []string{"idx_threads_user_created", "idx_posts_user_created"} {
		var got string
		if err := pools.reader.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("缺少索引 %q: %v", name, err)
		}
	}

	// 用真实的 RecentWriteStats 语句看执行计划，确认两个索引都被选中。
	query := `
		SELECT COUNT(DISTINCT created_at), COALESCE(MAX(created_at), 0), COALESCE(MIN(created_at), 0)
		FROM (
			SELECT created_at FROM threads WHERE user_id = ? AND created_at >= ?
			UNION ALL
			SELECT created_at FROM posts   WHERE user_id = ? AND created_at >= ?
		)`
	rows, err := pools.reader.QueryContext(ctx,
		`EXPLAIN QUERY PLAN `+query, int64(1), int64(0), int64(1), int64(0))
	if err != nil {
		t.Fatalf("查看执行计划: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("扫描执行计划: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := plan.String()
	t.Logf("限流查询执行计划:\n%s", text)
	for _, name := range []string{"idx_threads_user_created", "idx_posts_user_created"} {
		if !strings.Contains(text, name) {
			t.Errorf("执行计划未使用 %s，可能退化为全表扫描:\n%s", name, text)
		}
	}
}

func containsVersion(applied []int64, want int64) bool {
	for _, v := range applied {
		if v == want {
			return true
		}
	}
	return false
}

// TestMigrationOrderIsStable 防止新增迁移打乱版本号顺序，
// 也顺带锁定「当前共 9 个迁移」这一事实（新增迁移时需同步更新）。
func TestMigrationOrderIsStable(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("读取迁移列表: %v", err)
	}
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if len(migrations) != len(want) {
		t.Fatalf("迁移数量应为 %d，实际 %d（新增迁移请同步更新本断言）", len(want), len(migrations))
	}
	for i, m := range migrations {
		if m.Version != want[i] {
			t.Errorf("第 %d 个迁移版本应为 %d，实际 %d", i+1, want[i], m.Version)
		}
	}
}
