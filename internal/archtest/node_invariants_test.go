package archtest

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-048 节点三不变量静态守卫（P2-M0）：
//  1. 0008 迁移必须给 nodes 加 status 列，NOT NULL DEFAULT 'active' + CHECK(active/archived)；
//  2. 0002 的 5 个 seed slug 必须原样存在，且仍是 INSERT OR IGNORE；
//  3. 公开列表 ListNodes 的 SQL 必须带 status='active' 过滤；
//  4. 生产 Go 代码不得出现 DELETE FROM nodes（归档不物理删）。
//
// 每条守卫都有负向注入，证明拿掉规则会报警——静态断言假绿比没有守卫更危险。

var expectedSeedSlugs = []string{
	"/dev/ai", "/dev/rust", "/dev/jobs", "/etc/rant", "/bin/help",
}

// compact 去掉所有空白，容忍 SQL 换行与空格差异。
func compact(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// stripSQLComments 去掉 -- 行注释，避免注释里的示例 SQL 触发假阳性。
func stripSQLComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

func readMigration(dir, name string) (string, bool) {
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	return string(body), true
}

func collectNodeDDLIssues(migrationsDir string) ([]string, error) {
	var issues []string
	body, ok := readMigration(migrationsDir, "0008_nodes_status.sql")
	if !ok {
		body = ""
		issues = append(issues, "缺少 0008_nodes_status.sql 迁移")
	}
	clean := compact(stripSQLComments(body))
	if !strings.Contains(clean, "ALTERTABLEnodes") {
		issues = append(issues, "0008 必须对 nodes 执行 ALTER TABLE")
	}
	if !strings.Contains(clean, "ADDCOLUMNstatus") {
		issues = append(issues, "0008 必须 ADD COLUMN status")
	}
	if !strings.Contains(clean, "NOTNULLDEFAULT'active'") {
		issues = append(issues, "0008 的 status 必须 NOT NULL DEFAULT 'active' 以回填历史行")
	}
	if !strings.Contains(clean, "statusIN('active','archived')") {
		issues = append(issues, "0008 的 CHECK 必须限定 status IN ('active','archived')")
	}
	if strings.Contains(clean, "merged_into_id") {
		issues = append(issues, "nodes 合并已推 P3，M0 迁移不得引入 merged_into_id")
	}
	return issues, nil
}

func collectSeedSlugsIssues(migrationsDir string) ([]string, error) {
	var issues []string
	body, ok := readMigration(migrationsDir, "0002_seed_nodes.sql")
	if !ok {
		return []string{"缺少 0002_seed_nodes.sql 迁移"}, nil
	}
	clean := compact(stripSQLComments(body))
	if !strings.Contains(clean, "INSERTORIGNORE") {
		issues = append(issues, "0002 必须保持 INSERT OR IGNORE（重放不覆盖用户改名）")
	}
	for _, slug := range expectedSeedSlugs {
		if !strings.Contains(body, "'"+slug+"'") {
			issues = append(issues, "seed slug 被改动或删除: "+slug)
		}
	}
	return issues, nil
}

func collectNodeQueryIssues(infraDir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(infraDir)
	if err != nil {
		return nil, err
	}
	fn, ok := findFuncDecl(files, "ListNodes")
	if !ok {
		return []string{"扫描不到 ListNodes 实现，公开列表过滤守卫失效"}, nil
	}
	var issues []string
	filtered := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if strings.Contains(compact(strings.Trim(lit.Value, "`\"")), "status='active'") {
			filtered = true
		}
		return true
	})
	if !filtered {
		issues = append(issues, "ListNodes 的 SQL 必须带 WHERE status = 'active'（归档节点不得出现在公开列表）")
	}
	return issues, nil
}

// collectNodeDeleteIssues 只扫 internal/ 与 cmd/，与测试计数口径一致，不碰 data/ 下的演练副本。
func collectNodeDeleteIssues(root string) ([]string, error) {
	var issues []string
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue // 负向注入的临时目录可能只有其中一个子目录
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(strings.ToUpper(string(body)), "DELETE FROM NODES") {
				issues = append(issues, "生产代码不得物理删节点: "+path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return issues, nil
}

func assertNoIssues(t *testing.T, issues []string, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) > 0 {
		t.Fatalf("%s 未通过:\n  %s", label, strings.Join(issues, "\n  "))
	}
}

func TestNodeInvariantsGuards(t *testing.T) {
	root := findModuleRoot(t)
	migrations := filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations")
	infra := filepath.Join(root, "internal", "infrastructure", "sqlite")

	ddl, err := collectNodeDDLIssues(migrations)
	assertNoIssues(t, ddl, err, "节点 status DDL 守卫")
	seed, err := collectSeedSlugsIssues(migrations)
	assertNoIssues(t, seed, err, "seed slug 守卫")
	query, err := collectNodeQueryIssues(infra)
	assertNoIssues(t, query, err, "公开列表过滤守卫")
	del, err := collectNodeDeleteIssues(root)
	assertNoIssues(t, del, err, "禁止物理删守卫")
}

// ---- 负向注入：证明每条守卫不是摆设 ----

func TestGuardDetectsWeakNodeStatusDDL(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0008_nodes_status.sql"), []byte(`
ALTER TABLE nodes ADD COLUMN status TEXT;
`), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeDDLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"NOT NULL DEFAULT 'active'", "CHECK 必须限定"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("注入弱 DDL 后未检出 %q: %s", want, joined)
		}
	}
}

func TestGuardDetectsMergedIntoInMigration(t *testing.T) {
	dir := t.TempDir()
	src := `
ALTER TABLE nodes ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
  CHECK (status IN ('active','archived'));
ALTER TABLE nodes ADD COLUMN merged_into_id INTEGER;
`
	if err := os.WriteFile(filepath.Join(dir, "0008_nodes_status.sql"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeDDLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "merged_into_id") {
		t.Fatalf("注入 merged_into_id 后未检出: %v", issues)
	}
}

func TestGuardDetectsChangedSeedSlug(t *testing.T) {
	dir := t.TempDir()
	src := `INSERT OR IGNORE INTO nodes (slug, name, description) VALUES
  ('/dev/ai', 'AI', ''),
  ('/dev/rust', 'Rust', ''),
  ('/dev/jobs', 'Jobs', ''),
  ('/etc/rant', 'Rant', '');
`
	if err := os.WriteFile(filepath.Join(dir, "0002_seed_nodes.sql"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectSeedSlugsIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "/bin/help") {
		t.Fatalf("删掉 seed slug 后未检出: %v", issues)
	}
}

func TestGuardDetectsUnfilteredListNodes(t *testing.T) {
	dir := t.TempDir()
	src := `package sqlite
func (r *ForumRepos) ListNodes(ctx context.Context) ([]node.Node, error) {
	return r.pools.QueryContext(ctx, selectNodes + " ORDER BY id ASC")
}
`
	if err := os.WriteFile(filepath.Join(dir, "repo_forum.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeQueryIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "status = 'active'") {
		t.Fatalf("注入无过滤 ListNodes 后未检出: %v", issues)
	}
}

func TestGuardDetectsPhysicalNodeDelete(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := `package bad
func drop() { db.Exec("DELETE FROM nodes WHERE id = ?", 1) }
`
	if err := os.WriteFile(filepath.Join(root, "internal", "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeDeleteIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "bad.go") {
		t.Fatalf("注入 DELETE FROM nodes 后未检出: %v", issues)
	}
}
