package archtest

import (
	"go/ast"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// P2-M1 节点治理静态守卫。五条不变量，每条都配负向注入：
//  1. 四个 /node 命令的用例入口都过同一个 requireAdmin → CanAdmin 谓词；
//  2. cmd 层不自行判定权限，指纹只经 Principal.KeyFingerprint 进入 Actor；
//  3. seed slug 的「可归档不可改名」守卫在 RenameNode 内真实存在；
//  4. ArchiveNode 只 UPDATE nodes，绝不触碰 threads；
//  5. 三个变更用例各自在 Write 闭包内 AppendOutbox（outbox_test.go 守时序，这里守覆盖）。
//
// M0 的「禁止 DELETE FROM nodes」守卫由 node_invariants_test.go 覆盖，
// 本文件在主守卫里调用它复核，不重复实现。

var nodeAdminUseCases = []string{"ListAdminNodes", "CreateNode", "RenameNode", "ArchiveNode"}
var nodeAdminWriteCases = []string{"CreateNode", "RenameNode", "ArchiveNode"}

func textOfFunc(fn *ast.FuncDecl) string {
	var b strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok {
			b.WriteString(lit.Value)
		}
		return true
	})
	return b.String()
}

// collectAdminPredicateIssues 守「判定只有一处」：用例入口 → requireAdmin → CanAdmin。
func collectAdminPredicateIssues(root string) ([]string, error) {
	forumDir := filepath.Join(root, "internal", "application", "forum")
	files, err := parseNamedGoFilesIn(forumDir)
	if err != nil {
		return nil, err
	}
	var issues []string
	for _, name := range nodeAdminUseCases {
		fn, ok := findFuncDecl(files, name)
		if !ok {
			issues = append(issues, "扫描不到治理用例 "+name)
			continue
		}
		if !callNames(fn)["requireAdmin"] {
			issues = append(issues, name+" 必须过单点 requireAdmin 谓词")
		}
	}
	requireFn, ok := findFuncDecl(files, "requireAdmin")
	if !ok {
		issues = append(issues, "扫描不到 requireAdmin 单点谓词")
	} else {
		calls := callNames(requireFn)
		if !calls["CanAdmin"] {
			issues = append(issues, "requireAdmin 必须先调 CanAdmin 单点谓词")
		}
		// 治理权与账号状态解耦：requireAdmin 里一旦出现 checkWrite，
		// 白名单内的访客（UserID=0）就会被挡在门外，正是被修掉的鸡生蛋 bug。
		if calls["checkWrite"] {
			issues = append(issues, "requireAdmin 不得调用 checkWrite（访客管理员必须能 bootstrap）")
		}
	}

	adminFiles, err := parseNamedGoFilesIn(filepath.Join(root, "internal", "application", "admin"))
	if err != nil {
		return nil, err
	}
	canAdminCount := 0
	for _, nf := range adminFiles {
		for _, d := range nf.file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "CanAdmin" {
				canAdminCount++
			}
		}
	}
	if canAdminCount != 1 {
		issues = append(issues, "CanAdmin 必须全项目唯一，实际找到 "+strconv.Itoa(canAdminCount)+" 个")
	}

	// cmd 层只传会话身份，不自己写权限判断。
	cmdRoot := filepath.Join(root, "cmd")
	if _, statErr := os.Stat(cmdRoot); statErr == nil {
		err = filepath.Walk(cmdRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), "CanAdmin(") {
				issues = append(issues, "cmd 层不得自行判定权限（应统一由 application 用例过 CanAdmin）: "+path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// 指纹只能从 Principal 映射进 Actor，不能从命令参数来。
	actorFn, ok := findFuncDecl(files, "ActorFromPrincipal")
	if !ok {
		issues = append(issues, "扫描不到 ActorFromPrincipal，会话指纹来源守卫失效")
	} else {
		found := false
		ast.Inspect(actorFn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "KeyFingerprint" {
				found = true
			}
			return true
		})
		if !found {
			issues = append(issues, "ActorFromPrincipal 必须携带 Principal.KeyFingerprint（服务端指纹）")
		}
	}
	return issues, nil
}

// collectSeedRenameGuardIssues 守「seed 可归档不可改名」的代码层实现。
func collectSeedRenameGuardIssues(forumDir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(forumDir)
	if err != nil {
		return nil, err
	}
	var issues []string
	seedFn, ok := findFuncDecl(files, "isSeedNodeSlug")
	if !ok {
		return append(issues, "扫描不到 isSeedNodeSlug 守卫"), nil
	}
	_ = seedFn
	// seed 常量在包级 map 里，不在函数体内；扫该函数所在文件的全部字符串字面量。
	var literals strings.Builder
	for _, nf := range files {
		for _, decl := range nf.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "isSeedNodeSlug" {
				for _, lit := range stringLiterals(fn) {
					literals.WriteString(lit)
					literals.WriteByte('\n')
				}
				body, err := os.ReadFile(nf.fset.File(nf.file.Pos()).Name())
				if err != nil {
					return nil, err
				}
				literals.Write(body)
			}
		}
	}
	for _, slug := range expectedSeedSlugs {
		if !strings.Contains(literals.String(), slug) {
			issues = append(issues, "seed slug 守卫缺失: "+slug)
		}
	}
	renameFn, ok := findFuncDecl(files, "RenameNode")
	if !ok {
		return append(issues, "扫描不到 RenameNode 用例"), nil
	}
	calls := callNames(renameFn)
	if !calls["isSeedNodeSlug"] {
		issues = append(issues, "RenameNode 必须调用 isSeedNodeSlug 拒绝 seed 改名")
	}
	if !strings.Contains(textOfFunc(renameFn), "seedNodeSlugs") && !calls["isSeedNodeSlug"] {
		issues = append(issues, "RenameNode 缺少 seed 拒绝分支")
	}
	return issues, nil
}

// collectNodeArchiveSQLIssues 守归档只改 nodes.status，threads 一行不动。
func collectNodeArchiveSQLIssues(infraDir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(infraDir)
	if err != nil {
		return nil, err
	}
	fn, ok := findFuncDecl(files, "ArchiveNode")
	if !ok {
		return []string{"扫描不到仓储 ArchiveNode 实现"}, nil
	}
	var issues []string
	hasUpdate := false
	for _, lit := range stringLiterals(fn) {
		clean := compact(lit)
		if strings.Contains(clean, "UPDATEnodesSETstatus='archived'") {
			hasUpdate = true
		}
		if strings.Contains(strings.ToLower(clean), "threads") || strings.Contains(strings.ToUpper(clean), "DELETE") {
			issues = append(issues, "归档 SQL 不得触碰 threads，也不得物理删: "+lit)
		}
	}
	if !hasUpdate {
		issues = append(issues, "ArchiveNode 必须执行 UPDATE nodes SET status='archived'")
	}
	return issues, nil
}

// collectNodeAuditOutboxIssues 守三个写用例各自把审计事件塞进写事务。
func collectNodeAuditOutboxIssues(forumDir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(forumDir)
	if err != nil {
		return nil, err
	}
	var issues []string
	for _, name := range nodeAdminWriteCases {
		fn, ok := findFuncDecl(files, name)
		if !ok {
			issues = append(issues, "扫描不到写用例 "+name)
			continue
		}
		calls := callNames(fn)
		if !calls["Write"] || !calls["AppendOutbox"] {
			issues = append(issues, name+" 必须在 Write 闭包内 AppendOutbox 审计事件")
		}
		if calls["Publish"] {
			issues = append(issues, name+" 不得直接 Publish（只能在提交后由 Outbox 投递）")
		}
	}
	return issues, nil
}

func TestNodeAdminGuards(t *testing.T) {
	root := findModuleRoot(t)
	forumDir := filepath.Join(root, "internal", "application", "forum")

	predicate, err := collectAdminPredicateIssues(root)
	assertNoIssues(t, predicate, err, "单点谓词守卫")
	seed, err := collectSeedRenameGuardIssues(forumDir)
	assertNoIssues(t, seed, err, "seed 不可改名守卫")
	archive, err := collectNodeArchiveSQLIssues(filepath.Join(root, "internal", "infrastructure", "sqlite"))
	assertNoIssues(t, archive, err, "归档不动 threads 守卫")
	audit, err := collectNodeAuditOutboxIssues(forumDir)
	assertNoIssues(t, audit, err, "治理审计 outbox 覆盖守卫")
	// M0 守卫复核：生产代码不得物理删节点（不重复实现）。
	del, err := collectNodeDeleteIssues(root)
	assertNoIssues(t, del, err, "禁止物理删节点守卫")
}

// ---- 负向注入：证明每条守卫拿掉规则就会报警 ----

func TestGuardDetectsMissingRequireAdmin(t *testing.T) {
	root := t.TempDir()
	writeSyntheticNodeAdmin(t, root, `package forum
func (s *Service) RenameNode(slug, name string) error {
	return s.repos.Write(ctx, func(tx WriteTx) error { return nil })
}
`)
	issues, err := collectAdminPredicateIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "RenameNode 必须过单点 requireAdmin") {
		t.Fatalf("注入跳过 requireAdmin 后未检出: %v", issues)
	}
}

func TestGuardDetectsCheckWriteInRequireAdmin(t *testing.T) {
	cases := map[string]string{
		"旧写法：先 checkWrite": `package forum
func (s *Service) requireAdmin(actor Actor) error {
	if err := actor.checkWrite(); err != nil { return ErrAdminDenied }
	if !admin.CanAdmin(actor.Fingerprint, s.adminWhitelist) { return ErrAdminDenied }
	return nil
}
`,
		"checkWrite 放在 CanAdmin 之后": `package forum
func (s *Service) requireAdmin(actor Actor) error {
	if !admin.CanAdmin(actor.Fingerprint, s.adminWhitelist) { return ErrAdminDenied }
	return actor.checkWrite()
}
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeSyntheticNodeAdmin(t, root, src)
			issues, err := collectAdminPredicateIssues(root)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(issues, "\n"), "requireAdmin 不得调用 checkWrite") {
				t.Fatalf("注入 checkWrite 后未检出: %v", issues)
			}
		})
	}
}

func TestGuardDetectsCmdSideAdminDecision(t *testing.T) {
	root := t.TempDir()
	writeSyntheticNodeAdmin(t, root, "package forum\n")
	cmdDir := filepath.Join(root, "cmd")
	if err := os.MkdirAll(cmdDir, 0o750); err != nil {
		t.Fatal(err)
	}
	src := `package main
func bad(fp string) bool { return admin.CanAdmin(fp, whitelist) }
`
	if err := os.WriteFile(filepath.Join(cmdDir, "bad.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectAdminPredicateIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "cmd 层不得自行判定权限") {
		t.Fatalf("注入 cmd 层权限判定后未检出: %v", issues)
	}
}

func TestGuardDetectsMissingSeedRenameGuard(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func isSeedNodeSlug(slug string) bool {
	_, ok := seedNodeSlugs[slug]
	return ok
}
func (s *Service) RenameNode(slug, name string) error { return nil }
`
	if err := os.WriteFile(filepath.Join(dir, "node_admin.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectSeedRenameGuardIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "isSeedNodeSlug 拒绝 seed 改名") {
		t.Fatalf("注入无 seed 守卫的 RenameNode 后未检出: %v", issues)
	}
}

func TestGuardDetectsMissingSeedSlug(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func isSeedNodeSlug(slug string) bool { return slug == "/dev/ai" }
func (s *Service) RenameNode(slug, name string) error {
	if isSeedNodeSlug(slug) { return ErrSeedNodeRename }
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "node_admin.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectSeedRenameGuardIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "/bin/help") {
		t.Fatalf("删掉 seed slug 后未检出: %v", issues)
	}
}

func TestGuardDetectsArchiveTouchingThreads(t *testing.T) {
	dir := t.TempDir()
	src := `package sqlite
func (w *sqlWriteTx) ArchiveNode(id int64) error {
	_, err := w.tx.Exec("UPDATE threads SET status = 'archived' WHERE node_id = ?", id)
	return err
}
`
	if err := os.WriteFile(filepath.Join(dir, "repo_node_admin.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeArchiveSQLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "UPDATE nodes SET status='archived'") || !strings.Contains(joined, "threads") {
		t.Fatalf("注入改 threads 的归档实现后未检出: %v", issues)
	}
}

func TestGuardDetectsMissingAuditOutbox(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) CreateNode() error {
	return s.repos.Write(ctx, func(tx WriteTx) error { return tx.AppendOutbox(ctx, ev) })
}
func (s *Service) RenameNode() error {
	return s.repos.Write(ctx, func(tx WriteTx) error { return nil }) // 故意漏掉审计
}
func (s *Service) ArchiveNode() error {
	return s.repos.Write(ctx, func(tx WriteTx) error { return tx.AppendOutbox(ctx, ev) })
}
`
	if err := os.WriteFile(filepath.Join(dir, "node_admin.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectNodeAuditOutboxIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "RenameNode 必须在 Write 闭包内 AppendOutbox") {
		t.Fatalf("注入缺失审计后未检出: %v", issues)
	}
}

// writeSyntheticNodeAdmin 给负向用例造一个最小可扫的临时模块骨架：
// forum 里放被注入的文件，admin 里放唯一的 CanAdmin，避免扫描器因路径缺失而假失败。
func writeSyntheticNodeAdmin(t *testing.T, root, src string) {
	t.Helper()
	forumDir := filepath.Join(root, "internal", "application", "forum")
	adminDir := filepath.Join(root, "internal", "application", "admin")
	if err := os.MkdirAll(forumDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(adminDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(forumDir, "node_admin.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "can_admin.go"),
		[]byte("package admin\nfunc CanAdmin() bool { return true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
