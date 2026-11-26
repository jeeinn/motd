package main

// R-14 / ADR-078 命令接线与渲染测试：验证 /game 接到了 game mode、
// 棋盘走整屏重绘、零 ESC 内联、非法游戏名零回显、访客不上榜。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	appgame "github.com/jeeinn/motd/internal/application/game"
	"github.com/jeeinn/motd/internal/application/input"
	domgame "github.com/jeeinn/motd/internal/domain/game"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func TestGameCommandStartsModeAndRendersCleanFrame(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, err := h.reg.Parse("/game play 2048")
	if err != nil {
		t.Fatal(err)
	}
	sess.reset()
	h.runGame(ctx, sess, pc, inv)
	if pc.game == nil {
		t.Fatal("/game play 应进入 game mode")
	}
	if pc.game.session.Key() != domgame.Key2048 {
		t.Fatalf("应开 2048，实际 %s", pc.game.session.Key())
	}
	out := sess.output()
	if !strings.Contains(out, "\x1b[H\x1b[2J") {
		t.Fatal("棋盘必须走整屏干净重绘（H+2J）")
	}
	if !strings.Contains(out, "motd · 2048") || !strings.Contains(out, "得分 0") {
		t.Fatalf("首帧应含标题与得分: %q", out)
	}
	// 棋盘只用 ASCII（除固定中文提示外，棋盘字符不得含非 ASCII）。
	for _, line := range strings.Split(out, "\r\n") {
		if strings.Contains(line, "得分") || strings.Contains(line, "motd") || strings.Contains(line, "方向") {
			continue
		}
		for _, r := range line {
			if r > 0x7f && r != '\x1b' {
				t.Fatalf("棋盘行含非 ASCII 字符: %q", line)
			}
		}
	}
}

func TestGameUnknownNameZeroEcho(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	// 用户输入含特殊字符的非法游戏名；响应不得回显原文。
	inv, err := h.reg.Parse("/game play <script>alert(1)</script>")
	if err != nil {
		t.Fatal(err)
	}
	sess.reset()
	h.runGame(ctx, sess, pc, inv)
	if pc.game != nil {
		t.Fatal("非法游戏名不应进入 game mode")
	}
	out := sess.output()
	if strings.Contains(out, "<script>") || strings.Contains(out, "alert") {
		t.Fatalf("非法输入被回显: %q", out)
	}
	if !strings.Contains(out, "未知游戏") {
		t.Fatalf("应给固定未知游戏提示: %q", out)
	}
}

func TestGameStepRendersAndQuitExits(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)

	// 方向键驱动一帧。
	sess.reset()
	done, err := pc.game.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight})
	if err != nil || done {
		t.Fatalf("方向键应产生一帧且不结束: done=%v err=%v", done, err)
	}
	if !strings.Contains(sess.output(), "\x1b[H\x1b[2J") {
		t.Fatal("每步都应整屏重绘")
	}
	// q 退出。
	done, err = pc.game.step(ctx, sess, input.KeyMsg{Kind: input.KeyRune, Rune: 'q'})
	if err != nil || !done {
		t.Fatalf("q 应退出: done=%v err=%v", done, err)
	}
}

func TestGameGuestDoesNotWriteScore(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	// userID=0 → 访客。
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)

	// 访客结算路径由 settle 的 userID==0 分支保证不调 Service；
	// 本测试只驱动开局，不产生写。断言 game_scores 表为空。
	if pc.game.userID != 0 {
		t.Fatalf("访客 gameSession.userID 应为 0，实际 %d", pc.game.userID)
	}
	var n int
	if err := pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM game_scores`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("访客局不应写成绩表，实际 %d 行", n)
	}
}

func TestGameBoardCommandShowsEmptyState(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game top")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)
	out := sess.output()
	if !strings.Contains(out, "本周排行榜") || !strings.Contains(out, "暂无记录") {
		t.Fatalf("空榜应给固定文案: %q", out)
	}
}

var _ = command.GroupGame

// ---- 游戏结束态交互回归（2026-11-26 实测反馈）----
// 问题：原实现 frame.Over 时立即 return true → 调用方 pc.game=nil 复原论坛页，
// 用户没看到最终得分/结算就被弹回。修复：结束态停留，Enter/Esc 才退出，
// 方向键忽略。下面用例按此语义断言。

func TestGameOverStateWaitsForConfirm(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)

	// 构造必死：蛇头放到右边界、朝右，下一步撞墙。
	gs := pc.game
	gs.session.ForceStateForTest()

	// 第一步：撞墙 → 进入结束态，但不退出。
	sess.reset()
	done, err := gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight})
	if err != nil || done {
		t.Fatalf("撞墙应进入结束态而非退出: done=%v err=%v", done, err)
	}
	out := sess.output()
	if !strings.Contains(out, "本局结束，按 Enter 或 Esc 退出") {
		t.Fatalf("结束帧应含确认退出提示: %q", out)
	}
	if !strings.Contains(out, "得分") {
		t.Fatalf("结束帧应显示得分: %q", out)
	}

	// 第二步：方向键在结束态必须被忽略（不退出、不重绘）。
	sess.reset()
	done, err = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowLeft})
	if err != nil || done {
		t.Fatalf("结束态方向键不得退出: done=%v err=%v", done, err)
	}
	if sess.output() != "" {
		t.Fatalf("结束态方向键应零输出，实际 %q", sess.output())
	}

	// 第三步：Enter 确认退出。
	done, err = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyEnter})
	if err != nil || !done {
		t.Fatalf("Enter 应确认退出: done=%v err=%v", done, err)
	}
}

func TestGameOverSettlesOnce(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	h.runGame(ctx, sess, pc, inv)

	gs := pc.game
	gs.session.ForceStateForTest()
	frame, err := gs.session.Step(domgame.DirRight)
	if err != nil || !frame.Over {
		t.Fatalf("应撞墙结束: over=%v err=%v", frame.Over, err)
	}
	_ = gs.settleOnce(ctx, frame)
	second := gs.settleOnce(ctx, frame)
	if second != "本局已结算" {
		t.Fatalf("重复结算应返回已结算提示，实际 %q", second)
	}
	var n int
	if err := pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM game_scores`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 1 {
		t.Fatalf("结算不得重复写榜，实际 %d 行", n)
	}
}
