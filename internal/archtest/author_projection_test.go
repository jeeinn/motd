package archtest

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-014/019 静态守卫：作者名来自 users 表，是外部文本，
// 只能经 ext()（RenderUser）渲染；任何传进 sys()（RenderSystem）的写法
// 都会在含 ESC 的用户名上炸，且破坏信任域分离。

// renderCallsReferencing 返回 path 中传给函数 funcName、且实参源码命中任一 needle 的片段。
func renderCallsReferencing(t *testing.T, path, funcName string, needles ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != funcName {
			return true
		}
		for _, arg := range call.Args {
			var b strings.Builder
			if err := printer.Fprint(&b, fset, arg); err != nil {
				t.Fatalf("打印实参: %v", err)
			}
			frag := b.String()
			for _, needle := range needles {
				if strings.Contains(frag, needle) {
					hits = append(hits, frag)
					break
				}
			}
		}
		return true
	})
	return hits
}

func TestAuthorTextNeverEntersRenderSystem(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "presentation", "terminal", "view.go")

	if hits := renderCallsReferencing(t, path, "sys", "Author", "authorSegment"); len(hits) > 0 {
		t.Fatalf("sys()（RenderSystem）不得接收用户来源的作者文本: %q", hits)
	}
	if hits := renderCallsReferencing(t, path, "ext", "authorSegment"); len(hits) == 0 {
		t.Fatal("作者段必须经 ext()（RenderUser）渲染，守卫可能漏扫")
	}
}

// 负向注入：把作者段塞进 sys()，守卫必须报错，避免静态断言假绿。
func TestAuthorRenderSystemGuardDetectsInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "view.go")
	src := `package terminal
func bad() { sys(&b, authorSegment(r), safe.StyleDim) }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if hits := renderCallsReferencing(t, path, "sys", "Author", "authorSegment"); len(hits) == 0 {
		t.Fatal("注入 sys(&b, authorSegment(r)) 后守卫未报违规")
	}
}
