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

// P1-M2 静态守卫：认证限流 / 审计判定必须纯内存、不碰 SQL；
// AuditLogger.Record 只能入队，不能把落库拖回认证临界路径；
// 注销必须先撤销会话、后写审计（撤销不可回滚，审计失败不回滚撤销）。
// 每条守卫都配负向注入，防止守卫自身失效造成假绿。

var authSecurityBannedImports = []string{
	"database/sql",
	"modernc.org/sqlite",
	"github.com/jeeinn/motd/internal/infrastructure",
}

var authSecurityFiles = []string{
	"ratelimit.go", "audit.go", "security.go", "logout.go",
}

func scanAuthSecurityImports(dir string, files []string) ([]string, error) {
	var out []string
	for _, name := range files {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, banned := range authSecurityBannedImports {
				if p == banned || strings.HasPrefix(p, banned+"/") {
					out = append(out, name+": "+p)
				}
			}
		}
	}
	return out, nil
}

func TestAuthSecurityLayersHaveNoPersistenceImports(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "auth")
	found, err := scanAuthSecurityImports(dir, authSecurityFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("限流/审计判定层不得出现持久化依赖:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestAuthSecurityGuardDetectsInjection(t *testing.T) {
	tmp := t.TempDir()
	src := `package auth

import (
	"database/sql"
	_ "github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

var _ = sql.ErrNoRows
`
	if err := os.WriteFile(filepath.Join(tmp, "ratelimit.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := scanAuthSecurityImports(tmp, []string{"ratelimit.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("注入的两条持久化 import 应全部检出，实际 %v", found)
	}
}

// collectRecordBodyCalls 收集 Record 函数体内出现的可疑标识符。
func collectRecordBodyCalls(path string) ([]string, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, false, err
	}
	banned := map[string]bool{
		"Flush": true, "Submit": true, "sink": true,
		"Exec": true, "ExecContext": true, "Query": true,
		"QueryContext": true, "Prepare": true, "BeginTx": true,
	}
	var out []string
	seen := false
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Record" || fd.Body == nil {
			continue
		}
		seen = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && banned[id.Name] {
				out = append(out, id.Name)
			}
			return true
		})
	}
	return out, seen, nil
}

func TestAuditRecordIsEnqueueOnly(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "auth", "audit.go")
	found, seen, err := collectRecordBodyCalls(path)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("未找到 Record 函数，守卫可能失效")
	}
	if len(found) > 0 {
		t.Fatalf("Record 只能入队，不得落库/调用 sink: %v", found)
	}
}

func TestAuditRecordGuardDetectsInjection(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "audit.go")
	src := `package auth

type logger struct{ sink AuditSink }

func (l *logger) Record(e AuditEvent) error {
	return l.sink.Flush(nil, []AuditEvent{e})
}
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, seen, err := collectRecordBodyCalls(path)
	if err != nil {
		t.Fatal(err)
	}
	if !seen || len(found) < 2 {
		t.Fatalf("把 Flush 塞进 Record 必须被检出: seen=%v found=%v", seen, found)
	}
}

// callPositions 返回指定方法名在文件中的调用位置，用于顺序守卫。
func callPositions(path, method string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == method {
			out = append(out, fset.Position(call.Pos()).Offset)
		}
		return true
	})
	return out, nil
}

func TestLogoutRevokesBeforeAudit(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "auth", "logout.go")
	revoke, err := callPositions(path, "RevokeUserSessions")
	if err != nil {
		t.Fatal(err)
	}
	audit, err := callPositions(path, "NoteLogout")
	if err != nil {
		t.Fatal(err)
	}
	if len(revoke) != 1 || len(audit) != 1 {
		t.Fatalf("应各有一处调用: revoke=%v audit=%v", revoke, audit)
	}
	if revoke[0] >= audit[0] {
		t.Fatal("注销必须先撤销会话再写审计（审计失败不得回滚撤销）")
	}
}

func TestRateLimitErrorCarriesRetryAfter(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "auth", "ratelimit.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, decl := range f.Decls {
		ts, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range ts.Specs {
			typ, ok := spec.(*ast.TypeSpec)
			if !ok || typ.Name.Name != "RateLimitError" {
				continue
			}
			st, ok := typ.Type.(*ast.StructType)
			if !ok {
				continue
			}
			seen = true
			found := false
			for _, field := range st.Fields.List {
				for _, nm := range field.Names {
					if nm.Name == "RetryAfter" {
						found = true
					}
				}
			}
			if !found {
				t.Fatal("RateLimitError 必须带 RetryAfter，供终端显示剩余秒数")
			}
		}
	}
	if !seen {
		t.Fatal("未找到 RateLimitError 定义")
	}
}
