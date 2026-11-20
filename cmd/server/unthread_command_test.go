package main

// ADR-053 补丁：/unthread <thread-id> 命令接线回归。
// 关键场景：主题被 /hide 后从 /top 版块列表直接恢复——隐藏主题打不开详情，
// 所以这条命令不得要求详情页。真实 Service + SQLite，不回显原文。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/viewmodel"
)

func runUnthreadCommand(t *testing.T, ctx context.Context, h *inputHandler, pc *pageController, sess *fakePageSession, line string) error {
	t.Helper()
	inv, err := h.reg.Parse(line)
	if err != nil {
		return err
	}
	sess.reset()
	return pc.command(ctx, sess, inv)
}

// hide → 回到 /top → /unthread <id>：主题重新可见、可打开详情、落 active。
func TestUnthreadCommandRestoresHiddenThreadFromTopScreen(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1) // 作者 alice(1)
	sess := hideSession(2)                            // bob(2) 指纹在白名单
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide"); err != nil {
		t.Fatalf("准备隐藏主题失败: %v", err)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "hidden" {
		t.Fatalf("隐藏未落库: %q", got)
	}
	// 隐藏主题在公开列表已不可见；回到 /top 用独立命令恢复。
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatalf("前置应处于版块列表屏，实际 screen=%d", pc.screen.Kind)
	}
	if err := runUnthreadCommand(t, ctx, h, pc, sess, "/unthread "+stringInt(tid)); err != nil {
		t.Fatalf("从任意屏恢复隐藏主题应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已恢复主题") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail || pc.threadID != tid {
		t.Fatalf("恢复后应落到该主题详情，实际 screen=%d threadID=%d", pc.screen.Kind, pc.threadID)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("主题应恢复 active，实际 %q", got)
	}
}

// 幂等：已恢复的主题再 /unthread 一次仍成功，不留错误文案。
func TestUnthreadCommandIsIdempotentFromTopScreen(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runUnthreadCommand(t, ctx, h, pc, sess, "/unthread "+stringInt(tid)); err != nil {
			t.Fatalf("第 %d 次恢复应幂等成功: %v", i+1, err)
		}
		if !strings.Contains(sess.output(), "已恢复主题") {
			t.Fatalf("第 %d 次缺少固定成功文案: %q", i+1, sess.output())
		}
		if err := pc.showNodes(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("幂等恢复后仍应为 active: %q", got)
	}
}

// 越权：登录用户与访客指纹不在白名单，统一固定文案，数据零变更。
func TestUnthreadCommandDeniesNonAdmin(t *testing.T) {
	for _, sc := range []struct {
		name string
		sess *fakePageSession
	}{
		{"登录用户", nodeAdminSession(1, "SHA256:"+strings.Repeat("B", 43), false)},
		{"访客", nodeAdminSession(0, "SHA256:"+strings.Repeat("B", 43), true)},
	} {
		t.Run(sc.name, func(t *testing.T) {
			ctx, h, pools, writer := setupHideRuntime(t)
			tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
			pc := h.newPageController(sc.sess)
			if err := pc.showNodes(ctx, sc.sess); err != nil {
				t.Fatal(err)
			}
			if err := runUnthreadCommand(t, ctx, h, pc, sc.sess, "/unthread "+stringInt(tid)); err == nil {
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
			if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
				t.Fatalf("越权后数据不得变更: %q", got)
			}
		})
	}
}

// 参数边界：注册表收口参数个数；非法 ID 与不存在目标都给固定文案、不回显原文。
func TestUnthreadCommandArgBoundariesAndNotFound(t *testing.T) {
	for line, want := range map[string]error{
		"/unthread":     command.ErrArgCount,
		"/unthread 1 2": command.ErrArgCount,
	} {
		if _, err := command.Default().Parse(line); !errors.Is(err, want) {
			t.Errorf("Parse(%q) = %v，期望 %v", line, err, want)
		}
	}
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}

	malicious := "\x1b[2J999"
	if err := runUnthreadCommand(t, ctx, h, pc, sess, "/unthread "+malicious); err != nil {
		t.Fatalf("非法参数应给固定提示: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, "主题 ID 必须是正整数") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "999") {
		t.Fatalf("非法 ID 回显了原文或提示不对: %q", out)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("非法参数不得改数据: %q", got)
	}

	if err := runUnthreadCommand(t, ctx, h, pc, sess, "/unthread 987654"); err == nil {
		t.Fatal("目标不存在应失败并给固定提示")
	}
	out = sess.output()
	if !strings.Contains(out, "对象不存在") {
		t.Fatalf("缺少固定文案: %q", out)
	}
	for _, leak := range []string{"写入事务失败", "不可重试", "987654"} {
		if strings.Contains(out, leak) {
			t.Fatalf("回显了内部细节或 ID: %q", out)
		}
	}
}
