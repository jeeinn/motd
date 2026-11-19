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

// 邮箱认证的两条机械守卫（§2.9）：
//  1. 发信（Sender.Send）绝不出现在写事务闭包内 —— 3s 写超时扛不住 SMTP；
//  2. email_tokens 表 / INSERT 只存 code_hash，绝不出现明文 code 列。
//
// 两条都带负向注入用例，证明守卫本身不是空转。

// sendInWriteTxViolation 描述一处违规：Send 调用落在了传给 Write/Submit 的闭包里。
type sendInWriteTxViolation struct {
	file string
	line int
}

// checkSendOutsideWriteTx 对一个 .go 源文件做 AST 扫描。
// 判定：找到 selector 为 Write/Submit 的 CallExpr，其参数里的 FuncLit 记为一个
// 「事务体区间」；若任何 selector 为 Send 的 CallExpr 落在该区间内 → 违规。
func checkSendOutsideWriteTx(path string) ([]sendInWriteTxViolation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var txBodies []token.Position
	var sends []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "Write", "Submit":
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.FuncLit); ok {
						txBodies = append(txBodies, fset.Position(lit.Body.Lbrace), fset.Position(lit.Body.Rbrace))
					}
				}
			case "Send":
				sends = append(sends, fset.Position(call.Pos()))
			}
		}
		return true
	})

	var out []sendInWriteTxViolation
	for _, send := range sends {
		for i := 0; i+1 < len(txBodies); i += 2 {
			open, close := txBodies[i], txBodies[i+1]
			if open.Filename == send.Filename && send.Offset >= open.Offset && send.Offset <= close.Offset {
				out = append(out, sendInWriteTxViolation{file: send.Filename, line: send.Line})
			}
		}
	}
	return out, nil
}

func goFilesIn(root, rel string) ([]string, error) {
	dir := filepath.Join(root, rel)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func TestEmailSendIsNeverInsideWriteTransaction(t *testing.T) {
	root := findModuleRoot(t)
	files, err := goFilesIn(root, filepath.Join("internal", "application", "auth"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("未扫描到 auth 源文件，守卫可能漏扫")
	}
	var total int
	for _, path := range files {
		vs, err := checkSendOutsideWriteTx(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range vs {
			total++
			t.Errorf("发信在写事务闭包内: %s:%d", v.file, v.line)
		}
	}
	// 该守卫必须真的扫到 Send 调用，否则「零违规」是假绿。
	foundSend := false
	for _, path := range files {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), ".Send(") {
			foundSend = true
		}
	}
	if !foundSend {
		t.Fatal("未发现任何 .Send( 调用，守卫可能失效")
	}
	if total > 0 {
		t.Fatalf("共 %d 处违规", total)
	}
}

func TestSendGuardDetectsInjectedViolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.go")
	src := `package bad

func run(store Store, sender Sender) error {
	return store.Write(ctx, func(tx Tx) error {
		return sender.Send(ctx, "a@b.com", "subject", "body")
	})
}
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	vs, err := checkSendOutsideWriteTx(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("注入的 Send-in-Write 必须被检出 1 处，实际 %d", len(vs))
	}
}

// ---- code_hash only ----

var emailTokensInsertRE = regexp.MustCompile(`(?is)INSERT\s+INTO\s+email_tokens\b.*?\(([^)]*)\)`)
var plaintextCodeColumnRE = regexp.MustCompile(`(?i)(^|[\s,])(code|plain_code|code_plain|verify_code)([\s,]|$)`)

// checkEmailTokenSQL 扫描一段源码里的 SQL 字面量：凡是 INSERT INTO email_tokens
// 的列清单里出现裸 code 列（非 code_hash）即违规；同时必须能看到 code_hash。
func checkEmailTokenSQL(src string) (plainCol bool, hasHash bool) {
	for _, m := range emailTokensInsertRE.FindAllStringSubmatch(src, -1) {
		if len(m) < 2 {
			continue
		}
		cols := m[1]
		if strings.Contains(strings.ToLower(cols), "code_hash") {
			hasHash = true
		}
		if plaintextCodeColumnRE.MatchString(cols) {
			plainCol = true
		}
	}
	return
}

func TestEmailTokensStoreHashOnly(t *testing.T) {
	root := findModuleRoot(t)
	files, err := goFilesIn(root, filepath.Join("internal", "infrastructure", "sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	foundInsert := false
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "INSERT INTO email_tokens") && !strings.Contains(string(b), "INSERT INTO email_tokens") {
			continue
		}
		foundInsert = true
		plain, hash := checkEmailTokenSQL(string(b))
		if plain {
			t.Errorf("%s 的 email_tokens INSERT 含明文 code 列", path)
		}
		if !hash {
			t.Errorf("%s 的 email_tokens INSERT 未见 code_hash 列", path)
		}
	}
	if !foundInsert {
		t.Fatal("未发现 email_tokens INSERT，守卫可能漏扫")
	}

	// DDL 本身也不许有裸 code 列。
	ddl, err := os.ReadFile(filepath.Join(root, "internal", "infrastructure", "sqlite", "migrations", "0001_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	idx := strings.Index(string(ddl), "CREATE TABLE email_tokens")
	if idx < 0 {
		t.Fatal("迁移中找不到 email_tokens 建表语句")
	}
	block := string(ddl)[idx:]
	if end := strings.Index(block, "CREATE INDEX"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "code_hash") || plaintextCodeColumnRE.MatchString(block) {
		t.Fatal("email_tokens DDL 必须只有 code_hash，无明文 code 列")
	}
}

func TestCodeHashGuardDetectsInjectedPlainColumn(t *testing.T) {
	bad := `INSERT INTO email_tokens (email, code, purpose, expires_at) VALUES (?,?,?,?)`
	plain, hash := checkEmailTokenSQL(bad)
	if !plain || hash {
		t.Fatalf("注入的明文 code 列必须被检出: plain=%v hash=%v", plain, hash)
	}
	good := `INSERT INTO email_tokens (email, code_hash, purpose, expires_at) VALUES (?,?,?,?)`
	plain, hash = checkEmailTokenSQL(good)
	if plain || !hash {
		t.Fatalf("code_hash 列必须被接受: plain=%v hash=%v", plain, hash)
	}
}
