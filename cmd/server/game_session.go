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
}

func newGameSession(svc *appgame.Service, key domgame.GameKey, userID int64) (*gameSession, error) {
	s, err := domgame.NewSession(key)
	if err != nil {
		return nil, err
	}
	return &gameSession{session: s, service: svc, userID: userID, start: time.Now()}, nil
}

// step 应用一次按键。返回 done=true 表示本局结束（调用方摘掉 pc.game 并复页）。
func (g *gameSession) step(ctx context.Context, sess pageSession, m input.KeyMsg) (bool, error) {
	// 退出键：q / Esc / Ctrl-C。
	if m.Kind == input.KeyEsc || m.Kind == input.KeyCtrlC ||
		(m.Kind == input.KeyRune && (m.Rune == 'q' || m.Rune == 'Q')) {
		return true, nil // 调用方负责 pc.render(sess) 复原原页
	}
	dir, ok := keyDirection(m)
	if !ok {
		// 非方向键：重绘当前帧（忽略无效输入，不报错、不刷屏）。
		return false, g.renderFrame(sess, "")
	}
	frame, err := g.session.Step(dir)
	if err != nil {
		return false, g.renderFrame(sess, "")
	}
	if frame.Over {
		return true, g.settle(ctx, sess, frame)
	}
	return false, g.renderFrame(sess, "")
}

// settle 结算：仅登录用户 + 有效局写榜；访客/无效局给固定提示。
func (g *gameSession) settle(ctx context.Context, sess pageSession, frame domgame.Frame) error {
	var notice string
	if g.service != nil && g.userID > 0 && frame.Valid {
		res, err := g.service.Submit(ctx, string(g.session.Key()), frame.Score, true, g.userID,
			int(time.Since(g.start).Seconds()))
		if err != nil {
			notice = "成绩记录失败，本次未上榜"
		} else if res.Counted {
			notice = fmt.Sprintf("本局 %d 分，已计入周榜（/game top 查看）", frame.Score)
		} else {
			notice = "今日挑战次数已用完，明天再来刷新纪录"
		}
	} else if g.userID == 0 {
		notice = "访客成绩不计榜，登录后可上榜"
	} else {
		notice = "本局未达上榜门槛"
	}
	return g.renderFrame(sess, notice)
}

// renderFrame 把领域帧组装成整屏文本并输出。
func (g *gameSession) renderFrame(sess pageSession, notice string) error {
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
	if notice != "" {
		b.WriteString(systemLine("[提示] "+notice, safe.StyleNotice))
		b.WriteString("\r\n")
	}
	b.WriteString(systemLine(f.Hint, safe.StyleDim))
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
		segSystem(sess, fmt.Sprintf("  %d 分", e.Score))
		segCRLF(sess)
	}
}
