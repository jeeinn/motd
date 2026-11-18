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

// P0-15 静态守卫：把「协议细节留在 infrastructure」与「Device Flow 不需要
// client_secret」两条约束变成会失败的测试，而不是靠 code review。

var authBannedProtocolImports = []string{
	"net/http", "net/url", "encoding/json", "net",
}

// collectAuthProtocolImports 扫描 application/auth 的非测试源码。
// 端口定义在 application（使用方），HTTP / JSON 实现必须留在 infrastructure。
func collectAuthProtocolImports(dir string) ([]string, error) {
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
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range authBannedProtocolImports {
				if p == b {
					out = append(out, name+": import "+p)
				}
			}
		}
	}
	return out, nil
}

func TestAuthLayerHasNoProtocolImports(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "auth")
	found, err := collectAuthProtocolImports(dir)
	if err != nil {
		t.Fatalf("扫描 application/auth 失败: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("认证层不得 import HTTP / JSON 协议包（端口在 application，实现在 infrastructure）:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestAuthGuardDetectsProtocolImports(t *testing.T) {
	tmp := t.TempDir()
	src := "package auth\n\nimport \"net/http\"\nimport _ \"encoding/json\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectAuthProtocolImports(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("注入的两条协议 import 都应被检出，实际 %v", found)
	}
}

// collectClientSecretUsage 只在标识符与字符串字面量里找 client_secret；
// 注释里出现「不需要 client_secret」是文档，不算违规。
func collectClientSecretUsage(dir string) ([]string, error) {
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
			switch v := n.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING && strings.Contains(strings.ToLower(v.Value), "client_secret") {
					out = append(out, name+": 字符串字面量出现 client_secret")
				}
			case *ast.Ident:
				if strings.Contains(strings.ToLower(v.Name), "clientsecret") {
					out = append(out, name+": 标识符 "+v.Name)
				}
			}
			return true
		})
	}
	return out, nil
}

func TestGitHubClientHasNoClientSecret(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "infrastructure", "github")
	found, err := collectClientSecretUsage(dir)
	if err != nil {
		t.Fatalf("扫描 infrastructure/github 失败: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("Device Flow 换 token 不需要 client_secret（§7 R1），不得引入该凭据面:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestGitHubGuardDetectsClientSecret(t *testing.T) {
	tmp := t.TempDir()
	src := "package github\n\nvar ClientSecret = \"client_secret\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectClientSecretUsage(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 2 {
		t.Fatalf("注入的标识符与字符串字面量都应被检出，实际 %v", found)
	}
}
