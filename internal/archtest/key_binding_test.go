package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// P0-18 静态守卫：把 §4.6「绑定服务端持有的公钥指纹、不接受客户端公钥文本」
// 与「解绑后撤销会话、软吊销不物理删除」变成会失败的测试。
//
//  1. BindRequest 的 Fingerprint / PublicKey 只能来自 sess 的认证阶段产物；
//  2. sess.PublicKey() 在 handler 里只允许出现一次（绝不写回终端）；
//  3. 解绑必须是软吊销（SET revoked_at），禁止 DELETE FROM ssh_keys；
//  4. ssh_keys DDL 必须有 fingerprint UNIQUE / revoked_at / public_key NOT NULL；
//  5. SessionRevoker 的调用点必须在写事务闭包之外（失败回滚时不能提前踢人）；
//  6. /key 命令的 MaxArgs 必须锁在 2，子命令语义由 auth.ParseKeyCommand 收口。

func parseGoFilesIn(dir string) ([]*ast.File, []*token.FileSet, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []*ast.File
	var fsets []*token.FileSet
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
		fsets = append(fsets, fset)
	}
	return files, fsets, nil
}

// ---------------------------------------------------------------------------
// 1. BindRequest 的指纹 / 公钥文本只能来自服务端 Session
// ---------------------------------------------------------------------------

func isSessCall(e ast.Expr, wantSel string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != wantSel {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "sess"
}

// collectBadBindSources 返回违规描述以及扫描到的 BindRequest 字面量总数。
func collectBadBindSources(dir string) ([]string, int, error) {
	files, _, err := parseGoFilesIn(dir)
	if err != nil {
		return nil, 0, err
	}
	var out []string
	total := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "BindRequest" {
				return true
			}
			total++
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Fingerprint", "PublicKey":
					if !isSessCall(kv.Value, key.Name) {
						out = append(out, "BindRequest."+key.Name+" 不是来自 sess."+key.Name+"()")
					}
				}
			}
			return true
		})
	}
	return out, total, nil
}

func TestBindRequestSourcesAreServerHeld(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "cmd", "server")
	found, total, err := collectBadBindSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("未扫描到 BindRequest 字面量，守卫可能失效")
	}
	if len(found) > 0 {
		t.Fatalf("公钥绑定只接受认证阶段服务端持有的 ssh.PublicKey（§4.6）:\n  %s",
			strings.Join(found, "\n  "))
	}
}

func TestGuardDetectsClientSuppliedPublicKey(t *testing.T) {
	tmp := t.TempDir()
	bad := `package main

func bad(inv Invocation, sess *Session) {
	_ = auth.BindRequest{
		UserID:      1,
		Fingerprint: sess.Fingerprint(),
		PublicKey:   inv.Args[1],
	}
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	found, total, err := collectBadBindSources(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(found) != 1 {
		t.Fatalf("注入的客户端公钥文本应被检出，total=%d found=%v", total, found)
	}
	if err := os.Remove(filepath.Join(tmp, "bad.go")); err != nil {
		t.Fatal(err)
	}
	good := `package main

func good(sess *Session) {
	_ = auth.BindRequest{
		Fingerprint: sess.Fingerprint(),
		PublicKey:   sess.PublicKey(),
	}
}
`
	if err := os.WriteFile(filepath.Join(tmp, "good.go"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	found, _, err = collectBadBindSources(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("服务端 Session 取值不应被判违规: %v", found)
	}
}

// ---------------------------------------------------------------------------
// 2. sess.PublicKey() 不得出现在输出路径
// ---------------------------------------------------------------------------

func countSessPublicKeyCalls(dir string) (int, error) {
	files, _, err := parseGoFilesIn(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "PublicKey" {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			if ok && x.Name == "sess" {
				n++
			}
			return true
		})
	}
	return n, nil
}

func TestPublicKeyTextIsNeverWrittenToTerminal(t *testing.T) {
	root := findModuleRoot(t)
	path := filepath.Join(root, "cmd", "server", "ssh_handler.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "ssh_handler.go"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := countSessPublicKeyCalls(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sess.PublicKey() 只能出现一次（且仅在 BindRequest 里），实际 %d 次；把它写回终端即为渲染旁路", n)
	}
}

func TestGuardDetectsPublicKeyEcho(t *testing.T) {
	tmp := t.TempDir()
	src := `package main

func echo(sess *Session) {
	_ = auth.BindRequest{PublicKey: sess.PublicKey()}
	fmt.Fprint(sess, sess.PublicKey())
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := countSessPublicKeyCalls(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("回显公钥文本应被检出（计数 2），实际 %d", n)
	}
}

// ---------------------------------------------------------------------------
// 3. 解绑必须是软吊销，禁止物理 DELETE
// ---------------------------------------------------------------------------

var hardDeleteRe = regexp.MustCompile(`(?i)DELETE\s+FROM\s+ssh_keys`)

func collectKeyBindingHardDeletes(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, hit := range hardDeleteRe.FindAllString(string(b), -1) {
			out = append(out, name+": "+hit)
		}
	}
	return out, nil
}

func hasSoftRevoke(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return false, err
		}
		if strings.Contains(string(b), "SET revoked_at") {
			return true, nil
		}
	}
	return false, nil
}

func TestUnbindUsesSoftRevokeNoDelete(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "infrastructure", "sqlite")
	found, err := collectKeyBindingHardDeletes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("解绑禁止 DELETE FROM ssh_keys（会让吊销退化成访客放行）: %v", found)
	}
	ok, err := hasSoftRevoke(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("解绑必须以 SET revoked_at 软吊销")
	}
}

func TestGuardDetectsHardDelete(t *testing.T) {
	tmp := t.TempDir()
	src := "package sqlite\n\nconst q = `DELETE FROM ssh_keys WHERE fingerprint = ?`\n"
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := collectKeyBindingHardDeletes(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("注入的物理删除应被检出，实际 %v", found)
	}
	ok, err := hasSoftRevoke(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("只有 DELETE 的源码不应被判定为软吊销")
	}
}

// ---------------------------------------------------------------------------
// 4. ssh_keys DDL 结构约束
// ---------------------------------------------------------------------------

var fingerprintColRe = regexp.MustCompile(`(?im)^\s*fingerprint\b[^\n]*`)

func fingerprintColumnDef(sqlText string) string {
	return strings.TrimSpace(fingerprintColRe.FindString(extractTableBody(sqlText, "ssh_keys")))
}

func sshKeysDDLHasColumns(sqlText string) bool {
	body := extractTableBody(sqlText, "ssh_keys")
	return strings.Contains(body, "revoked_at") &&
		regexp.MustCompile(`(?i)public_key\s+TEXT\s+NOT\s+NULL`).MatchString(body)
}

func TestSSHKeysDDLHasUniqueFingerprintAndRevokedAt(t *testing.T) {
	sqlText := migrationSQL(t)
	def := fingerprintColumnDef(sqlText)
	if def == "" {
		t.Fatal("ssh_keys.fingerprint 列不存在，守卫失效")
	}
	if !strings.Contains(strings.ToLower(def), "unique") {
		t.Fatalf("§4.6 一钥只属一号：fingerprint 必须 UNIQUE，实际定义 %q", def)
	}
	if !sshKeysDDLHasColumns(sqlText) {
		t.Fatal("ssh_keys 必须有 revoked_at 与 public_key TEXT NOT NULL")
	}
}

func TestGuardDetectsMissingFingerprintUnique(t *testing.T) {
	const bad = `
CREATE TABLE ssh_keys (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL,
  fingerprint TEXT NOT NULL,
  public_key TEXT NOT NULL,
  revoked_at INTEGER
);`
	if def := fingerprintColumnDef(bad); strings.Contains(strings.ToLower(def), "unique") {
		t.Fatal("缺少 UNIQUE 时守卫不应误判为有效")
	}
	if !sshKeysDDLHasColumns(bad) {
		t.Fatal("列存在性判定不应误报")
	}
	const good = `
CREATE TABLE ssh_keys (
  id INTEGER PRIMARY KEY,
  fingerprint TEXT NOT NULL UNIQUE,
  public_key TEXT NOT NULL,
  revoked_at INTEGER
);`
	if def := fingerprintColumnDef(good); !strings.Contains(strings.ToLower(def), "unique") {
		t.Fatal("含 UNIQUE 时应通过")
	}
}

// ---------------------------------------------------------------------------
// 5. SessionRevoker 调用点必须在写事务闭包之外
// ---------------------------------------------------------------------------

func collectRevokerOrder(dir string) (writeEnd, revokePos []int, err error) {
	files, fsets, e := parseGoFilesIn(dir)
	if e != nil {
		return nil, nil, e
	}
	for i, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Write":
				writeEnd = append(writeEnd, fsets[i].Position(call.End()).Offset)
			case "RevokeSessions":
				revokePos = append(revokePos, fsets[i].Position(call.Pos()).Offset)
			}
			return true
		})
	}
	return writeEnd, revokePos, nil
}

func TestSessionRevokerIsCalledAfterWriteCommit(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "auth")
	// 只看 key_binding_service.go，避免 LoginService 的 Write 干扰顺序判定。
	tmp := t.TempDir()
	b, err := os.ReadFile(filepath.Join(dir, "key_binding_service.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "key_binding_service.go"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	writeEnds, revokePos, err := collectRevokerOrder(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(writeEnds) == 0 || len(revokePos) == 0 {
		t.Fatalf("应同时扫描到 Write 与 RevokeSessions: write=%v revoke=%v", writeEnds, revokePos)
	}
	lastWrite := 0
	for _, e := range writeEnds {
		if e > lastWrite {
			lastWrite = e
		}
	}
	for _, pos := range revokePos {
		if pos <= lastWrite {
			t.Fatalf("RevokeSessions 必须在写事务闭包之外（提交成功后）调用: revoke=%d lastWriteEnd=%d", pos, lastWrite)
		}
	}
}

func TestGuardDetectsRevokerInsideWrite(t *testing.T) {
	tmp := t.TempDir()
	bad := `package auth

func bad(s *Service) {
	s.store.Write(ctx, func(tx WriteTx) error {
		s.revoker.RevokeSessions(ctx, "fp")
		return nil
	})
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEnds, revokePos, err := collectRevokerOrder(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(writeEnds) == 0 || len(revokePos) == 0 {
		t.Fatalf("应扫描到 Write 与 RevokeSessions: write=%v revoke=%v", writeEnds, revokePos)
	}
	if revokePos[0] > writeEnds[0] {
		t.Fatal("注入的「Revoker 在写事务闭包内」场景未被检出（顺序守卫失效）")
	}
}

// ---------------------------------------------------------------------------
// 6. /key 命令参数上界锁死
// ---------------------------------------------------------------------------

func keyCommandMaxArgs(dir string) (int, bool, error) {
	files, _, err := parseGoFilesIn(dir)
	if err != nil {
		return 0, false, err
	}
	found, seen := -1, false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			isKey := false
			maxArgs := -1
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Name":
					lit, ok := kv.Value.(*ast.BasicLit)
					if ok && lit.Value == `"key"` {
						isKey = true
					}
				case "MaxArgs":
					lit, ok := kv.Value.(*ast.BasicLit)
					if ok {
						switch lit.Value {
						case "0":
							maxArgs = 0
						case "1":
							maxArgs = 1
						case "2":
							maxArgs = 2
						default:
							maxArgs = 99
						}
					}
				}
			}
			if isKey && maxArgs >= 0 {
				found, seen = maxArgs, true
			}
			return true
		})
	}
	return found, seen, nil
}

func TestKeyCommandMaxArgsIsLocked(t *testing.T) {
	root := findModuleRoot(t)
	dir := filepath.Join(root, "internal", "application", "command")
	maxArgs, seen, err := keyCommandMaxArgs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("未扫描到 /key 命令定义，守卫可能失效")
	}
	if maxArgs != 2 {
		t.Fatalf("/key 的 MaxArgs 必须为 2（add 1 参 / rm 2 参），实际 %d；放宽会让公钥文本参数混进命令表", maxArgs)
	}
}

func TestGuardDetectsLooseKeyCommandArity(t *testing.T) {
	tmp := t.TempDir()
	src := `package command

var defs = []Command{
	{Name: "key", Usage: "/key add", Summary: "x", MinArgs: 1, MaxArgs: 9},
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	maxArgs, seen, err := keyCommandMaxArgs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !seen || maxArgs == 2 {
		t.Fatalf("放宽的 MaxArgs 应被检出: seen=%v max=%d", seen, maxArgs)
	}
}
