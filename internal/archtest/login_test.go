package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// P0-17 静态守卫：把 §4.3 / §7 R7 / R16 的六条约束变成会失败的测试。
//
//  1. users / identities 不得出现 token 列（令牌只在进程内流转）。
//  2. users.primary_email 不得有 UNIQUE 约束 —— 这是「禁止按邮箱自动合并」的
//     结构保证：R7 明确无唯一约束是**有意设计**，允许同邮箱多账号。
//  3. identities 必须有 UNIQUE (provider, provider_uid) —— 并发同 UID 首登幂等。
//  4. 登录相关结构体不得有 token 字段（令牌不进任何持久化 / 返回值结构）。
//  5. FindAccountByEmail 的返回值必须被丢弃 —— 邮箱命中只能生成 Notice，
//     绝不能据此复用旧账号（账号接管防线，§4.3 步骤 2 / §8 风险表）。
//  6. provider_uid 必须由 GitHub 数字 id 经 strconv.FormatInt 生成，
//     不能用 login / email（两者都可被用户改写）。

var tableBodyRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+([a-z_]+)\s*\((.*?)\)\s*;`)

// extractTableBody 返回指定表的定义体；表不存在时返回空串。
func extractTableBody(sqlText, table string) string {
	for _, m := range tableBodyRe.FindAllStringSubmatch(sqlText, -1) {
		if strings.EqualFold(m[1], table) {
			return m[2]
		}
	}
	return ""
}

func migrationSQL(t *testing.T) string {
	t.Helper()
	root := findModuleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatalf("读取 0001_init.sql: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 1. users / identities 不得有 token 列
// ---------------------------------------------------------------------------

var loginTokenColumnRe = regexp.MustCompile(`(?i)\b(access_token|refresh_token|token_hash|token)\s+(TEXT|VARCHAR|BLOB|INTEGER)`)

func collectLoginTokenColumns(sqlText string) []string {
	var out []string
	for _, table := range []string{"users", "identities"} {
		body := extractTableBody(sqlText, table)
		for _, hit := range loginTokenColumnRe.FindAllString(body, -1) {
			out = append(out, table+": "+strings.TrimSpace(hit))
		}
	}
	return out
}

func TestLoginTablesHaveNoTokenColumn(t *testing.T) {
	if found := collectLoginTokenColumns(migrationSQL(t)); len(found) > 0 {
		t.Fatalf("users / identities 不得持久化任何令牌，发现: %v", found)
	}
}

func TestGuardDetectsLoginTokenColumn(t *testing.T) {
	const bad = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  access_token TEXT,
  primary_email TEXT
);
CREATE TABLE identities (
  id INTEGER PRIMARY KEY,
  token_hash TEXT
);`
	found := collectLoginTokenColumns(bad)
	if len(found) != 2 {
		t.Fatalf("两张表各注入一个 token 列，应检出 2 条，实际 %v", found)
	}
}

// ---------------------------------------------------------------------------
// 2. users.primary_email 不得 UNIQUE（R7：允许同邮箱多账号）
// ---------------------------------------------------------------------------

var primaryEmailColRe = regexp.MustCompile(`(?im)^\s*primary_email\b[^\n,]*`)

func primaryEmailColumnDef(sqlText string) string {
	body := extractTableBody(sqlText, "users")
	m := primaryEmailColRe.FindString(body)
	return strings.TrimSpace(m)
}

func TestPrimaryEmailHasNoUniqueConstraint(t *testing.T) {
	def := primaryEmailColumnDef(migrationSQL(t))
	if def == "" {
		t.Fatal("users.primary_email 列不存在，守卫失效")
	}
	if strings.Contains(strings.ToLower(def), "unique") {
		t.Fatalf("§7 R7：primary_email 必须无唯一约束（允许同邮箱多账号、禁止按邮箱自动合并），实际定义: %q", def)
	}
}

func TestGuardDetectsUniquePrimaryEmail(t *testing.T) {
	const bad = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  primary_email TEXT UNIQUE,
  username TEXT NOT NULL
);`
	if def := primaryEmailColumnDef(bad); !strings.Contains(strings.ToLower(def), "unique") {
		t.Fatalf("注入的 UNIQUE 应被解析出来，实际 %q", def)
	}
	// 反向：正常 DDL 不应被误判。
	const good = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  primary_email TEXT,
  username TEXT NOT NULL COLLATE NOCASE UNIQUE
);`
	if def := primaryEmailColumnDef(good); strings.Contains(strings.ToLower(def), "unique") {
		t.Fatalf("username 的 UNIQUE 不应被误算到 primary_email，实际 %q", def)
	}
}

// ---------------------------------------------------------------------------
// 3. identities 必须有 UNIQUE(provider, provider_uid)
// ---------------------------------------------------------------------------

var identityUniqueRe = regexp.MustCompile(`(?is)UNIQUE\s*\(\s*provider\s*,\s*provider_uid\s*\)`)

func hasIdentityUnique(sqlText string) bool {
	return identityUniqueRe.MatchString(extractTableBody(sqlText, "identities"))
}

func TestIdentitiesHasProviderUniqueConstraint(t *testing.T) {
	if !hasIdentityUnique(migrationSQL(t)) {
		t.Fatal("identities 必须有 UNIQUE (provider, provider_uid)：并发同 UID 首登只产生一个账号的结构保证")
	}
}

func TestGuardDetectsMissingIdentityUnique(t *testing.T) {
	const bad = `
CREATE TABLE identities (
  id INTEGER PRIMARY KEY,
  provider TEXT NOT NULL,
  provider_uid TEXT NOT NULL
);`
	if hasIdentityUnique(bad) {
		t.Fatal("缺少 UNIQUE 时守卫不应通过")
	}
	const good = `
CREATE TABLE identities (
  id INTEGER PRIMARY KEY,
  provider TEXT NOT NULL,
  provider_uid TEXT NOT NULL,
  UNIQUE (provider, provider_uid)
);`
	if !hasIdentityUnique(good) {
		t.Fatal("含 UNIQUE 时应通过")
	}
}

// ---------------------------------------------------------------------------
// 4. 登录结构体不得有 token 字段
// ---------------------------------------------------------------------------

var loginStructNames = map[string]bool{
	"LoginResult":     true,
	"IdentityProfile": true,
	"NewAccount":      true,
	"NewIdentity":     true,
	"AccountView":     true,
}

// collectTokenFields 扫目录下非测试源码，找指定结构体里名字含 token 的字段。
func collectTokenFields(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !loginStructNames[ts.Name.Name] {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				for _, id := range field.Names {
					if strings.Contains(strings.ToLower(id.Name), "token") {
						out = append(out, ts.Name.Name+"."+id.Name)
					}
				}
			}
			return true
		})
	}
	return out, nil
}

func TestLoginStructsHaveNoTokenField(t *testing.T) {
	root := findModuleRoot(t)
	found, err := collectTokenFields(filepath.Join(root, "internal", "application", "auth"))
	if err != nil {
		t.Fatalf("扫描 application/auth: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("登录结构体不得携带令牌字段（token 只在 Login 参数里进程内流转）: %v", found)
	}
}

func TestGuardDetectsTokenField(t *testing.T) {
	tmp := t.TempDir()
	src := `package auth

type LoginResult struct {
	UserID      int64
	AccessToken string
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectTokenFields(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != "LoginResult.AccessToken" {
		t.Fatalf("注入的令牌字段应被检出，实际 %v", found)
	}
}

// ---------------------------------------------------------------------------
// 5. FindAccountByEmail 的返回值必须被丢弃（禁止按邮箱复用账号）
// ---------------------------------------------------------------------------

// collectEmailLookupAssignments 找出 login_service.go 里所有 FindAccountByEmail
// 赋值语句的左侧第一个表达式是否为 `_`。返回违规描述。
func collectEmailLookupAssignments(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) == 0 {
				return true
			}
			if !containsEmailLookupCall(assign.Rhs) {
				return true
			}
			id, ok := assign.Lhs[0].(*ast.Ident)
			if !ok || id.Name != "_" {
				out = append(out, name+": FindAccountByEmail 的返回值未被丢弃")
			}
			return true
		})
	}
	return out, nil
}

func containsEmailLookupCall(exprs []ast.Expr) bool {
	for _, e := range exprs {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "FindAccountByEmail" {
			return true
		}
	}
	return false
}

func TestEmailLookupResultIsDiscarded(t *testing.T) {
	root := findModuleRoot(t)
	found, err := collectEmailLookupAssignments(filepath.Join(root, "internal", "application", "auth"))
	if err != nil {
		t.Fatalf("扫描 application/auth: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("邮箱命中的账号只能用于生成 Notice，绝不能复用（§4.3 步骤 2 / §8 账号接管）:\n  %s",
			strings.Join(found, "\n  "))
	}
}

func TestGuardDetectsEmailAccountReuse(t *testing.T) {
	tmp := t.TempDir()
	bad := `package auth

func bad(tx LoginWriteTx, ctx interface{ Done() <-chan struct{} }) {
	hit, err := tx.FindAccountByEmail(nil, "a@b.com")
	_ = hit
	_ = err
}
`
	good := `package auth

func good(tx LoginWriteTx) {
	_, err := tx.FindAccountByEmail(nil, "a@b.com")
	_ = err
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectEmailLookupAssignments(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("保留返回值应被检出，实际 %v", found)
	}

	if err := os.Remove(filepath.Join(tmp, "bad.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "good.go"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err = collectEmailLookupAssignments(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("丢弃返回值时不应报警，实际 %v", found)
	}
}

// ---------------------------------------------------------------------------
// 6. provider_uid 由数字 id 生成
// ---------------------------------------------------------------------------

// hasNumericIDFormatting 检查 login.go 里存在 strconv.FormatInt(<x>.ID, 10)。
func hasNumericIDFormatting(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			return false, err
		}
		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "FormatInt" {
				return true
			}
			// 第二个参数必须是字面量 10（十进制）。
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Value != "10" {
				return true
			}
			// 第一个参数必须是 <something>.ID。
			inner, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "ID" {
				return true
			}
			found = true
			return false
		})
		if found {
			return true, nil
		}
	}
	return false, nil
}

func TestProviderUIDUsesNumericID(t *testing.T) {
	root := findModuleRoot(t)
	ok, err := hasNumericIDFormatting(filepath.Join(root, "internal", "application", "auth"))
	if err != nil {
		t.Fatalf("扫描 application/auth: %v", err)
	}
	if !ok {
		t.Fatal("provider_uid 必须由 GitHub 数字 id 经 strconv.FormatInt(ID, 10) 生成，不能用 login / email")
	}
}

func TestGuardDetectsNonNumericProviderUID(t *testing.T) {
	tmp := t.TempDir()
	bad := `package auth

import "strconv"

func uid(login string) string {
	return strconv.Itoa(len(login))
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := hasNumericIDFormatting(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("非 FormatInt(ID, 10) 的写法应被检出为不合格")
	}

	good := `package auth

import "strconv"

type gh struct{ ID int64 }

func uid(g gh) string {
	return strconv.FormatInt(g.ID, 10)
}
`
	if err := os.WriteFile(filepath.Join(tmp, "good.go"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = hasNumericIDFormatting(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("FormatInt(ID, 10) 应被接受")
	}
}
