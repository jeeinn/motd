package main

// ADR-053 /unhide 命令接线回归：注册表解析 → 页面控制器 → 真实 Service + SQLite。
// 权限与状态语义由 application / sqlite 测试守；这里守「接上了、口径对、不回显原文」。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// 隐藏回复 → 恢复回复：全过程停留在详情页，公开列表重新包含该条（命令级往返）。
func TestUnhideCommandRestoresReplyByID(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, firstID, replyID := setupDeleteThread(t, ctx, writer, 1) // 作者 alice(1)
	sess := hideSession(2)                                       // bob(2) 指纹在白名单
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide "+stringInt(replyID)); err != nil {
		t.Fatalf("准备隐藏回复失败: %v", err)
	}
	if got := statusOf(t, pools, ctx, "posts", replyID); got != "hidden" {
		t.Fatalf("隐藏未落库: %q", got)
	}
	// /hide <id> 会刷新详情页，因此可以直接在同一屏恢复。
	if err := runUnhideCommand(t, ctx, h, pc, sess, "/unhide "+stringInt(replyID)); err != nil {
		t.Fatalf("管理员恢复他人回复应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已恢复回复") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("恢复回复后应停留在详情，实际 screen=%d", pc.screen.Kind)
	}
	for _, id := range []int64{firstID, replyID} {
		if got := statusOf(t, pools, ctx, "posts", id); got != "normal" {
			t.Fatalf("回复应恢复 normal id=%d: %q", id, got)
		}
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("主题不得被牵连: %q", got)
	}
}

// 无参 /unhide 作用于当前主题。对未隐藏的目标幂等成功（真实 DB 路径由集成测试覆盖）。
func TestUnhideCommandNoArgOnCurrentThread(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runUnhideCommand(t, ctx, h, pc, sess, "/unhide"); err != nil {
		t.Fatalf("无参恢复当前主题应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已恢复主题") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("恢复主题后应回列表，实际 screen=%d", pc.screen.Kind)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("主题应保持/恢复 active: %q", got)
	}
}

// 越权：指纹不在白名单的登录用户与访客都拿到固定文案，数据零变更。
func TestUnhideCommandDeniesNonAdminWithFixedText(t *testing.T) {
	for _, sc := range []struct {
		name string
		sess *fakePageSession
	}{
		{"登录用户", nodeAdminSession(1, "SHA256:"+strings.Repeat("B", 43), false)},
		{"访客", nodeAdminSession(0, "SHA256:"+strings.Repeat("B", 43), true)},
	} {
		t.Run(sc.name, func(t *testing.T) {
			ctx, h, pools, writer := setupHideRuntime(t)
			tid, _, replyID := setupDeleteThread(t, ctx, writer, 1)
			// 先把回复隐藏，才有东西可恢复；用白名单会话准备数据。
			adminSess := hideSession(2)
			adminPC := h.newPageController(adminSess)
			if err := adminPC.showDetail(ctx, adminSess, tid); err != nil {
				t.Fatal(err)
			}
			if err := runHideCommand(t, ctx, h, adminPC, adminSess, "/hide "+stringInt(replyID)); err != nil {
				t.Fatalf("准备隐藏失败: %v", err)
			}
			pc := h.newPageController(sc.sess)
			if err := pc.showDetail(ctx, sc.sess, tid); err != nil {
				t.Fatal(err)
			}
			if err := runUnhideCommand(t, ctx, h, pc, sc.sess, "/unhide "+stringInt(replyID)); err == nil {
				t.Fatal("越权恢复应失败并给固定提示")
			}
			out := sc.sess.output()
			if !strings.Contains(out, "仅管理员可执行") {
				t.Fatalf("越权文案不一致: %q", out)
			}
			for _, leak := range []string{"写入事务失败", "不可重试", "CanAdmin"} {
				if strings.Contains(out, leak) {
					t.Fatalf("回显了内部细节 %q: %q", leak, out)
				}
			}
			if got := statusOf(t, pools, ctx, "posts", replyID); got != "hidden" {
				t.Fatalf("越权后数据不得变更: %q", got)
			}
		})
	}
}

// bootstrap：未绑定账号的访客只要指纹在白名单，也能恢复他人回复。
func TestUnhideCommandAllowsWhitelistedGuest(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, replyID := setupDeleteThread(t, ctx, writer, 1)
	adminSess := hideSession(2)
	adminPC := h.newPageController(adminSess)
	if err := adminPC.showDetail(ctx, adminSess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, adminPC, adminSess, "/hide "+stringInt(replyID)); err != nil {
		t.Fatalf("准备隐藏失败: %v", err)
	}

	guest := nodeAdminSession(0, hideFP, true)
	pc := h.newPageController(guest)
	if err := pc.showDetail(ctx, guest, tid); err != nil {
		t.Fatal(err)
	}
	if err := runUnhideCommand(t, ctx, h, pc, guest, "/unhide "+stringInt(replyID)); err != nil {
		t.Fatalf("白名单访客恢复应成功: %v", err)
	}
	if !strings.Contains(guest.output(), "已恢复回复") {
		t.Fatalf("访客治理被拒: %q", guest.output())
	}
	if got := statusOf(t, pools, ctx, "posts", replyID); got != "normal" {
		t.Fatalf("访客恢复未落库: %q", got)
	}
}

// 参数边界与不回显原文：多于一个参数 / 非法 ID / 不在详情页。
func TestUnhideCommandArgBoundariesAndNoEcho(t *testing.T) {
	for line, want := range map[string]error{
		"/unhide a b": command.ErrArgCount,
		"/unhide 1 2": command.ErrArgCount,
	} {
		if _, err := command.Default().Parse(line); !errors.Is(err, want) {
			t.Errorf("Parse(%q) = %v，期望 %v", line, err, want)
		}
	}
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}

	malicious := "\x1b[2J999"
	if err := runUnhideCommand(t, ctx, h, pc, sess, "/unhide "+malicious); err != nil {
		t.Fatalf("非法参数应给固定提示: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, "ID 必须是正整数") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "999") {
		t.Fatalf("非法 ID 回显了原文或提示不对: %q", out)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("非法参数不得改数据: %q", got)
	}

	// 不在详情页时不猜目标。
	sess2 := hideSession(2)
	pc2 := h.newPageController(sess2)
	if err := runUnhideCommand(t, ctx, h, pc2, sess2, "/unhide"); err != nil {
		t.Fatalf("无目标应给固定提示: %v", err)
	}
	if !strings.Contains(sess2.output(), "请先打开一个主题再恢复") {
		t.Fatalf("缺少明确提示: %q", sess2.output())
	}
}

// 目标不存在：固定文案「对象不存在」，不暴露事务内部链，不回显 ID 文本。
func TestUnhideCommandNotFoundUsesFixedText(t *testing.T) {
	ctx, h, _, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runUnhideCommand(t, ctx, h, pc, sess, "/unhide 987654"); err == nil {
		t.Fatal("目标不存在应失败并给固定提示")
	}
	out := sess.output()
	if !strings.Contains(out, "对象不存在") {
		t.Fatalf("缺少固定文案: %q", out)
	}
	for _, leak := range []string{"写入事务失败", "不可重试", "987654"} {
		if strings.Contains(out, leak) {
			t.Fatalf("回显了内部细节或 ID: %q", out)
		}
	}
}

func runUnhideCommand(t *testing.T, ctx context.Context, h *inputHandler, pc *pageController, sess *fakePageSession, line string) error {
	t.Helper()
	inv, err := h.reg.Parse(line)
	if err != nil {
		return err
	}
	sess.reset()
	return pc.command(ctx, sess, inv)
}
