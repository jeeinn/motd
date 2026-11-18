package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// allowedANSI 是 ansi.go 里 ESC 字面量的完整白名单（ADR-027）：
// 纯 SGR、EL 清行、CUU 上移模板。其余一律拒绝，且模板里的 %d 只由
// 服务端按「上一屏行数」填充，不接收用户输入。
func allowedANSI(s string) bool {
	if sgrOnly(s) {
		return true
	}
	switch s {
	case "\x1b[K", "\x1b[%dA":
		return true
	}
	return false
}

// TestSafeRendererANSIOnlyFromTable 把「受控 ANSI 生成」变成会失败的测试：
// ESC 字面量只能出现在 ansi.go，且必须属于 allowedANSI 白名单。
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
			} else if !allowedANSI(v) {
				t.Errorf("ansi.go 里的 ESC 字面量必须属于 SGR/EL/CUU 白名单，实际 %q", v)
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
	if allowedANSI(found["bad.go"][0]) {
		t.Fatal("注入的清屏序列不能被误判为合法白名单序列")
	}
}

// TestCursorControlGuardRejectsForbiddenSequences 是 ADR-027 的负向注入：
// 放开 EL/CUU 之后，清屏、光标定位、备用屏、OSC 剪贴板/标题、带参数 EL
// 必须仍然被拒。逐条验证，避免「白名单放宽一次，红线全线失守」。
func TestCursorControlGuardRejectsForbiddenSequences(t *testing.T) {
	bad := []string{
		"\x1b[2J",          // ED 清屏
		"\x1b[J",           // ED 无参数
		"\x1b[H", "\x1b[f", // CUP 光标定位
		"\x1b[B",             // CUD 下移
		"\x1b[?1049h",        // DECSET 备用屏
		"\x1b[2K",            // 带参数的 EL（只允许无参数 K）
		"\x1b]52;c;YWJj\x07", // OSC 52 剪贴板
		"\x1b]0;pwned\x07",   // OSC 改窗口标题
		"\x1b[31m\x1b[2J",    // SGR 后面夹带清屏
	}
	for _, s := range bad {
		if allowedANSI(s) {
			t.Errorf("禁止序列被误判为合法: %q", s)
		}
	}
	for _, s := range []string{"\x1b[0m", "\x1b[1;4m", "\x1b[K", "\x1b[%dA"} {
		if !allowedANSI(s) {
			t.Errorf("白名单序列被误拒: %q", s)
		}
	}
}

// TestUserContentPathHasNoCursorControl 守住 §3.5 红线：用户内容路径
// （RenderUser / SanitizeText / Markdown 解析）绝不调用光标控制。
func TestUserContentPathHasNoCursorControl(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "presentation", "terminal", "safe")
	for _, name := range []string{"render.go", "sanitize.go", "markdown.go"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"CursorUp(", "ClearToEOL(", "cursorUpFmt", "clearToEOLANSI"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s 不得引用光标控制 %q：用户内容路径与服务端控制必须分离", name, banned)
			}
		}
	}
}

// TestCursorControlsOnlyUsedByFrame 保证光标控制只有一个消费点：
// presentation/terminal 的 RenderFrame。cmd 层只通过它间接使用。
func TestCursorControlsOnlyUsedByFrame(t *testing.T) {
	root := findModuleRoot(t)
	internalDir := filepath.Join(root, "internal")
	allowed := map[string]bool{
		"internal/presentation/terminal/safe/ansi.go": true, // 定义处
		"internal/presentation/terminal/frame.go":     true, // 唯一调用处
	}
	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "CursorUp(") && !strings.Contains(string(b), "ClearToEOL(") {
			return nil
		}
		if !allowed[rel] {
			t.Errorf("%s 不得直接调用光标控制（只允许 ansi.go 定义 / frame.go 消费）", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 internal 失败: %v", err)
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
