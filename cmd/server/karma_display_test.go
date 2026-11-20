package main

// v0.5.0 / P3-M2 / ADR-057：状态栏 karma 显示回归。
// 验证 pageController.viewContext 从 DB 读 karma 并派生等级；访客恒 0/L1；
// 未实现 KarmaReader 的 fake 保持零值（可选接口断言的兼容路径）。

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/jeeinn/motd/internal/domain/user"
)

func TestViewContextFillsKarmaFromDB(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	// 给用户 1 (alice) 设置 karma=12 → 应为 L2（阈值 10）。
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE users SET karma = 12 WHERE id = 1`)
		return err
	}); err != nil {
		t.Fatalf("设置 karma: %v", err)
	}
	_ = pools // 已由 setup 持有

	pc := h.newPageController(&bytes.Buffer{})
	sess := newFakePageSession(1)
	vc := pc.viewContext(sess)
	if vc.Karma != 12 {
		t.Fatalf("viewContext 应读到 karma=12，实际 %d", vc.Karma)
	}
	if got := user.Level(vc.Karma); got != 2 {
		t.Fatalf("karma=12 应派生 L2，实际 L%d", got)
	}
}

func TestViewContextGuestKarmaStaysZero(t *testing.T) {
	_, h, _, _, _ := setupMVPRuntime(t)
	pc := h.newPageController(&bytes.Buffer{})
	vc := pc.viewContext(newFakePageSession(0)) // 访客
	if vc.Karma != 0 {
		t.Fatalf("访客 karma 应为 0，实际 %d", vc.Karma)
	}
	if !vc.Guest {
		t.Fatal("访客会话应标记 Guest")
	}
}
