package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// P2-M3 / ADR-052 静态守卫：回复数是展示投影，只能挂在只读端口上。
// 若有人把 ReplyCountsByThreadIDs 塞进 WriteTx，就会把一次列表读卷进写事务边界，
// 违背「写事务稀缺」约束；这里用 AST 断言它在 ReplyCountReader 里、被 Repos 嵌入，
// 且不在 WriteTx 中。配负向注入，防止断言恒绿。

func parseInterfaces(path string) (map[string]*ast.InterfaceType, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*ast.InterfaceType)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if iface, ok := ts.Type.(*ast.InterfaceType); ok {
				out[ts.Name.Name] = iface
			}
		}
	}
	return out, nil
}

func interfaceMethodNames(iface *ast.InterfaceType) map[string]bool {
	out := make(map[string]bool)
	if iface == nil || iface.Methods == nil {
		return out
	}
	for _, m := range iface.Methods.List {
		if _, ok := m.Type.(*ast.FuncType); !ok {
			continue
		}
		for _, name := range m.Names {
			out[name.Name] = true
		}
	}
	return out
}

func interfaceEmbeds(iface *ast.InterfaceType, name string) bool {
	if iface == nil || iface.Methods == nil {
		return false
	}
	for _, m := range iface.Methods.List {
		if id, ok := m.Type.(*ast.Ident); ok && id.Name == name {
			return true
		}
	}
	return false
}

// checkReplyCountPort 返回违规说明；无违规返回空字符串。供正向与负向注入共用。
func checkReplyCountPort(path string) (string, error) {
	ifaces, err := parseInterfaces(path)
	if err != nil {
		return "", err
	}
	const method = "ReplyCountsByThreadIDs"
	if !interfaceMethodNames(ifaces["ReplyCountReader"])[method] {
		return "ReplyCountReader 应声明 " + method, nil
	}
	if !interfaceEmbeds(ifaces["Repos"], "ReplyCountReader") {
		return "Repos 应嵌入 ReplyCountReader", nil
	}
	if interfaceMethodNames(ifaces["WriteTx"])[method] {
		return "ReplyCountsByThreadIDs 不得进入 WriteTx（只读投影不得卷入写事务）", nil
	}
	return "", nil
}

func TestReplyCountPortStaysOnReadSide(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "internal", "application", "forum", "ports.go")
	msg, err := checkReplyCountPort(path)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "" {
		t.Fatal(msg)
	}
}

// 负向注入：把方法塞进 WriteTx，守卫必须报错，避免静态断言假绿。
func TestReplyCountPortGuardDetectsWriteTxInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.go")
	src := `package forum
import "context"
type ReplyCountReader interface { ReplyCountsByThreadIDs(context.Context, []int64) (map[int64]int, error) }
type Repos interface { ReplyCountReader }
type WriteTx interface { ReplyCountsByThreadIDs(context.Context, []int64) (map[int64]int, error) }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := checkReplyCountPort(path); err != nil {
		t.Fatal(err)
	} else if msg == "" {
		t.Fatal("注入 WriteTx.ReplyCountsByThreadIDs 后守卫未报违规")
	}
}
