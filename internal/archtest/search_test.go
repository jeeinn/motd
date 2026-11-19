package archtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P1-M3 检索静态守卫：把「FTS5 + trigram」「MATCH 参数化」「LIKE 转义」
// 三条从约定变成会失败的测试，并配负向注入证明守卫不是摆设。

func checkFTSMigrationSQL(sql string) []string {
	var issues []string
	for _, want := range []string{
		"CREATE VIRTUAL TABLE posts_fts USING fts5",
		"content='posts'",
		"content_rowid='id'",
		"tokenize='trigram'",
		"CREATE TRIGGER posts_fts_insert AFTER INSERT ON posts",
		"CREATE TRIGGER posts_fts_update AFTER UPDATE ON posts",
		"CREATE TRIGGER posts_fts_delete AFTER DELETE ON posts",
		"status = 'normal'",
		"posts_fts_indexed",
	} {
		if !strings.Contains(sql, want) {
			issues = append(issues, "0007 迁移缺少: "+want)
		}
	}
	return issues
}

func TestFTSMigrationIsComplete(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0007_posts_fts.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if issues := checkFTSMigrationSQL(string(b)); len(issues) != 0 {
		t.Fatalf("0007 迁移不完整:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestGuardDetectsMissingTrigram(t *testing.T) {
	bad := `CREATE VIRTUAL TABLE posts_fts USING fts5(body_md, content='posts');
CREATE TRIGGER posts_fts_insert AFTER INSERT ON posts BEGIN SELECT 1; END;
CREATE TRIGGER posts_fts_update AFTER UPDATE ON posts BEGIN SELECT 1; END;
CREATE TRIGGER posts_fts_delete AFTER DELETE ON posts BEGIN SELECT 1; END;
`
	if issues := checkFTSMigrationSQL(bad); len(issues) == 0 {
		t.Fatal("注入的「无 trigram / 无状态过滤」迁移未被检出")
	}
}

// checkSearchSQLText 检查仓储源码里的 SQL 使用方式：
// MATCH / LIKE 的值必须来自绑定参数，且 LIKE 必须带 ESCAPE。
func checkSearchSQLText(src string) []string {
	var issues []string
	if !strings.Contains(src, "MATCH ?") {
		issues = append(issues, "MATCH 必须参数化（应出现 MATCH ?）")
	}
	if !strings.Contains(src, "LIKE ? ESCAPE") {
		issues = append(issues, "LIKE 回退必须带 ESCAPE 子句")
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "fmt.Sprintf") &&
			(strings.Contains(line, "MATCH") || strings.Contains(line, "LIKE")) {
			issues = append(issues, "检索 SQL 不得用 fmt.Sprintf 拼接查询词")
		}
		if strings.Contains(line, `MATCH '"`) || strings.Contains(line, `MATCH "'`) {
			issues = append(issues, "MATCH 不得把查询词拼进引号字面量")
		}
	}
	if !strings.Contains(src, "FTSMatchTerm(") {
		issues = append(issues, "FTS 查询词未经过 FTSMatchTerm 转义")
	}
	if !strings.Contains(src, "EscapeLikePattern(") {
		issues = append(issues, "LIKE 查询词未经过 EscapeLikePattern 转义")
	}
	return issues
}

func TestSearchSQLIsParameterizedAndEscaped(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "infrastructure", "sqlite", "repo_search.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if issues := checkSearchSQLText(string(b)); len(issues) != 0 {
		t.Fatalf("检索 SQL 守卫未通过:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestGuardDetectsInlineMATCHInterpolation(t *testing.T) {
	bad := `package sqlite
func search(q string) {
	rows, _ := db.Query("SELECT rowid FROM posts_fts WHERE posts_fts MATCH '" + q + "'")
	rows2, _ := db.Query(fmt.Sprintf("SELECT rowid FROM posts WHERE body LIKE '%s'", q))
}
`
	issues := checkSearchSQLText(bad)
	if len(issues) == 0 {
		t.Fatal("注入的 MATCH 字符串拼接未被检出")
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"MATCH 必须参数化", "LIKE 回退必须带 ESCAPE", "fmt.Sprintf", "引号字面量"} {
		if !strings.Contains(joined, want) {
			t.Errorf("违规报告缺少 %q:\n%s", want, joined)
		}
	}
}

func TestGuardDetectsLIKEWithoutEscape(t *testing.T) {
	bad := `package sqlite
func search(q string) {
	db.Query("SELECT id FROM posts WHERE body LIKE ? LIMIT 20", q)
	_ = forum.FTSMatchTerm(q)
	_ = forum.EscapeLikePattern(q)
}
`
	if issues := checkSearchSQLText(bad); len(issues) == 0 {
		t.Fatal("注入的无 ESCAPE LIKE 未被检出")
	}
}

// 应用层的转义策略也要看得见：引号翻倍 + LIKE 三字符转义 + 摘要字节上限。
func TestSearchEscapePolicyIsPresent(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "forum", "search.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		`strings.ReplaceAll(s,`,
		`""`,
		`strings.NewReplacer`,
		`SearchMaxExcerptBytes`,
		`FTSMinRunes`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("search.go 缺少转义/边界策略: %s", want)
		}
	}
}
