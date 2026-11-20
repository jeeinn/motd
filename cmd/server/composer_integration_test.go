package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
)

// P1-M4 文案硬约束：正文提交只有「哨兵 . / Ctrl-D」两条，Enter 只换行。
// Ctrl+Enter 在通用 SSH 字节流层与 Enter 不可区分，P1 绝不能把它写进主提示。
func TestComposerHintsNeverMentionCtrlEnter(t *testing.T) {
	hints := []string{composerTitleHint, composerBodyHint, composerBodyAfterTitleHint, composerEditHint, composerLimitHint}
	for _, h := range hints {
		lower := strings.ToLower(h)
		if strings.Contains(lower, "ctrl+enter") || strings.Contains(lower, "ctrl-enter") {
			t.Fatalf("提示文案禁止出现 Ctrl+Enter: %q", h)
		}
	}
	for _, h := range []string{composerBodyHint, composerBodyAfterTitleHint, composerEditHint} {
		if !strings.Contains(h, input.SubmitSentinel) || !strings.Contains(h, "Ctrl-D") || !strings.Contains(h, "Enter 换行") {
			t.Fatalf("正文提示必须同时含哨兵 / Ctrl-D / Enter 换行语义: %q", h)
		}
	}

	// 真实 startComposer / 阶段切换输出也必须守住文案（防止有人绕过常量写死新串）。
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if strings.Contains(strings.ToLower(out), "ctrl+enter") {
		t.Fatalf("标题提示禁止 Ctrl+Enter: %q", out)
	}
	typeInto(t, ctx, sess, pc, "标题")
	sess.reset()
	pressEnter(t, ctx, sess, pc) // 标题 Enter → 进入正文阶段，输出正文提示
	out = sess.output()
	if strings.Contains(strings.ToLower(out), "ctrl+enter") {
		t.Fatalf("正文提示禁止 Ctrl+Enter: %q", out)
	}
	if !strings.Contains(out, "Ctrl-D") || !strings.Contains(out, input.SubmitSentinel) {
		t.Fatalf("正文提示必须含哨兵与 Ctrl-D: %q", out)
	}
}

// 进入正文编辑态的公共前置：已登录 → 版块 → 新主题 → 标题确认。
func enterBodyForNewThread(t *testing.T) (context.Context, *pageController, *fakePageSession) {
	t.Helper()
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "多行标题")
	pressEnter(t, ctx, sess, pc) // 标题仍 Enter 确认
	if pc.editor == nil || pc.editor.stage != stageBody {
		t.Fatal("标题确认后应进入正文编辑态")
	}
	return ctx, pc, sess
}

func TestPageControllerBodyEnterNewlineThenSentinelSubmits(t *testing.T) {
	ctx, pc, sess := enterBodyForNewThread(t)

	typeInto(t, ctx, sess, pc, "第一行")
	pressEnter(t, ctx, sess, pc) // 正文 Enter = 换行，不提交
	if pc.editor == nil || pc.editor.stage != stageBody {
		t.Fatal("正文 Enter 不得提交")
	}
	typeInto(t, ctx, sess, pc, "第二行")
	// 单独一行 "." + Enter 提交。
	pressEnter(t, ctx, sess, pc)
	typeInto(t, ctx, sess, pc, input.SubmitSentinel)
	pressEnter(t, ctx, sess, pc)
	if pc.editor != nil {
		t.Fatal("哨兵提交后应退出编辑态")
	}

	threads, err := pc.service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(threads.Rows) != 1 {
		t.Fatalf("发帖未落库: rows=%d err=%v", len(threads.Rows), err)
	}
	detail, err := pc.service.GetThread(ctx, threads.Rows[0].ID)
	if err != nil || len(detail.Posts) != 1 {
		t.Fatalf("首帖正文缺失: posts=%d err=%v", len(detail.Posts), err)
	}
	if got, want := detail.Posts[0].BodyMD, "第一行\n第二行"; got != want {
		t.Fatalf("多行正文落库不符: got=%q want=%q", got, want)
	}
}

func TestPageControllerBodyPasteWithNewlinesDoesNotSubmit(t *testing.T) {
	ctx, pc, sess := enterBodyForNewThread(t)

	// 粘贴块含内部换行与 "." 行：必须整块进正文，不被误判为哨兵。
	paste := "粘贴第一行\n.\n粘贴第二行"
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyPaste, Text: paste}); err != nil {
		t.Fatalf("粘贴失败: %v", err)
	}
	if pc.editor == nil || pc.editor.stage != stageBody {
		t.Fatal("粘贴内部换行不得触发提交")
	}
	// Ctrl-D 提交。
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyCtrlD}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	threads, err := pc.service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(threads.Rows) != 1 {
		t.Fatalf("发帖未落库: rows=%d err=%v", len(threads.Rows), err)
	}
	detail, err := pc.service.GetThread(ctx, threads.Rows[0].ID)
	if err != nil || len(detail.Posts) != 1 {
		t.Fatalf("首帖正文缺失: posts=%d err=%v", len(detail.Posts), err)
	}
	if got, want := detail.Posts[0].BodyMD, paste; got != want {
		t.Fatalf("粘贴正文应整块保留: got=%q want=%q", got, want)
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
}

func TestPageControllerBodyBackspaceMergesEmptyLineWithWhitelistedANSI(t *testing.T) {
	ctx, pc, sess := enterBodyForNewThread(t)
	typeInto(t, ctx, sess, pc, "第一行")
	pressEnter(t, ctx, sess, pc) // 正文 Enter = 换行，光标到空行行首
	sess.reset()

	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyBackspace}); err != nil {
		t.Fatalf("空行退格应成功: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, "\x1b[1A") || !strings.Contains(out, "\x1b[K") {
		t.Fatalf("跨行合并应发 CUU+EL: %q", out)
	}
	if !strings.Contains(out, "第一行") {
		t.Fatalf("重绘应含合并后的上一行文本: %q", out)
	}
	cmdOnlyWhitelistedANSI(t, out)
	for _, banned := range []string{"\x1b[H", "\x1b[f", "\x1b[B", "\x1b[2J", "\x1b[J", "\x1b[?1049h"} {
		if strings.Contains(out, banned) {
			t.Fatalf("输出含禁用序列 %q: %q", banned, out)
		}
	}
	if got, want := pc.editor.body.Text(), "第一行"; got != want {
		t.Fatalf("合并后缓冲应只剩上一行: got=%q want=%q", got, want)
	}

	// 合并后继续输入接在末尾，提交落库仍是单行。
	typeInto(t, ctx, sess, pc, "继续")
	submitBody(t, ctx, sess, pc)
	threads, err := pc.service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(threads.Rows) != 1 {
		t.Fatalf("发帖未落库: rows=%d err=%v", len(threads.Rows), err)
	}
	detail, err := pc.service.GetThread(ctx, threads.Rows[0].ID)
	if err != nil || len(detail.Posts) != 1 {
		t.Fatalf("首帖正文缺失: posts=%d err=%v", len(detail.Posts), err)
	}
	if got, want := detail.Posts[0].BodyMD, "第一行继续"; got != want {
		t.Fatalf("合并后正文落库不符: got=%q want=%q", got, want)
	}
}

func TestPageControllerBodyLimitHintAndNoEcho(t *testing.T) {
	ctx, pc, sess := enterBodyForNewThread(t)
	// 收紧上限以低成本回归提示路径（生产仍用 forum.MaxBodyBytes）。
	pc.editor.body = input.NewComposer(input.WithMaxBytes(3))
	sess.reset()

	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyPaste, Text: "12345"}); err != nil {
		t.Fatalf("超长粘贴不应返回错误: %v", err)
	}
	out := sess.output()
	if !strings.Contains(out, composerLimitHint) {
		t.Fatalf("超限应给出固定提示: %q", out)
	}
	if strings.Contains(out, "12345") {
		t.Fatalf("被拒内容不得回显: %q", out)
	}
	if got := pc.editor.body.Text(); got != "" {
		t.Fatalf("超限粘贴后缓冲应为空: %q", got)
	}
}

// 编辑态 Esc 走 pageController.editorKey，仍输出「（已取消编辑）」并退出编辑态；
// 浏览态 Esc 的静默改造不得波及这条真正在取消的路径。
func TestEditorEscStillCancelsWithFixedText(t *testing.T) {
	ctx, pc, sess := enterBodyForNewThread(t)
	sess.reset()
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyEsc}); err != nil {
		t.Fatalf("编辑态 Esc 不应报错: %v", err)
	}
	if got := sess.output(); !strings.Contains(got, "（已取消编辑）") {
		t.Fatalf("编辑态 Esc 必须输出取消文案，实际 %q", got)
	}
	if pc.editor != nil {
		t.Fatal("编辑态 Esc 必须退出编辑态")
	}
}
