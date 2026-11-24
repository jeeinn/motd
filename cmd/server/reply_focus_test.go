package main

// ADR-068：回复成功后把焦点回填到自己刚发的回复（复用 ADR-066 restoreFocus）。
// 本文件只守提交落点的焦点锚定；back() 的焦点回填由 back_focus_test.go 守。

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// seedThreadFirstPost 在 node 1 建主题 + 首帖（ownerID），不经写用例、不占冷却配额，
// 返回主题 ID。
func seedThreadFirstPost(t *testing.T, ctx context.Context, writer *sqlite.Writer, ownerID int64) int64 {
	t.Helper()
	const now = int64(1_700_000_000)
	var threadID int64
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (node_id,user_id,title,score,last_reply_at,created_at) VALUES (1,?,'焦点验收主题',0,?,?)`,
			ownerID, now, now)
		if err != nil {
			return err
		}
		threadID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,?,'首帖正文','normal',?)`,
			threadID, ownerID, now)
		return err
	}); err != nil {
		t.Fatalf("准备主题与首帖: %v", err)
	}
	return threadID
}

// seedOtherReply 在指定主题插一条 userID 的既有回复（模拟他人回复已落库）。
func seedOtherReply(t *testing.T, ctx context.Context, writer *sqlite.Writer, threadID, userID int64, body string, createdAt int64) {
	t.Helper()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,?,?, 'normal',?)`,
			threadID, userID, body, createdAt)
		return err
	}); err != nil {
		t.Fatalf("准备他人回复: %v", err)
	}
}

// O2-T1：正常回复成功，焦点落在自己刚发的回复，notice=saved。
func TestReplyFocusLandsOnOwnPost(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	threadID := seedThreadFirstPost(t, ctx, writer, 1)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "我的新回复")
	submitBody(t, ctx, sess, pc)

	if pc.editor != nil {
		t.Fatal("提交成功后应摘出编辑器")
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("应留在详情屏，实际 %v", pc.screen.Kind)
	}

	var newPostID int64
	if err := pools.QueryRowContext(ctx,
		`SELECT id FROM posts WHERE thread_id=? AND user_id=1 ORDER BY id DESC LIMIT 1`, threadID).Scan(&newPostID); err != nil {
		t.Fatalf("查新回复: %v", err)
	}
	want := indexOfRow(pc, newPostID)
	if want == 0 {
		t.Fatal("新回复不在详情屏 Rows")
	}
	if pc.screen.Selection.FocusedIndex != want {
		t.Fatalf("焦点应在自己回复 Index=%d，实际 %d", want, pc.screen.Selection.FocusedIndex)
	}
	if code := pc.screen.StatusBar.Notice.Code; code != "saved" {
		t.Fatalf("notice 应为 saved，实际 %q", code)
	}
}

// O2-T2：他人回复已在集合中时，按自己 postID 精准命中，不落到首行（他人回复）。
func TestReplyFocusIgnoresOtherReplies(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	threadID := seedThreadFirstPost(t, ctx, writer, 1)
	// 他人（user2）回复先落库，排在 user1 新回复之前。
	seedOtherReply(t, ctx, writer, threadID, 2, "别人先说的", 1_700_000_001)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "我的跟进回复")
	submitBody(t, ctx, sess, pc)

	var newPostID int64
	if err := pools.QueryRowContext(ctx,
		`SELECT id FROM posts WHERE thread_id=? AND user_id=1 ORDER BY id DESC LIMIT 1`, threadID).Scan(&newPostID); err != nil {
		t.Fatalf("查新回复: %v", err)
	}
	want := indexOfRow(pc, newPostID)
	if want == 0 {
		t.Fatal("自己的回复不在 Rows")
	}
	first := pc.screen.Rows[0].Index
	if pc.screen.Selection.FocusedIndex != want {
		t.Fatalf("焦点应精准命中自己回复 Index=%d，实际 %d", want, pc.screen.Selection.FocusedIndex)
	}
	if pc.screen.Selection.FocusedIndex == first {
		t.Fatal("他人回复在首行时不应回退首行")
	}
}

// O2-T3：锚点不在 Rows（软删/超上限同源）时 restoreFocus 回退首行，不报错不落空。
func TestReplyFocusMissFallsBackFirst(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	threadID := seedThreadFirstPost(t, ctx, writer, 1)
	// 既有回复让详情屏 Rows 非空。
	seedOtherReply(t, ctx, writer, threadID, 2, "别人先说的", 1_700_000_001)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	pc.restoreFocus(999_999) // 不存在的锚点
	if len(pc.screen.Rows) == 0 {
		t.Fatal("详情屏应有回复行")
	}
	first := pc.screen.Rows[0].Index
	if pc.screen.Selection.FocusedIndex != first {
		t.Fatalf("未命中应回退首行 Index=%d，实际 %d", first, pc.screen.Selection.FocusedIndex)
	}
}

// O2-T4：超 MaxPostsPerThread 预置后回复，新回复不在返回集合，焦点保持首行、不 panic。
func TestReplyFocusOverLimitKeepsFirst(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	threadID := seedThreadFirstPost(t, ctx, writer, 1)

	// 预置 200 条他人回复（最小 ID 前 200 条占满返回窗口）。
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for i := 0; i < 200; i++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,2,?, 'normal',?)`,
				threadID, "占位回复", 1_700_000_002+int64(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("预置 200 条: %v", err)
	}

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "超上限后的回复")
	submitBody(t, ctx, sess, pc)

	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("应留在详情屏，实际 %v", pc.screen.Kind)
	}
	first := pc.screen.Rows[0].Index
	if pc.screen.Selection.FocusedIndex != first {
		t.Fatalf("超上限未命中应保持首行 Index=%d，实际 %d", first, pc.screen.Selection.FocusedIndex)
	}
	if code := pc.screen.StatusBar.Notice.Code; code != "saved" {
		t.Fatalf("notice 应为 saved，实际 %q", code)
	}
}

// O2-T5：发帖（creating=true）回列表，焦点维持首行，不被回复锚定逻辑误伤。
func TestNewThreadFocusUnaffected(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	_ = writer

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "新帖标题")
	pressEnter(t, ctx, sess, pc)
	typeInto(t, ctx, sess, pc, "新帖正文")
	submitBody(t, ctx, sess, pc)

	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("发帖成功应回主题列表，实际 %v", pc.screen.Kind)
	}
	if len(pc.screen.Rows) == 0 {
		t.Fatal("回列表后应有主题行")
	}
	first := pc.screen.Rows[0].Index
	if pc.screen.Selection.FocusedIndex != first {
		t.Fatalf("列表焦点应维持首行 Index=%d，实际 %d", first, pc.screen.Selection.FocusedIndex)
	}
}
