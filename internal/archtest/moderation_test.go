package archtest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// P2-M1b + ADR-053 内容治理静态守卫。E 类缺陷（语法合法、单测也过、跑起来出事）在这里逼到明面：
//  1. HidePost / HideThread / UnhidePost / UnhideThread 用例入口只经 requireAdmin → CanAdmin 单点谓词；
//     不得直接调 CanAdmin、不得走作者判定 canModify、不得用写路径 checkWrite；
//  2. 审计 AppendOutbox 必须在 Write 闭包内，四个用例各用固定命令名
//     （post/thread hide 与 post/thread unhide）；
//  3. 仓储只 UPDATE status，带各自的幂等条件，绝不物理删 / 级联；
//  4. 生产代码不得出现 DELETE FROM posts / threads（全仓扫描，不只软删函数）。
//
// 每条守卫配负向注入，证明拿掉规则就会报警——静态断言假绿比没有守卫更危险。

// moderationCase 把「用例名 → 审计命令名 / 目标表 / 目标状态 / 幂等 IN 条件」绑在一起，
// 避免隐藏与恢复共用索引时错位（这是最容易写假绿的地方）。
type moderationCase struct {
	name         string
	auditCommand string
	table        string
	targetStatus string
	guard        string
}

// 应用用例（5 条）：Unthread 是 /unthread 的用例入口，审计命令名独立。
var moderationAppCases = []moderationCase{
	{"HidePost", "post hide", "posts", "hidden", "'normal', 'hidden'"},
	{"HideThread", "thread hide", "threads", "hidden", "'active', 'hidden'"},
	{"UnhidePost", "post unhide", "posts", "normal", "'hidden', 'normal'"},
	{"UnhideThread", "thread unhide", "threads", "active", "'hidden', 'active'"},
	{"Unthread", "thread unthread", "threads", "active", "'hidden', 'active'"},
}

// 仓储实现（4 个）：Unthread 复用 UnhideThread 的写端口，没有独立 repo 方法。
var moderationSQLCases = []moderationCase{
	{"HidePost", "post hide", "posts", "hidden", "'normal', 'hidden'"},
	{"HideThread", "thread hide", "threads", "hidden", "'active', 'hidden'"},
	{"UnhidePost", "post unhide", "posts", "normal", "'hidden', 'normal'"},
	{"UnhideThread", "thread unhide", "threads", "active", "'hidden', 'active'"},
}

// 负向注入里的「干净对照」：注入某一处缺陷时，其余用例必须合规，
// 否则「扫描不到 UnhidePost」这类噪音会稀释断言。
const cleanUnhideAppSrc = `func (s *Service) UnhidePost(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post unhide", "post:1", 0, actor, now))
	})
}
func (s *Service) UnhideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread unhide", "thread:1", 0, actor, now))
	})
}
`

const cleanUnthreadAppSrc = `func (s *Service) Unthread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread unthread", "thread:1", 0, actor, now))
	})
}
`

const cleanUnhideSQLSrc = `func (w *sqlWriteTx) UnhidePost(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE posts SET status = 'normal' WHERE id = ? AND status IN ('hidden', 'normal')", id)
	return err
}
func (w *sqlWriteTx) UnhideThread(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE threads SET status = 'active' WHERE id = ? AND status IN ('hidden', 'active')", id)
	return err
}
`

// collectModerationAppIssues 守用例层的权限单点与审计覆盖。
func collectModerationAppIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	for _, tc := range moderationAppCases {
		fn, ok := findFuncDecl(files, tc.name)
		if !ok {
			issues = append(issues, "扫描不到治理用例 "+tc.name)
			continue
		}
		calls := callNames(fn)
		if !calls["requireAdmin"] {
			issues = append(issues, tc.name+" 必须过单点 requireAdmin 谓词")
		}
		for _, forbidden := range []string{"CanAdmin", "canModify", "checkWrite", "Publish"} {
			if calls[forbidden] {
				issues = append(issues, tc.name+" 不得直接调用 "+forbidden+"（权限/投递只有单点入口）")
			}
		}
		if !calls["Write"] || !calls["AppendOutbox"] {
			issues = append(issues, tc.name+" 必须在 Write 闭包内 AppendOutbox 审计事件")
		}
		if !strings.Contains(strings.Join(stringLiterals(fn), " "), tc.auditCommand) {
			issues = append(issues, tc.name+" 审计命令名必须是固定字面量 "+tc.auditCommand)
		}
	}
	return issues, nil
}

// collectModerationSQLIssues 守仓储只改 status、幂等、不硬删。
func collectModerationSQLIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	for _, tc := range moderationSQLCases {
		fn, ok := findFuncDecl(files, tc.name)
		if !ok {
			issues = append(issues, "扫描不到仓储实现 "+tc.name)
			continue
		}
		lits := strings.Join(stringLiterals(fn), " ")
		sql := compact(lits)
		wantUpdate := "UPDATE" + tc.table + "SETstatus='" + tc.targetStatus + "'"
		if !strings.Contains(sql, wantUpdate) {
			issues = append(issues, tc.name+" 必须执行 UPDATE "+tc.table+" SET status = '"+tc.targetStatus+"'")
		}
		if !strings.Contains(sql, compact(tc.guard)) {
			issues = append(issues, tc.name+" 缺少幂等条件 status IN "+tc.guard)
		}
		if strings.Contains(strings.ToUpper(lits), "DELETE FROM") {
			issues = append(issues, tc.name+" 不得物理删（hidden / unhide 都是软状态迁移）")
		}
	}
	return issues, nil
}

// collectHardDeleteIssues 全仓扫描生产 Go 代码，堵住任何绕过软删的硬删。
// posts_fts_indexed 等影子表不受此限：\b 确保 posts/threads 后不能接下划线。
var hardDeletePattern = regexp.MustCompile(`(?i)DELETE\s+FROM\s+(posts|threads)\b`)

func collectHardDeleteIssues(root string) ([]string, error) {
	var issues []string
	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue // 负向注入的临时目录可能只有其中一个子目录
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if hardDeletePattern.Match(body) {
				issues = append(issues, "生产代码不得物理删帖子/主题: "+path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return issues, nil
}

func TestModerationGuards(t *testing.T) {
	root := findModuleRoot(t)
	app, err := collectModerationAppIssues(filepath.Join(root, "internal", "application", "forum"))
	assertNoIssues(t, app, err, "治理用例单点谓词与审计守卫")
	sql, err := collectModerationSQLIssues(filepath.Join(root, "internal", "infrastructure", "sqlite"))
	assertNoIssues(t, sql, err, "hidden 软删 SQL 守卫")
	del, err := collectHardDeleteIssues(root)
	assertNoIssues(t, del, err, "禁止物理删帖子/主题守卫")

	// 复用 outbox_test.go 的时序守卫：AppendOutbox 必须在 Write 内、Publish 不得在写事务内。
	// 这里不重复实现，只复核覆盖（命令层与治理路径都在 forum 目录下）。
	placement, err := collectOutboxPlacementIssues(filepath.Join(root, "internal", "application", "forum"))
	assertNoIssues(t, placement, err, "治理审计 outbox 时序守卫")
}

// ---- 负向注入 ----

func TestGuardDetectsModerationSkippingRequireAdmin(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) HidePost(ctx context.Context, actor Actor, id int64) error {
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post hide", "post:1", 0, actor, now))
	})
}
func (s *Service) HideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread hide", "thread:1", 0, actor, now))
	})
}
` + cleanUnhideAppSrc + cleanUnthreadAppSrc
	if err := os.WriteFile(filepath.Join(dir, "moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(issues, "\n"), "HidePost 必须过单点 requireAdmin") {
		t.Fatalf("注入跳过 requireAdmin 的用例后未检出: %v", issues)
	}
}

func TestGuardDetectsDirectCanAdminInModeration(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) HidePost(ctx context.Context, actor Actor, id int64) error {
	if !admin.CanAdmin(actor.Fingerprint, s.adminWhitelist) { return ErrAdminDenied }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post hide", "post:1", 0, actor, now))
	})
}
func (s *Service) HideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread hide", "thread:1", 0, actor, now))
	})
}
` + cleanUnhideAppSrc + cleanUnthreadAppSrc
	if err := os.WriteFile(filepath.Join(dir, "moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "HidePost 不得直接调用 CanAdmin") ||
		!strings.Contains(joined, "HidePost 必须过单点 requireAdmin") {
		t.Fatalf("注入第二套权限判定后未检出: %v", issues)
	}
}

func TestGuardDetectsModerationAuditOutsideWrite(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) HidePost(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	ev := adminActionEvent("post hide", "post:1", 0, actor, now)
	if err := s.repos.Write(ctx, func(tx WriteTx) error { return nil }); err != nil { return err }
	return s.outbox.store.AppendOutbox(ctx, ev)
}
func (s *Service) HideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread hide", "thread:1", 0, actor, now))
	})
}
` + cleanUnhideAppSrc + cleanUnthreadAppSrc
	if err := os.WriteFile(filepath.Join(dir, "moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(issues, "\n"), "必须过单点 requireAdmin") {
		t.Fatalf("注入的越权用例应通过 requireAdmin 守卫: %v", issues)
	}
	// 用例级守卫只看「有没有 AppendOutbox」；位置由 outbox 时序守卫（AST 偏移）判定。
	placement, err := collectOutboxPlacementIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(placement, "\n"), "AppendOutbox 必须出现在 Write 闭包内") {
		t.Fatalf("注入写事务外审计后时序守卫未检出: %v", placement)
	}
}

func TestGuardDetectsHardDeleteOfPostsOrThreads(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"internal", "cmd"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	bad := `package sqlite
func bad(ctx context.Context) {
	tx.ExecContext(ctx, "DELETE FROM posts WHERE id = ?", id)
	tx.ExecContext(ctx, "DELETE FROM threads WHERE id = ?", id)
}
`
	if err := os.WriteFile(filepath.Join(root, "internal", "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectHardDeleteIssues(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("注入硬删未检出（或重复报）: %v", issues)
	}
	// FTS 影子表不在禁止范围内，守卫不能误报。
	if err := os.WriteFile(filepath.Join(root, "internal", "bad.go"),
		[]byte("package sqlite\nfunc ok() { tx.Exec(\"DELETE FROM posts_fts_indexed\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err = collectHardDeleteIssues(root)
	if err != nil || len(issues) != 0 {
		t.Fatalf("FTS 影子表被误报: issues=%v err=%v", issues, err)
	}
}

func TestGuardDetectsBadModerationSQL(t *testing.T) {
	dir := t.TempDir()
	src := `package sqlite
func (w *sqlWriteTx) HidePost(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "DELETE FROM posts WHERE id = ?", id)
	return err
}
func (w *sqlWriteTx) HideThread(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE threads SET status = 'deleted' WHERE id = ?", id)
	return err
}
` + cleanUnhideSQLSrc
	if err := os.WriteFile(filepath.Join(dir, "repo_moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationSQLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{
		"HidePost 必须执行 UPDATE posts SET status = 'hidden'",
		"HidePost 不得物理删",
		"HideThread 必须执行 UPDATE threads SET status = 'hidden'",
		"HideThread 缺少幂等条件",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("注入的违规 SQL 未检出 %q:\n%s", want, joined)
		}
	}
}

// ADR-053 新增守卫的负向注入：unhide 若绕过 requireAdmin，必须报警。
func TestGuardDetectsUnhideSkippingRequireAdmin(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) HidePost(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post hide", "post:1", 0, actor, now))
	})
}
func (s *Service) HideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread hide", "thread:1", 0, actor, now))
	})
}
func (s *Service) UnhidePost(ctx context.Context, actor Actor, id int64) error {
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post unhide", "post:1", 0, actor, now))
	})
}
func (s *Service) UnhideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread unhide", "thread:1", 0, actor, now))
	})
}
` + cleanUnthreadAppSrc
	if err := os.WriteFile(filepath.Join(dir, "moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "UnhidePost 必须过单点 requireAdmin") {
		t.Fatalf("注入 unhide 越权后未检出: %v", issues)
	}
	if strings.Contains(joined, "UnhideThread 必须过单点 requireAdmin") {
		t.Fatalf("合规的 UnhideThread 被误报: %v", issues)
	}
}

// ADR-053 新增守卫的负向注入：unhide 若硬删或写错目标状态，必须报警。
func TestGuardDetectsBadUnhideSQL(t *testing.T) {
	dir := t.TempDir()
	src := `package sqlite
func (w *sqlWriteTx) HidePost(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE posts SET status = 'hidden' WHERE id = ? AND status IN ('normal', 'hidden')", id)
	return err
}
func (w *sqlWriteTx) HideThread(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE threads SET status = 'hidden' WHERE id = ? AND status IN ('active', 'hidden')", id)
	return err
}
func (w *sqlWriteTx) UnhidePost(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE posts SET status = 'normal' WHERE id = ? AND status IN ('hidden', 'normal')", id)
	return err
}
func (w *sqlWriteTx) UnhideThread(ctx context.Context, id int64) error {
	_, err := w.tx.ExecContext(ctx, "DELETE FROM threads WHERE id = ?", id)
	return err
}
`
	if err := os.WriteFile(filepath.Join(dir, "repo_moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationSQLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{
		"UnhideThread 必须执行 UPDATE threads SET status = 'active'",
		"UnhideThread 缺少幂等条件 status IN 'hidden', 'active'",
		"UnhideThread 不得物理删",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("注入的违规 unhide SQL 未检出 %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "UnhidePost 必须执行") || strings.Contains(joined, "扫描不到") {
		t.Fatalf("合规的 UnhidePost 被误报: %v", issues)
	}
}

// ADR-053 补丁守卫：/unthread 的用例入口若绕过 requireAdmin，必须报警。
func TestGuardDetectsUnthreadSkippingRequireAdmin(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) HidePost(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("post hide", "post:1", 0, actor, now))
	})
}
func (s *Service) HideThread(ctx context.Context, actor Actor, id int64) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread hide", "thread:1", 0, actor, now))
	})
}
` + cleanUnhideAppSrc + `func (s *Service) Unthread(ctx context.Context, actor Actor, id int64) error {
	return s.repos.Write(ctx, func(tx WriteTx) error {
		return tx.AppendOutbox(ctx, adminActionEvent("thread unthread", "thread:1", 0, actor, now))
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "moderation.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectModerationAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "Unthread 必须过单点 requireAdmin") {
		t.Fatalf("注入 unthread 越权后未检出: %v", issues)
	}
	for _, clean := range []string{"HidePost", "UnhidePost", "UnhideThread"} {
		if strings.Contains(joined, clean+" 必须过单点 requireAdmin") {
			t.Fatalf("合规的 %s 被误报: %v", clean, issues)
		}
	}
}
