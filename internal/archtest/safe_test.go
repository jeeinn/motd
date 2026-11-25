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

// allowedANSI 是 ansi.go 里 ESC 字面量的完整白名单（ADR-027，ADR-072 扩充）：
// 纯 SGR、EL 清行、CUU 上移模板，以及整屏重绘所需的**无参** CUP home [H
// 与**无参** ED 2 [2J。其余一律拒绝，且模板里的 %d 只由服务端按「上一屏行数」
// 填充，不接收用户输入。
//
// 注意 [J（无参 ED）**不在**白名单：放开的是明确的 2J，而不是整个 ED 家族。
func allowedANSI(s string) bool {
	if sgrOnly(s) {
		return true
	}
	switch s {
	case "\x1b[K", "\x1b[%dA", "\x1b[H", "\x1b[2J":
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
				t.Errorf("ansi.go 里的 ESC 字面量必须属于 SGR/EL/CUU/H/2J 白名单，实际 %q", v)
			}
		}
	}
	if total == 0 {
		t.Fatal("未扫描到任何 ESC 常量，守卫可能漏扫")
	}
}

// TestSafeGuardDetectsInjectedANSI 自证守卫仍然有效：注入一个**仍禁止**的
// ED 3J（清 scrollback）字面量，collectANSILiterals 必须检出，且 allowedANSI
// 必须拒绝。样本不能用 2J/[H——它们已被 ADR-072 放开，否则此测试会永远通过。
func TestSafeGuardDetectsInjectedANSI(t *testing.T) {
	tmp := t.TempDir()
	src := "package safe\n\nconst injected = \"\\x1b[3J\"\n"
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
		t.Fatal("注入的清历史序列不能被误判为合法白名单序列")
	}
}

// TestCursorControlGuardRejectsForbiddenSequences 是 ADR-027/ADR-072 的负向注入：
// 放开无参 H 与 2J 之后，清历史 3J、1J、无参 ED [J、带参 CUP、CUD、备用屏
// 进出、OSC 剪贴板/标题、带参数 EL 必须仍然被拒。逐条验证，避免
// 「白名单放宽一次，红线全线失守」。
func TestCursorControlGuardRejectsForbiddenSequences(t *testing.T) {
	bad := []string{
		"\x1b[J",             // ED 无参数（仍禁）
		"\x1b[1J",            // ED 1：清光标前（仍禁）
		"\x1b[3J",            // ED 3：清 scrollback（永禁）
		"\x1b[f",             // CUP 带参等价式（仍禁）
		"\x1b[2;3H",          // CUP 带参（只放开无参 [H）
		"\x1b[B",             // CUD 下移
		"\x1b[?1049h",        // DECSET 进入备用屏
		"\x1b[?1049l",        // DECRST 退出备用屏
		"\x1b[2K",            // 带参数的 EL（只允许无参数 K）
		"\x1b]52;c;YWJj\x07", // OSC 52 剪贴板
		"\x1b]0;pwned\x07",   // OSC 改窗口标题
		"\x1b[31m\x1b[3J",    // SGR 后面夹带清历史
		"\x1b[31m\x1b[2;3H",  // SGR 后面夹带带参 CUP
	}
	for _, s := range bad {
		if allowedANSI(s) {
			t.Errorf("禁止序列被误判为合法: %q", s)
		}
	}
	for _, s := range []string{"\x1b[0m", "\x1b[1;4m", "\x1b[K", "\x1b[%dA", "\x1b[H", "\x1b[2J",
		"\x1b[1;92m", "\x1b[2;32m"} { // v0.9.2 / ADR-076：/matrix 两档绿
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
		for _, banned := range []string{
			"CursorUp(", "ClearToEOL(", "CursorHome(", "ClearScreen(",
			"cursorUpFmt", "clearToEOLANSI", "cursorHomeANSI", "clearScreenANSI",
		} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s 不得引用光标控制 %q：用户内容路径与服务端控制必须分离", name, banned)
			}
		}
	}
}

// TestCursorControlsOnlyUsedByFrame 保证光标/清屏控制只在受控消费点出现：
// presentation/terminal 的 frame.go（RenderFrame / RenderCleanFrame）与
// matrix.go（/matrix 屏内雨帘帧组装，v0.9.2 / ADR-076）。
// cmd 层只通过它们间接使用。
func TestCursorControlsOnlyUsedByFrame(t *testing.T) {
	root := findModuleRoot(t)
	internalDir := filepath.Join(root, "internal")
	allowed := map[string]bool{
		"internal/presentation/terminal/safe/ansi.go": true, // 定义处
		"internal/presentation/terminal/frame.go":     true, // 页面整屏重绘
		"internal/presentation/terminal/matrix.go":    true, // /matrix 屏内雨帘（v0.9.2）
	}
	calls := []string{"CursorUp(", "ClearToEOL(", "CursorHome(", "ClearScreen("}
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
		used := false
		for _, c := range calls {
			if strings.Contains(string(b), c) {
				used = true
				break
			}
		}
		if !used {
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
// ---------------------------------------------------------------------------
// v0.3.0 跨行退格合并：composer 与 line_echo 不得内联 ESC 字面量（ADR-027）
// ---------------------------------------------------------------------------

// collectFileANSILiterals 与 collectANSILiterals 同判定，粒度到单文件，
// 便于对跨行实现的两个文件与负向注入样本逐条检查。
func collectFileANSILiterals(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, err := strconv.Unquote(lit.Value)
		if err == nil && strings.Contains(val, "\x1b") {
			out = append(out, val)
		}
		return true
	})
	return out, nil
}

// TestCrossLineMergeNoRawANSI 守住跨行合并的单一 ANSI 来源：composer 是纯应用
// 逻辑、line_echo 只能调用 safe 包的服务端白名单函数（CursorUp / ClearToEOL）。
// 两处出现任何 ESC 字面量即判违规——即便字面量本身属于白名单也不许内联，
// 否则就从「白名单物理隔离」退化成「靠纪律约束」。
func TestCrossLineMergeNoRawANSI(t *testing.T) {
	root := findModuleRoot(t)
	for _, rel := range []string{
		filepath.Join("cmd", "server", "line_echo.go"),
		filepath.Join("internal", "application", "input", "composer.go"),
	} {
		lits, err := collectFileANSILiterals(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if len(lits) > 0 {
			t.Errorf("%s 不得内联 ESC 字面量，须经 safe 包白名单函数生成: %q", rel, lits)
		}
	}
}

// TestCrossLineMergeRejectsInjectedForbiddenANSI 是负向注入：把带参 CUP/CUD/
// ED 1J/3J/无参 [J/DECSET 塞进跨行回显样本，守卫与 allowedANSI 都必须拒绝。
// 样本不再含单独的无参 [H 与 2J——它们已被 ADR-072 放开，留在这里会误判。
func TestCrossLineMergeRejectsInjectedForbiddenANSI(t *testing.T) {
	for _, seq := range []string{"\x1b[1;1H", "\x1b[2;3H", "\x1b[f", "\x1b[B", "\x1b[1J", "\x1b[3J", "\x1b[J", "\x1b[?1049h"} {
		src := "package main\nconst bad = " + strconv.Quote(seq) + "\n"
		tmp := filepath.Join(t.TempDir(), "line_echo.go")
		if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		lits, err := collectFileANSILiterals(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if len(lits) == 0 {
			t.Errorf("注入的禁用序列 %q 未被守卫检出", seq)
		}
		if allowedANSI(seq) {
			t.Errorf("禁用序列 %q 被 allowedANSI 误判为合法", seq)
		}
	}
}

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
