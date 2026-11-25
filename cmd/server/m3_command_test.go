package main

// P1-M3 命令接线测试：/vote 与 /search 从注册表解析到真实 Service + SQLite。
// 这里只验证"接上了、参数没回显、状态变了"，业务规则由 application/infra 测试负责。

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func setupM3Thread(t *testing.T, ctx context.Context, writer *sqlite.Writer, body string) (int64, int64) {
	t.Helper()
	var threadID, postID int64
	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := int64(1_700_000_000)
		res, err := tx.ExecContext(ctx,
			`INSERT INTO threads (node_id,user_id,title,score,last_reply_at,created_at) VALUES (1,1,'M3 主题',0,?,?)`, now, now)
		if err != nil {
			return err
		}
		threadID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,1,?,'normal',?)`, threadID, body, now)
		if err != nil {
			return err
		}
		postID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("准备 M3 数据: %v", err)
	}
	return threadID, postID
}

func TestVoteCommandWiresToService(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, _ := setupM3Thread(t, ctx, writer, "正文")
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatalf("打开主题: %v", err)
	}

	run := func(line string) {
		t.Helper()
		inv, err := h.reg.Parse(line)
		if err != nil {
			t.Fatalf("解析 %q: %v", line, err)
		}
		sess.reset()
		if err := pc.command(ctx, sess, inv); err != nil {
			t.Fatalf("执行 %q: %v", line, err)
		}
	}
	score := func() int64 {
		t.Helper()
		var n int64
		if err := pools.QueryRowContext(ctx, `SELECT score FROM threads WHERE id = ?`, tid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	run("/vote up")
	if score() != 1 {
		t.Fatalf("/vote up 后分数应为 1，实际 %d", score())
	}
	run("/vote up") // 幂等
	if score() != 1 {
		t.Fatalf("重复 /vote up 不应加分，实际 %d", score())
	}
	run("/vote down")
	if score() != -1 {
		t.Fatalf("/vote down 后分数应为 -1，实际 %d", score())
	}
	run("/vote rm")
	if score() != 0 {
		t.Fatalf("/vote rm 后分数应为 0，实际 %d", score())
	}
	// 非法子命令必须给出固定用法提示，不回显用户原文。
	run("/vote bogus")
	if !strings.Contains(sess.output(), "用法：/vote") {
		t.Fatalf("非法投票参数应给固定提示: %q", sess.output())
	}
}

// R-15：/vote 带 id 参数走回复投票，分数记到父主题；非法 id 给固定提示不回显原文。
func TestVoteCommandOnPostWiresToService(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, pid := setupM3Thread(t, ctx, writer, "正文")
	sess := newFakePageSession(2) // 投票者非作者，避免 self-vote 干扰
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatalf("打开主题: %v", err)
	}

	run := func(line string) {
		t.Helper()
		inv, err := h.reg.Parse(line)
		if err != nil {
			t.Fatalf("解析 %q: %v", line, err)
		}
		sess.reset()
		if err := pc.command(ctx, sess, inv); err != nil {
			t.Fatalf("执行 %q: %v", line, err)
		}
	}
	score := func() int64 {
		t.Helper()
		var n int64
		if err := pools.QueryRowContext(ctx, `SELECT score FROM threads WHERE id = ?`, tid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// 带 post id → TargetPost，分数加到父主题（不是回复自身，回复无 score 列）。
	run("/vote up " + strconv.FormatInt(pid, 10))
	if score() != 1 {
		t.Fatalf("回复投票后父主题分数应为 1，实际 %d", score())
	}
	// 父主题投票与回复投票是两条独立记录，可对同一主题各投一票。
	run("/vote up")
	if score() != 2 {
		t.Fatalf("主题与回复各投一票后分数应为 2，实际 %d", score())
	}
	// 撤销回复票只回退它自己那一票。
	run("/vote rm " + strconv.FormatInt(pid, 10))
	if score() != 1 {
		t.Fatalf("撤销回复票后分数应为 1，实际 %d", score())
	}
	// 非正整数 id 给固定提示，不回显原文。
	run("/vote up abc")
	if !strings.Contains(sess.output(), "回复 ID 必须是正整数") {
		t.Fatalf("非法 id 应给固定提示: %q", sess.output())
	}
	if strings.Contains(sess.output(), "abc") {
		t.Fatalf("不得回显用户原文: %q", sess.output())
	}
}

func TestVoteCommandRejectsGuestWithoutWriting(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	tid, _ := setupM3Thread(t, ctx, writer, "正文")
	sess := newFakePageSession(0) // 访客
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	inv, err := h.reg.Parse("/vote up")
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, inv); err == nil {
		t.Fatal("访客投票应返回错误")
	}
	var score int64
	if err := pools.QueryRowContext(ctx, `SELECT score FROM threads WHERE id = ?`, tid).Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		t.Fatalf("访客投票不应改分数，实际 %d", score)
	}
}

func TestSearchCommandShowsResultsAndPaginates(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	_, _ = setupM3Thread(t, ctx, writer, "检索专用词 命中")
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)

	inv, err := h.reg.Parse("/search 检索专用")
	if err != nil {
		t.Fatalf("解析 /search: %v", err)
	}
	if err := pc.command(ctx, sess, inv); err != nil {
		t.Fatalf("执行 /search: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenSearchResults {
		t.Fatalf("应进入检索结果屏，实际 %d", pc.screen.Kind)
	}
	if len(pc.screen.Rows) != 1 || !strings.Contains(pc.screen.Rows[0].Body, "检索专用词") {
		t.Fatalf("结果行应含摘要: %+v", pc.screen.Rows)
	}
	if !strings.Contains(sess.output(), "检索结果") {
		t.Fatalf("终端未显示检索屏标题: %q", sess.output())
	}
	if pc.searchQuery != "检索专用" || pc.searchPage != 1 {
		t.Fatalf("检索态未记录: q=%q page=%d", pc.searchQuery, pc.searchPage)
	}

	// 无更多结果时 n 键不应改变页码。
	if err := pc.key(ctx, sess, actionNextPage); err != nil {
		t.Fatal(err)
	}
	if pc.searchPage != 1 {
		t.Fatalf("无下一页时页码应保持 1，实际 %d", pc.searchPage)
	}
}
