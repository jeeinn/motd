package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P1-M3 投票静态守卫。E 类缺陷（语法合法、单测也过、跑起来错）在这里逼到明面：
//  1. 投票写路径绝不能调 TouchThread（会推进 last_reply_at，SortLatest 错乱）；
//  2. 投票写路径必须同时含 GetVote / UpsertVote / DeleteVote / AdjustScore / AppendOutbox；
//  3. AdjustScore 的 SQL 只改 score，绝不出现 last_reply_at；
//  4. UpsertVote 必须走 ON CONFLICT，靠唯一键保证幂等。
//
// 每条守卫都有负向注入测试，证明拿掉任一条都会报警。

type namedGoFile struct {
	name string
	file *ast.File
	fset *token.FileSet
}

func parseNamedGoFilesIn(dir string) ([]namedGoFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []namedGoFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, namedGoFile{name: e.Name(), file: f, fset: fset})
	}
	return out, nil
}

func findFuncDecl(files []namedGoFile, name string) (*ast.FuncDecl, bool) {
	for _, nf := range files {
		for _, decl := range nf.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
				return fn, true
			}
		}
	}
	return nil, false
}

func callNames(fn *ast.FuncDecl) map[string]bool {
	out := make(map[string]bool)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			out[f.Sel.Name] = true
		case *ast.Ident:
			out[f.Name] = true
		}
		return true
	})
	return out
}

func stringLiterals(fn *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			out = append(out, strings.Trim(lit.Value, "`\""))
		}
		return true
	})
	return out
}

// collectVoteAppIssues 扫描 application/forum：投票写路径的结构纪律。
func collectVoteAppIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	foundVoteFile := false
	for _, nf := range files {
		if nf.name != "vote.go" {
			continue
		}
		foundVoteFile = true
		for _, decl := range nf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if callNames(fn)["TouchThread"] {
				issues = append(issues, "vote.go 不得调用 TouchThread（投票不能推进 last_reply_at）")
			}
		}
	}
	if !foundVoteFile {
		issues = append(issues, "扫描不到 application/forum/vote.go，守卫可能失效")
	}
	fn, ok := findFuncDecl(files, "applyVote")
	if !ok {
		return append(issues, "扫描不到 applyVote，投票写路径守卫失效"), nil
	}
	calls := callNames(fn)
	for _, want := range []string{"Write", "GetVote", "UpsertVote", "DeleteVote", "AdjustScore", "AppendOutbox"} {
		if !calls[want] {
			issues = append(issues, "applyVote 写路径缺少 "+want+" 调用")
		}
	}
	return issues, nil
}

// collectVoteInfraIssues 扫描 infrastructure/sqlite：SQL 层面的不变量。
func collectVoteInfraIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	adjust, ok := findFuncDecl(files, "AdjustScore")
	if !ok {
		return append(issues, "扫描不到 AdjustScore 实现，守卫失效"), nil
	}
	lits := stringLiterals(adjust)
	hasScoreUpdate := false
	for _, s := range lits {
		if strings.Contains(s, "last_reply_at") {
			issues = append(issues, "AdjustScore 的 SQL 不得出现 last_reply_at")
		}
		if strings.Contains(s, "score = score +") {
			hasScoreUpdate = true
		}
	}
	if !hasScoreUpdate {
		issues = append(issues, "AdjustScore 缺少条件加法 UPDATE（score = score + ?）")
	}
	upsert, ok := findFuncDecl(files, "UpsertVote")
	if !ok {
		return append(issues, "扫描不到 UpsertVote 实现，守卫失效"), nil
	}
	hasConflict := false
	for _, s := range stringLiterals(upsert) {
		if strings.Contains(s, "ON CONFLICT") {
			hasConflict = true
		}
	}
	if !hasConflict {
		issues = append(issues, "UpsertVote 缺少 ON CONFLICT，无法在唯一键上幂等")
	}
	return issues, nil
}

func TestVoteWritePathDoesNotTouchThreadActivity(t *testing.T) {
	root := findModuleRoot(t)
	app := filepath.Join(root, "internal", "application", "forum")
	infra := filepath.Join(root, "internal", "infrastructure", "sqlite")
	ai, err := collectVoteAppIssues(app)
	if err != nil {
		t.Fatal(err)
	}
	ii, err := collectVoteInfraIssues(infra)
	if err != nil {
		t.Fatal(err)
	}
	if all := append(ai, ii...); len(all) > 0 {
		t.Fatalf("投票守卫未通过:\n  %s", strings.Join(all, "\n  "))
	}
}
