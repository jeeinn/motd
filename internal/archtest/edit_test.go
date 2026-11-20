package archtest

// P2-M2 编辑路径静态守卫（ADR-051）。E 类缺陷（语法合法、单测也过、跑起来出事）在这里逼到明面：
//  1. EditPost / CanEditPost 用例入口只经 canModify（作者权）；不得调 requireAdmin（治理权）、
//     不得调 CanAdmin / checkWrite；EditPost 必须在 Write 事务内取作者并改正文；
//  2. UPDATE SQL 只 SET body_md / edited_at，绝不出现 status / created_at / last_reply_at；
//  3. 已编辑标记走 sys()（RenderSystem 固定文案），不走 ext()（用户内容净化链）；
//  4. 每条守卫配负向注入，证明拿掉规则就会报警——静态断言假绿比没有守卫更危险。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collectEditAppIssues 守用例层的权限单点与写事务编排。
func collectEditAppIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string

	edit, ok := findFuncDecl(files, "EditPost")
	if !ok {
		issues = append(issues, "扫描不到应用用例 EditPost，守卫失效")
	} else {
		calls := callNames(edit)
		for _, want := range []string{"Write", "PostOwner", "canModify", "EditPost"} {
			if !calls[want] {
				issues = append(issues, "EditPost 用例写路径缺少 "+want+" 调用")
			}
		}
		for _, forbidden := range []string{"requireAdmin", "CanAdmin", "checkWrite"} {
			if calls[forbidden] {
				issues = append(issues, "EditPost 不得调用 "+forbidden+"（编辑是作者权，不是治理/发帖权）")
			}
		}
		// 不写审计事件：作者编辑自己的内容是普通写（与 ADR-050 的 hidden 治理不同）。
		for _, forbidden := range []string{"AppendOutbox", "afterCommit", "publish"} {
			if calls[forbidden] {
				issues = append(issues, "EditPost 不得写审计事件 "+forbidden)
			}
		}
		// UPDATE 不能碰主题活跃度（同投票的 TouchThread 教训）。
		if calls["TouchThread"] {
			issues = append(issues, "EditPost 不得调 TouchThread（会推进 last_reply_at）")
		}
	}

	pre, ok := findFuncDecl(files, "CanEditPost")
	if !ok {
		issues = append(issues, "扫描不到只读判权用例 CanEditPost，守卫失效")
	} else {
		calls := callNames(pre)
		if !calls["canModify"] {
			issues = append(issues, "CanEditPost 必须复用 canModify 单一谓词")
		}
		if calls["Write"] || calls["requireAdmin"] {
			issues = append(issues, "CanEditPost 不得占写事务或走治理权")
		}
	}
	return issues, nil
}

// collectEditSQLIssues 守仓储只 UPDATE body_md / edited_at，不碰状态与时间列。
func collectEditSQLIssues(dir string) ([]string, error) {
	files, err := parseNamedGoFilesIn(dir)
	if err != nil {
		return nil, err
	}
	var issues []string
	fn, ok := findFuncDecl(files, "EditPost")
	if !ok {
		return []string{"扫描不到仓储 EditPost 实现，守卫失效"}, nil
	}
	lits := strings.Join(stringLiterals(fn), " ")
	up := strings.ToUpper(lits)
	if !strings.Contains(up, "UPDATE POSTS SET") {
		issues = append(issues, "EditPost 必须执行 UPDATE posts SET")
	}
	if !strings.Contains(lits, "body_md = ?") || !strings.Contains(lits, "edited_at = ?") {
		issues = append(issues, "EditPost 的 SET 只应含 body_md / edited_at")
	}
	for _, forbidden := range []string{"last_reply_at", "created_at", "lastreply"} {
		if strings.Contains(strings.ToLower(lits), forbidden) {
			issues = append(issues, "EditPost SQL 不得触碰 "+forbidden)
		}
	}
	// status 只应作为 WHERE 条件更新守卫存在，不得出现在 SET 子句里。
	setIdx := strings.Index(up, "SET")
	whereIdx := strings.Index(up, "WHERE")
	if setIdx >= 0 && whereIdx > setIdx && strings.Contains(up[setIdx:whereIdx], "STATUS") {
		issues = append(issues, "EditPost 的 SET 子句不得修改 status")
	}
	if strings.Contains(up, "DELETE FROM") {
		issues = append(issues, "EditPost 不得物理删")
	}
	return issues, nil
}

// collectEditViewIssues 守「已编辑」固定文案走 RenderSystem（sys），不进用户内容链（ext）。
// 复用 author_projection_test.go 的 renderCallsReferencing（同包、同 AST 口径）。
func collectEditViewIssues(t *testing.T, path string) []string {
	t.Helper()
	var issues []string
	if len(renderCallsReferencing(t, path, "sys", "已编辑")) == 0 {
		issues = append(issues, "已编辑标记必须走 sys()（RenderSystem 固定文案）")
	}
	if len(renderCallsReferencing(t, path, "ext", "已编辑")) > 0 {
		issues = append(issues, "已编辑标记不得走 ext()（用户内容净化链）")
	}
	return issues
}

func TestEditGuards(t *testing.T) {
	root := findModuleRoot(t)
	app, err := collectEditAppIssues(filepath.Join(root, "internal", "application", "forum"))
	assertNoIssues(t, app, err, "编辑用例权限与写事务守卫")
	sql, err := collectEditSQLIssues(filepath.Join(root, "internal", "infrastructure", "sqlite"))
	assertNoIssues(t, sql, err, "编辑 SQL 只改正文与时间守卫")
	view := collectEditViewIssues(t, filepath.Join(root, "internal", "presentation", "terminal", "view.go"))
	assertNoIssues(t, view, nil, "已编辑标记走 RenderSystem 守卫")
}

// ---- 负向注入 ----

func TestGuardDetectsEditWrongPermission(t *testing.T) {
	dir := t.TempDir()
	src := `package forum
func (s *Service) EditPost(ctx context.Context, actor Actor, in EditPostInput) error {
	if err := s.requireAdmin(actor); err != nil { return err }
	return s.repos.Write(ctx, func(tx WriteTx) error {
		owner, err := tx.PostOwner(ctx, in.PostID)
		if !canModify(actor.UserID, owner) { return ErrNotEditor }
		return tx.EditPost(ctx, in.PostID, in.Body, 0)
	})
}
func (s *Service) CanEditPost(ctx context.Context, actor Actor, id int64) error {
	owner, _ := s.repos.PostAuthor(ctx, id)
	if owner != actor.UserID { return ErrNotEditor }
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "edit.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectEditAppIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"EditPost 不得调用 requireAdmin", "CanEditPost 必须复用 canModify"} {
		if !strings.Contains(joined, want) {
			t.Errorf("注入违规权限后未检出 %q:\n%s", want, joined)
		}
	}
}

func TestGuardDetectsEditSQLTouchingForbiddenColumns(t *testing.T) {
	dir := t.TempDir()
	src := `package sqlite
func (w *sqlWriteTx) EditPost(ctx context.Context, id int64, body string, at int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE posts SET body_md = ?, edited_at = ?, status = 'normal', last_reply_at = ? WHERE id = ?", body, at, at, id)
	return err
}
`
	if err := os.WriteFile(filepath.Join(dir, "repo_edit.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectEditSQLIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"SET 子句不得修改 status", "不得触碰 last_reply_at"} {
		if !strings.Contains(joined, want) {
			t.Errorf("注入违规 SQL 后未检出 %q:\n%s", want, joined)
		}
	}
}

func TestGuardDetectsEditedMarkerViaExt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "view.go")
	src := `package terminal
func bad() { ext(&b, "已编辑 "+formatTime(s.Detail.EditedAt), 80) }
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	issues := collectEditViewIssues(t, path)
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "已编辑标记不得走 ext()") {
		t.Fatalf("注入 ext 渲染已编辑标记后未检出:\n%s", joined)
	}
}
