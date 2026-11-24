package main

// ADR-066：详情返回主题列表 / 主题列表返回节点列表时，按行 ID 回填焦点，
// 未命中回退首行。本文件只守 back() 的焦点回填；导航与重绘既有测试另置。

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// indexOfRow 按行 ID 返回页内 Index；找不到返回 0。
func indexOfRow(pc *pageController, id int64) int {
	for _, r := range pc.screen.Rows {
		if r.ID == id {
			return r.Index
		}
	}
	return 0
}

// T1：打开第二行原帖 → q 返回，焦点应落在原帖行，而不是首行。
func TestBackFromDetailRestoresThreadFocus(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	// last_reply_at DESC：B(2000) 在首行，A(1000) 在第二行。
	idA := seedThread(t, ctx, writer, 1, 1, "返回焦点验收原帖", 1000)
	seedThread(t, ctx, writer, 1, 1, "另一帖子", 2000)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showThreads(ctx, sess, 1, "/dev/ai", "", 0); err != nil {
		t.Fatalf("进入版块: %v", err)
	}
	if err := pc.key(ctx, sess, actionMoveDown); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreadDetail || pc.threadID != idA {
		t.Fatalf("应打开原帖 A，实际 kind=%v threadID=%d", pc.screen.Kind, pc.threadID)
	}

	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatalf("q 返回失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads {
		t.Fatalf("应回到主题列表，实际 %v", pc.screen.Kind)
	}
	want := indexOfRow(pc, idA)
	if want == 0 {
		t.Fatal("返回后的列表里找不到原帖行")
	}
	if pc.screen.Selection.FocusedIndex != want {
		t.Fatalf("返回后焦点应在原帖行 Index=%d，实际 %d", want, pc.screen.Selection.FocusedIndex)
	}
}

// T2：原帖已被软删、ID 不在新列表 → 未命中回退首行，不报错不落空焦点。
func TestBackFromDetailDeletedThreadFallsBackToFirst(t *testing.T) {
	ctx, h, _, _, writer := setupMVPRuntime(t)
	idA := seedThread(t, ctx, writer, 1, 1, "将被删除的原帖", 1000)
	seedThread(t, ctx, writer, 1, 1, "保留的帖子", 2000)

	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showThreads(ctx, sess, 1, "/dev/ai", "", 0); err != nil {
		t.Fatalf("进入版块: %v", err)
	}
	if err := pc.key(ctx, sess, actionMoveDown); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	// 软删原帖：列表重查时不再出现。
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE threads SET status = 'deleted' WHERE id = ?`, idA)
		return err
	}); err != nil {
		t.Fatalf("软删原帖: %v", err)
	}

	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatalf("q 返回失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads || len(pc.screen.Rows) == 0 {
		t.Fatalf("应回到非空主题列表，实际 kind=%v rows=%d", pc.screen.Kind, len(pc.screen.Rows))
	}
	first := pc.screen.Rows[0].Index
	if pc.screen.Selection.FocusedIndex != first {
		t.Fatalf("原帖缺失应回退首行 Index=%d，实际 %d", first, pc.screen.Selection.FocusedIndex)
	}
}

// T3：节点列表打开第二个节点 → q 返回节点列表，焦点回到原节点行。
func TestBackFromThreadListRestoresNodeFocus(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatalf("首屏版块列表: %v", err)
	}
	// 种子节点顺序：1=/dev/ai（首行），2=/dev/rust；下移到第二行打开。
	if err := pc.key(ctx, sess, actionMoveDown); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads || pc.nodeID != 2 {
		t.Fatalf("应打开 /dev/rust（nodeID=2），实际 kind=%v nodeID=%d", pc.screen.Kind, pc.nodeID)
	}

	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatalf("q 返回失败: %v", err)
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatalf("应回到节点列表，实际 %v", pc.screen.Kind)
	}
	want := indexOfRow(pc, 2)
	if want == 0 {
		t.Fatal("返回后的节点列表里找不到 nodeID=2")
	}
	if pc.screen.Selection.FocusedIndex != want {
		t.Fatalf("返回后焦点应在原节点行 Index=%d，实际 %d", want, pc.screen.Selection.FocusedIndex)
	}
}
