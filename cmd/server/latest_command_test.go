package main

// /latest 跨版块列表的接线回归（v0.7.1 / P4.5-M2 / ADR-063）。
// 页面链路不重写，这里只锁「接上了、上下文对了、导航闭环了」：
//   - 任意屏 /latest 进跨版块列表（nodeID=0，状态栏位置=latest）；
//   - 列表行带版块 slug，版块内列表不带；
//   - Enter 开主题 → q 回跨版块列表（保持上下文）；
//   - /latest 上 /new 给出固定提示，不崩、不静默；
//   - 检索结果打开详情后按 q 回版块列表，而不是「位置为空」的空白列表。

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func seedThread(t *testing.T, ctx context.Context, writer *sqlite.Writer,
	nodeID, userID int64, title string, lastReply int64) int64 {
	t.Helper()
	var id int64
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at)
			 VALUES (?, ?, ?, 0, ?, ?)`, nodeID, userID, title, lastReply, lastReply)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	}); err != nil {
		t.Fatalf("准备主题 %q: %v", title, err)
	}
	return id
}

func TestLatestCommandShowsCrossNodeList(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	seedThread(t, ctx, writer, 1, 1, "AI 版块的帖子", 1000)
	seedThread(t, ctx, writer, 2, 2, "Rust 版块的帖子", 2000)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.reset()

	if err := pc.command(ctx, sess, command.Invocation{Name: "latest"}); err != nil {
		t.Fatalf("/latest 执行失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("应进入主题列表屏，实际 %v", pc.screen.Kind)
	}
	if pc.nodeID != 0 || pc.nodeSlug != latestScope {
		t.Fatalf("跨版块上下文应为 (0, %q)，实际 (%d, %q)", latestScope, pc.nodeID, pc.nodeSlug)
	}
	if len(pc.screen.Rows) != 2 {
		t.Fatalf("应有两行（两个版块各一条），实际 %d", len(pc.screen.Rows))
	}
	// 排序：last_reply_at DESC → Rust 在前。
	if pc.screen.Rows[0].ID != 2 {
		t.Fatalf("应按最后回复时间倒序，首行应为 id=2，实际 %d", pc.screen.Rows[0].ID)
	}
	if pc.screen.Rows[0].Meta.Node != "/dev/rust" || pc.screen.Rows[1].Meta.Node != "/dev/ai" {
		t.Fatalf("跨版块行必须带版块 slug，实际 %q / %q",
			pc.screen.Rows[0].Meta.Node, pc.screen.Rows[1].Meta.Node)
	}
	out := stripSGR(sess.output())
	if !strings.Contains(out, "位置: latest") {
		t.Errorf("状态栏应显示「位置: latest」，实际输出:\n%s", out)
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
}

// 版块内列表保持原样：不填 slug（既有断言不破）。
func TestThreadListInsideNodeHasNoSlug(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	seedThread(t, ctx, writer, 1, 1, "版块内帖子", 1000)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showThreads(ctx, sess, 1, "/dev/ai", "", 0); err != nil {
		t.Fatalf("版块内列表渲染失败: %v", err)
	}
	if len(pc.screen.Rows) != 1 {
		t.Fatalf("应有一行，实际 %d", len(pc.screen.Rows))
	}
	if got := pc.screen.Rows[0].Meta.Node; got != "" {
		t.Fatalf("版块内列表不应填 slug（会与状态栏重复），实际 %q", got)
	}
	if !strings.Contains(stripSGR(sess.output()), "位置: /dev/ai") {
		t.Errorf("状态栏应显示本次进入的版块，实际输出:\n%s", sess.output())
	}
}

// 导航闭环：/latest → Enter 开主题 → q 回跨版块列表（游标/上下文保持）→ /top 回版块列表。
func TestLatestNavigationLoop(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	seedThread(t, ctx, writer, 1, 1, "帖一", 1000)
	seedThread(t, ctx, writer, 2, 2, "帖二", 2000)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, command.Invocation{Name: "latest"}); err != nil {
		t.Fatal(err)
	}

	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatalf("Enter 打开主题失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail || pc.threadID == 0 {
		t.Fatalf("应进入详情页，实际 kind=%v threadID=%d", pc.screen.Kind, pc.threadID)
	}
	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatalf("q 返回失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads || pc.nodeID != 0 || pc.nodeSlug != latestScope {
		t.Fatalf("q 应回到跨版块列表，实际 kind=%v nodeID=%d slug=%q",
			pc.screen.Kind, pc.nodeID, pc.nodeSlug)
	}
	if len(pc.screen.Rows) != 2 {
		t.Fatalf("返回后应保持原列表，实际 %d 行", len(pc.screen.Rows))
	}

	if err := pc.command(ctx, sess, command.Invocation{Name: "top"}); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatalf("/top 应回版块列表，实际 %v", pc.screen.Kind)
	}
}

// /latest 上 /new：没有版块上下文，必须给固定提示而不是建帖或崩溃。
func TestLatestRejectsNewThreadWithoutNode(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, command.Invocation{Name: "latest"}); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatalf("/new 应给出提示而不是报错: %v", err)
	}
	if !strings.Contains(sess.output(), "请先进入一个版块再发新主题") {
		t.Fatalf("应给出固定提示，实际:\n%s", sess.output())
	}
	if pc.editor != nil {
		t.Fatal("/latest 上不应进入发帖编辑器")
	}
}

// 检索结果 → 详情 → q：nodeID/nodeSlug 被清零的残缺状态必须回版块列表，
// 不能落到「位置: 」（空）的主题列表屏。
func TestBackFromSearchDetailGoesToNodeList(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	threadID := seedThread(t, ctx, writer, 1, 1, "可检索主题", 1000)
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id, user_id, body_md, status, created_at)
			 VALUES (?, 1, 'uniquetermfortest', 'normal', 1000)`, threadID)
		return err
	}); err != nil {
		t.Fatalf("准备检索内容: %v", err)
	}

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showSearch(ctx, sess, "uniquetermfortest", 1); err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenSearchResults || len(pc.screen.Rows) == 0 {
		t.Fatalf("应命中一条，实际 kind=%v rows=%d", pc.screen.Kind, len(pc.screen.Rows))
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatalf("打开检索结果失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("应进入详情页，实际 %v", pc.screen.Kind)
	}
	sess.reset()
	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatalf("返回失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatalf("检索来源的详情应回版块列表，实际 kind=%v（nodeID=%d slug=%q）",
			pc.screen.Kind, pc.nodeID, pc.nodeSlug)
	}
	if strings.Contains(sess.output(), "位置: \r\n") {
		t.Fatalf("不应出现空的位置行，实际:\n%s", sess.output())
	}
}

// 空列表提示不能写「该版块」：同一个屏也承载跨版块列表。
func TestLatestEmptyHintDoesNotSayNode(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0) // 访客
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "latest"}); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if !strings.Contains(out, "暂无主题") {
		t.Fatalf("空列表应给出提示，实际:\n%s", out)
	}
	if strings.Contains(out, "该版块暂无主题") {
		t.Fatalf("跨版块屏不应说「该版块」，实际:\n%s", out)
	}
}

// sgrRe 匹配 SGR（颜色/样式）序列：断言文案时先剥掉样式，
// 否则「位置: 」与值之间夹着 ESC[...m 会让子串匹配失败。
var sgrRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripSGR(s string) string { return sgrRe.ReplaceAllString(s, "") }
