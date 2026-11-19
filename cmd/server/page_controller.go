package main

// MVP 终端页面控制器（评审稿 v0.2 §2.3 / §3.1 / §6；P0-24 + P0-25 最小集）。
//
// 定位：inputHandler 是进程级单例，页面状态必须**每会话一份**，因此本结构体
// 由 Serve 在连接建立时创建。它只做「输入 → 用例 → ViewModel → Renderer」的编排：
//   - 业务规则全部复用 application/forum.Service（唯一事实来源，原则一）；
//   - 屏幕状态全部复用 application/viewmodel（不含渲染概念，ADR-014）；
//   - 输出全部经 presentation/terminal.RenderScreen → safe.RenderUser/RenderSystem
//     （分层安全渲染，§3.5 / ADR-019），不直接 Write 用户字节。
//
// 明确不做（范围守门，见 docs/MVP-DELIVERY.md）：独立登录引导页由 P0-19
// 最小交互层承接；帮助浮层由 ?/help 注册表文本承接；搜索占位留 P1；
// 不做全屏重绘 / 光标定位，避免引入清屏类 ANSI 破坏 §3.5 的信任域边界。

import (
	"context"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// pageSession 是页面控制器对当前连接的受限视图。*sshd.Session 天然实现；
// 测试用 fake 实现，不必起真实 SSH 就能回归整条业务链路。
type pageSession interface {
	io.Writer
	ID() string
	Principal() auth.Principal
	Window() (width, height uint32, term string)
	SetState(presence.State) error
}

// pageDeps 是进程级共享依赖，每会话只复制一份控制器状态。
type pageDeps struct {
	service  *forum.Service
	users    appuser.StatusReader
	presence *presence.Registry
	reg      *command.Registry
}

type pageController struct {
	service  *forum.Service
	users    appuser.StatusReader
	presence *presence.Registry
	reg      *command.Registry

	screen   viewmodel.Screen
	nodeID   int64
	nodeSlug string
	threadID int64
	cursor   string
	offset   int
	// P1-M3 检索态：关键词与 1-based 页码，供 n/p 翻页与刷新复用。
	searchQuery string
	searchPage  int
	notice      viewmodel.Notice
	editor      *pageEditor
	// 行编辑回显器：与页面状态一样每会话一份（ADR-026）。
	echo *lineEcho
	// 上一屏占用的行数（ADR-027）。>0 表示屏幕连续、可原地重绘；
	// 任何非屏幕输出（提示/回显/登录文本）都会把它清零退回追加模式。
	screenLines int
}

type editorStage uint8

const (
	stageTitle editorStage = iota
	stageBody
)

// pageEditor 是发帖 / 回复的行编辑态（ADR-026）：
// 输入经 lineEcho 净化后回显，提交时由 forum.Service 校验；
// 原始字节（KeyMsg.Raw / Text）永不直接进入输出流。
type pageEditor struct {
	stage    editorStage
	creating bool // true=新主题（需标题+正文），false=回复（只需正文）
	runes    []rune
	title    string
	nodeID   int64
	threadID int64
}

func (h *inputHandler) newPageController(out io.Writer) *pageController {
	return &pageController{
		service:  h.forum,
		users:    h.users,
		presence: h.presence,
		reg:      h.reg,
		screen:   emptyScreen(),
		echo:     newLineEcho(out),
	}
}

// emptyScreen 给「首次查询失败」等场景一个合法、可渲染的空屏。
func emptyScreen() viewmodel.Screen {
	return viewmodel.Screen{
		Kind:      viewmodel.ScreenNodes,
		StatusBar: viewmodel.StatusBar{Identity: viewmodel.Identity{Guest: true}, Level: 1},
	}
}

// ---------------------------------------------------------------------------
// 渲染与 ViewContext
// ---------------------------------------------------------------------------

func (p *pageController) viewContext(sess pageSession) forum.ViewContext {
	principal := sess.Principal()
	return forum.ViewContext{
		Online:    p.online(),
		Active:    p.active(),
		Guest:     !principal.IsUser(),
		RowOffset: p.offset,
		Context:   p.nodeSlug,
		Notice:    p.notice,
	}
}

func (p *pageController) online() int {
	if p.presence == nil {
		return 0
	}
	return p.presence.Online()
}

func (p *pageController) active() int {
	if p.presence == nil {
		return 0
	}
	return p.presence.Active()
}

// render 把当前 Screen 交给终端 Renderer，并在页面层完成 LF→CRLF。
func (p *pageController) render(sess pageSession) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	width, _, _ := sess.Window()
	text, err := terminal.RenderScreen(p.screen, int(width))
	if err != nil {
		// 失败关闭：渲染器不接受畸形 ViewModel / 含控制字符的系统文本时，
		// 宁可只给一行固定提示，也不把可疑内容写进 PTY。
		return p.writeFixed(sess, "页面渲染失败，已停止输出")
	}
	// screenLines>0 → 屏幕连续，原地重绘；=0 → 中间插入过其它输出，追加。
	// RenderFrame 在极窄终端发生自动折行时也会自行降级为追加。
	frame, lines := terminal.RenderFrame(toCRLF(text), p.screenLines, int(width))
	p.screenLines = lines
	_, err = io.WriteString(sess, frame)
	return err
}

// invalidateScreen 由调用方在输出任何非屏幕文本前调用（ADR-027）。
func (p *pageController) invalidateScreen() { p.screenLines = 0 }

// writeFixed 只输出服务端编译期常量，走 RenderSystem 路径。
func (p *pageController) writeFixed(sess pageSession, text string) error {
	p.invalidateScreen() // 非屏幕输出打断连续性，下次渲染必须追加
	rendered, err := safe.RenderSystem(text, safe.StylePlain)
	if err != nil {
		return err
	}
	_, err = io.WriteString(sess, toCRLF(rendered)+"\r\n")
	return err
}

// consumeNotice 让一条提示只在下一次成功渲染里出现一次。
func (p *pageController) setScreen(sc viewmodel.Screen) {
	p.screen = sc
	p.notice = viewmodel.Notice{}
}

func (p *pageController) fail(sess pageSession, err error) error {
	p.notice = forum.NoticeFromError(err)
	p.screen.StatusBar.Notice = p.notice
	if renderErr := p.render(sess); renderErr != nil {
		return renderErr
	}
	p.notice = viewmodel.Notice{}
	return err
}

// ---------------------------------------------------------------------------
// 四个页面（P0-24 收敛后的最小集）
// ---------------------------------------------------------------------------

func (p *pageController) showNodes(ctx context.Context, sess pageSession) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	v := p.viewContext(sess)
	v.RowOffset = 0
	sc, err := p.service.ScreenNodes(ctx, v)
	if err != nil {
		return p.fail(sess, err)
	}
	p.setScreen(sc)
	p.nodeID, p.nodeSlug, p.threadID, p.cursor, p.offset = 0, "", 0, "", 0
	return p.render(sess)
}

func (p *pageController) showThreads(ctx context.Context, sess pageSession, nodeID int64, slug, cursor string, offset int) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	v := p.viewContext(sess)
	v.RowOffset = offset
	sc, err := p.service.ScreenThreads(ctx, forum.ListThreadsQuery{
		NodeID: nodeID, Sort: forum.SortLatest, Cursor: cursor,
	}, v)
	if err != nil {
		return p.fail(sess, err)
	}
	p.setScreen(sc)
	p.nodeID, p.nodeSlug, p.threadID, p.cursor, p.offset = nodeID, slug, 0, cursor, offset
	return p.render(sess)
}

func (p *pageController) showDetail(ctx context.Context, sess pageSession, threadID int64) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	v := p.viewContext(sess)
	v.RowOffset = 0
	sc, err := p.service.ScreenThreadDetail(ctx, threadID, v)
	if err != nil {
		return p.fail(sess, err)
	}
	p.setScreen(sc)
	p.threadID = threadID
	return p.render(sess)
}

func (p *pageController) refresh(ctx context.Context, sess pageSession) error {
	switch p.screen.Kind {
	case viewmodel.ScreenThreadDetail:
		return p.showDetail(ctx, sess, p.threadID)
	case viewmodel.ScreenThreads:
		return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset)
	case viewmodel.ScreenSearchResults:
		return p.showSearch(ctx, sess, p.searchQuery, p.searchPage)
	default:
		return p.showNodes(ctx, sess)
	}
}

// ---------------------------------------------------------------------------
// 导航（P0-25 键位收尾）
// ---------------------------------------------------------------------------

func (p *pageController) move(sess pageSession, delta int) error {
	rows := p.screen.Rows
	if len(rows) == 0 {
		return nil
	}
	min, max := rows[0].Index, rows[len(rows)-1].Index
	next := p.screen.Selection.FocusedIndex + delta
	if next < min {
		next = min
	}
	if next > max {
		next = max
	}
	if next == p.screen.Selection.FocusedIndex {
		return nil
	}
	p.screen.Selection.FocusedIndex = next
	p.screen.Selection.TopRowIndex = next
	return p.render(sess)
}

func (p *pageController) focusedRow() (viewmodel.Row, bool) {
	for _, r := range p.screen.Rows {
		if r.Index == p.screen.Selection.FocusedIndex {
			return r, true
		}
	}
	return viewmodel.Row{}, false
}

func (p *pageController) open(ctx context.Context, sess pageSession) error {
	row, ok := p.focusedRow()
	if !ok {
		return nil
	}
	switch p.screen.Kind {
	case viewmodel.ScreenNodes:
		return p.showThreads(ctx, sess, row.ID, row.Meta.Node, "", 0)
	case viewmodel.ScreenThreads:
		return p.showDetail(ctx, sess, row.ID)
	case viewmodel.ScreenSearchResults:
		return p.showDetail(ctx, sess, row.ID)
	default:
		return nil
	}
}

func (p *pageController) back(ctx context.Context, sess pageSession) error {
	switch p.screen.Kind {
	case viewmodel.ScreenThreadDetail:
		return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset)
	case viewmodel.ScreenThreads:
		return p.showNodes(ctx, sess)
	case viewmodel.ScreenSearchResults:
		return p.showNodes(ctx, sess)
	default:
		return nil
	}
}

func (p *pageController) nextPage(ctx context.Context, sess pageSession) error {
	if p.screen.Kind == viewmodel.ScreenSearchResults {
		if !p.screen.Pagination.HasMore || p.searchPage >= 1<<20 {
			return nil
		}
		return p.showSearch(ctx, sess, p.searchQuery, p.searchPage+1)
	}
	if p.screen.Kind != viewmodel.ScreenThreads || !p.screen.Pagination.HasMore {
		return nil
	}
	offset := p.offset + p.screen.Pagination.Loaded
	return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.screen.Pagination.NextCursor, offset)
}

func (p *pageController) firstPage(ctx context.Context, sess pageSession) error {
	if p.screen.Kind == viewmodel.ScreenSearchResults {
		if p.searchPage <= 1 {
			return nil
		}
		return p.showSearch(ctx, sess, p.searchQuery, 1)
	}
	if p.screen.Kind != viewmodel.ScreenThreads || p.cursor == "" {
		return nil
	}
	return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, "", 0)
}

func (p *pageController) key(ctx context.Context, sess pageSession, a cmdAction) error {
	switch a {
	case actionMoveDown:
		return p.move(sess, 1)
	case actionMoveUp:
		return p.move(sess, -1)
	case actionOpen:
		return p.open(ctx, sess)
	case actionBack:
		return p.back(ctx, sess)
	case actionReply:
		return p.startComposer(sess, false)
	case actionNextPage:
		return p.nextPage(ctx, sess)
	case actionPrevPage:
		return p.firstPage(ctx, sess)
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// 斜杠命令（命令表仍是唯一入口）
// ---------------------------------------------------------------------------

func (p *pageController) command(ctx context.Context, sess pageSession, inv command.Invocation) error {
	switch inv.Name {
	case "top":
		return p.showNodes(ctx, sess)
	case "open":
		// 参数来自用户文本：只做严格数字解析，绝不回显原文。
		id, err := strconv.ParseInt(inv.Args[0], 10, 64)
		if err != nil || id <= 0 {
			return p.writeFixed(sess, "主题 ID 必须是正整数")
		}
		return p.showDetail(ctx, sess, id)
	case "new":
		return p.startComposer(sess, true)
	case "reply":
		return p.startComposer(sess, false)
	case "vote":
		return p.vote(ctx, sess, inv.Args[0])
	case "search":
		return p.showSearch(ctx, sess, strings.Join(inv.Args, " "), 1)
	default:
		return p.writeFixed(sess, "该命令暂未接入（按 ? 查看全部命令）")
	}
}

// ---------------------------------------------------------------------------
// 编辑态（发帖 / 回复）
// ---------------------------------------------------------------------------

func (p *pageController) startComposer(sess pageSession, creating bool) error {
	if !sess.Principal().IsUser() {
		return p.writeFixed(sess, "访客只读，请先 /login github 登录后再发帖")
	}
	if creating {
		if p.screen.Kind != viewmodel.ScreenThreads || p.nodeID <= 0 {
			return p.writeFixed(sess, "请先进入一个版块再发新主题")
		}
	} else if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, "请先打开一个主题再回复")
	}

	stage := stageBody
	if creating {
		stage = stageTitle
	}
	p.editor = &pageEditor{
		stage: stage, creating: creating, nodeID: p.nodeID, threadID: p.threadID,
	}
	// 当前所有编辑字段都非敏感；P1 的邮箱登录密码字段改为 setSecret(true)，
	// 对应 viewmodel.Field.Secret 的语义（回显字符由 Renderer 决定）。
	p.echo.setSecret(false)
	_ = sess.SetState(presence.StateComposing)
	if creating {
		return p.writeFixed(sess, "新建主题：输入标题后按 Enter（Esc 取消）")
	}
	return p.writeFixed(sess, "回复：输入正文后按 Enter（Esc 取消）")
}

func (p *pageController) editorKey(ctx context.Context, sess pageSession, m input.KeyMsg) error {
	e := p.editor
	if e == nil {
		return nil
	}
	switch m.Kind {
	case input.KeyEsc, input.KeyCtrlC:
		p.echo.newline()
		return p.cancelEditor(sess)
	case input.KeyBackspace:
		before := len(e.runes)
		if before > 0 {
			e.runes = e.runes[:before-1]
		}
		p.echo.backspace(len(e.runes) < before)
	case input.KeyRune:
		before := len(e.runes)
		e.runes = append(e.runes, m.Rune)
		p.clampEditor(e)
		// 只回显真正保留下来的字符（超长截断时被丢弃的字符不回显）。
		if len(e.runes) > before {
			p.echo.rune(m.Rune)
		}
	case input.KeyPaste:
		// 粘贴块可含换行，因此多行正文可通过一次粘贴提交。
		before := len(e.runes)
		e.runes = append(e.runes, []rune(m.Text)...)
		p.clampEditor(e)
		p.echo.paste(string(e.runes[before:]))
	case input.KeyEnter:
		p.echo.newline()
		return p.submitEditor(ctx, sess)
	default:
		// 其余按键不产生任何动作，也不回显。
	}
	return nil
}

// clampEditor 在输入侧就把长度收在用例层上限内，避免一次超长提交白跑一趟事务。
func (p *pageController) clampEditor(e *pageEditor) {
	if e.stage == stageTitle {
		if utf8.RuneCountInString(string(e.runes)) > forum.MaxTitleRunes {
			e.runes = []rune(string(e.runes))[:forum.MaxTitleRunes]
		}
		return
	}
	for len(string(e.runes)) > forum.MaxBodyBytes {
		e.runes = e.runes[:len(e.runes)-1]
	}
}

func (p *pageController) cancelEditor(sess pageSession) error {
	p.editor = nil
	_ = sess.SetState(presence.StateBrowsing)
	return p.writeFixed(sess, "（已取消编辑）")
}

func (p *pageController) submitEditor(ctx context.Context, sess pageSession) error {
	e := p.editor
	if e == nil {
		return nil
	}
	text := string(e.runes)
	if strings.TrimSpace(text) == "" {
		e.runes = nil
		return p.writeFixed(sess, "内容不能为空，请重新输入（Esc 取消）")
	}

	if e.stage == stageTitle {
		e.title = text
		e.runes = nil
		e.stage = stageBody
		return p.writeFixed(sess, "标题已接收，输入正文后按 Enter")
	}

	actor, err := p.actor(ctx, sess)
	if err != nil {
		return p.cancelEditorWithNotice(sess, err)
	}

	var writeErr error
	if e.creating {
		_, writeErr = p.service.CreateThread(ctx, actor, forum.CreateThreadInput{
			NodeID: e.nodeID, Title: e.title, Body: text,
		})
	} else {
		_, writeErr = p.service.CreatePost(ctx, actor, forum.CreatePostInput{
			ThreadID: e.threadID, Body: text,
		})
	}

	threadID, nodeID, nodeSlug := e.threadID, e.nodeID, p.nodeSlug
	creating := e.creating
	p.editor = nil
	_ = sess.SetState(presence.StateBrowsing)

	if writeErr != nil {
		return p.fail(sess, writeErr)
	}
	p.notice = viewmodel.Notice{
		Kind: viewmodel.NoticeSuccess, Code: "saved", Message: "已发布",
	}
	if creating {
		return p.showThreads(ctx, sess, nodeID, nodeSlug, "", 0)
	}
	return p.showDetail(ctx, sess, threadID)
}

func (p *pageController) cancelEditorWithNotice(sess pageSession, err error) error {
	p.editor = nil
	_ = sess.SetState(presence.StateBrowsing)
	return p.fail(sess, err)
}

// actor 把认证快照映射成用例层 Actor；账号状态取 DB 现值，写用例还会在
// 事务内二次复核（P0-10 / ADR-020），这里不做权限最终判定。
func (p *pageController) actor(ctx context.Context, sess pageSession) (forum.Actor, error) {
	principal := sess.Principal()
	if !principal.IsUser() {
		return forum.Actor{}, forum.ErrReadOnly
	}
	if p.users == nil {
		return forum.Actor{}, forum.ErrForbidden
	}
	status, err := p.users.Status(ctx, principal.UserID)
	if err != nil {
		return forum.Actor{}, err
	}
	return forum.ActorFromPrincipal(principal, status), nil
}
