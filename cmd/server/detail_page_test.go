package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// R-06 详情屏分页的行为测试：末条回复可达、正文可跨页、p 回第 1 页。

// seedReplies 直接写库造回复，绕过发帖限流（这里只验证渲染与分页，不验证写入）。
func seedReplies(t *testing.T, ctx context.Context, writer *sqlite.Writer, threadID int64, n int) int64 {
	t.Helper()
	const base = int64(1_700_000_000)
	last := int64(0)
	for i := 0; i < n; i++ {
		if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO posts (thread_id, user_id, body_md, status, created_at) VALUES (?, 1, ?, 'normal', ?)`,
				threadID, fmt.Sprintf("回复 #%d", i+1), base+int64(i))
			if err != nil {
				return err
			}
			last, err = res.LastInsertId()
			return err
		}); err != nil {
			t.Fatalf("写入第 %d 条回复: %v", i+1, err)
		}
	}
	return last
}

// replaceBody 改掉主题正文（首帖），用于构造超长正文场景。
func replaceBody(t *testing.T, ctx context.Context, writer *sqlite.Writer, threadID int64, body string) {
	t.Helper()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE posts SET body_md = ? WHERE thread_id = ?
			   AND id = (SELECT MIN(id) FROM posts WHERE thread_id = ?)`,
			body, threadID, threadID)
		return err
	}); err != nil {
		t.Fatalf("替换正文: %v", err)
	}
}

func TestDetailPageReachesLastReply(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	// 24 行终端：正文 1 行时一页约装 17 条单行回复，25 条必然要翻页。
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)
	lastReplyID := seedReplies(t, ctx, writer, threadID, 25)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatalf("打开详情: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("应进入详情屏，实际 %v", pc.screen.Kind)
	}
	if pc.screen.Pagination.Page != 1 {
		t.Fatalf("首屏应为第 1 页，实际 %d", pc.screen.Pagination.Page)
	}
	if !pc.screen.Pagination.HasMore {
		t.Fatal("25 条回复在 24 行终端下应有下一页")
	}

	seen := map[int64]bool{}
	for _, r := range pc.screen.Rows {
		seen[r.ID] = true
	}

	for i := 0; i < 10 && pc.screen.Pagination.HasMore; i++ {
		if err := pc.detailNextPage(ctx, sess); err != nil {
			t.Fatalf("翻页失败: %v", err)
		}
		for _, r := range pc.screen.Rows {
			if seen[r.ID] {
				t.Fatalf("回复 %d 在翻页中重复出现", r.ID)
			}
			seen[r.ID] = true
		}
	}
	if pc.screen.Pagination.HasMore {
		t.Fatal("应已翻到末页")
	}
	// 核心断言：第 25 条必须可见——这正是分页前永远看不到的那条。
	if !seen[lastReplyID] {
		t.Fatalf("末条回复 %d 未出现在任何一页", lastReplyID)
	}
	if len(seen) != 25 {
		t.Fatalf("应覆盖全部 25 条回复，实际 %d", len(seen))
	}
	if pc.screen.Pagination.Page < 2 {
		t.Fatalf("25 条回复至少应翻 2 页，实际页码 %d", pc.screen.Pagination.Page)
	}
}

func TestDetailFirstPageRestoresBody(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)
	seedReplies(t, ctx, writer, threadID, 25)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	if err := pc.detailNextPage(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Pagination.Page != 2 {
		t.Fatalf("n 之后应为第 2 页，实际 %d", pc.screen.Pagination.Page)
	}
	if pc.screen.Detail.BodyTo == 0 {
		t.Fatal("第 2 页应带上正文行窗口（标记正文已翻完），实际未设置")
	}

	if err := pc.detailFirstPage(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Pagination.Page != 1 {
		t.Fatalf("p 应回到第 1 页，实际 %d", pc.screen.Pagination.Page)
	}
	if pc.screen.Detail.BodyFrom != 0 {
		t.Fatalf("回到第 1 页后正文应从第 0 行开始，实际 %d", pc.screen.Detail.BodyFrom)
	}
}

func TestDetailLongBodySpansPages(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	// 40 行正文，24 行终端一屏装不完。
	long := strings.Repeat("正文第一段\n", 20) + strings.Repeat("正文第二段\n", 19)
	replaceBody(t, ctx, writer, threadID, long)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	if len(pc.screen.Rows) != 0 {
		t.Fatalf("长正文的第一页不应塞回复，实际 %d 行", len(pc.screen.Rows))
	}
	if pc.screen.Detail.BodyTo <= pc.screen.Detail.BodyFrom {
		t.Fatal("第一页应给出正文行窗口")
	}
	firstTo := pc.screen.Detail.BodyTo
	if firstTo >= 39 {
		t.Fatalf("39 行正文不应一页装完，实际到第 %d 行", firstTo)
	}

	if err := pc.detailNextPage(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Detail.BodyFrom != firstTo {
		t.Fatalf("第 2 页正文应从第 %d 行续起，实际 %d", firstTo, pc.screen.Detail.BodyFrom)
	}
}

// R-06 P2：没有更多回复时不能提示 n——此前详情屏固定写「n 下一页」但按键空转。
func TestDetailFooterOmitsNextWhenNoMore(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	sess.reset()
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if strings.Contains(out, "n 下一页") {
		t.Fatalf("只有正文、没有更多回复时不应提示 n：%q", out)
	}
	if strings.Contains(out, "还有更多") {
		t.Fatalf("没有更多内容时不应提示还有更多：%q", out)
	}
	if strings.Contains(out, "第 1 页") {
		t.Fatalf("单页详情不应显示页码：%q", out)
	}
}

// 反向：有更多回复时，footer 与提示都必须出现，且页码正确。
func TestDetailFooterShowsNextWhenMore(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)
	seedReplies(t, ctx, writer, threadID, 25)

	sess.reset()
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if !strings.Contains(out, "n 下一页") {
		t.Fatalf("有更多回复时 footer 应提示 n：%q", out)
	}
	if !strings.Contains(out, "第 1 页") {
		t.Fatalf("分页详情应显示页码：%q", out)
	}
	if strings.Contains(out, "p 回第 1 页") {
		t.Fatalf("第 1 页不该提示 p：%q", out)
	}
}

// 回复后的末页视图：p 能回第 1 页，n 不再有意义（没有下一页）。
func TestDetailTailAfterReplyCanReturnFirstPage(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)
	seedReplies(t, ctx, writer, threadID, 25)

	if err := pc.showDetailAtReply(ctx, sess, threadID, 0); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Pagination.HasMore {
		t.Fatal("末页不应再有下一页")
	}
	if !pc.screen.Pagination.HasPrev {
		t.Fatal("末页之前还有回复时 HasPrev 应为 true")
	}
	if pc.screen.Pagination.Page != 0 {
		t.Fatalf("末页不写页码（不知道第几页），实际 %d", pc.screen.Pagination.Page)
	}
	// n 在末页应是空操作，不能跳回第 1 页。
	if err := pc.detailNextPage(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Pagination.Page != 0 {
		t.Fatalf("末页按 n 应无变化，实际页码 %d", pc.screen.Pagination.Page)
	}
	// p 回第 1 页。
	if err := pc.detailFirstPage(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Pagination.Page != 1 {
		t.Fatalf("p 应回到第 1 页，实际 %d", pc.screen.Pagination.Page)
	}
	if pc.screen.Detail.BodyFrom != 0 {
		t.Fatalf("第 1 页正文应从第 0 行起，实际 %d", pc.screen.Detail.BodyFrom)
	}
}

// 真机回归 1：正文已翻完的回复页，不得把整段正文再渲染一遍。
// 根因曾是"窗口为空(BodyFrom==BodyTo)"与"整段输出"共用同一个 0/0 值。
func TestDetailReplyPageOmitsConsumedBody(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	long := strings.Repeat("正文第一段\n", 40)
	replaceBody(t, ctx, writer, threadID, long)
	seedReplies(t, ctx, writer, threadID, 25)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	// 翻到"正文已翻完且有回复"的那一页：正文与回复同页衔接是设计行为，
	// 真正要防的是正文翻完之后还被整段重画。
	for i := 0; i < 12 && !(len(pc.screen.Rows) > 0 && pc.screen.Detail.BodyFrom == pc.screen.Detail.BodyTo); i++ {
		if err := pc.detailNextPage(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if len(pc.screen.Rows) == 0 {
		t.Fatal("应已翻到含回复的页")
	}
	if !pc.screen.Detail.BodyWindowed {
		t.Fatal("详情屏必须显式标记正文窗口，否则会被当整段渲染")
	}
	if pc.screen.Detail.BodyFrom != pc.screen.Detail.BodyTo {
		t.Fatalf("正文应已翻完（窗口为空），实际 [%d,%d)", pc.screen.Detail.BodyFrom, pc.screen.Detail.BodyTo)
	}

	sess.reset()
	if err := pc.render(sess); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.output(), "正文第一段") {
		t.Fatalf("回复页不应重复渲染正文：%q", sess.output())
	}
}

// 真机回归 2：本页没有回复但后面还有页时，不能说「暂无回复」。
func TestDetailNoRowsWithMoreHidesEmptyHint(t *testing.T) {
	ctx, h, rt, _, writer := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	replaceBody(t, ctx, writer, threadID, strings.Repeat("第一段\n", 40))
	seedReplies(t, ctx, writer, threadID, 5)

	sess.reset()
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	if len(pc.screen.Rows) != 0 || !pc.screen.Pagination.HasMore {
		t.Fatalf("前提：首页应为纯正文且有后续，实际 rows=%d hasMore=%v", len(pc.screen.Rows), pc.screen.Pagination.HasMore)
	}
	if strings.Contains(sess.output(), "暂无回复") {
		t.Fatalf("后面还有回复时不应显示「暂无回复」：%q", sess.output())
	}
}
