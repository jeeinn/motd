package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// P0-11 验收项：ViewModel 里出现任何样式/终端概念即为不合格。
// 这里把评审意见固化成静态守卫：扫描 application/viewmodel 的 AST，
// 禁止渲染库 import、渲染相关标识符与控制序列字面量。
//
// 注意不把 "cursor" 一刀切：keyset 分页的 NextCursor 是数据语义，
// 与光标位置（CursorPos）是两回事，禁用表只拦后者。

var renderingBannedImports = []string{
	"github.com/charmbracelet", "lipgloss", "bubbletea", "glamour",
	"termenv", "xterm", "ansi", "database/sql", "modernc.org/sqlite",
}

// 短词按「驼峰词段精确相等」匹配，避免 EmptyRows 里的 pty、
// Description 里的 esc 这类子串误报。长组合词另用整名子串匹配。
var renderingBannedWords = []string{
	"ansi", "csi", "osc", "sgr", "color", "colour",
	"foreground", "background", "bold", "italic", "underline",
	"width", "height", "frame", "pty", "terminal",
	"render", "renderer", "style", "pixel",
}

var renderingBannedPhrases = []string{
	"cursorpos", "cursorposition", "escseq", "linecol", "screenwidth", "screenheight",
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// identifierSegments 把驼峰标识符拆成小写词段：
// EmptyRows → [empty rows]，FrameID → [frame id]，ANSI → [ansi]。
func identifierSegments(name string) []string {
	s := camelBoundary.ReplaceAllString(name, "${1}|${2}")
	s = strings.ReplaceAll(s, "_", "|")
	s = strings.ReplaceAll(s, "-", "|")
	var out []string
	for _, part := range strings.Split(strings.ToLower(s), "|") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func hasBannedWord(name string) (string, bool) {
	low := strings.ToLower(name)
	for _, phrase := range renderingBannedPhrases {
		if strings.Contains(low, phrase) {
			return phrase, true
		}
	}
	for _, seg := range identifierSegments(name) {
		for _, w := range renderingBannedWords {
			if seg == w {
				return w, true
			}
		}
	}
	return "", false
}

var controlSequences = []string{`\x1b`, `\033`, `\u001b`, `\x9b`, `ESC[`}

// scanViewModelDir 返回 viewmodel 包内所有「渲染概念」违规描述。
// 抽成对目录的纯扫描函数，便于用临时目录做负向测试。
func scanViewModelDir(root string) ([]string, error) {
	dir := filepath.Join(root, "internal", "application", "viewmodel")
	var problems []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0) // 不解析注释：注释里的中文说明不受约束
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range renderingBannedImports {
				if strings.Contains(strings.ToLower(p), b) {
					problems = append(problems, rel+": 禁止 import "+p)
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if w, ok := hasBannedWord(x.Name); ok {
					problems = append(problems, rel+": 标识符含渲染概念 "+x.Name+"（命中 "+w+"）")
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				for _, seq := range controlSequences {
					if strings.Contains(x.Value, seq) || strings.ContainsRune(x.Value, '\x1b') {
						problems = append(problems, rel+": 字符串含终端控制序列 "+seq)
					}
				}
			}
			return true
		})
		return nil
	})
	return problems, err
}

// TestViewModelIsRenderingAgnostic 是主守卫。
func TestViewModelIsRenderingAgnostic(t *testing.T) {
	root := findModuleRoot(t)
	problems, err := scanViewModelDir(root)
	if err != nil {
		t.Fatalf("扫描 viewmodel 包失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("ViewModel 出现 %d 处渲染概念:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// TestViewModelGuardDetectsInjection 用临时目录证明守卫本身会失败，
// 避免「禁用表写错 / 扫描漏文件」造成的假绿。
func TestViewModelGuardDetectsInjection(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "application", "viewmodel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `package viewmodel

import _ "github.com/charmbracelet/lipgloss"

type Bad struct {
	Color   string
	Width   int
	FrameID int
	Escape  string
}

var _ = Bad{Escape: "\x1b[31m"}
`
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := scanViewModelDir(root)
	if err != nil {
		t.Fatalf("扫描临时目录: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("注入渲染概念后守卫未报任何违规，守卫失效")
	}

	joined := strings.Join(problems, "\n")
	for _, want := range []string{"禁止 import", "Color", "Width", "Frame", "终端控制序列"} {
		if !strings.Contains(joined, want) {
			t.Errorf("违规报告缺少 %q:\n%s", want, joined)
		}
	}
}

// TestApplicationHasNoRenderingImports 覆盖 forum 等 application 子包里的
// ViewModel 构造代码：状态映射可以存在，但不得引入任何渲染库。
func TestApplicationHasNoRenderingImports(t *testing.T) {
	root := findModuleRoot(t)
	appDir := filepath.Join(root, "internal", "application")
	banned := []string{"github.com/charmbracelet", "lipgloss", "bubbletea", "glamour", "termenv", "xterm"}

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
				if strings.Contains(strings.ToLower(p), b) {
					t.Errorf("%s 不得 import %q：application 层只输出语义状态", filepath.ToSlash(rel), p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 application 目录失败: %v", err)
	}
}

// 防止渲染库检测被遗漏：确保禁用表非空、正负样本都按预期分类。
func TestRenderingGuardTablesAreUsable(t *testing.T) {
	if len(renderingBannedImports) == 0 || len(renderingBannedWords) == 0 ||
		len(renderingBannedPhrases) == 0 || len(controlSequences) == 0 {
		t.Fatal("禁用表为空，守卫会假绿")
	}
	for _, b := range append(append([]string{}, renderingBannedImports...), renderingBannedWords...) {
		if b == "" {
			t.Fatal("禁用表含空串，会匹配所有标识符并误报")
		}
	}

	banned := []string{"Color", "Width", "FrameID", "ANSI", "LineStyle", "CursorPos", "TerminalWidth"}
	for _, name := range banned {
		if _, ok := hasBannedWord(name); !ok {
			t.Errorf("%q 应被判为渲染概念", name)
		}
	}
	// 合法语义标识符不得误伤：分页游标、空行、描述、转义字段。
	allowed := []string{"NextCursor", "EmptyRows", "Description", "Escape", "Detail", "RetryAfterSec"}
	for _, name := range allowed {
		if w, ok := hasBannedWord(name); ok {
			t.Errorf("%q 是数据语义，不应命中 %q", name, w)
		}
	}
}
