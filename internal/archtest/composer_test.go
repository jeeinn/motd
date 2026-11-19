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

// P1-M4 静态守卫（ADR-034）：把三条易被后续改动破坏的约束变成会失败的测试。
//
//  1. composer 是纯应用逻辑：不 import transport / infrastructure / presentation / cmd；
//  2. 粘贴整块进正文：composer 不合成 KeyMsg、不递归 Feed；decoder 的 paste 分支
//     不产出 KeyEnter / KeyRune（否则内部换行会被 re-split 成按键、误触哨兵提交）；
//  3. 服务端固定文案绝不出现 "Ctrl+Enter"（字节流层不可靠，P1 不做主提交键）。

func parseOneGo(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return f
}

// ---------------------------------------------------------------------------
// 1. composer 的依赖方向
// ---------------------------------------------------------------------------

func composerForbiddenImports(f *ast.File) []string {
	var out []string
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range []string{"/transport", "/infrastructure", "/presentation", "/cmd"} {
			if strings.Contains(p, bad) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func TestComposerDoesNotImportUpperLayers(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "input", "composer.go")
	bad := composerForbiddenImports(parseOneGo(t, path))
	if len(bad) > 0 {
		t.Fatalf("composer 必须保持纯应用逻辑，不得依赖: %v", bad)
	}
}

func TestGuardDetectsComposerUpperLayerImport(t *testing.T) {
	src := `package input

import _ "github.com/jeeinn/motd/internal/transport/ssh"
`
	tmp := filepath.Join(t.TempDir(), "composer.go")
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := composerForbiddenImports(parseOneGo(t, tmp))
	if len(bad) != 1 || !strings.Contains(bad[0], "/transport/") {
		t.Fatalf("注入的上层 import 应被检出，实际 %v", bad)
	}
}

// ---------------------------------------------------------------------------
// 2. 粘贴块不得被 re-split 成按键
// ---------------------------------------------------------------------------

func isName(n ast.Expr, want string) bool {
	switch v := n.(type) {
	case *ast.Ident:
		return v.Name == want
	case *ast.SelectorExpr:
		return v.Sel.Name == want
	}
	return false
}

// pasteResplitIssues 返回 composer 里「合成按键 / 递归 Feed / 缺少 Paste 分支」的问题。
func pasteResplitIssues(f *ast.File) []string {
	var out []string
	hasPasteCase := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CaseClause:
			for _, e := range v.List {
				if isName(e, "KeyPaste") {
					hasPasteCase = true
				}
			}
		case *ast.CompositeLit:
			if isName(v.Type, "KeyMsg") {
				out = append(out, "composer 合成 KeyMsg（粘贴可能被拆成按键）")
			}
		case *ast.CallExpr:
			if isName(v.Fun, "Feed") {
				out = append(out, "composer 递归调用 Feed（粘贴块被 re-split）")
			}
		}
		return true
	})
	if !hasPasteCase {
		out = append(out, "composer 缺少 KeyPaste 分支，守卫可能失效")
	}
	return out
}

func TestComposerTreatsPasteAsSingleBlock(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "input", "composer.go")
	if got := pasteResplitIssues(parseOneGo(t, path)); len(got) > 0 {
		t.Fatalf("粘贴必须整块进正文:\n  %s", strings.Join(got, "\n  "))
	}
}

func TestGuardDetectsPasteResplit(t *testing.T) {
	src := `package input

func (c *Composer) bad(m KeyMsg) {
	for _, r := range m.Text {
		_, _ = c.Feed(KeyMsg{Kind: KeyRune, Rune: r})
	}
}
`
	tmp := filepath.Join(t.TempDir(), "composer.go")
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := pasteResplitIssues(parseOneGo(t, tmp))
	if len(got) < 2 {
		t.Fatalf("注入的 re-split 应被检出（合成 KeyMsg + 递归 Feed），实际 %v", got)
	}
}

// decoderPasteBranchIssues 检查 decoder 的 inPaste 分支不产出按键事件。
func decoderPasteBranchIssues(f *ast.File) ([]string, bool) {
	var out []string
	hasPasteMsg := false
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isName(cl.Type, "KeyMsg") {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok || !isName(kv.Key, "Kind") {
				continue
			}
			if isName(kv.Value, "KeyPaste") {
				hasPasteMsg = true
			}
		}
		return true
	})

	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || !isName(ifs.Cond, "inPaste") {
			return true
		}
		ast.Inspect(ifs.Body, func(inner ast.Node) bool {
			cl, ok := inner.(*ast.CompositeLit)
			if !ok || !isName(cl.Type, "KeyMsg") {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok || !isName(kv.Key, "Kind") {
					continue
				}
				if isName(kv.Value, "KeyEnter") || isName(kv.Value, "KeyRune") {
					out = append(out, "decoder 粘贴分支内产出 KeyEnter/KeyRune")
				}
			}
			return true
		})
		return true
	})
	return out, hasPasteMsg
}

func TestDecoderPasteEmitsSinglePasteEvent(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "transport", "ssh", "decoder.go")
	issues, hasPaste := decoderPasteBranchIssues(parseOneGo(t, path))
	if !hasPaste {
		t.Fatal("decoder 必须产出 KeyPaste，守卫可能失效")
	}
	if len(issues) > 0 {
		t.Fatalf("粘贴块内部换行不得被拆成按键:\n  %s", strings.Join(issues, "\n  "))
	}
}

func TestGuardDetectsDecoderPasteResplit(t *testing.T) {
	src := `package ssh

func bad(d *Decoder) {
	if d.inPaste {
		out = append(out, input.KeyMsg{Kind: input.KeyEnter})
	}
}
`
	tmp := filepath.Join(t.TempDir(), "decoder.go")
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, _ := decoderPasteBranchIssues(parseOneGo(t, tmp))
	if len(issues) != 1 {
		t.Fatalf("注入的粘贴分支 KeyEnter 应被检出，实际 %v", issues)
	}
}

// ---------------------------------------------------------------------------
// 3. 服务端文案禁 Ctrl+Enter
// ---------------------------------------------------------------------------

func ctrlEnterInSources(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		lower := strings.ToLower(string(b))
		if strings.Contains(lower, "ctrl+enter") || strings.Contains(lower, "ctrl-enter") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func TestNoCtrlEnterInServerFixedText(t *testing.T) {
	root := findModuleRoot(t)
	// 文案常驻 cmd/server；composer 内核也一并扫，避免有人把禁用组合写进
	// 常量附近再从那里复制回提示。
	for _, rel := range [][]string{
		{"cmd", "server"},
		{"internal", "application", "input"},
	} {
		dir := filepath.Join(append([]string{root}, rel...)...)
		found, err := ctrlEnterInSources(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 {
			t.Fatalf("%s 的源码禁止出现 Ctrl+Enter（P1 不做主提交键，ADR-034）: %v", filepath.Join(rel...), found)
		}
	}
}

func TestGuardDetectsCtrlEnterText(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "hint.go"), []byte("package main\nconst h = \"Ctrl+Enter 提交\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := ctrlEnterInSources(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("注入的 Ctrl+Enter 文案应被检出，实际 %v", found)
	}
}
