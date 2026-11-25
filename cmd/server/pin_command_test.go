package main

// R-15 / ADR-079 /pin /unpin 命令接线回归：注册表解析 → 页面控制器 →
// 真实 Service + SQLite。权限单点由 application/archtest 守，这里守
// "接上了、mark 落库、回列表、文案固定、每版块 1 条"。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

func markOf(t *testing.T, pools interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ctx context.Context, threadID int64) int8 {
	t.Helper()
	var m int8
	if err := pools.QueryRowContext(ctx, `SELECT mark FROM threads WHERE id = ?`, threadID).Scan(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 无参 /pin：管理员置顶当前主题 → mark=1、回版块列表、固定文案。
func TestPinCommandPinsCurrentThread(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := hideSession(2) // bob 指纹在白名单
	pc := h.newPageController(sess)
	if err := pc.showThreads(ctx, sess, 1, "/dev/ai", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/pin"); err != nil {
		t.Fatalf("管理员置顶应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已置顶当前主题") {
		t.Fatalf("缺少固定成功文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("置顶后应回列表，实际 screen=%d", pc.screen.Kind)
	}
	if got := markOf(t, pools, ctx, tid); got != 1 {
		t.Fatalf("主题 mark 应为 1，实际 %d", got)
	}
}

// /unpin：取消置顶 → mark=0、刷新详情。
func TestUnpinCommandUnpinsCurrentThread(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE threads SET mark = 1 WHERE id = ?`, tid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sess := hideSession(2)
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/unpin"); err != nil {
		t.Fatalf("管理员取消置顶应成功: %v", err)
	}
	if !strings.Contains(sess.output(), "已取消置顶") {
		t.Fatalf("缺少固定文案: %q", sess.output())
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail {
		t.Fatalf("取消置顶后应刷新详情，实际 screen=%d", pc.screen.Kind)
	}
	if got := markOf(t, pools, ctx, tid); got != 0 {
		t.Fatalf("主题 mark 应为 0，实际 %d", got)
	}
}

// 每版块 1 条：pin 新主题会自动清掉同版块旧置顶。
func TestPinCommandKeepsOnlyOnePinnedPerNode(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid1, _, _ := setupDeleteThread(t, ctx, writer, 1)
	tid2, _, _ := setupDeleteThread(t, ctx, writer, 2) // 同版块？setupDeleteThread 固定 node 1
	sess := hideSession(2)
	pc := h.newPageController(sess)

	if err := pc.showDetail(ctx, sess, tid1); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/pin"); err != nil {
		t.Fatal(err)
	}
	if err := pc.showDetail(ctx, sess, tid2); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/pin"); err != nil {
		t.Fatal(err)
	}
	if got := markOf(t, pools, ctx, tid1); got != 0 {
		t.Fatalf("旧置顶应被清除，mark=%d", got)
	}
	if got := markOf(t, pools, ctx, tid2); got != 1 {
		t.Fatalf("新置顶 mark 应为 1，实际 %d", got)
	}
}

// 非管理员指纹执行 /pin 必须被拒（固定文案），mark 不变。
func TestPinCommandRejectsNonAdmin(t *testing.T) {
	ctx, h, pools, writer := setupHideRuntime(t)
	tid, _, _ := setupDeleteThread(t, ctx, writer, 1)
	sess := newFakePageSession(2) // 无白名单指纹
	pc := h.newPageController(sess)
	if err := pc.showDetail(ctx, sess, tid); err != nil {
		t.Fatal(err)
	}
	if err := runHideCommand(t, ctx, h, pc, sess, "/pin"); err != nil {
		t.Fatalf("拒绝应走固定文案而非 error: %v", err)
	}
	if !strings.Contains(sess.output(), "仅管理员") {
		t.Fatalf("非管理员应得固定拒绝文案: %q", sess.output())
	}
	if got := markOf(t, pools, ctx, tid); got != 0 {
		t.Fatalf("非管理员不得改 mark，实际 %d", got)
	}
}
