package main

// P2-M2 /edit 命令接线回归：注册表解析 → 页面控制器 → 真实 Service + SQLite。
// 权限/软语义由 forum 与 sqlite 测试守；这里守「接上了、口径对、不改错行、不回显原文」。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// 与 delete 验收数据同构：作者 1 的主题 + 首帖，回复由作者 2 发出。
func setupEditThread(t *testing.T, ctx context.Context, writer *sqlite.Writer, ownerID int64) (int64, int64, int64) {
	t.Helper()
	const now = int64(1_700_000_000)
	var threadID, firstID, replyID int64
	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (node_id,user_id,title,score,last_reply_at,created_at) VALUES (1,?,'编辑验收主题',0,?,?)`,
			ownerID, now, now)
		if err != nil {
			return err
		}
		threadID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,?,'qqqeditfirst','normal',?)`,
			threadID, ownerID, now)
		if err != nil {
			return err
		}
		firstID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,2,'qqqeditreply','normal',?)`,
			threadID, now+1)
		if err != nil {
			return err
		}
		replyID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("准备编辑验收数据: %v", err)
	}
	return threadID, firstID, replyID
}

func bodyOf(t *testing.T, pools *sqlite.Pools, ctx context.Context, postID int64) (string, sql.NullInt64) {
	t.Helper()
	var body string
	var edited sql.NullInt64
	if err := pools.QueryRowContext(ctx, `SELECT body_md, edited_at FROM posts WHERE id = ?`, postID).Scan(&body, &edited); err != nil {
		t.Fatalf("读取 post %d: %v", postID, err)
	}
	return body, edited
}

func runEditCommand(t *testing.T, ctx context.Context, h *inputHandler, pc *pageController, sess *fakePageSession, line string) error {
	t.Helper()
	inv, err := h.reg.Parse(line)
	if err != nil {
		t.Fatalf("解析 %q: %v", line, err)
	}
	sess.reset()
	return pc.command(ctx, sess, inv)
}

// 注册表参数边界：0/1 个参数合法，2 个参数在解析层被拒。
func TestEditCommandArgBoundaries(t *testing.T) {
	for _, tc := range []struct {
		line string
		ok   bool
		args int
	}{
		{"/edit", true, 0},
		{"/edit 42", true, 1},
		{"/edit 42 43", false, 0},
	} {
		inv, err := newInputHandler().withForum(nil, nil, nil).reg.Parse(tc.line)
		if tc.ok {
			if err != nil || inv.Name != "edit" || len(inv.Args) != tc.args {
				t.Fatalf("%q 应解析为 edit/%d 参数，实际 %+v err=%v", tc.line, tc.args, inv, err)
			}
		} else if err == nil {
			t.Fatalf("%q 参数越界应被拒绝", tc.line)
		}
	}
}

// 作者无参 /edit：进入 composer 重新输入全文，提交覆盖首帖正文并打 edited_at，停留详情。
func TestEditCommandAuthorEditsThreadBodyViaComposer(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, firstID, _ := setupEditThread(t, ctx, writer, 1)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	if err := runEditCommand(t, ctx, h, pc, sess, "/edit"); err != nil {
		t.Fatalf("进入编辑态失败: %v", err)
	}
	if pc.editor == nil || pc.editor.editPostID != firstID || pc.editor.stage != stageBody {
		t.Fatalf("无参 /edit 应定位首帖正文并进入正文阶段: %+v", pc.editor)
	}
	if !strings.Contains(sess.output(), "编辑：") {
		t.Fatalf("应给出编辑提示: %q", sess.output())
	}

	typeInto(t, ctx, sess, pc, "全新正文第一行")
	pressEnter(t, ctx, sess, pc)
	typeInto(t, ctx, sess, pc, "全新正文第二行")
	submitBody(t, ctx, sess, pc)

	body, edited := bodyOf(t, pools, ctx, firstID)
	if body != "全新正文第一行\n全新正文第二行" {
		t.Fatalf("正文未按 composer 新文覆盖: %q", body)
	}
	if !edited.Valid || edited.Int64 <= 0 {
		t.Fatalf("edited_at 应落库（非 NULL 正整数），实际 %+v", edited)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("编辑后应停留详情，实际 screen=%d", pc.screen.Kind)
	}
	if !strings.Contains(sess.output(), "已保存修改") {
		t.Fatalf("应给出成功提示: %q", sess.output())
	}
	if !strings.Contains(sess.output(), "已编辑 ") {
		t.Fatalf("刷新后详情应显示已编辑标记: %q", sess.output())
	}
	if finalBody, _ := bodyOf(t, pools, ctx, firstID); strings.Contains(finalBody, "qqqeditfirst") {
		t.Fatal("原文不应残留")
	}
}

// 作者带 id 编辑自己的回复（bob 编辑自己的回复，主题作者是 alice）。
func TestEditCommandEditsReplyByID(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, firstID, replyID := setupEditThread(t, ctx, writer, 1)
	sess := newFakePageSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	if err := runEditCommand(t, ctx, h, pc, sess, fmt.Sprintf("/edit %d", replyID)); err != nil {
		t.Fatalf("编辑回复失败: %v", err)
	}
	typeInto(t, ctx, sess, pc, "回复新正文")
	submitBody(t, ctx, sess, pc)

	if body, edited := bodyOf(t, pools, ctx, replyID); body != "回复新正文" || !edited.Valid {
		t.Fatalf("回复正文/edited_at 未更新: body=%q edited=%+v", body, edited)
	}
	if body, edited := bodyOf(t, pools, ctx, firstID); body != "qqqeditfirst" || edited.Valid {
		t.Fatalf("首帖不得被牵连: body=%q edited=%+v", body, edited)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("编辑回复后停留详情，实际 screen=%d", pc.screen.Kind)
	}
}

// 非作者 / 访客统一「仅作者可编辑」，不进 composer、不回显原文、数据零变更。
func TestEditCommandRejectsOtherAndGuestWithSameHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		userID int64
	}{
		{"非作者", 2},
		{"访客", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, h, _, pools, writer := setupMVPRuntime(t)
			tid, firstID, _ := setupEditThread(t, ctx, writer, 1)
			sess := newFakePageSession(tc.userID)
			pc := h.newPageController(sess)
			if err := pc.showDetail(ctx, sess, tid); err != nil {
				t.Fatal(err)
			}

			if err := runEditCommand(t, ctx, h, pc, sess, "/edit"); err == nil {
				t.Fatal("非作者/访客编辑应失败")
			}
			out := sess.output()
			if !strings.Contains(out, "仅作者可编辑") {
				t.Fatalf("提示口径不一致: %q", out)
			}
			if pc.editor != nil {
				t.Fatal("越权不得进入 composer")
			}
			// 提示行不得泄漏事务内部细节；正文是详情页固有内容，不算回显。
			for _, leak := range []string{"写入事务失败", "不可重试"} {
				if strings.Contains(out, leak) {
					t.Fatalf("提示泄漏内部细节 %q: %q", leak, out)
				}
			}
			if body, edited := bodyOf(t, pools, ctx, firstID); body != "qqqeditfirst" || edited.Valid {
				t.Fatalf("被拒后数据零变更: body=%q edited=%+v", body, edited)
			}
		})
	}
}

// 非法 id 固定提示，绝不回显用户原文（同 /delete）。
func TestEditCommandRejectsBadIDWithoutEcho(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, firstID, _ := setupEditThread(t, ctx, writer, 1)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	malicious := "\x1b[2J999"
	if err := runEditCommand(t, ctx, h, pc, sess, "/edit "+malicious); err != nil {
		t.Fatalf("非法参数应给固定提示而非报错: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, "ID 必须是正整数") {
		t.Fatalf("缺少固定提示: %q", out)
	}
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "999") {
		t.Fatalf("用户原文被回显: %q", out)
	}
	if body, edited := bodyOf(t, pools, ctx, firstID); body != "qqqeditfirst" || edited.Valid {
		t.Fatalf("非法参数不得改数据: body=%q edited=%+v", body, edited)
	}
}

// 不在详情页不猜目标，给明确提示。
func TestEditCommandRequiresDetailScreen(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := runEditCommand(t, ctx, h, pc, sess, "/edit"); err != nil {
		t.Fatalf("无目标应给固定提示: %v", err)
	}
	if !strings.Contains(sess.output(), "请先打开一个主题再编辑") {
		t.Fatalf("缺少明确提示: %q", sess.output())
	}
}

// 编辑不推进 last_reply_at：列表排序稳定。
func TestEditCommandDoesNotAdvanceLastReplyAt(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, firstID, _ := setupEditThread(t, ctx, writer, 1)
	var before int64
	if err := pools.QueryRowContext(ctx, `SELECT last_reply_at FROM threads WHERE id = ?`, tid).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runEditCommand(t, ctx, h, pc, sess, "/edit"); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "新正文")
	submitBody(t, ctx, sess, pc)

	var after int64
	if err := pools.QueryRowContext(ctx, `SELECT last_reply_at FROM threads WHERE id = ?`, tid).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("编辑推进了 last_reply_at: before=%d after=%d", before, after)
	}
	if body, _ := bodyOf(t, pools, ctx, firstID); body != "新正文" {
		t.Fatalf("正文未更新: %q", body)
	}
}
