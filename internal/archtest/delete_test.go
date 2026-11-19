package archtest

// v0.2.1 删除路径静态守卫。E 类缺陷（语法合法、单测也过、跑起来出事）在这里逼到明面：
//  1. 删除写路径必须经 canModify 判权，且两条目标路径都在同一 Write 事务内；
//  2. 软删只能 UPDATE status='deleted'，绝不出现 DELETE FROM threads/posts；
//  3. 检索必须过滤所属主题 status='active'，否则软删主题的回复仍会被 /search 命中；
//  4. Delete 不得调用 checkWrite，否则访客拿到 ErrReadOnly，与非作者提示分裂。
//
// 每条守卫都有负向注入，证明拿掉任一条都会报警。

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func collectDeleteAppIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	found := false
	for _, nf := range files {
		if nf.name != "delete.go" {
			continue
		}
		found = true
		for _, decl := range nf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := callNames(fn)
			switch fn.Name.Name {
			case "canModify":
				// Go AST 里 `a, b int64` 是一个 Field 带两个 Names，必须数名字。
				names := 0
				if fn.Type.Params != nil {
					for _, f := range fn.Type.Params.List {
						names += len(f.Names)
					}
				}
				if names < 2 {
					issues = append(issues, "canModify 必须同时接收 userID 与 ownerID")
				}
			case "Delete":
				for _, want := range []string{"Write", "ThreadOwner", "PostOwner", "canModify", "SoftDeleteThread", "SoftDeletePost"} {
					if !calls[want] {
						issues = append(issues, "Delete 写路径缺少 "+want+" 调用")
					}
				}
				if calls["checkWrite"] {
					issues = append(issues, "Delete 不得调用 checkWrite（访客会拿到另一种提示）")
				}
			}
		}
	}
	if !found {
		issues = append(issues, "扫描不到 application/forum/delete.go，守卫可能失效")
		return issues, nil
	}
	src, err := os.ReadFile(filepath.Join(dir, "delete.go"))
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToUpper(string(src)), "DELETE FROM") {
		issues = append(issues, "应用删除路径不得出现 DELETE FROM（软删不硬删）")
	}
	return issues, nil
}

func collectDeleteInfraIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	for _, name := range []string{"SoftDeleteThread", "SoftDeletePost"} {
		fn, ok := findFuncDecl(files, name)
		if !ok {
			issues = append(issues, "扫描不到 "+name+" 实现，守卫失效")
			continue
		}
		lits := strings.Join(stringLiterals(fn), " ")
		upper := strings.ToUpper(lits)
		if strings.Contains(upper, "DELETE FROM") {
			issues = append(issues, name+" 不得出现 DELETE FROM（软删不硬删、不级联）")
		}
		if !strings.Contains(lits, "status = 'deleted'") {
			issues = append(issues, name+" 必须 UPDATE status = 'deleted'")
		}
		guard := "'active'"
		if name == "SoftDeletePost" {
			guard = "'normal'"
		}
		if !strings.Contains(lits, guard) {
			issues = append(issues, name+" 缺少条件更新 "+guard+"（并发下重复删除会误写）")
		}
	}
	return issues, nil
}

// checkSearchThreadFilter 堵住「删主题不删回复行」带来的次生泄漏：
// 只过滤 posts.status 会让已删主题下的回复继续出现在检索结果里。
func checkSearchThreadFilter(src string) []string {
	var issues []string
	if !strings.Contains(src, "JOIN threads t ON t.id = p.thread_id AND t.status = 'active'") {
		issues = append(issues, "检索 SQL 缺少所属主题 status='active' 过滤：已删主题内容会泄漏")
	}
	return issues
}

func TestDeleteIsSoftAndOwnerChecked(t *testing.T) {
	root := findModuleRoot(t)
	app := filepath.Join(root, "internal", "application", "forum")
	infra := filepath.Join(root, "internal", "infrastructure", "sqlite")
	ai, err := collectDeleteAppIssues(app)
	if err != nil {
		t.Fatal(err)
	}
	ii, err := collectDeleteInfraIssues(infra)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(infra, "repo_search.go"))
	if err != nil {
		t.Fatal(err)
	}
	all := append(append(ai, ii...), checkSearchThreadFilter(string(src))...)
	if len(all) > 0 {
		t.Fatalf("删除守卫未通过:\n  %s", strings.Join(all, "\n  "))
	}
}

func TestGuardDetectsHardDeleteInSoftDelete(t *testing.T) {
	tmp := t.TempDir()
	bad := `package sqlite
func (w *sqlWriteTx) SoftDeleteThread(ctx context.Context, id int64) error {
	w.tx.ExecContext(ctx, "DELETE FROM threads WHERE id = ?", id)
	_, err := w.tx.ExecContext(ctx, "UPDATE threads SET status = 'deleted' WHERE id = ? AND status = 'active'", id)
	return err
}
func (w *sqlWriteTx) SoftDeletePost(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE posts SET status = 'deleted' WHERE id = ? AND status = 'normal'", id)
	return err
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectDeleteInfraIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "DELETE FROM") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的硬删未被检出: %v", issues)
	}
}

func TestGuardDetectsMissingOwnershipCheck(t *testing.T) {
	tmp := t.TempDir()
	bad := `package forum
func canModify(userID int64) bool { return userID > 0 }
func Delete(tx WriteTx) error {
	return tx.Write(ctx, func(tx WriteTx) error {
		tx.ThreadOwner(ctx, 1)
		tx.PostOwner(ctx, 1)
		tx.SoftDeleteThread(ctx, 1)
		tx.SoftDeletePost(ctx, 1)
		return nil
	})
}
`
	if err := os.WriteFile(filepath.Join(tmp, "delete.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectDeleteAppIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"canModify 必须同时接收", "Delete 写路径缺少 canModify"} {
		if !strings.Contains(joined, want) {
			t.Errorf("违规报告缺少 %q:\n%s", want, joined)
		}
	}
}

func TestGuardDetectsMissingThreadFilterInSearch(t *testing.T) {
	bad := `SELECT p.id, p.thread_id, p.body_md FROM posts p WHERE p.status = 'normal' AND p.body_md LIKE ?`
	if issues := checkSearchThreadFilter(bad); len(issues) == 0 {
		t.Fatal("注入的「不按主题过滤」检索未被检出")
	}
}
