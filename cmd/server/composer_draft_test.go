package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// setupDraftRuntime 与 setupMVPRuntime 同口径（真实 SQLite + 真实写入队列 +
// 真实 Service），但通过 newForumRuntime 的 nowFn 注入可变时钟：限流测试需要把
// 钟面 advance 过 60 秒冷却，真实墙钟无法在测试内推进。clock 由调用方持有，
// advance 直接改其值；固定取白天时刻，避开 night_owl 成就窗口。
func setupDraftRuntime(t *testing.T, clock *time.Time) (context.Context, *inputHandler, *forumRuntime, *sqlite.Pools, *sqlite.Writer) {
	t.Helper()
	ctx := context.Background()
	pools, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "motd.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(func() {
		writer.Stop()
		_ = pools.Close()
	})

	now := clock.Unix()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, name := range []string{"alice", "bob"} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
				name, now, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("准备测试账号: %v", err)
	}

	rt, err := newForumRuntime(pools, writer, nil, nil, nil, func() time.Time { return *clock })
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	h := newInputHandler().withForum(rt.Service, sqlite.NewUserStatusReader(pools), rt.Presence)
	return ctx, h, rt, pools, writer
}

// fixedDraftClock 取白天固定时刻，避免成就 gate 的夜间窗口干扰。
func fixedDraftClock() time.Time {
	return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
}

// seedFirstPost 在 node 1 用可控时钟发一条正常主题（含首帖），落库 created_at=当前钟面，
// 同时占用该用户的 60 秒冷却配额，返回主题 ID。
func seedFirstPost(t *testing.T, ctx context.Context, pc *pageController, sess *fakePageSession, rt *forumRuntime) int64 {
	t.Helper()
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "首帖标题")
	pressEnter(t, ctx, sess, pc)
	typeInto(t, ctx, sess, pc, "首帖正文")
	submitBody(t, ctx, sess, pc)
	page, err := rt.Service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("首帖未落库: rows=%d err=%v", len(page.Rows), err)
	}
	return page.Rows[0].ID
}

// TestDraftRetryableErrorClassification 对应 TC-A03：retryableDraftError 只收时机/准入类，
// 内容校验、对象不存在、内部参数错误一律不保留草稿。
func TestDraftRetryableErrorClassification(t *testing.T) {
	rate := &forum.RateLimitError{Reason: "60 秒冷却", RetryAfter: 42 * time.Second}
	if !retryableDraftError(rate) {
		t.Error("RateLimitError 应可重试留草稿")
	}
	if !retryableDraftError(forum.ErrReadOnly) {
		t.Error("ErrReadOnly 应可重试留草稿")
	}
	if !retryableDraftError(forum.ErrForbidden) {
		t.Error("ErrForbidden 应可重试留草稿")
	}

	for _, bad := range []error{
		nil,
		forum.ErrNotFound,
		forum.ErrInvalidNodeID,
		forum.ErrInvalidTitle,
		forum.ErrInvalidBody,
		&forum.ValidationError{Field: "title", Reason: "空白"},
	} {
		if retryableDraftError(bad) {
			t.Errorf("%v 不应保留草稿", bad)
		}
	}

	if _, ok := asRateLimit(rate); !ok {
		t.Error("asRateLimit 应识别 RateLimitError")
	}
	if _, ok := asRateLimit(forum.ErrForbidden); ok {
		t.Error("asRateLimit 不应把 ErrForbidden 当成限流")
	}
}

// TestDraftRateLimitRetainsInPlace 对应 TC-A01：限流走 A 路径，编辑器原地重建，
// 缓冲恰为原正文一次；提交后的新增输出不得再回显第二份正文（不调 pasteBlock，
// 也不向 echo.cols 叠加列宽），保持 StateComposing。
func TestDraftRateLimitRetainsInPlace(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "回复正文")
	colsBefore := len(pc.echo.cols)
	sess.reset()
	submitBody(t, ctx, sess, pc)

	if pc.editor == nil {
		t.Fatal("A 路径应原地保留编辑器，不得回屏")
	}
	if pc.draft == nil {
		t.Fatal("限流后应保存草稿")
	}
	if got := pc.editor.body.Text(); got != "回复正文" {
		t.Fatalf("重建缓冲应为原正文一次，实际 %q", got)
	}
	if pc.draft.body != "回复正文" {
		t.Fatalf("草稿正文应为原正文，实际 %q", pc.draft.body)
	}
	// A 不调 pasteBlock：列宽栈不得叠加第二遍（P0-2 退格错乱根因）。
	// setSecret(false) 复位会清空 cols（§4.4 既定），故只要求不增长。
	if len(pc.echo.cols) > colsBefore {
		t.Fatalf("A 重建不得向 echo.cols 叠加，%d → %d", colsBefore, len(pc.echo.cols))
	}
	if out := sess.output(); strings.Contains(out, "回复正文") {
		t.Fatalf("A 路径不得重复回显正文: %q", out)
	}
	if last := sess.states[len(sess.states)-1]; last != presence.StateComposing {
		t.Fatalf("A 路径应保持 StateComposing，实际 %v", last)
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
}

// TestDraftRateLimitResendAfterCooldown 对应 TC-A02：钟面越过 60 秒冷却后原地重发，
// 正文完整落库，成功后草稿与编辑器一并清除（TC-A07 的 A 分支）。
func TestDraftRateLimitResendAfterCooldown(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "回复正文")
	submitBody(t, ctx, sess, pc)
	if pc.editor == nil || pc.draft == nil {
		t.Fatal("限流后应进入 A 路径")
	}

	clock = clock.Add(61 * time.Second)
	sess.reset()
	submitBody(t, ctx, sess, pc)

	if pc.editor != nil {
		t.Fatal("重发成功后应退出编辑态")
	}
	if pc.draft != nil {
		t.Fatal("重发成功后草稿应清除")
	}
	detail, err := rt.Service.GetThread(ctx, threadID)
	if err != nil || len(detail.Posts) != 2 {
		t.Fatalf("重发未落库: posts=%d err=%v", len(detail.Posts), err)
	}
	if detail.Posts[1].BodyMD != "回复正文" {
		t.Fatalf("回复正文不完整: %q", detail.Posts[1].BodyMD)
	}
}

// TestDraftSuspendedThenResumeOnReply 对应 TC-A08：A 状态按 Esc 是挂起（非取消）——
// 编辑器摘出、草稿保留、回 StateBrowsing 并提示按 r；回到原帖按 r 恢复，
// 正文与原输入逐字一致。
func TestDraftSuspendedThenResumeOnReply(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "挂起正文")
	submitBody(t, ctx, sess, pc)
	if pc.editor == nil || pc.draft == nil {
		t.Fatal("限流后应进入 A 路径")
	}

	// Esc 挂起：摘编辑器、留草稿、回浏览屏。
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyEsc}); err != nil {
		t.Fatal(err)
	}
	if pc.editor != nil {
		t.Fatal("挂起后应摘出编辑器")
	}
	if pc.draft == nil {
		t.Fatal("挂起不得清除草稿")
	}
	if last := sess.states[len(sess.states)-1]; last != presence.StateBrowsing {
		t.Fatalf("挂起后应回 StateBrowsing，实际 %v", last)
	}
	if out := sess.output(); !strings.Contains(out, "按 r") {
		t.Fatalf("挂起提示应告知按 r 恢复: %q", out)
	}

	// 回到原帖按 r 恢复。
	sess.reset()
	if err := pc.key(ctx, sess, actionReply); err != nil {
		t.Fatal(err)
	}
	if pc.editor == nil {
		t.Fatal("按 r 应恢复编辑器")
	}
	if got := pc.editor.body.Text(); got != "挂起正文" {
		t.Fatalf("恢复后正文应与原输入一致，实际 %q", got)
	}
	if last := sess.states[len(sess.states)-1]; last != presence.StateComposing {
		t.Fatalf("恢复后应回 StateComposing，实际 %v", last)
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
}

func setUserStatus(t *testing.T, ctx context.Context, writer *sqlite.Writer, userID int64, status string) {
	t.Helper()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, status, userID)
		return err
	}); err != nil {
		t.Fatalf("更新账号状态: %v", err)
	}
}

// TestDraftFrozenRetainsAndRestores 对应 TC-A04 + TC-A07(B)：
// 冻结账号（ErrForbidden）走 B 路径存草稿、回屏；解冻结后 /reply 恢复，
// 粘贴来源的 CRLF 经 Load 归一化为 LF 逐字节一致；恢复后提交成功清草稿。
func TestDraftFrozenRetainsAndRestores(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, writer := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	// 冻结 alice：事务内 checkWritePermission 拒为 ErrForbidden（B 路径）。
	setUserStatus(t, ctx, writer, 1, "deleting")

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "冻结下的正文")
	// B 路径 submitEditor 预期返回 ErrForbidden（fail() 原样上抛），忽略。
	_ = pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyCtrlD})

	if pc.editor != nil {
		t.Fatal("B 路径应摘出编辑器")
	}
	if pc.draft == nil || pc.draft.body != "冻结下的正文" {
		t.Fatalf("B 应存完整草稿: %+v", pc.draft)
	}
	if out := sess.output(); !strings.Contains(out, "仅本次连接") {
		t.Fatalf("B 文案应含「仅本次连接」: %q", out)
	}

	// 粘贴来源的 CRLF 由 Load 归一化；解冻结并越过冷却后恢复、提交成功（A07）。
	pc.draft.body = "行1\r\n行2"
	setUserStatus(t, ctx, writer, 1, "active")
	clock = clock.Add(61 * time.Second)

	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	if pc.editor == nil {
		t.Fatal("恢复应进入编辑器")
	}
	if got := pc.editor.body.Text(); got != "行1\n行2" {
		t.Fatalf("CRLF 应归一化为 LF 逐字节一致，实际 %q", got)
	}

	submitBody(t, ctx, sess, pc)
	if pc.editor != nil || pc.draft != nil {
		t.Fatal("B 恢复后提交成功应清草稿与编辑器")
	}
	detail, err := rt.Service.GetThread(ctx, threadID)
	if err != nil || len(detail.Posts) != 2 || detail.Posts[1].BodyMD != "行1\n行2" {
		t.Fatalf("恢复后的回复未落库: posts=%d err=%v", len(detail.Posts), err)
	}
}

// TestDraftResumeSanitizesInjection 对应 TC-A05：草稿正文夹带 ESC/CSI/OSC52/
// U+202E/CR 时，恢复回显走 pasteBlock 的净化路径，输出只含白名单 ANSI，
// RLO 等双向控制符被剥离，不污染终端显示。
func TestDraftResumeSanitizesInjection(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	pc.draft = &composerDraft{
		creating: false, threadID: threadID,
		body: "正常\x1b[31m\x1b]52;c;YWJj\x07\u202E反向\r",
	}
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	cmdOnlyWhitelistedANSI(t, out)
	if strings.ContainsRune(out, '\u202E') {
		t.Fatalf("RLO(U+202E) 应被净化剥离: %q", out)
	}
}

// TestDraftActorFailureRetains 对应 TC-A09：actor() 阶段失败（账号不存在，
// ErrNotFound 收敛为 ErrForbidden）同样存草稿，B 文案含「仅本次连接」。
func TestDraftActorFailureRetains(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	owner := newFakePageSession(1)
	pco := h.newPageController(owner)
	threadID := seedFirstPost(t, ctx, pco, owner, rt)

	// 99999 账号不存在：actor() 把 ErrNotFound 收敛为 ErrForbidden。
	sess := newFakePageSession(99999)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	typeInto(t, ctx, sess, pc, "不存在账号正文")
	// actor 失败预期返回 ErrForbidden，忽略。
	_ = pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyCtrlD})

	if pc.editor != nil {
		t.Fatal("actor 失败应摘出编辑器")
	}
	if pc.draft == nil || pc.draft.body != "不存在账号正文" {
		t.Fatalf("actor 失败应存草稿: %+v", pc.draft)
	}
	if out := sess.output(); !strings.Contains(out, "仅本次连接") {
		t.Fatalf("actor 失败 B 文案应含「仅本次连接」: %q", out)
	}
	if last := sess.states[len(sess.states)-1]; last != presence.StateBrowsing {
		t.Fatalf("actor 失败应回 StateBrowsing，实际 %v", last)
	}
}

// TestDraftNewThreadResumeSkipsTitleStage 对应 TC-A10：发帖草稿恢复直接进
// stageBody（标题已带、正文预填），不再经过标题阶段。
func TestDraftNewThreadResumeSkipsTitleStage(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	seedFirstPost(t, ctx, pc, sess, rt) // 占用 60 秒冷却配额

	// 同版块第二次 /new：标题 Enter → 正文 Ctrl-D → CreateThread 限流 A。
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if pc.editor == nil || pc.editor.stage != stageTitle {
		t.Fatal("第二次 /new 应从标题阶段开始")
	}
	typeInto(t, ctx, sess, pc, "新帖标题")
	pressEnter(t, ctx, sess, pc)
	if pc.editor.stage != stageBody || pc.editor.title != "新帖标题" {
		t.Fatalf("标题确认后应进 stageBody 并记住标题: stage=%v title=%q", pc.editor.stage, pc.editor.title)
	}
	typeInto(t, ctx, sess, pc, "新帖正文")
	submitBody(t, ctx, sess, pc)
	if pc.editor == nil || pc.draft == nil {
		t.Fatal("发帖限流应进 A 路径")
	}

	// Esc 挂起后再 /new：恢复直接进 stageBody。
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyEsc}); err != nil {
		t.Fatal(err)
	}
	if pc.editor != nil || pc.draft == nil {
		t.Fatal("挂起应摘编辑器、留草稿")
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if pc.editor == nil || pc.editor.stage != stageBody {
		t.Fatal("发帖草稿恢复应跳过标题阶段直接进 stageBody")
	}
	if pc.editor.title != "新帖标题" {
		t.Fatalf("恢复后标题应已带，实际 %q", pc.editor.title)
	}
	if got := pc.editor.body.Text(); got != "新帖正文" {
		t.Fatalf("恢复后正文应预填，实际 %q", got)
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
}

// TestDraftEditDoesNotConsumeDraft 对应 TC-A11：/edit 不检出回复/发帖草稿，
// 从空缓冲开始，草稿保持不动（§4.6，避免 EditPost 覆盖原文风险）。
func TestDraftEditDoesNotConsumeDraft(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	pc.draft = &composerDraft{creating: false, threadID: threadID, body: "回复草稿"}

	if err := pc.startEditComposer(sess, 1); err != nil {
		t.Fatal(err)
	}
	if pc.editor == nil {
		t.Fatal("/edit 应进入编辑态")
	}
	if got := pc.editor.body.Text(); got != "" {
		t.Fatalf("/edit 必须从空缓冲开始，实际 %q", got)
	}
	if pc.draft == nil || pc.draft.body != "回复草稿" {
		t.Fatal("/edit 不得检出或消费草稿")
	}
}

// TestDraftResumeHintBeforeBody 对应线上 defect 1：恢复提示不得粘连在正文末尾。
// writeFixed 自带尾部 \r\n，提示必须在正文回显之前，否则提示贴正文末字、光标甩下一行。
func TestDraftResumeHintBeforeBody(t *testing.T) {
	clock := fixedDraftClock()
	_, h, _, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	d := &composerDraft{creating: false, threadID: 9, body: "草稿正文"}

	if err := pc.resumeDraft(sess, d); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	hintAt := strings.Index(out, "草稿已恢复")
	bodyAt := strings.Index(out, "草稿正文")
	if hintAt < 0 || bodyAt < 0 {
		t.Fatalf("提示与正文都应出现: %q", out)
	}
	if hintAt > bodyAt {
		t.Fatalf("恢复提示必须在正文之前（避免粘连 + 光标错位）: hint=%d body=%d out=%q", hintAt, bodyAt, out)
	}
	cmdOnlyWhitelistedANSI(t, out)
}

// TestDraftResumeBackspaceErasesPrecisely 对应线上 defect 2：恢复后退格要真正
// 删掉正文末字并正确擦屏。末字「乙」占 2 列，断言 \b \b 恰好 2 组且紧跟正文。
func TestDraftResumeBackspaceErasesPrecisely(t *testing.T) {
	clock := fixedDraftClock()
	ctx, h, _, _, _ := setupDraftRuntime(t, &clock)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.resumeDraft(sess, &composerDraft{creating: false, threadID: 9, body: "草稿甲乙"}); err != nil {
		t.Fatal(err)
	}
	if got := pc.editor.body.Text(); got != "草稿甲乙" {
		t.Fatalf("恢复正文不符: %q", got)
	}

	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyBackspace}); err != nil {
		t.Fatal(err)
	}
	if got := pc.editor.body.Text(); got != "草稿甲" {
		t.Fatalf("退格应删末字，缓冲现为 %q", got)
	}
	out := sess.output()
	if strings.Count(out, "\b \b") != 2 {
		t.Fatalf("末字占 2 列，应有 2 组退格擦除，实际 %d: %q", strings.Count(out, "\b \b"), out)
	}
	// 擦除序列必须紧跟在回显正文之后（同一行），不能夹在提示与正文之间。
	// 恢复时先输出提示（自带 CRLF），再回显正文，故 "草稿甲乙" 后应紧接 2 组 \b \b。
	if !strings.Contains(out, "草稿甲乙\b \b\b \b") {
		t.Fatalf("退格擦除应紧跟回显正文末字（同一行）: %q", out)
	}
	cmdOnlyWhitelistedANSI(t, out)
}
