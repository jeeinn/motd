package main

// R-14 / ADR-078 命令接线与渲染测试：验证 /game 接到了 game mode、
// 棋盘走整屏重绘、零 ESC 内联、非法游戏名零回显、访客不上榜。

import (
	"strings"
	"testing"
	"time"

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
// 用户没看到最终得分/结算就被弹回。修复：结束态停留，方向键忽略；
// Enter 再来一局，Esc 退出（2026-11-26 键位调整）。

func TestGameOverStateWaitsForConfirm(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)

	// 构造必死：蛇头放到右边界、朝右，下一步撞墙。
	// running=true 跳过暂停态首键（新语义：首个方向键只启动不推进）。
	gs := pc.game
	gs.session.ForceStateForTest()
	gs.running = true

	// 第一步：撞墙 → 进入结束态，但不退出。
	sess.reset()
	done, err := gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight})
	if err != nil || done {
		t.Fatalf("撞墙应进入结束态而非退出: done=%v err=%v", done, err)
	}
	out := sess.output()
	if !strings.Contains(out, "本局结束，按 Enter 再来一局 / Esc 退出") {
		t.Fatalf("结束帧应含重开/退出提示: %q", out)
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

	// 第三步：Enter 重开一局（不退出、状态复位、渲染新首帧）。
	oldSession := gs.session
	sess.reset()
	done, err = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyEnter})
	if err != nil || done {
		t.Fatalf("Enter 应重开而非退出: done=%v err=%v", done, err)
	}
	if gs.session == oldSession || gs.over || gs.settled || gs.running {
		t.Fatal("Enter 后应重建 session 并复位所有状态")
	}
	if !strings.Contains(sess.output(), "已暂停 · 按方向键开始") {
		t.Fatalf("重开后应显示暂停态新首帧: %q", sess.output())
	}

	// 第四步：Esc 退出。
	done, err = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyEsc})
	if err != nil || !done {
		t.Fatalf("Esc 应退出: done=%v err=%v", done, err)
	}
}

// 结束态 Enter 重开后仍能正常结算写榜（新局独立计分，不继承旧局 settled）。
func TestGameRestartResetsSettlement(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	h.runGame(ctx, sess, pc, inv)
	gs := pc.game
	gs.session.ForceStateForTest()
	gs.running = true
	_, _ = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight}) // 撞墙结算
	if !gs.over || !gs.settled {
		t.Fatal("撞墙后应结算")
	}
	_, _ = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyEnter}) // 重开
	if gs.settled || gs.over {
		t.Fatal("重开应复位结算标记")
	}
	// 新局再次撞墙应能重新结算（不可因旧 settled 被跳过）。
	gs.session.ForceStateForTest()
	gs.running = true
	_, _ = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight})
	var n int
	if err := pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM game_scores`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// 两局都是无效局（未吃食物），本就不写榜；这里只断言流程不卡死。
	if n != 0 {
		t.Fatalf("无效局不应写榜，实际 %d 行", n)
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

// 2026-11-26 实测 bug 端到端回归：结算写榜后 /game top 无参必须能看到。
func TestGameTopAfterSubmitShowsScore(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	// 模拟一次有效局结算（登录用户 uid=1）。
	res, err := h.gameSvc.Submit(ctx, "2048", 1228, true, 1, 60)
	if err != nil || !res.Counted {
		t.Fatalf("结算应计入: %+v err=%v", res, err)
	}
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game top")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)
	out := sess.output()
	if !strings.Contains(out, "1228") || !strings.Contains(out, "[2048]") {
		t.Fatalf("无参周榜应显示刚结算的成绩与游戏名: %q", out)
	}
	if strings.Contains(out, "暂无记录") {
		t.Fatalf("不应显示空榜: %q", out)
	}
}

// ---- Snake 暂停态与自动移动（2026-11-26）----

func TestSnakeStartsPausedAndDirectionKeyStarts(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	sess.reset()
	h.runGame(ctx, sess, pc, inv)
	if !strings.Contains(sess.output(), "按方向键开始") {
		t.Fatalf("进入应为暂停态并提示开始: %q", sess.output())
	}
	gs := pc.game
	if gs.running {
		t.Fatal("刚进入不应自动运行")
	}
	headBefore := gs.session.Frame().Rows[5][5].Text // 初始蛇头位置标记

	// 首个方向键：启动但不推进蛇身。
	sess.reset()
	done, err := gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowRight})
	if err != nil || done {
		t.Fatalf("首键应启动不退出: done=%v err=%v", done, err)
	}
	if !gs.running {
		t.Fatal("首键后应进入运行态")
	}
	if !strings.Contains(sess.output(), "方向键改向") {
		t.Fatalf("运行态提示应更新: %q", sess.output())
	}
	headAfter := gs.session.Frame().Rows[5][5].Text
	if headBefore != headAfter || gs.session.Frame().Rows[5][6].Text == "@" {
		t.Fatalf("首键不应推进蛇身（@ 应仍在 x=5）: before=%q after=%q", headBefore, headAfter)
	}
}

func TestSnakeTickAutoAdvancesWithThrottle(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	h.runGame(ctx, sess, pc, inv)
	gs := pc.game
	gs.running = true
	now := time.Now()
	gs.lastTick = now

	// 间隔内 tick：不推进（蛇头仍在 x=5）。
	sess.reset()
	_, _ = gs.tick(ctx, sess, now.Add(50*time.Millisecond))
	if sess.output() != "" {
		t.Fatalf("节流期内应零输出，实际 %q", sess.output())
	}
	if gs.session.Frame().Rows[5][6].Text == "@" {
		t.Fatal("节流期内蛇头不应移动")
	}

	// 超过间隔 tick：自动前进一步（@ 到 x=6）。
	sess.reset()
	_, _ = gs.tick(ctx, sess, now.Add(snakeStepInterval+10*time.Millisecond))
	if !strings.Contains(sess.output(), "\x1b[H\x1b[2J") {
		t.Fatal("自动推进应整屏重绘")
	}
	if gs.session.Frame().Rows[5][6].Text != "@" {
		t.Fatalf("超间隔应自动前进一步到 x=6，实际第5行: %v", gs.session.Frame().Rows[5])
	}
}

func TestSnakeDirectionKeyChangesHeading(t *testing.T) {
	ctx, h, _, pools, writer := setupMVPRuntime(t)
	h.gameSvc = appgame.NewService(sqlite.NewGameStore(pools, writer), nil, nil)
	sess := newFakePageSession(1)
	pc := h.newPageController(sess)
	inv, _ := h.reg.Parse("/game play snake")
	h.runGame(ctx, sess, pc, inv)
	gs := pc.game
	// 首键向上：只启动并改向，不推进。
	_, _ = gs.step(ctx, sess, input.KeyMsg{Kind: input.KeyArrowUp})
	if gs.session.CurrentDirection() != domgame.DirUp {
		t.Fatal("首键应改变朝向")
	}
	if gs.session.Frame().Rows[5][5].Text != "@" {
		t.Fatal("首键不应推进蛇身")
	}
	// 下一次 tick 应沿新方向（向上）前进。
	now := time.Now()
	gs.lastTick = now.Add(-snakeStepInterval - time.Second)
	_, _ = gs.tick(ctx, sess, now)
	if gs.session.Frame().Rows[4][5].Text != "@" {
		t.Fatal("自动推进应沿新方向向上")
	}
}
