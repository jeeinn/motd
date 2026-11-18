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

// collectAuthPublicKeyParsing 扫描 application/auth 目录，找出「业务层自己
// 解析或计算公钥」的痕迹（§4.6）：指纹必须由 transport/ssh 在认证阶段从
// 服务端持有的 ssh.PublicKey 算出，业务层不得从客户端提交的文本反推。
//
// 抽成纯函数，让守卫本身可被负向注入测试覆盖。
func collectAuthPublicKeyParsing(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
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
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "golang.org/x/crypto/ssh" {
				out = append(out, name+": import golang.org/x/crypto/ssh")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "ParseAuthorizedKey", "MarshalAuthorizedKey", "FingerprintSHA256":
				out = append(out, name+": 出现 "+id.Name)
			}
			return true
		})
	}
	return out, nil
}

func TestAuthLayerDoesNotParsePublicKeys(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "auth")
	found, err := collectAuthPublicKeyParsing(dir)
	if err != nil {
		t.Fatalf("扫描 application/auth 失败: %v", err)
	}
	if len(found) > 0 {
		t.Fatalf("认证层不得自行解析 / 计算公钥（§4.6）:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestAuthGuardDetectsPublicKeyParsing(t *testing.T) {
	tmp := t.TempDir()
	src := "package auth\n\nimport gossh \"golang.org/x/crypto/ssh\"\n\nvar _ = gossh.ParseAuthorizedKey\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectAuthPublicKeyParsing(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 2 {
		t.Fatalf("注入的 SSH 库 import 与公钥解析调用都应被检出，实际 %v", found)
	}
}
