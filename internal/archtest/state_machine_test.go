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

// P0-16 静态守卫：把三条容易被后续改动破坏的约束变成会失败的测试。
//
//  1. 状态机不得自己 sleep / 起 ticker：轮询节奏由 P0-19 会话循环持有，
//     状态机只做纯决策（否则无法在无真实等待的情况下回归，也会拖住 SSH 会话）。
//  2. DeviceAuthStatus 四态枚举必须与 0001_init.sql 的 CHECK 约束逐字一致，
//     防止领域层与 DDL 悄悄漂移（一边加了状态另一边没加，写入直接被 DB 拒绝）。
//  3. device_auth_states 表不得出现任何 token 列：access_token 只在进程内
//     流转给 P0-17，绝不落库（Q3 / §4.4 / ADR-021 边界 3）。

// ---------------------------------------------------------------------------
// 1. 状态机不阻塞时间
// ---------------------------------------------------------------------------

var blockingTimeSelectors = map[string]bool{
	"Sleep": true, "NewTicker": true, "NewTimer": true,
	"Tick": true, "After": true, "AfterFunc": true,
}

// collectBlockingTimeCalls 扫描目录下非测试源码，找出 time.Sleep/NewTicker/... 调用。
func collectBlockingTimeCalls(dir string) ([]string, error) {
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
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "time" || !blockingTimeSelectors[sel.Sel.Name] {
				return true
			}
			out = append(out, name+": time."+sel.Sel.Name)
			return true
		})
	}
	return out, nil
}

func TestStateMachineDoesNotBlockOnTime(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "auth")
	found, err := collectBlockingTimeCalls(dir)
	if err != nil {
		t.Fatalf("扫描 application/auth 失败: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("状态机不得持有轮询节奏（不 sleep / 不起 ticker），实际:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestGuardDetectsBlockingTimeCall(t *testing.T) {
	tmp := t.TempDir()
	src := "package auth\n\nimport \"time\"\n\nfunc spin() {\n\ttime.Sleep(time.Second)\n\t_ = time.NewTicker(time.Second)\n}\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectBlockingTimeCalls(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("注入的两处阻塞式时间调用都应被检出，实际 %v", found)
	}
}

// ---------------------------------------------------------------------------
// 2. 四态枚举 vs DDL CHECK
// ---------------------------------------------------------------------------

var (
	tableDDLRe = regexp.MustCompile(`(?s)CREATE TABLE device_auth_states \((.*?)\);`)
	statusInRe = regexp.MustCompile(`(?s)CHECK\s*\(\s*status\s+IN\s*\(([^)]*)\)`)
	quotedRe   = regexp.MustCompile(`'([a-z_]+)'`)
	goStatusRe = regexp.MustCompile(`Status[A-Za-z]+\s+DeviceAuthStatus\s*=\s*"([a-z_]+)"`)
)

// deviceAuthStatusesFromDDL 只取 device_auth_states 表段落里的 status CHECK，
// 避免误命中 users.status 的 CHECK（两者都叫 status，值域完全不同）。
func deviceAuthStatusesFromDDL(sqlText string) []string {
	m := tableDDLRe.FindStringSubmatch(sqlText)
	if len(m) < 2 {
		return nil
	}
	in := statusInRe.FindStringSubmatch(m[1])
	if len(in) < 2 {
		return nil
	}
	var out []string
	for _, q := range quotedRe.FindAllStringSubmatch(in[1], -1) {
		out = append(out, q[1])
	}
	return out
}

func deviceAuthStatusesFromGo(src string) []string {
	var out []string
	for _, m := range goStatusRe.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestDeviceAuthStatusMatchesDDL(t *testing.T) {
	root := findModuleRoot(t)
	ddl, err := os.ReadFile(filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, "internal", "application", "auth", "state.go"))
	if err != nil {
		t.Fatal(err)
	}
	fromDDL := deviceAuthStatusesFromDDL(string(ddl))
	fromGo := deviceAuthStatusesFromGo(string(src))

	if len(fromDDL) != 4 {
		t.Fatalf("DDL 应恰有 4 个状态，实际 %v", fromDDL)
	}
	if len(fromGo) != 4 {
		t.Fatalf("Go 枚举应恰有 4 个状态，实际 %v", fromGo)
	}
	if strings.Join(fromDDL, ",") != strings.Join(fromGo, ",") {
		t.Fatalf("领域枚举与 DDL CHECK 不一致：DDL=%v Go=%v", fromDDL, fromGo)
	}
}

func TestGuardDetectsStatusDrift(t *testing.T) {
	const goodDDL = `
CREATE TABLE users (
  status TEXT NOT NULL CHECK (status IN ('active','pending','deleting','deleted'))
);
CREATE TABLE device_auth_states (
  status TEXT NOT NULL CHECK (status IN ('pending','authorized','denied','expired'))
);
`
	if got := deviceAuthStatusesFromDDL(goodDDL); strings.Join(got, ",") != "pending,authorized,denied,expired" {
		t.Fatalf("守卫解析 DDL 出错，实际 %v", got)
	}
	// 把 DDL 里的 expired 改成别的值 → 必须与 Go 枚举不一致。
	drifted := strings.Replace(goodDDL, "'denied','expired'", "'denied','expired_x'", 1)
	const goSrc = `
const (
	StatusPending    DeviceAuthStatus = "pending"
	StatusAuthorized DeviceAuthStatus = "authorized"
	StatusDenied     DeviceAuthStatus = "denied"
	StatusExpired    DeviceAuthStatus = "expired"
)
`
	if strings.Join(deviceAuthStatusesFromDDL(drifted), ",") == strings.Join(deviceAuthStatusesFromGo(goSrc), ",") {
		t.Fatal("DDL 与枚举漂移时守卫应能检出差异")
	}
	// 表段落缺失（例如有人删表）也不能假绿。
	if got := deviceAuthStatusesFromDDL("CREATE TABLE users (id INTEGER);"); len(got) != 0 {
		t.Fatalf("缺少 device_auth_states 表时应返回空，实际 %v", got)
	}
}

// ---------------------------------------------------------------------------
// 3. 状态表不得含 token 列
// ---------------------------------------------------------------------------

var tokenColumnRe = regexp.MustCompile(`(?i)\b(access_token|refresh_token|token_hash|token)\s+(TEXT|VARCHAR|BLOB|INTEGER)`)

func collectTokenColumns(sqlText string) []string {
	m := tableDDLRe.FindStringSubmatch(sqlText)
	if len(m) < 2 {
		return nil
	}
	var out []string
	for _, hit := range tokenColumnRe.FindAllString(m[1], -1) {
		out = append(out, strings.TrimSpace(hit))
	}
	return out
}

func TestDeviceAuthStatesHasNoTokenColumn(t *testing.T) {
	root := findModuleRoot(t)
	ddl, err := os.ReadFile(filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if found := collectTokenColumns(string(ddl)); len(found) > 0 {
		t.Fatalf("device_auth_states 不得持久化任何令牌，发现列: %v", found)
	}
}

func TestGuardDetectsTokenColumn(t *testing.T) {
	const bad = `CREATE TABLE device_auth_states (
  id INTEGER PRIMARY KEY,
  access_token TEXT,
  status TEXT NOT NULL CHECK (status IN ('pending','authorized','denied','expired'))
);`
	if got := collectTokenColumns(bad); len(got) == 0 {
		t.Fatal("注入的 access_token 列应被检出")
	}
}
