package main

// /game 会话态与渲染（R-14 / ADR-078）。
//
// game mode 挂在 pageController（类比 emailFlow），每会话一份。本文件只做编排：
//   - 把归一化按键映射为领域 Direction
//   - 把领域 Frame 组装成纯文本（所有样式经 safe.RenderSystem，零内联 ESC）
//   - 整屏走 RenderCleanFrame（H+2J），scrollback 零增长（ADR-072/076）
//   - 结算调 application/game.Service.Submit（服务端算分，无客户端分数入参）
//
// 退出后由调用方 pc.game=nil + pc.render(sess) 复原原页，不改 screen/notice/selection。
import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/command"
	appgame "github.com/jeeinn/motd/internal/application/game"
	"github.com/jeeinn/motd/internal/application/input"
	domgame "github.com/jeeinn/motd/internal/domain/game"
	"github.com/jeeinn/motd/internal/presentation/terminal"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// gameSession 是每会话一份的游戏态。
type gameSession struct {
	session *domgame.Session
	service *appgame.Service
	userID  int64 // 0=访客（可玩不上榜）
	start   time.Time
	// R-14 游戏结束态：棋盘满/撞墙后不立即退页，先停在结束画面让用户看清
	// 最终得分与结算提示，等 Enter / Esc / q 确认再退出。方向键在结束态被忽略。
	over    bool
	settled bool // 结算只做一次（结束态可能多次按键，防重复写榜）

	// Snake 自动移动（2026-11-26）：进入为暂停态，方向键启动后由会话循环
	// 按固定间隔推进。节奏由 cmd 层持有（archtest 守卫状态机零 ticker）。
	running  bool
	lastTick time.Time
}

// snakeStepInterval 是蛇自动前进间隔。180ms ≈ 5.5 格/秒，手速可跟且不致瞬间撞墙；
// 这是**会话循环**的节流值，不是状态机的定时器（ADR-078 / archtest 边界）。
const snakeStepInterval = 180 * time.Millisecond

func newGameSession(svc *appgame.Service, key domgame.GameKey, userID int64) (*gameSession, error) {
	s, err := domgame.NewSession(key)
	if err != nil {
		return nil, err
	}
	return &gameSession{session: s, service: svc, userID: userID, start: time.Now()}, nil
}

// step 应用一次按键。返回 done=true 表示**用户确认退出**（调用方摘掉 pc.game 并复页）。
//
// 两态交互（2026-11-26 实测反馈修复）：
//   - 进行中：方向键/WASD 走一步；q/Esc/Ctrl-C 中途放弃并直接退出。
//   - 结束态（over）：棋盘停在最后一帧并给出「得分 + 结算 + 按 Enter/ Esc 退出」
//     提示；此时方向键一律忽略（不再喂领域机，否则 ErrSessionOver 会被当无效键
//     静默重绘），只有 Enter / Esc / q / Ctrl-C 才确认退出。
func (g *gameSession) step(ctx context.Context, sess pageSession, m input.KeyMsg) (bool, error) {
	if g.over {
		// 结束态：Enter / Esc / q / Ctrl-C 确认退出，其余键（含方向键）保持结束画面。
		if m.Kind == input.KeyEnter || isGameQuitKey(m) {
			return true, nil
		}
		return false, nil // 不重绘、不退出：避免方向键把用户直接弹回论坛页
	}
	// 进行中：q / Esc / Ctrl-C 中途放弃。
	if isGameQuitKey(m) {
		return true, nil
	}
	dir, ok := keyDirection(m)
	if !ok {
		// 非方向键：重绘当前帧（忽略无效输入，不报错、不刷屏）。
		return false, g.renderFrame(sess, "")
	}
	// Snake 暂停态：第一个方向键只设定朝向并启动自动移动，本次不推进蛇身
	// （避免启动瞬间白走一格，用户还没反应就撞墙）。后续方向键负责改向。
	if g.session.Key() == domgame.KeySnake && !g.running {
		g.running = true
		g.lastTick = time.Now()
		// 设定方向但不移动：领域机没有"只转向"的公开方法，借 Step 前向一步后
		// 立即回退不可行（有随机食物）。改为在 snakeState 加 Turn 方法（见下）。
		g.session.Turn(dir)
		return false, g.renderFrame(sess, "")
	}
	return g.advance(ctx, sess, dir)
}

// advance 推进一帧（方向键改向 / 自动 tick 共用），处理结束态与结算。
func (g *gameSession) advance(ctx context.Context, sess pageSession, dir domgame.Direction) (bool, error) {
	frame, err := g.session.Step(dir)
	if err != nil {
		return false, g.renderFrame(sess, "")
	}
	if frame.Over {
		// 进入结束态：先结算一次，再把「得分 + 结算提示 + 确认退出提示」
		// 渲染在最后一帧上，停留等用户确认，不立即返回论坛页。
		g.over = true
		g.running = false
		notice := g.settleOnce(ctx, frame)
		return false, g.renderFrame(sess, notice, "本局结束，按 Enter 或 Esc 退出")
	}
	return false, g.renderFrame(sess, "")
}

// tick 由会话循环（cmd 层）以固定节奏调用：Snake 启动后按当前方向自动前进。
// 节流在本方法内做——主循环的 ESC-poll ticker 约 25ms，直接每次推进会太快。
// ctx/sess 用于结算；返回是否因本 tick 退出（结束态不退出，恒 false）。
func (g *gameSession) tick(ctx context.Context, sess pageSession, now time.Time) (bool, error) {
	if g == nil || g.over || !g.running || g.session.Key() != domgame.KeySnake {
		return false, nil
	}
	if now.Sub(g.lastTick) < snakeStepInterval {
		return false, nil
	}
	g.lastTick = now
	return g.advance(ctx, sess, g.session.CurrentDirection())
}

// isGameQuitKey 是进行中/结束态通用的放弃键（q / Esc / Ctrl-C）。
func isGameQuitKey(m input.KeyMsg) bool {
	return m.Kind == input.KeyEsc || m.Kind == input.KeyCtrlC ||
		(m.Kind == input.KeyRune && (m.Rune == 'q' || m.Rune == 'Q'))
}

// settleOnce 结算：仅登录用户 + 有效局写榜；访客/无效局给固定提示。
// settled 防重复——结束态多次按键只写一次榜。
func (g *gameSession) settleOnce(ctx context.Context, frame domgame.Frame) string {
	if g.settled {
		return "本局已结算"
	}
	g.settled = true
	if g.service != nil && g.userID > 0 && frame.Valid {
		res, err := g.service.Submit(ctx, string(g.session.Key()), frame.Score, true, g.userID,
			int(time.Since(g.start).Seconds()))
		if err != nil {
			return "成绩记录失败，本次未上榜"
		}
		if res.Counted {
			return fmt.Sprintf("本局 %d 分，已计入周榜（/game top 查看）", frame.Score)
		}
		return "今日挑战次数已用完，明天再来刷新纪录"
	}
	if g.userID == 0 {
		return "访客成绩不计榜，登录后可上榜"
	}
	return "本局未达上榜门槛"
}

// renderFrame 把领域帧组装成整屏文本并输出。notices 是可变行——每行独立走
// RenderSystem 并单独补 CRLF，**绝不把 \r 拼进 RenderSystem 输入**（它拒绝控制字符，
// 会静默丢弃整段，参见 writeGameBoard 的同类修正）。
func (g *gameSession) renderFrame(sess pageSession, notices ...string) error {
	var b strings.Builder
	title := "motd · " + string(g.session.Key())
	b.WriteString(systemLine(title, safe.StyleAccent))
	b.WriteString("\r\n")
	f := g.session.Frame()
	for _, row := range f.Rows {
		b.WriteString(renderGameRow(row))
		b.WriteString("\r\n")
	}
	b.WriteString(systemLine(fmt.Sprintf("得分 %d", f.Score), safe.StylePlain))
	b.WriteString("\r\n")
	for _, notice := range notices {
		if notice != "" {
			b.WriteString(systemLine("[提示] "+notice, safe.StyleNotice))
			b.WriteString("\r\n")
		}
	}
	if !g.over {
		// 结束态已有「按 Enter 或 Esc 退出」提示行，不再叠加领域的「按 q 退出」。
		hint := f.Hint
		if g.session.Key() == domgame.KeySnake {
			if !g.running {
				hint = "已暂停 · 按方向键开始，蛇会自动前进"
			} else {
				hint = "方向键改向 · q 退出"
			}
		}
		b.WriteString(systemLine(hint, safe.StyleDim))
	}
	return writeGameCleanFrame(sess, b.String())
}

// systemLine 走 safe.RenderSystem（固定文案，不含用户数据）。
func systemLine(text string, style safe.Style) string {
	s, err := safe.RenderSystem(text, style)
	if err != nil {
		return text // 失败关闭：固定文案已是可信内容，直接输出也无注入面
	}
	return s
}

// renderGameRow 把一行格子按 Ink 分段上色。每段走一次 safe.RenderSystem，
// SGR 只来自白名单常量（ADR-027），绝不内联 ESC。同色相邻格合并为一段，
// 减少 SGR 跳变（带宽考量，ADR-076 教训）。
func renderGameRow(row []domgame.Cell) string {
	if len(row) == 0 {
		return ""
	}
	var b strings.Builder
	segStart := 0
	for i := 1; i <= len(row); i++ {
		if i == len(row) || row[i].Ink != row[segStart].Ink {
			var text strings.Builder
			for j := segStart; j < i; j++ {
				text.WriteString(row[j].Text)
			}
			b.WriteString(systemLine(text.String(), inkStyle(row[segStart].Ink)))
			segStart = i
		}
	}
	return b.String()
}

func inkStyle(ink domgame.Ink) safe.Style {
	switch ink {
	case domgame.InkBoard:
		return safe.StyleDim
	case domgame.InkMerged, domgame.InkHead, domgame.InkFood:
		return safe.StyleNotice
	case domgame.InkBody, domgame.InkValue:
		return safe.StyleAccent
	default:
		return safe.StylePlain
	}
}

func writeGameCleanFrame(sess pageSession, text string) error {
	_, height, _ := sess.Window()
	if height == 0 {
		height = 24
	}
	frame, _ := terminal.RenderCleanFrame(toCRLF(text), int(height))
	_, err := io.WriteString(sess, frame)
	return err
}

// keyDirection 把归一化按键映射为领域方向。WASD 与方向键等价。
func keyDirection(m input.KeyMsg) (domgame.Direction, bool) {
	switch m.Kind {
	case input.KeyArrowUp:
		return domgame.DirUp, true
	case input.KeyArrowDown:
		return domgame.DirDown, true
	case input.KeyArrowLeft:
		return domgame.DirLeft, true
	case input.KeyArrowRight:
		return domgame.DirRight, true
	case input.KeyRune:
		switch m.Rune {
		case 'w', 'W':
			return domgame.DirUp, true
		case 's', 'S':
			return domgame.DirDown, true
		case 'a', 'A':
			return domgame.DirLeft, true
		case 'd', 'D':
			return domgame.DirRight, true
		}
	}
	return 0, false
}

// ---- /game 命令处理 ----

// runGame 处理 /game 子命令。所有错误只给固定文案，绝不回显用户原始输入
// （ADR-026 零回显；Name/Sub 来自注册表白名单可安全分支）。
func (h *inputHandler) runGame(ctx context.Context, sess pageSession, pc *pageController, inv command.Invocation) {
	if h.gameSvc == nil {
		writeSystem(sess, "游戏服务尚未就绪\r\n")
		return
	}
	switch inv.Sub {
	case "", "list":
		writeSystem(sess,
			"可玩游戏（/game play <name>）：\r\n"+
				"  2048   方向键/WASD 合并数字，凑出更大方块\r\n"+
				"  snake  方向键/WASD 控蛇吃食物，别撞墙\r\n"+
				"访客可玩但不上榜；登录后成绩计入周榜（/game top）\r\n")
	case "play":
		if pc.game != nil {
			writeSystem(sess, "已有游戏进行中，按 q 退出后再开新局\r\n")
			return
		}
		key := domgame.GameKey(inv.Args[0])
		if !key.Valid() {
			// 零回显：无论用户输入什么，只给固定提示。
			writeSystem(sess, "未知游戏，请输入 /game list 查看可玩列表\r\n")
			return
		}
		uid := sess.Principal().UserID // 访客为 0
		gs, err := newGameSession(h.gameSvc, key, uid)
		if err != nil {
			writeSystem(sess, "游戏启动失败，请稍后再试\r\n")
			return
		}
		pc.game = gs
		_ = gs.renderFrame(sess, "")
	case "top":
		allTime := len(inv.Args) > 0 && inv.Args[0] == "all"
		var key string
		if allTime && len(inv.Args) > 1 {
			key = inv.Args[1]
		} else if !allTime && len(inv.Args) > 0 {
			key = inv.Args[0]
		}
		if key != "" && !domgame.GameKey(key).Valid() {
			writeSystem(sess, "未知游戏，请输入 /game list 查看可玩列表\r\n")
			return
		}
		var entries []appgame.Entry
		var err error
		title := "本周排行榜"
		if allTime {
			title = "历史最佳荣誉墙"
			entries, err = h.gameSvc.AllTime(ctx, key, 10)
		} else {
			title = "本周排行榜（" + h.gameSvc.CurrentWeekKey() + "）"
			entries, err = h.gameSvc.Top(ctx, key, 10)
		}
		if err != nil {
			writeSystem(sess, "排行榜查询失败，请稍后再试\r\n")
			return
		}
		writeGameBoard(sess, title, entries)
	default:
		writeSystem(sess, "用法：/game [list|play <name>|top [all] [game]]\r\n")
	}
}

// writeGameBoard 分段渲染榜单：标题/名次/分数走 RenderSystem，用户名逐段 RenderUser
// （ADR-019/026 信任域隔离；archtest/author_projection_test.go 静态守卫）。
func writeGameBoard(sess io.Writer, title string, entries []appgame.Entry) {
	// 分段渲染：换行用 segCRLF 单独写，绝不把 \r 拼进 RenderSystem
	// （safe.RenderSystem 拒绝控制字符，含 \r 会静默丢弃整段）。
	segSystem(sess, title)
	segCRLF(sess)
	if len(entries) == 0 {
		segSystem(sess, "暂无记录，成为第一个上榜的人")
		segCRLF(sess)
		return
	}
	for i, e := range entries {
		segSystem(sess, fmt.Sprintf(" %2d. ", i+1))
		segUser(sess, "@"+e.Username)
		// 跨游戏榜（/game top 无参）显示来源游戏；game_key 来自服务端白名单常量，
		// 仍走 RenderSystem 固定文案路径，不拼用户数据。
		if e.GameKey != "" && domgame.GameKey(e.GameKey).Valid() {
			segSystem(sess, " ["+e.GameKey+"]")
		}
		segSystem(sess, fmt.Sprintf("  %d 分", e.Score))
		segCRLF(sess)
	}
}
