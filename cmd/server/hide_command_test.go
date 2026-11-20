package main

// P2-M1b /hide 命令接线回归：注册表解析 → 页面控制器 → 真实 Service + SQLite。
// 权限与软删语义由 application / sqlite 测试守；这里守「接上了、口径对、不回显原文」。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

const hideFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 合法指纹格式

// 与 newFakePageSession 的区别是带上服务端会话指纹（它在白名单里）。
func hideSession(userID int64) *fakePageSession {
	p := auth.Guest(hideFP)
	if userID > 0 {
		p = auth.Principal{Kind: auth.KindUser, UserID: userID, KeyFingerprint: hideFP}
	}
	return &fakePageSession{principal: p}
}

func setupHideRuntime(t *testing.T) (context.Context, *inputHandler, *sqlite.Pools, *sqlite.Writer) {
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
	now := time.Now().Unix()
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
		t.Fatalf("准备账号: %v", err)
	}
	rt, err := newForumRuntime(pools, writer, nil, map[string]struct{}{hideFP: {}}, nil)
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	h := newInputHandler().withForum(rt.Service, sqlite.NewUserStatusReader(pools), rt.Presence)
	return ctx, h, pools, writer
}

func runHideCommand(t *testing.T, ctx context.Context, h *inputHandler, pc *pageController, sess *fakePageSession, line string) error {
	t.Helper()
	inv, err := h.reg.Parse(line)
	if err != nil {
		return err // 参数边界测试直接断言解析错误
	}
	sess.reset()
	return pc.command(ctx, sess, inv)
}

// 无参 /hide：管理员隐藏当前主题（作者是他人），回列表，回复行保留。
func TestHideCommandHidesCurrentThread(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, firstID, replyID := setupDeleteThread(t, ctx, writer, 1) // 作者 alice(1)
	sess := hideSession(2)                               // bob(2) 指纹在白名单
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide"); err != nil {
		t.Fatalf("管理员隐藏主题应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已隐藏主题") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("隐藏主题后应回列表，实际 screen=%d", pc.screen.Kind)
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "hidden" {
		t.Fatalf("主题应写 hidden，实际 %q", got)
	}
	if got := statusOf(t, pools, ctx, "posts", firstID); got != "normal" ||
		statusOf(t, pools, ctx, "posts", replyID) != "normal" {
		t.Fatal("隐藏主题不得级联改回复行")
	}
}

// 带 id：管理员隐藏他人回复，主题与其它回复不受影响，停留在详情刷新。
func TestHideCommandHidesReplyByID(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, firstID, replyID := setupDeleteThread(t, ctx, writer, 1) // 作者 alice(1)
	spamID := insertReply(t, ctx, writer, tid, 1)                // 另一条 alice 的回复
	sess := hideSession(2)                                // bob(2) 在白名单
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide "+stringInt(spamID)); err != nil {
		t.Fatalf("管理员隐藏他人回复应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已隐藏回复") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("隐藏回复后应停留在详情，实际 screen=%d", pc.screen.Kind)
	}
	if got := statusOf(t, pools, ctx, "posts", spamID); got != "hidden" {
		t.Fatalf("回复应写 hidden，实际 %q", got)
	}
	for _, id := range []int64{firstID, replyID} {
		if got := statusOf(t, pools, ctx, "posts", id); got != "normal" {
			t.Fatalf("其它回复不得被牵连 id=%d: %q", id, got)
		}
	}
	if got := statusOf(t, pools, ctx, "threads", tid); got != "active" {
		t.Fatalf("主题不得被牵连: %q", got)
	}
}

func insertReply(t *testing.T, ctx context.Context, writer *sqlite.Writer, threadID, userID int64) int64 {
	t.Helper()
	var id int64
	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO posts (thread_id,user_id,body_md,status,created_at) VALUES (?,?,'qqqhidereply','normal',?)`,
			threadID, userID, time.Now().Unix())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("准备回复: %v", err)
	}
	return id
}

// 越权：指纹不在白名单的登录用户与访客都拿到固定文案，数据零变更。
func TestHideCommandDeniesNonAdminWithFixedText(t *testing.T) {
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
			if err := pc.showDetail(ctx, sc.sess, tid); err != nil {
				t.Fatal(err)
			}
			if err := runHideCommand(t, ctx, h, pc, sc.sess, "/hide"); err == nil {
				t.Fatal("越权隐藏应失败并给固定提示")
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
				t.Fatalf("越权后主题不得变更: %q", got)
			}
		})
	}
}

// bootstrap：未绑定账号的访客只要指纹在白名单，也能隐藏他人回复。
func TestHideCommandAllowsWhitelistedGuest(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, replyID := setupDeleteThread(t, ctx, writer, 1)
	sess := nodeAdminSession(0, hideFP, true)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide "+stringInt(replyID)); err != nil {
		t.Fatalf("白名单访客隐藏应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已隐藏回复") {
		t.Fatalf("访客治理被拒: %q", sess.output())
	}
	if got := statusOf(t, pools, ctx, "posts", replyID); got != "hidden" {
		t.Fatalf("访客治理未落库: %q", got)
	}
}

// 参数边界与不回显原文：多于一个参数 / 非法 ID / 不在详情页。
func TestHideCommandArgBoundariesAndNoEcho(t *testing.T) {
	for line, want := range map[string]error{
		"/hide a b": command.ErrArgCount,
		"/hide 1 2": command.ErrArgCount,
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
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide "+malicious); err != nil {
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
	if err := runHideCommand(t, ctx, h, pc2, sess2, "/hide"); err != nil {
		t.Fatalf("无目标应给固定提示: %v", err)
	}
	if !strings.Contains(sess2.output(), "请先打开一个主题再隐藏") {
		t.Fatalf("缺少明确提示: %q", sess2.output())
	}
}

// 目标不存在：固定文案「对象不存在」，不暴露事务内部链，不回显 ID 文本。
func TestHideCommandNotFoundUsesFixedText(t *testing.T) {
	ctx, h, _, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/hide 987654"); err == nil {
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

func stringInt(id int64) string {
	return strconv.FormatInt(id, 10)
}
