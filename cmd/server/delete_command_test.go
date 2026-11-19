package main

// v0.2.1 /delete 的接线回归：注册表解析 → 页面控制器 → 真实 Service + SQLite。
// 这里验证「接上了、口径对、不回显原文」；权限与软删语义由 application/infra 测试守。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func setupDeleteThread(t *testing.T, ctx context.Context, writer *sqlite.Writer, ownerID int64) (int64, int64, int64) {
	t.Helper()
	const now = int64(1_700_000_000)
	var threadID, firstID, replyID int64
	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (node_id,user_id,title,score,last_reply_at,created_at) VALUES (1,?,'删除验收主题',0,?,?)`,
			ownerID, now, now)
		if err != nil {
			return err
		}
		threadID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,?,'qqqdeletefirst','normal',?)`,
			threadID, ownerID, now)
		if err != nil {
			return err
		}
		firstID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,2,'qqqdeletereply','normal',?)`,
			threadID, now+1)
		if err != nil {
			return err
		}
		replyID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("准备删除验收数据: %v", err)
	}
	return threadID, firstID, replyID
}

func statusOf(t *testing.T, pools *sqlite.Pools, ctx context.Context, table string, id int64) string {
	t.Helper()
	var s string
	if err := pools.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM %s WHERE id = ?`, table), id).Scan(&s); err != nil {
		t.Fatalf("查询 %s:%d 状态: %v", table, id, err)
	}
	return s
}

func runDeleteCommand(t *testing.T, ctx context.Context, h *inputHandler, pc *pageController, sess *fakePageSession, line string) error {
	t.Helper()
	inv, err := h.reg.Parse(line)
	if err != nil {
		t.Fatalf("解析 %q: %v", line, err)
	}
	sess.reset()
	return pc.command(ctx, sess, inv)
}

// 作者执行无参 /delete：主题软删、回列表、回复行仍在、已删主题 open 不泄漏状态。
func TestDeleteCommandAuthorDeletesCurrentThread(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, _, replyID := setupDeleteThread(t, ctx, writer, 1)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	if err := runDeleteCommand(t, ctx, h, pc, sess, "/delete"); err != nil {
		t.Fatalf("作者删主题应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已删除") {
		t.Fatalf("应给出成功提示: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("删主题后应回到列表，实际 screen=%d", pc.screen.Kind)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "deleted" {
		t.Fatalf("主题应软删，实际 %q", got)
	}
	if got := statusOf(t, pools, ctx, "posts", replyID); got != "normal" {
		t.Fatalf("软删主题不得级联改回复，实际 %q", got)
	}

	// 验收③：已软删主题直接 /open → 不存在，且不泄漏 deleted/hidden 状态。
	if err := runDeleteCommand(t, ctx, h, pc, sess, fmt.Sprintf("/open %d", tid)); err == nil {
		t.Fatal("已删主题 /open 应失败")
	}
	out := sess.output()
	if !strings.Contains(out, "不存在") {
		t.Fatalf("应提示内容不存在: %q", out)
	}
	for _, leak := range []string{"deleted", "hidden", "已删除", "已隐藏"} {
		if strings.Contains(out, leak) {
			t.Fatalf("提示泄漏了内部状态 %q: %q", leak, out)
		}
	}
}

// 非作者与访客：统一「仅作者可删除」，数据零变更。
func TestDeleteCommandRejectsOtherAndGuestWithSameHint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		userID int64
	}{
		{"非作者", 2},
		{"访客", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, h, _, pools, writer := setupMVPRuntime(t)
			tid, _, _ := setupDeleteThread(t, ctx, writer, 1) // 作者是 1
			sess := newFakePageSession(tc.userID)
			pc := h.newPageController(sess)
			if err := pc.showDetail(ctx, sess, tid); err != nil {
				t.Fatal(err)
			}
			if err := runDeleteCommand(t, ctx, h, pc, sess, "/delete"); err == nil {
				t.Fatal("非作者/访客删除应失败")
			}
			out := sess.output()
			if !strings.Contains(out, "仅作者可删除") {
				t.Fatalf("提示口径不一致: %q", out)
			}
			for _, leak := range []string{"写入事务失败", "不可重试"} {
				if strings.Contains(out, leak) {
					t.Fatalf("提示泄漏了事务内部细节 %q: %q", leak, out)
				}
			}
			if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
				t.Fatalf("被拒后主题不得变更，实际 %q", got)
			}
		})
	}
}

// 带 id：只删当前主题下那条回复，主题与首帖不受影响。
// 用 bob(2) 删自己的回复（主题作者是 alice(1)）——所有权判定按内容作者，不按主题作者。
func TestDeleteCommandDeletesReplyByID(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, firstID, replyID := setupDeleteThread(t, ctx, writer, 1)
	sess := newFakePageSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	if err := runDeleteCommand(t, ctx, h, pc, sess, fmt.Sprintf("/delete %d", replyID)); err != nil {
		t.Fatalf("作者删回复应成功: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("删回复后应停留在详情刷新，实际 screen=%d", pc.screen.Kind)
	}
	if got := statusOf(t, pools, ctx, "posts", replyID); got != "deleted" {
		t.Fatalf("回复应软删，实际 %q", got)
	}
	if got := statusOf(t, pools, ctx, "posts", firstID); got != "normal" {
		t.Fatalf("首帖不得被牵连，实际 %q", got)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("主题不得被牵连，实际 %q", got)
	}
}

// 非法 id 给固定提示，绝不回显用户原文（同 /open 的安全约束）。
func TestDeleteCommandRejectsBadIDWithoutEcho(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	malicious := "\x1b[2J999"
	if err := runDeleteCommand(t, ctx, h, pc, sess, "/delete "+malicious); err != nil {
		t.Fatalf("非法参数应给固定提示而非报错: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, "ID 必须是正整数") {
		t.Fatalf("缺少固定提示: %q", out)
	}
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "999") {
		t.Fatalf("用户原文被回显: %q", out)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("非法参数不得改数据，实际 %q", got)
	}
}

// 不在详情页时不猜目标，直接给明确提示。
func TestDeleteCommandRequiresDetailScreen(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := runDeleteCommand(t, ctx, h, pc, sess, "/delete"); err != nil {
		t.Fatalf("无目标应给固定提示: %v", err)
	}
	if !strings.Contains(sess.output(), "请先打开一个主题再删除") {
		t.Fatalf("缺少明确提示: %q", sess.output())
	}
}
