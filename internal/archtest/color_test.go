package archtest

// P4-T1 配色守卫：三种服务端配色 Style 只能由 ansi.go 定义、view.go 消费。
//
// 背景：终端基础多色引入 Accent/Notice/Error 三个 SGR 前景色。它们只用于
// 服务端固定文案的标签/前缀（状态栏 L3/L4、[错误]/[提示]），用户文本主体
// 永不上色。这里用静态扫描把约束固化，避免后来者在 Markdown 解析或
// ViewModel 字段里偷偷加入「用户可选颜色」的通道。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// colorStyleNames 是新增的三个配色 Style 标识符。
var colorStyleNames = []string{"StyleAccent", "StyleNotice", "StyleError"}

// colorStyleAllowlist 是配色 Style 允许出现的非测试源文件（相对模块根）。
// ansi.go 定义 SGR 映射；view.go 是服务端固定文案的唯一消费点。
var colorStyleAllowlist = map[string]bool{
	"internal/presentation/terminal/safe/ansi.go": true,
	"internal/presentation/terminal/view.go":      true,
}

// userPalettePaths 是用户内容路径：任何配色标识符出现在这里都算越界。
var userPalettePaths = []string{"render.go", "sanitize.go", "markdown.go"}

// scanUserPaletteLeak 检查两件事：
//  1. 用户内容路径（RenderUser / SanitizeText / Markdown 解析）不引用配色 Style；
//  2. styleFor() 函数体不返回配色 Style（用户 Markdown 不能选色）。
//
// 抽成对 root 的纯函数，便于用临时目录做负向注入。
func scanUserPaletteLeak(root string) []string {
	var problems []string
	dir := filepath.Join(root, "internal", "presentation", "terminal", "safe")

	for _, name := range userPalettePaths {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, "读取 "+name+" 失败: "+err.Error())
			continue
		}
		for _, style := range colorStyleNames {
			if strings.Contains(string(src), style) {
				problems = append(problems, name+" 不得引用配色 "+style+"：用户内容路径不能选色")
			}
		}
	}

	path := filepath.Join(dir, "render.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		problems = append(problems, "解析 render.go 失败: "+err.Error())
		return problems
	}
	seen := false
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "styleFor" || fd.Body == nil {
			continue
		}
		seen = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			for _, style := range colorStyleNames {
				if id.Name == style {
					problems = append(problems, "styleFor() 不得返回配色 "+style)
				}
			}
			return true
		})
	}
	if !seen {
		problems = append(problems, "未找到 styleFor()，守卫可能失效")
	}
	return problems
}

// scanColorStyleUsage 扫描 internal 下所有非测试 Go 文件：
// 配色 Style 只允许出现在 colorStyleAllowlist 列出的两个文件里。
func scanColorStyleUsage(root string) []string {
	var problems []string
	internalDir := filepath.Join(root, "internal")
	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, style := range colorStyleNames {
			if !strings.Contains(string(src), style) {
				continue
			}
			if !colorStyleAllowlist[rel] {
				problems = append(problems, rel+" 不得引用 "+style+"：配色只允许 ansi.go 定义、view.go 消费")
			}
		}
		return nil
	})
	if err != nil {
		problems = append(problems, "扫描 internal 失败: "+err.Error())
	}
	return problems
}

// TestUserMarkdownCannotSelectPalette 是主守卫之一：用户内容与 palette 隔离。
func TestUserMarkdownCannotSelectPalette(t *testing.T) {
	problems := scanUserPaletteLeak(findModuleRoot(t))
	if len(problems) > 0 {
		t.Fatalf("用户内容路径出现配色引用 %d 处:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// TestColorStylesOnlyUsedBySystemPath 是主守卫之二：单一消费点。
func TestColorStylesOnlyUsedBySystemPath(t *testing.T) {
	problems := scanColorStyleUsage(findModuleRoot(t))
	if len(problems) > 0 {
		t.Fatalf("配色 Style 出现在非授权文件 %d 处:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// TestUserPaletteGuardDetectsInjection 证明守卫本身会失败：
// 在 temp 根里同时把配色塞进 markdown.go 与 styleFor()，两者都必须被检出。
func TestUserPaletteGuardDetectsInjection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "presentation", "terminal", "safe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"render.go": `package safe

func styleFor(k Kind) Style {
	return StyleError
}
`,
		"sanitize.go": "package safe\n",
		"markdown.go": "package safe\nvar _ = StyleNotice\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	problems := scanUserPaletteLeak(root)
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"markdown.go", "StyleNotice", "styleFor()", "StyleError"} {
		if !strings.Contains(joined, want) {
			t.Errorf("注入样本未被守卫检出 %q:\n%s", want, joined)
		}
	}
}

// TestColorStyleUsageGuardDetectsInjection 证明单一消费点守卫不是假绿：
// temp 根里的授权文件放行了，未授权文件必须报违规。
func TestColorStyleUsageGuardDetectsInjection(t *testing.T) {
	root := t.TempDir()
	allowed := []string{
		"internal/presentation/terminal/safe/ansi.go",
		"internal/presentation/terminal/view.go",
	}
	for _, rel := range allowed {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "package p\nvar _ = StyleAccent\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	badPath := filepath.Join(root, "internal", "application", "forum", "bad.go")
	if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, []byte("package forum\nvar _ = StyleAccent\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	problems := scanColorStyleUsage(root)
	if len(problems) != 1 || !strings.Contains(problems[0], "forum/bad.go") {
		t.Fatalf("只有未授权文件的配色引用应被检出，实际: %v", problems)
	}
}
