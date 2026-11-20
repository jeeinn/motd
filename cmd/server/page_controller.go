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
	"errors"
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
	// P1-M5 邮箱认证：pending 状态每会话一份，/register|/recover 后供
	// /verify 使用；重连即失效（验证码 TTL 仅 10 分钟）。emailFlow 非空时优先吃按键。
	pendingEmail   string
	pendingPurpose string
	emailFlow      *emailFlow
	// v0.3.1：命令输入提示每会话只打一次。pageController 由 Serve 每连接新建，
	// 所以重连是新会话、会重新打印一次；绝不放到进程级 inputHandler 上。
	entryHintShown bool
}

type editorStage uint8

const (
	stageTitle editorStage = iota
	stageBody
)

// 编辑态提示文案（服务端编译期常量，走 RenderSystem / 固定文本路径）。
//
// P1-M4 硬约束：正文是**多行**——Enter 换行、单独一行 "." 或 Ctrl-D 提交。
// 绝不能把「Ctrl 与 Enter 的组合键」写进主提示：通用 SSH 终端字节流层无法
// 可靠区分该组合与普通 Enter，P1 不做主提交键（二轮拍板，ADR-034）；
// archtest + 单测双重断言。
const (
	composerTitleHint          = "新建主题：输入标题后按 Enter 确认（Esc 取消）"
	composerBodyHint           = "回复：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）"
	composerBodyAfterTitleHint = "标题已接收：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）"
	composerLimitHint          = "正文已达长度上限，请精简后继续（Esc 取消）"
)

// pageEditor 是发帖 / 回复的编辑态（ADR-026 + P1-M4 多行）：
//   - title 阶段：单行 runes 缓冲，Enter 确认（标题仍保持单行语义）；
//   - body 阶段：正文交给纯应用逻辑 input.Composer，Enter 换行、哨兵/Ctrl-D 提交。
//
// 两阶段的输入都经 lineEcho 净化后回显，提交时由 forum.Service 校验；
// 原始字节（KeyMsg.Raw / Text）永不直接进入输出流。
type pageEditor struct {
	stage    editorStage
	creating bool            // true=新主题（需标题+正文），false=回复（只需正文）
	runes    []rune          // 仅 stageTitle 使用：标题单行缓冲
	body     *input.Composer // 仅 stageBody 使用：多行正文缓冲
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

// maybeShowCommandEntryHint 只在本次会话首次按 / 进入命令输入时打印提示，
// 之后静默。实测一次会话里 help/top/verify/recover 后各刷一次，滚动严重。
// out 用 io.Writer 是为了能在没有真实 SSH 会话的单元测试里回归。
func (p *pageController) maybeShowCommandEntryHint(out io.Writer) {
	if p.entryHintShown {
		return
	}
	p.entryHintShown = true
	_, _ = io.WriteString(out, commandEntryHint)
}

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
	case "delete":
		return p.delete(ctx, sess, inv.Args)
	case "search":
		return p.showSearch(ctx, sess, strings.Join(inv.Args, " "), 1)
	case "node":
		return p.nodeCommand(ctx, sess, inv)
	default:
		return p.writeFixed(sess, "该命令暂未接入（按 ? 查看全部命令）")
	}
}

// delete 的确切口径（v0.2.1）：
//   - /delete        → 删除当前打开的主题；
//   - /delete <id>   → 删除当前主题下 ID 为 <id> 的回复。
//
// 两者都要求当前处于主题详情页（目标明确、删后导航确定）。
// id 严格按十进制正整数解析，与 /open 同款：不回显用户原文。
// 非作者 / 访客 / 目标不存在统一由用例层给出「仅作者可删除」。
func (p *pageController) delete(ctx context.Context, sess pageSession, args []string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, "请先打开一个主题再删除")
	}

	targetType, targetID := forum.DeleteThread, p.threadID
	if len(args) > 0 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return p.writeFixed(sess, "ID 必须是正整数")
		}
		targetType, targetID = forum.DeletePost, id
	}

	res, err := p.service.Delete(ctx, p.deleteActor(ctx, sess), targetType, targetID)
	if err != nil {
		return p.fail(sess, err)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "deleted", Message: "已删除"}
	if res.TargetType == forum.DeleteThread {
		// 主题已软删，回列表；node 上下文在 showDetail 时保持不变。
		return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset)
	}
	return p.showDetail(ctx, sess, p.threadID)
}

// deleteActor 只提供所有权判定需要的 UserID：访客保持零值，由 canModify 统一拒绝。
// 刻意不复用 p.actor（它给访客 ErrReadOnly），否则访客与非作者会得到两种提示，
// 破坏「仅作者可删除」的防枚举口径。
func (p *pageController) deleteActor(ctx context.Context, sess pageSession) forum.Actor {
	principal := sess.Principal()
	if !principal.IsUser() {
		return forum.Actor{Guest: true}
	}
	a := forum.Actor{UserID: principal.UserID}
	if p.users != nil {
		if status, err := p.users.Status(ctx, principal.UserID); err == nil {
			a.Status = status
		}
	}
	return a
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
	e := &pageEditor{
		stage: stage, creating: creating, nodeID: p.nodeID, threadID: p.threadID,
		body: input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes)),
	}
	if stage == stageBody {
		e.body.Start()
	}
	p.editor = e
	// 当前所有编辑字段都非敏感；P1 的邮箱登录密码字段改为 setSecret(true)，
	// 对应 viewmodel.Field.Secret 的语义（回显字符由 Renderer 决定）。
	p.echo.setSecret(false)
	_ = sess.SetState(presence.StateComposing)
	if creating {
		return p.writeFixed(sess, composerTitleHint)
	}
	return p.writeFixed(sess, composerBodyHint)
}

// editorKey 是编辑态的单入口：取消键两阶段共用，其余按键按阶段分流。
func (p *pageController) editorKey(ctx context.Context, sess pageSession, m input.KeyMsg) error {
	e := p.editor
	if e == nil {
		return nil
	}
	switch m.Kind {
	case input.KeyEsc, input.KeyCtrlC:
		p.echo.newline()
		return p.cancelEditor(sess)
	}
	if e.stage == stageTitle {
		return p.titleKey(ctx, sess, m)
	}
	return p.bodyKey(ctx, sess, m)
}

// titleKey 保持标题的单行语义：Enter 确认，不换行、不走 composer。
func (p *pageController) titleKey(ctx context.Context, sess pageSession, m input.KeyMsg) error {
	e := p.editor
	switch m.Kind {
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
		// 标题是单行字段：粘贴块里的换行在标题里压成一个空格，避免把
		// 多行内容塞进标题（正文的多行粘贴由 composer 原样保留）。
		text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", " "), "\n", " ")
		before := len(e.runes)
		e.runes = append(e.runes, []rune(text)...)
		p.clampEditor(e)
		p.echo.paste(string(e.runes[before:]))
	case input.KeyEnter:
		p.echo.newline()
		return p.submitEditor(ctx, sess)
	default:
		// 其余按键不产生任何动作，也不回显。Ctrl-D 在标题阶段**不**提交，
		// 保持「标题 Enter 确认」的单一语义。
	}
	return nil
}

// bodyKey 把正文按键交给 composer，再按返回的语义动作回显。
// 上限类错误只输出一句固定提示，被拒的字符不回显、状态不变。
func (p *pageController) bodyKey(ctx context.Context, sess pageSession, m input.KeyMsg) error {
	e := p.editor
	if e.body == nil {
		e.body = input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes))
	}
	if !e.body.Active() {
		e.body.Start()
	}
	op, err := e.body.Feed(m)
	if err != nil {
		if errors.Is(err, input.ErrTooManyLines) || errors.Is(err, input.ErrTooManyBytes) {
			return p.writeFixed(sess, composerLimitHint)
		}
		return nil
	}
	switch op.Kind {
	case input.OpRune:
		p.echo.rune(op.Rune)
	case input.OpPaste:
		p.echo.pasteBlock(op.Text)
	case input.OpBackspace:
		if op.MergedLines {
			p.echo.mergeBackspace(op.MergedText)
		} else {
			p.echo.backspace(op.Deleted)
		}
	case input.OpNewline, input.OpSubmit:
		p.echo.newline()
		if op.Kind == input.OpSubmit {
			return p.submitEditor(ctx, sess)
		}
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
	if e.stage == stageBody && e.body != nil {
		text = e.body.Text()
	}
	if strings.TrimSpace(text) == "" {
		if e.stage == stageBody && e.body != nil {
			e.body.Start() // 提交后已失活，空内容退回时重新开始，允许继续输入
		}
		e.runes = nil
		return p.writeFixed(sess, "内容不能为空，请重新输入（Esc 取消）")
	}

	if e.stage == stageTitle {
		e.title = text
		e.runes = nil
		e.stage = stageBody
		if e.body == nil {
			e.body = input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes))
		}
		e.body.Start()
		return p.writeFixed(sess, composerBodyAfterTitleHint)
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
