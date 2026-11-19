package archtest

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R8 静态守卫：回复的父帖必须在同一主题。
//
// 两道防线都要在源码里看得见：
//  1. 应用层 InsertPost 必须在写事务内先调用 validateParentThread（先查父帖
//     thread_id，再落库），不能只靠数据库触发器给出可读性差的错误；
//  2. 0005 迁移必须含 BEFORE INSERT / BEFORE UPDATE 两条触发器与跨主题 RAISE(ABORT)
//     （SQLite 的 CREATE TRIGGER 不支持 INSERT, UPDATE 连写，故拆两条）。
//
// 负向注入测试证明：任一把守卫拿掉，扫描都会报错（守卫不是摆设）。

// collectParentThreadIssues 扫描 dir 下的 Go 源码，返回缺失校验的描述。
func collectParentThreadIssues(dir string) ([]string, error) {
	files, _, err := parseGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	insertCallsCheck := false
	helperHasQuery := false
	helperHasCompare := false

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			switch fn.Name.Name {
			case "InsertPost":
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var name string
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					case *ast.Ident:
						name = fun.Name
					}
					if name == "validateParentThread" {
						insertCallsCheck = true
					}
					return true
				})
			case "validateParentThread":
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.BasicLit:
						if node.Kind == token.STRING &&
							strings.Contains(node.Value, "thread_id") &&
							strings.Contains(node.Value, "posts") {
							helperHasQuery = true
						}
					case *ast.BinaryExpr:
						if node.Op == token.NEQ {
							helperHasCompare = true
						}
					}
					return true
				})
			}
		}
	}

	if !insertCallsCheck {
		issues = append(issues, "InsertPost 未在同事务内调用 validateParentThread（缺少先查父帖 thread_id 的校验）")
	}
	if !helperHasQuery {
		issues = append(issues, "validateParentThread 缺少读取父帖 thread_id 的查询")
	}
	if !helperHasCompare {
		issues = append(issues, "validateParentThread 缺少父帖 thread_id 与目标 thread_id 的不一致比较")
	}
	return issues, nil
}

// checkParentTriggerSQL 检查迁移 SQL 是否含触发器四要素；抽成函数供负向注入复用。
func checkParentTriggerSQL(sql string) []string {
	var issues []string
	for _, want := range []string{
		"BEFORE INSERT ON posts",
		"BEFORE UPDATE ON posts",
		"NEW.parent_post_id IS NOT NULL",
		"thread_id = NEW.thread_id",
		"RAISE(ABORT, 'parent post must be in same thread')",
	} {
		if !strings.Contains(sql, want) {
			issues = append(issues, "0005 迁移缺少: "+want)
		}
	}
	return issues
}

func TestInsertPostValidatesParentThreadInSameTx(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "infrastructure", "sqlite")
	issues, err := collectParentThreadIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("同 thread 校验守卫未通过:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestParentTriggerMigrationIsComplete(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0005_post_parent_thread.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if issues := checkParentTriggerSQL(string(b)); len(issues) != 0 {
		t.Fatalf("0005 触发器不完整:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestGuardDetectsMissingParentThreadValidation(t *testing.T) {
	// 注入一个「InsertPost 直接插库、不查父帖」的版本，守卫必须报错。
	tmp := t.TempDir()
	bad := `package sqlite

type fakeTx struct{}

func (w *fakeTx) InsertPost() {
	w.tx.Exec("INSERT INTO posts (thread_id, parent_post_id) VALUES (?, ?)")
}

func (w *fakeTx) validateParentThread() {
	w.tx.Query("SELECT thread_id FROM posts WHERE id = ?")
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectParentThreadIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "InsertPost 未在同事务内调用 validateParentThread") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的「缺少同 thread 校验」未被检出: %v", issues)
	}
}

func TestGuardDetectsMissingParentThreadComparison(t *testing.T) {
	tmp := t.TempDir()
	bad := `package sqlite

type fakeTx struct{}

func (w *fakeTx) InsertPost() {
	w.validateParentThread()
}

func (w *fakeTx) validateParentThread() {
	w.tx.Query("SELECT thread_id FROM posts WHERE id = ?")
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectParentThreadIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "不一致比较") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的「父 thread_id 未做不一致比较」未被检出: %v", issues)
	}
}

func TestGuardDetectsMissingTriggerRAISE(t *testing.T) {
	bad := `CREATE TRIGGER posts_parent_same_thread_insert BEFORE INSERT ON posts
WHEN NEW.parent_post_id IS NOT NULL
BEGIN
  SELECT 1;
END;

CREATE TRIGGER posts_parent_same_thread_update BEFORE UPDATE ON posts
WHEN NEW.parent_post_id IS NOT NULL
BEGIN
  SELECT 1;
END;
`
	if issues := checkParentTriggerSQL(bad); len(issues) == 0 {
		t.Fatal("注入的「触发器缺少跨主题校验」未被检出")
	}
}
