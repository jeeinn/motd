package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// collectANSILiterals 扫描目录下非测试 .go 文件里的字符串字面量，
// 收集所有含 ESC 的值。抽成纯函数是为了让守卫本身可被负向测试覆盖。
func collectANSILiterals(dir string) (map[string][]string, error) {
	out := make(map[string][]string)
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
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.Contains(val, "\x1b") {
				out[name] = append(out[name], val)
			}
			return true
		})
	}
	return out, nil
}

// sgrOnly 判定一个字符串是不是纯 SGR 序列（\x1b[数字;...m）。
func sgrOnly(s string) bool {
	if !strings.HasPrefix(s, "\x1b[") || !strings.HasSuffix(s, "m") || len(s) < 4 {
		return false
	}
	for _, c := range s[2 : len(s)-1] {
		if (c < '0' || c > '9') && c != ';' {
			return false
		}
	}
	return true
}

// TestSafeRendererANSIOnlyFromTable 把「受控 ANSI 生成」变成会失败的测试：
// ESC 字面量只能出现在 ansi.go，且必须全部是纯 SGR。
func TestSafeRendererANSIOnlyFromTable(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "presentation", "terminal", "safe")
	found, err := collectANSILiterals(dir)
	if err != nil {
		t.Fatalf("扫描 safe 包失败: %v", err)
	}
	total := 0
	for file, vals := range found {
		for _, v := range vals {
			total++
			if file != "ansi.go" {
				t.Errorf("%s 出现 \\x1b 字面量：ESC 序列只能定义在 ansi.go 的常量表", file)
			} else if !sgrOnly(v) {
				t.Errorf("ansi.go 里的 ESC 字面量必须是纯 SGR，实际 %q", v)
			}
		}
	}
	if total == 0 {
		t.Fatal("未扫描到任何 ESC 常量，守卫可能漏扫")
	}
}

func TestSafeGuardDetectsInjectedANSI(t *testing.T) {
	tmp := t.TempDir()
	src := "package safe\n\nconst injected = \"\\x1b[2J\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectANSILiterals(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found["bad.go"]) != 1 {
		t.Fatalf("注入的 ESC 字面量必须被检出，实际 %v", found)
	}
	if sgrOnly(found["bad.go"][0]) {
		t.Fatal("注入的清屏序列不能被误判为合法 SGR")
	}
}

// TestRenderSystemHasNoSanitizeCall 证明服务端路径与用户路径不共用 sanitize（§3.5）。
func TestRenderSystemHasNoSanitizeCall(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "presentation", "terminal", "safe", "render.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "RenderSystem" || fd.Body == nil {
			continue
		}
		seen = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "SanitizeText" {
				t.Error("RenderSystem 不得调用 SanitizeText：服务端文本与用户文本必须走不同路径")
			}
			return true
		})
	}
	if !seen {
		t.Fatal("未找到 RenderSystem 函数，守卫可能失效")
	}
}
