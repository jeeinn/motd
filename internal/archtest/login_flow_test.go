package archtest

// P0-19 静态守卫：把 docs/DEVIATIONS.md R18~R20 与 ADR-025 的约束变成会失败的测试。
//
//  1. 呈现链路不得把 device_code / access_token / 响应原文写进终端；
//  2. LoginView / LoginDirective 不得有敏感字段；
//  3. 自动绑定必须复用 KeyBindingService.Bind，且指纹/公钥只来自当前连接；
//  4. Session 升级必须在 Bind 成功之后，且 session.go 里只有一处写 principal；
//  5. 交互层不得 sleep / 起 ticker / 引 HTTP（Q3 进程内调用）；
//  6. 偏差登记文档必须存在且含 R18 / R19 / R20 段落。
//
// 每条守卫都配一个负向注入：把违规代码塞进临时目录，守卫必须能检出。

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func parseLoginGoFile(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return fset, f
}

func loginExprText(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

func loginCallName(fset *token.FileSet, e ast.Expr) string {
	switch fn := e.(type) {
	case *ast.SelectorExpr:
		return loginExprText(fset, fn.X) + "." + fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	default:
		return loginExprText(fset, e)
	}
}

func loginNonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. 呈现链路不得写敏感字段
// ---------------------------------------------------------------------------

var loginSensitiveRe = regexp.MustCompile(`(?i)\.Token\b|\.DeviceCode\b|\bdeviceCode\b|\.Message\b`)

func isTerminalWriteCall(name string) bool {
	switch name {
	case "io.WriteString", "fmt.Fprintf", "fmt.Fprint", "fmt.Fprintln":
		return true
	}
	// loginPresenter 的语义方法同样直接写终端，必须一并纳管，
	// 否则「把 token 交给 f.view.line」会绕过守卫。
	for _, suffix := range []string{".Write", ".line", ".external", ".showStart", ".showSuccess"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// collectSensitiveWrites 收集「写终端调用参数里出现敏感字段」的位置文本。
func collectSensitiveWrites(t *testing.T, paths []string) []string {
	t.Helper()
	var hits []string
	for _, path := range paths {
		fset, f := parseLoginGoFile(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isTerminalWriteCall(loginCallName(fset, call.Fun)) {
				return true
			}
			for _, arg := range call.Args {
				txt := loginExprText(fset, arg)
				if loginSensitiveRe.MatchString(txt) {
					hits = append(hits, filepath.Base(path)+": "+txt)
				}
			}
			return true
		})
	}
	return hits
}

func TestLoginPresentationNeverWritesSecrets(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "cmd", "server")
	paths := []string{
		filepath.Join(dir, "login_flow.go"),
		filepath.Join(dir, "login_view.go"),
	}
	if hits := collectSensitiveWrites(t, paths); len(hits) > 0 {
		t.Fatalf("呈现链路不得写 device_code / token / 响应原文: %v", hits)
	}
}

func TestLoginPresentationNegativeInjectionIsDetected(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "login_flow.go")
	src := "package main\nimport \"io\"\nfunc f(out io.Writer, r struct{ Token string }) {\n\t_, _ = io.WriteString(out, r.Token)\n}\n"
	if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := collectSensitiveWrites(t, []string{bad}); len(hits) == 0 {
		t.Fatal("把 Token 塞进 io.WriteString 必须被检出")
	}
}

// ---------------------------------------------------------------------------
// 2. 呈现投影 / 指令结构不得有敏感字段
// ---------------------------------------------------------------------------

var loginStructSensitiveRe = regexp.MustCompile(`(?i)token|devicecode|access_?token`)

func collectSensitiveViewFields(t *testing.T, path string) []string {
	t.Helper()
	_, f := parseLoginGoFile(t, path)
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || (ts.Name.Name != "LoginView" && ts.Name.Name != "LoginDirective") {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			for _, nm := range field.Names {
				if loginStructSensitiveRe.MatchString(nm.Name) {
					hits = append(hits, ts.Name.Name+"."+nm.Name)
				}
			}
		}
		return true
	})
	return hits
}

func TestLoginViewStructsHaveNoSecretFields(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "auth", "login_flow.go")
	if hits := collectSensitiveViewFields(t, path); len(hits) > 0 {
		t.Fatalf("呈现结构不得含令牌字段: %v", hits)
	}
}

func TestLoginViewStructNegativeInjectionIsDetected(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "login_flow.go")
	src := "package auth\ntype LoginView struct {\n\tUserCode string\n\tAccessToken string\n}\n"
	if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := collectSensitiveViewFields(t, bad); len(hits) == 0 {
		t.Fatal("给 LoginView 加 AccessToken 字段必须被检出")
	}
}

// ---------------------------------------------------------------------------
// 3~4. 自动绑定复用 Bind、指纹来自 conn、升级只在 Bind 之后
// ---------------------------------------------------------------------------

type loginFinishCalls struct {
	bindEnd    []int
	upgradePos []int
	bindArgs   []string
}

func collectLoginFinishCalls(t *testing.T, path string) loginFinishCalls {
	t.Helper()
	fset, f := parseLoginGoFile(t, path)
	var out loginFinishCalls
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			sel, ok := node.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "BindRequest" {
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || (key.Name != "Fingerprint" && key.Name != "PublicKey") {
					continue
				}
				out.bindArgs = append(out.bindArgs, key.Name+"="+loginExprText(fset, kv.Value))
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Bind":
				out.bindEnd = append(out.bindEnd, int(node.End()))
			case "UpgradeToUser":
				out.upgradePos = append(out.upgradePos, int(node.Pos()))
			}
		}
		return true
	})
	return out
}

// loginFinishValid 是判定逻辑的纯函数，主测试与负向注入共用同一把尺子。
func loginFinishValid(c loginFinishCalls) error {
	if len(c.bindEnd) != 1 || len(c.upgradePos) != 1 {
		return fmt.Errorf("Bind / UpgradeToUser 应各恰好 1 次，实际 %d / %d", len(c.bindEnd), len(c.upgradePos))
	}
	if c.bindEnd[0] >= c.upgradePos[0] {
		return errors.New("会话升级必须在 Bind 成功之后（源码位置）")
	}
	joined := strings.Join(c.bindArgs, " ")
	for _, want := range []string{"Fingerprint=conn.Fingerprint()", "PublicKey=conn.PublicKey()"} {
		if !strings.Contains(joined, want) {
			return fmt.Errorf("BindRequest 字段必须只来自 conn，缺少 %q，实际 %v", want, c.bindArgs)
		}
	}
	return nil
}

func TestLoginFinishUsesBindThenUpgrade(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "cmd", "server", "login_flow.go")
	if err := loginFinishValid(collectLoginFinishCalls(t, path)); err != nil {
		t.Fatal(err)
	}
}

func TestLoginFinishNegativeInjectionIsDetected(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "login_flow.go")
	// 把升级挪到 Bind 之前，并把指纹换成命令参数：两处违规都应被判定拒绝。
	src := "package main\n" +
		"func finish(inv []string) {\n" +
		"\t_ = conn.UpgradeToUser(1)\n" +
		"\t_ = f.keys.Bind(ctx, auth.BindRequest{Fingerprint: inv[0], PublicKey: conn.PublicKey()})\n" +
		"}\n"
	if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loginFinishValid(collectLoginFinishCalls(t, bad)); err == nil {
		t.Fatal("升级提前 / 指纹来自命令参数的违规写法必须被检出")
	}
}

// ---------------------------------------------------------------------------
// 4b. session.go 里 principal 只有一处赋值
// ---------------------------------------------------------------------------

func countPrincipalAssignments(t *testing.T, path string) int {
	t.Helper()
	_, f := parseLoginGoFile(t, path)
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "principal" {
				n++
			}
		}
		return true
	})
	return n
}

func TestSessionPrincipalHasSingleWriteSite(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "transport", "ssh", "session.go")
	if n := countPrincipalAssignments(t, path); n != 1 {
		t.Fatalf("Session.principal 只应在 UpgradeToUser 里写一次，实际 %d 处", n)
	}
}

func TestSessionPrincipalNegativeInjectionIsDetected(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "session.go")
	src := "package ssh\nfunc f(s *Session) {\n\ts.principal = auth.Guest(\"x\")\n\ts.principal = auth.Guest(\"y\")\n}\n"
	if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := countPrincipalAssignments(t, bad); n < 2 {
		t.Fatal("新增第二处 principal 赋值必须被检出")
	}
}

// ---------------------------------------------------------------------------
// 5. 交互层不得 sleep / 起 ticker / 引 HTTP
// ---------------------------------------------------------------------------

var loginBlockingRe = regexp.MustCompile(`time\.Sleep|time\.NewTicker|time\.After\b`)

func collectLoginBlockingCalls(t *testing.T, paths []string) []string {
	t.Helper()
	var hits []string
	for _, path := range paths {
		fset, f := parseLoginGoFile(t, path)
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "net/http") {
				hits = append(hits, filepath.Base(path)+": import net/http")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := loginCallName(fset, call.Fun)
			if loginBlockingRe.MatchString(name) {
				hits = append(hits, filepath.Base(path)+": "+name)
			}
			return true
		})
	}
	return hits
}

func TestLoginInteractionLayerHasNoOwnClockOrHTTP(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "cmd", "server")
	paths := []string{filepath.Join(dir, "login_flow.go"), filepath.Join(dir, "login_view.go")}
	if hits := collectLoginBlockingCalls(t, paths); len(hits) > 0 {
		t.Fatalf("交互层不得自持节奏 / 开 HTTP 端点（Q3）: %v", hits)
	}
}

func TestLoginInteractionNegativeInjectionIsDetected(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "login_flow.go")
	src := "package main\nimport \"time\"\nfunc f() { time.Sleep(time.Second) }\n"
	if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := collectLoginBlockingCalls(t, []string{bad}); len(hits) == 0 {
		t.Fatal("交互层塞 time.Sleep 必须被检出")
	}
}

// ---------------------------------------------------------------------------
// 6. 偏差登记文档的完整性（用户明确要求「方便事后核对」）
// ---------------------------------------------------------------------------

func TestDeviationsDocumentRecordsP019(t *testing.T) {
	root := findModuleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "DEVIATIONS.md"))
	if err != nil {
		t.Fatalf("偏差登记文档必须存在: %v", err)
	}
	text := string(b)
	for _, want := range []string{"R18", "R19", "R20", "P0-24 替换点", "一致性核对表", "波次冲突"} {
		if !strings.Contains(text, want) {
			t.Errorf("偏差文档缺少关键段落 %q", want)
		}
	}
}
