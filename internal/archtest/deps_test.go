package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 依赖方向（评审稿 v0.2 §2.2 / ADR-001）：
//
//	transport      → application → domain
//	presentation   → application
//	infrastructure → application/domain（实现 application 定义的接口）
//	plugin         → application（受限接口）
//	domain         → 不依赖任何上层
//
// 下表是「禁止表」：key 是发起方分组，value 是它不允许出现的目标分组。
// 同组之间允许互相引用（例如 domain/user → domain/post）。
var forbidden = map[string][]string{
	"domain":         {"application", "transport", "presentation", "infrastructure", "plugin", "cmd"},
	"application":    {"transport", "presentation", "infrastructure", "plugin", "cmd"},
	"infrastructure": {"transport", "presentation", "plugin", "cmd"},
	"transport":      {"infrastructure", "presentation", "plugin", "cmd"},
	"presentation":   {"transport", "infrastructure", "plugin", "cmd"},
	"plugin":         {"transport", "presentation", "infrastructure", "cmd"},
}

// expectedGroups 是 §2.2 目录结构里必须存在的分组。
// 用于防止「扫描逻辑漏掉了某个目录 → 测试假通过」这类守卫自身的失效。
var expectedGroups = []string{
	"domain", "application", "infrastructure", "transport", "presentation", "plugin",
}

const (
	modulePath     = "github.com/jeeinn/motd"
	internalPrefix = modulePath + "/internal/"
	cmdPrefix      = modulePath + "/cmd/"
)

type edge struct {
	From string // 发起方分组
	To   string // 目标分组
	File string // 相对路径，仅用于报错定位
	Imp  string // 原始 import path
}

type violation struct {
	edge
	Reason string
}

// findModuleRoot 从当前测试文件所在目录向上找 go.mod。
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前目录: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod，无法确定 module 根目录")
		}
		dir = parent
	}
}

// collectEdges 扫描 root 下所有 .go 文件，收集模块内部的跨分组 import。
// 只做 AST 层的 import 解析，不编译代码、不启动 go 命令，因此快且确定。
func collectEdges(root string) ([]edge, map[string]int, error) {
	var edges []edge
	filesPerGroup := make(map[string]int)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		from, ok := groupOfFile(filepath.ToSlash(rel))
		if !ok {
			return nil
		}
		filesPerGroup[from]++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			to, ok := groupOfImport(strings.Trim(imp.Path.Value, `"`))
			if !ok {
				continue // 标准库 / 第三方依赖，不在约束范围内
			}
			edges = append(edges, edge{From: from, To: to, File: filepath.ToSlash(rel), Imp: imp.Path.Value})
		}
		return nil
	})
	return edges, filesPerGroup, err
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "data", "docs", "vendor", "node_modules", "bin", "dist", ".workbuddy":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// groupOfFile 从相对路径推导发起方分组：internal/<group>/... 或 cmd/...
func groupOfFile(rel string) (string, bool) {
	switch {
	case strings.HasPrefix(rel, "internal/"):
		rest := strings.TrimPrefix(rel, "internal/")
		group, _, ok := strings.Cut(rest, "/")
		if !ok {
			return "", false
		}
		return group, group != ""
	case strings.HasPrefix(rel, "cmd/"):
		return "cmd", true
	default:
		return "", false
	}
}

// groupOfImport 从 import path 推导目标分组；非本模块路径返回 false。
func groupOfImport(imp string) (string, bool) {
	switch {
	case strings.HasPrefix(imp, internalPrefix):
		group, _, ok := strings.Cut(strings.TrimPrefix(imp, internalPrefix), "/")
		return group, ok && group != ""
	case strings.HasPrefix(imp, cmdPrefix):
		return "cmd", true
	default:
		return "", false
	}
}

// check 对一组边应用禁止表，返回全部违规。
// 抽成纯函数是为了让守卫本身可被负向测试覆盖（P0-03 验收要求）。
func check(edges []edge) []violation {
	var out []violation
	for _, e := range edges {
		if e.From == e.To {
			continue // 同组引用合法
		}
		if slices.Contains(forbidden[e.From], e.To) {
			out = append(out, violation{
				edge:   e,
				Reason: e.From + " 不得依赖 " + e.To,
			})
		}
	}
	return out
}

func formatViolations(vs []violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString("  - ")
		b.WriteString(v.File)
		b.WriteString(": ")
		b.WriteString(v.Imp)
		b.WriteString(" → ")
		b.WriteString(v.Reason)
		b.WriteString("\n")
	}
	return b.String()
}

// TestDependencyDirection 是主守卫：真实代码里不允许出现任何反向依赖。
func TestDependencyDirection(t *testing.T) {
	root := findModuleRoot(t)
	edges, _, err := collectEdges(root)
	if err != nil {
		t.Fatalf("扫描 import 图失败: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("未扫描到任何模块内部 import，扫描逻辑可能失效")
	}
	if vs := check(edges); len(vs) > 0 {
		t.Fatalf("检测到 %d 条违反 §2.2 依赖方向的引用:\n%s", len(vs), formatViolations(vs))
	}
}

// TestGuardDetectsForbiddenEdge 复现 P0-03 验收场景：
// 真的往 domain 里加一条反向 import，守卫必须报出来。
// 用临时目录而不是改项目文件，避免测试本身污染工作区。
func TestGuardDetectsForbiddenEdge(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "internal", "domain", "probe")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `package probe

import _ "github.com/jeeinn/motd/internal/infrastructure/sqlite"
import _ "github.com/jeeinn/motd/cmd/server"
`
	if err := os.WriteFile(filepath.Join(target, "bad.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	edges, _, err := collectEdges(root)
	if err != nil {
		t.Fatalf("扫描临时目录: %v", err)
	}
	vs := check(edges)
	if len(vs) != 2 {
		t.Fatalf("注入 2 条反向 import 应全部被检出，实际检出 %d 条:\n%s", len(vs), formatViolations(vs))
	}

	var sawInfrastructure, sawCmd bool
	for _, v := range vs {
		if v.From != "domain" {
			t.Fatalf("违规发起方应为 domain，实际 %q", v.From)
		}
		sawInfrastructure = sawInfrastructure || v.To == "infrastructure"
		sawCmd = sawCmd || v.To == "cmd"
	}
	if !sawInfrastructure || !sawCmd {
		t.Fatalf("未完整检出两条违规: infrastructure=%v cmd=%v", sawInfrastructure, sawCmd)
	}
}

// TestGuardDetectsEveryForbiddenGroup 保证禁止表覆盖每一类反向引用，
// 避免「规则表漏了一行 → 某类耦合悄悄混进来」。
func TestGuardDetectsEveryForbiddenGroup(t *testing.T) {
	var edges []edge
	for from, targets := range forbidden {
		for _, to := range targets {
			edges = append(edges, edge{From: from, To: to, File: "synthetic", Imp: modulePath + "/internal/" + to})
		}
	}
	vs := check(edges)
	if len(vs) != len(edges) {
		t.Fatalf("禁止表中的 %d 条规则应全部生效，实际检出 %d 条", len(edges), len(vs))
	}
}

// TestGuardCoversAllInternalGroups 防止扫描漏目录造成假绿。
func TestGuardCoversAllInternalGroups(t *testing.T) {
	root := findModuleRoot(t)
	_, filesPerGroup, err := collectEdges(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range expectedGroups {
		if filesPerGroup[group] == 0 {
			t.Errorf("分组 %q 未被扫描到任何 .go 文件，守卫可能漏扫", group)
		}
		if _, ok := forbidden[group]; !ok {
			t.Errorf("分组 %q 没有对应的依赖规则", group)
		}
	}
	// 同组引用必须合法，否则规则表过严会误报。
	if vs := check([]edge{{From: "domain", To: "domain"}}); len(vs) != 0 {
		t.Error("同组引用不应被判为违规")
	}
}

// TestApplicationHasNoSQLImports 把 P0-08 的约束「端口定义在使用方、不返回 *sql.Rows」
// 固化成守卫：application 层一旦出现 SQL 驱动 import，说明有人绕过了 UnitOfWork 直连数据库。
// groupOfImport 只识别模块内部路径，因此这里单独扫全部 import（含标准库与第三方）。
func TestApplicationHasNoSQLImports(t *testing.T) {
	root := findModuleRoot(t)
	appDir := filepath.Join(root, "internal", "application")
	banned := []string{"database/sql", "modernc.org/sqlite"}

	err := filepath.WalkDir(appDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if p == b {
					t.Errorf("%s 不得 import %q：application 层只声明端口，SQL 实现必须留在 infrastructure", filepath.ToSlash(rel), p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 application 目录失败: %v", err)
	}
}

// TestGroupParsing 锁定路径解析逻辑，改目录结构时这条会先失败。
func TestGroupParsing(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"internal/domain/user/user.go", "domain", true},
		{"internal/infrastructure/sqlite/db.go", "infrastructure", true},
		{"cmd/server/main.go", "cmd", true},
		{"README.md", "", false},
		{"internal/domain", "", false},
	}
	for _, c := range cases {
		got, ok := groupOfFile(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("groupOfFile(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}

	imports := []struct {
		in   string
		want string
		ok   bool
	}{
		{internalPrefix + "domain/user", "domain", true},
		{internalPrefix + "infrastructure/sqlite", "infrastructure", true},
		{cmdPrefix + "server", "cmd", true},
		{"database/sql", "", false},
		{modulePath + "/other", "", false},
	}
	for _, c := range imports {
		got, ok := groupOfImport(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("groupOfImport(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
