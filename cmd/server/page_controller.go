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
// 最小交互层承接；帮助浮层由 ?/help 注册表文本承接；搜索占位留 P1。
// v0.9.0 / ADR-072：页面改为整屏干净重绘（ESC[H + ESC[2J），不再记忆上一屏
// 行数、也不再走「追加 vs 原地覆盖」双模式——每次渲染都从左上角重画。

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/presentation/terminal"
	"github.com/jeeinn/motd/internal/presentation/terminal/i18n"
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

// latestScope 是跨版块列表屏在状态栏「位置」里显示的固定串（v0.7.1 / ADR-063）。
// 它不是真实版块 slug——nodes.slug 必须以 / 开头（forum.ValidateSlug），
// 因此这个假 slug 不会与任何真实版块冲突，也不会被用来查询版块。
const latestScope = "latest"

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
	// R-06 详情屏分页：当前页起点、已加载回复批次与游标，随会话存活。
	detail detailState
	cursor string
	offset int
	// P1-M3 检索态：关键词与 1-based 页码，供 n/p 翻页与刷新复用。
	searchQuery string
	searchPage  int
	notice      viewmodel.Notice
	editor      *pageEditor
	// draft 是提交失败保留的临时草稿（仅本次连接内存），与 editor 分离：
	// 编辑器摘除后草稿仍在，供 B 回屏兜底与 A 挂起后按 r 恢复（§4.1）。
	draft *composerDraft
	// v0.6.0：Konami 码检测与解锁回调（每会话一份状态机）。
	konami   command.KonamiDetector
	onUnlock func(context.Context, int64) error
	// unlockedNodes 返回该用户已解锁的隐藏节点列表，供 viewContext 注入 ScreenNodes。
	unlockedNodes func(context.Context, int64) ([]node.Node, error)
	// 行编辑回显器：与页面状态一样每会话一份（ADR-026）。
	echo *lineEcho
	// locale 是会话显示语言（默认 zh），只用于服务端固定文案/前缀查表；
	// 绝不塞进 viewmodel.Screen——RenderScreen 仍是不locale的纯函数（见 view.go）。
	locale string
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

// 编辑态提示文案：引导与 5 条草稿状态已统一迁入 i18n.hintCatalog
// （internal/presentation/terminal/i18n，10 key × 2 locale），调用点只传 key，
// 默认 zh 文案与 v0.8.4 逐字节一致；前缀由 writeHint 单点查表拼接。
//
// P1-M4 硬约束：正文是**多行**——Enter 换行、单独一行 "." 或 Ctrl-D 提交。
// 绝不能把「Ctrl 与 Enter 的组合键」写进主提示：通用 SSH 终端字节流层无法
// 可靠区分该组合与普通 Enter，P1 不做主提交键（二轮拍板，ADR-034）；
// archtest + 单测双重断言。
//
// 超限错误走 writeFixed 普通色、不进 hintCatalog（D3 本期范围外）。
const composerLimitHint = "正文已达长度上限，请精简后继续（Esc 取消）"

// pageEditor 是发帖 / 回复的编辑态（ADR-026 + P1-M4 多行）：
//   - title 阶段：单行 runes 缓冲，Enter 确认（标题仍保持单行语义）；
//   - body 阶段：正文交给纯应用逻辑 input.Composer，Enter 换行、哨兵/Ctrl-D 提交。
//
// 两阶段的输入都经 lineEcho 净化后回显，提交时由 forum.Service 校验；
// 原始字节（KeyMsg.Raw / Text）永不直接进入输出流。
type pageEditor struct {
	stage      editorStage
	creating   bool            // true=新主题（需标题+正文），false=回复（只需正文）
	editPostID int64           // >0=编辑既有正文（重新输入全文），提交走 EditPost；0=发帖/回复
	runes      []rune          // 仅 stageTitle 使用：标题单行缓冲
	body       *input.Composer // 仅 stageBody 使用：多行正文缓冲
	title      string
	nodeID     int64
	threadID   int64
}

// composerDraft 是提交失败后保留的输入草稿，仅存于当前 SSH 连接的控制器内存。
// 不落库、不进日志、不进 RenderSystem；回显走粘贴同一条净化路径（ADR-026）。
// 读写收敛到 saveDraft / resumeDraft / matches，与 A/B 交互解耦（§4.1）。
type composerDraft struct {
	creating bool   // true=新主题（含已确认标题）；false=回复
	title    string // 发帖已确认的标题（回复为空）
	body     string // 正文全文，LF 连接
	nodeID   int64  // 发帖上下文
	threadID int64  // 回复上下文
}

// matches 按发帖/回复上下文判定草稿是否可沿用；nil 草稿永不命中。
func (d *composerDraft) matches(creating bool, nodeID, threadID int64) bool {
	if d == nil {
		return false
	}
	if creating {
		return d.creating && d.nodeID == nodeID
	}
	return !d.creating && d.threadID == threadID
}

func (h *inputHandler) newPageController(out io.Writer) *pageController {
	// 宽度从会话取得（fakePageSession 可配宽度用于窄屏测试）；out 只是普通
	// 写入器时宽度未知，lineEcho 退化为旧的物理行假设，兼容既有单测。
	width := 0
	if w, ok := out.(interface {
		Window() (uint32, uint32, string)
	}); ok {
		cols, _, _ := w.Window()
		width = int(cols)
	}
	return &pageController{
		service:       h.forum,
		users:         h.users,
		presence:      h.presence,
		reg:           h.reg,
		onUnlock:      h.onUnlock,
		unlockedNodes: h.unlockedNodes,
		screen:        emptyScreen(),
		echo:          newLineEchoWithWidth(out, width),
		locale:        i18n.LocaleZH,
	}
}

// newPageControllerWithLocale 供双语测试注入 locale；默认 zh 仍走 newPageController。
func (h *inputHandler) newPageControllerWithLocale(out io.Writer, locale string) *pageController {
	pc := h.newPageController(out)
	pc.setLocale(locale)
	return pc
}

// setLocale 切换会话显示语言，空值回退默认 zh。
func (p *pageController) setLocale(locale string) {
	if locale == "" {
		locale = i18n.LocaleZH
	}
	p.locale = locale
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
	vc := forum.ViewContext{
		Online:    p.online(),
		Active:    p.active(),
		Guest:     !principal.IsUser(),
		RowOffset: p.offset,
		Context:   p.nodeSlug,
		Notice:    p.notice,
	}
	// v0.5.0 / ADR-057：karma 由 DB 现值派生显示等级。用可选接口断言而非扩
	// StatusReader，避免所有只关心状态的调用方与测试 fake 被迫实现无关方法。
	// 未实现的 reader（如测试 fake）保持 karma=0，等级 L1（既有行为）。
	if principal.IsUser() {
		if kr, ok := p.users.(appuser.KarmaReader); ok {
			if karma, err := kr.Karma(context.Background(), principal.UserID); err == nil {
				vc.Karma = karma
			}
		}
		// v0.6.0：注入该用户已解锁的隐藏节点（如 Konami 码解锁的 /dev/null）。
		if p.unlockedNodes != nil {
			if nodes, err := p.unlockedNodes(context.Background(), principal.UserID); err == nil {
				vc.UnlockedHiddenNodes = nodes
			}
		}
	}
	return vc
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

// render 把当前 Screen 交给终端 Renderer，并在页面层完成 LF→CRLF，
// 再按整屏干净重绘组装帧（ESC[H 回左上角 + ESC[2J 清当前屏，ADR-072）。
//
// 不再记住上一屏行数：每次渲染都从 (1,1) 重画，因此窗口缩放后不会因旧行数
// 陈旧而错位，也不会残留上一屏更短时下方的空白带。height 只用于决定末行是否
// 补 CRLF（n>=height 时不补，避免底行换行触发滚屏把首行顶出可视区）。
func (p *pageController) render(sess pageSession) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	width, height, _ := sess.Window()
	if height == 0 {
		// 无 pty-req 高度信息（如 ssh -T）时兜底 24 行：否则 RenderCleanFrame
		// 取保守分支不补末行 CRLF，后续追加会贴在内容末行尾部（ADR-072 边界）。
		height = 24
	}
	text, err := terminal.RenderScreenWithLocale(p.screen, int(width), p.locale)
	if err != nil {
		// 失败关闭：渲染器不接受畸形 ViewModel / 含控制字符的系统文本时，
		// 宁可只给一行固定提示，也不把可疑内容写进 PTY。
		return p.writeFixed(sess, "页面渲染失败，已停止输出")
	}
	frame, _ := terminal.RenderCleanFrame(toCRLF(text), int(height))
	_, err = io.WriteString(sess, frame)
	return err
}

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

// writeFixed 只输出服务端编译期常量，走 RenderSystem 路径（普通色）。
// 追加一行；其后的下一次页面渲染会整屏清屏回页面（ADR-072），
// 这行提示留在 scrollback 可上翻。
func (p *pageController) writeFixed(sess pageSession, text string) error {
	rendered, err := safe.RenderSystem(text, safe.StylePlain)
	if err != nil {
		return err
	}
	_, err = io.WriteString(sess, toCRLF(rendered)+"\r\n")
	return err
}

// writeHint 按 key 从 i18n.hintCatalog 查表输出引导/草稿提示，走 Dim 灰阶——与
// 状态栏同色（ADR-014/027 已入白名单的 SGR 样式），区别于用户正文的普通色。
// hintPrefix 同样按 locale 查表、在此单点拼接，与 noticePrefix 的 default 分支对齐；
// 表内只存服务端常量（含 %s 槽位），仍整段走 RenderSystem 校验，用户文本不进此路径。
// 错误/失败提示仍用 writeFixed 保持醒目。样式由 RenderSystem 自带 reset 收尾，
// 不泄漏到后续用户回显。
func (p *pageController) writeHint(sess pageSession, key string, args ...any) error {
	text := i18n.Lookup(p.locale, i18n.KeyHintPrefix) + i18n.Lookup(p.locale, key, args...)
	rendered, err := safe.RenderSystem(text, safe.StyleDim)
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
	// nodeID=0 且 slug 为空是「无版块上下文」的残缺状态：检索结果（ScreenSearchResults
	// 会把 nodeID/nodeSlug 清零）打开详情后按 q、以及删除/治理后重建列表，都会带着
	// (0, "") 走到这里。按语义应回版块列表，而不是去查一个不存在的跨版块屏
	// （否则状态栏会显示「位置: 」（空串），用户不知身在何处）。v0.7.1 / ADR-063。
	if nodeID == 0 && slug == "" {
		return p.showNodes(ctx, sess)
	}
	// 游标为空即第一页：offset 必须归零，否则分页边界（n → p）会把上一页的行数
	// 带进 FirstIndex，序号从中间开始跳。v0.7.1 / ADR-063。
	if cursor == "" {
		offset = 0
	}
	v := p.viewContext(sess)
	v.RowOffset = offset
	// 状态栏「位置」必须用本次要进入的 slug，而不是 p.nodeSlug（上一次的位置）：
	// viewContext 读的是控制器上的旧字段，若不覆盖，进入版块/切到 /latest 的
	// 首帧会显示上一屏的位置（或干脆没有「位置」行）。v0.7.1 / ADR-063 修正。
	if slug != "" {
		v.Context = slug
	}
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
	// R-06：详情屏改为分页装配——正文按物理行切、回复按整条装箱。
	// 这里只重置状态并渲染第 1 页，取数与装箱由 detail_page.go 负责。
	p.threadID = threadID
	p.detail = detailState{page: 1}
	return p.renderDetailPage(ctx, sess, detailPagePos{})
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
		// 锚点必须在重建前捕获：返回后按 threadID 在新列表里定位原帖，
		// 而不是让 screenOf 把焦点重置为首行（ADR-066）。
		anchor := p.threadID
		if err := p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset); err != nil {
			return err
		}
		p.restoreFocus(anchor)
		return p.render(sess)
	case viewmodel.ScreenThreads:
		// showNodes 会把 nodeID 清零，锚点先捕获；按 nodeID 定位原节点。
		anchor := p.nodeID
		if err := p.showNodes(ctx, sess); err != nil {
			return err
		}
		p.restoreFocus(anchor)
		return p.render(sess)
	case viewmodel.ScreenSearchResults:
		return p.showNodes(ctx, sess)
	default:
		return nil
	}
}

// restoreFocus 在重建屏后按锚点行 ID 回填焦点；未命中或锚点 <=0 时保留
// screenOf 的首行默认。按 Row.ID 而非显示序号（Index）定位：列表重查后同一帖
// 可能换位置，按序号会落到另一篇（ADR-066）。
func (p *pageController) restoreFocus(anchor int64) {
	if anchor <= 0 {
		return
	}
	for _, r := range p.screen.Rows {
		if r.ID == anchor {
			p.screen.Selection.FocusedIndex = r.Index
			p.screen.Selection.TopRowIndex = r.Index
			return
		}
	}
}

func (p *pageController) nextPage(ctx context.Context, sess pageSession) error {
	if p.screen.Kind == viewmodel.ScreenThreadDetail {
		// R-06：详情屏的 n 是"往下读一页"——正文先翻完，再翻回复。
		return p.detailNextPage(ctx, sess)
	}
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
	if p.screen.Kind == viewmodel.ScreenThreadDetail {
		// R-06：详情屏的 p 与列表页同义——回到第 1 页，不是"上一页"。
		return p.detailFirstPage(ctx, sess)
	}
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
	case "edit":
		return p.edit(ctx, sess, inv.Args)
	case "hide":
		return p.hide(ctx, sess, inv.Args)
	case "unhide":
		return p.unhide(ctx, sess, inv.Args)
	case "unthread":
		return p.unthread(ctx, sess, inv.Args)
	case "search":
		return p.showSearch(ctx, sess, strings.Join(inv.Args, " "), 1)
	case "latest":
		// 跨版块列表 = 「全部版块的主题列表屏」：nodeID=0 走 ScreenThreads 的跨版块分支，
		// nodeSlug 取固定串 "latest"（版块 slug 必须以 / 开头，不可能与之冲突）。
		// 详情页 back() 用 p.nodeID/p.nodeSlug/p.cursor/p.offset 原样回到这里。
		return p.showThreads(ctx, sess, 0, latestScope, "", 0)
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

// edit 的确切口径（P2-M2 / ADR-051）：与 /delete 对称，且必须重新输入完整新正文。
//   - /edit        → 编辑当前主题正文（首帖）；
//   - /edit <id>   → 编辑当前主题下 ID 为 <id> 的回复；首帖/回复统一走 posts 表。
//
// 两者都要求处于详情页（导航确定）。id 严格按十进制正整数解析（照 /delete），
// 不回显原文。targetID 解析后先做**只读判权**（CanEditPost）再进入 composer，
// 避免非作者白输一遍；提交时 EditPost 在写事务内再次判权（防 TOCTOU）。
// 非作者 / 访客 / 目标不存在统一「仅作者可编辑」，不区分原因。
func (p *pageController) edit(ctx context.Context, sess pageSession, args []string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, "请先打开一个主题再编辑")
	}

	targetID := int64(0)
	if len(args) > 0 {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return p.writeFixed(sess, "ID 必须是正整数")
		}
		targetID = id
	} else {
		// 无参 = 当前主题正文（首帖）。详情屏分页后 Rows 只承载"某一页回复"，
		// 正文必须显式取（ThreadHead），不能再用 Rows/Posts[0] 推断——否则
		// 翻到第 2 页时会把某条回复当成正文删掉（R-06）。
		head, err := p.service.ThreadHead(ctx, p.threadID)
		if err != nil {
			return p.fail(sess, err)
		}
		if head.Post.ID == 0 {
			return p.writeFixed(sess, "主题正文不存在")
		}
		targetID = head.Post.ID
	}

	if err := p.service.CanEditPost(ctx, p.ownerActor(ctx, sess), targetID); err != nil {
		return p.fail(sess, err)
	}
	return p.startEditComposer(sess, targetID)
}

// ownerActor 只提供所有权判定需要的 UserID：访客保持零值，由 canModify 统一拒绝。
// 刻意不复用 p.actor（它给访客 ErrReadOnly），否则访客与非作者会得到两种提示，
// 破坏「仅作者可删除 / 仅作者可编辑」的防枚举口径。删除与编辑共用此构造。
func (p *pageController) ownerActor(ctx context.Context, sess pageSession) forum.Actor {
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

// deleteActor 保留为删除路径的语义化别名；权限构造与编辑同源（唯一 canModify）。
func (p *pageController) deleteActor(ctx context.Context, sess pageSession) forum.Actor {
	return p.ownerActor(ctx, sess)
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

	// 草稿预填（§4.5）：同上下文沿用恢复；不匹配则丢弃旧草稿全新开始。
	// /edit 不走本函数，天然不检出草稿（§4.6，避免 EditPost 覆盖原文风险）。
	if d := p.draft; d != nil {
		if d.matches(creating, p.nodeID, p.threadID) {
			return p.resumeDraft(sess, d)
		}
		p.draft = nil
		if err := p.writeHint(sess, i18n.KeyDraftDiscarded); err != nil {
			return err
		}
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
		return p.writeHint(sess, i18n.KeyComposerTitle)
	}
	return p.writeHint(sess, i18n.KeyComposerBody)
}

// startEditComposer 进入「编辑既有正文」态：复用 startComposer 的正文阶段
// （Enter 换行、哨兵/Ctrl-D 提交、Esc 取消），但跳过标题阶段与发帖/回复的身份检查
// （权限已由 edit 的 CanEditPost 前置判定），提交时走 EditPost 覆盖原文。
// 正文从空缓冲开始——用户必须重新输入完整新正文，不做就地行编辑、不预填原文。
// §4.6：本函数**不检出草稿**（editPostID>0、EditPost 覆盖原文），
// 回复/发帖草稿在此保持不动。
func (p *pageController) startEditComposer(sess pageSession, postID int64) error {
	e := &pageEditor{
		stage: stageBody, creating: false, editPostID: postID,
		threadID: p.threadID,
		body:     input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes)),
	}
	e.body.Start()
	p.editor = e
	p.echo.setSecret(false)
	_ = sess.SetState(presence.StateComposing)
	return p.writeHint(sess, i18n.KeyComposerEdit)
}

// editorKey 是编辑态的单入口：取消键两阶段共用，其余按键按阶段分流。
func (p *pageController) editorKey(ctx context.Context, sess pageSession, m input.KeyMsg) error {
	e := p.editor
	if e == nil {
		return nil
	}
	switch m.Kind {
	case input.KeyEsc:
		p.echo.newline()
		return p.cancelEditor(sess, false)
	case input.KeyCtrlC:
		p.echo.newline()
		return p.cancelEditor(sess, true)
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

// cancelEditor 处理编辑态退出：
//   - clearDraft=true（Ctrl-C）：取消并清草稿，用户明确放弃；
//   - clearDraft=false（Esc）：无草稿时原取消行为；有草稿（A 限流后）时挂起——
//     保草稿、回屏、提示按 r 恢复（用户拍板 D2，覆盖评审原"取消"语义）。
func (p *pageController) cancelEditor(sess pageSession, clearDraft bool) error {
	hadDraft := p.draft != nil
	p.editor = nil
	_ = sess.SetState(presence.StateBrowsing)
	if clearDraft {
		p.draft = nil
	}
	if hadDraft && !clearDraft {
		return p.writeHint(sess, i18n.KeyDraftSuspended)
	}
	return p.writeFixed(sess, "（已取消编辑）")
}

// retryableDraftError 判定失败是否值得保留草稿：时机/准入类（内容本身合法）保留；
// 内容/上下文无效与存储故障不保留（§4.2）。
func retryableDraftError(err error) bool {
	var rl *forum.RateLimitError
	return errors.As(err, &rl) ||
		errors.Is(err, forum.ErrReadOnly) ||
		errors.Is(err, forum.ErrForbidden)
}

func asRateLimit(err error) (*forum.RateLimitError, bool) {
	var rl *forum.RateLimitError
	ok := errors.As(err, &rl)
	return rl, ok
}

// saveDraft 从提交中的编辑器快照一份草稿（标题/正文/上下文）。
func (p *pageController) saveDraft(e *pageEditor, body string) *composerDraft {
	return &composerDraft{
		creating: e.creating,
		title:    e.title,
		body:     body,
		nodeID:   e.nodeID,
		threadID: e.threadID,
	}
}

// rebuildComposerFromDraft 是路径 A：限流失败后原地重建编辑缓冲（方案 B 追加重建）。
// setSecret(false) 已清空 cols；为重建列宽栈（保持退格精度），在提示之后用
// pasteBlock 把正文整段追加回显一遍，光标自然停在末行末字。正文逐 rune 经
// SanitizeText、换行走服务端 CRLF；不整屏重绘、不回 StateBrowsing、无 CUU。
// D6 代价（如实登记 RELEASE_NOTES）：正文多回显一遍会刷屏；提示离光标变远，
// 长正文时倒计时可能被顶出屏幕。
func (p *pageController) rebuildComposerFromDraft(sess pageSession, rl *forum.RateLimitError) error {
	d := p.draft
	e := &pageEditor{
		stage:    stageBody,
		creating: d.creating,
		title:    d.title,
		nodeID:   d.nodeID,
		threadID: d.threadID,
		body:     input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes)),
	}
	if err := e.body.Load(d.body); err != nil {
		// Load 兜底校验失败（理论不发生）：放弃原地重建，转 B 回屏。
		p.editor = nil
		_ = sess.SetState(presence.StateBrowsing)
		return p.failWithDraftHint(sess, rl)
	}
	p.editor = e
	p.echo.setSecret(false)
	if err := p.writeHint(sess, i18n.KeyDraftRetainedRate, rl.RetryAfter.Round(time.Second)); err != nil {
		return err
	}
	// 方案 B：正文整段追加回显一遍以重建 cols。pasteBlock 逐 rune 经 SanitizeText，
	// 换行按服务端 CRLF，cols 对应末行——光标停在末行末字，退格精度保持。
	p.echo.pasteBlock(e.body.Text())
	return nil
}

// failWithDraftHint 是路径 B：固定错误文案走 fail()，其后追加一行草稿保留提示。
// fail() 原样返回业务 err（非渲染失败），不能据此跳过提示；先完成提示再回传 err。
func (p *pageController) failWithDraftHint(sess pageSession, err error) error {
	_ = p.fail(sess, err)
	if werr := p.writeHint(sess, i18n.KeyDraftRetained); werr != nil {
		return werr
	}
	return err
}

// resumeDraft 按草稿重建编辑器并回显，供 B 恢复 / A 挂起后回原帖按 r。
// 与 A 原地重建不同：此处编辑器新建、屏幕无旧回显，故正文回显一次（§4.5）。
func (p *pageController) resumeDraft(sess pageSession, d *composerDraft) error {
	e := &pageEditor{
		stage:    stageBody,
		creating: d.creating,
		title:    d.title,
		nodeID:   d.nodeID,
		threadID: d.threadID,
		body:     input.NewComposer(input.WithMaxBytes(forum.MaxBodyBytes)),
	}
	if err := e.body.Load(d.body); err != nil {
		// 兜底校验失败：草稿失效，清掉后全新开始。
		p.draft = nil
		return p.writeFixed(sess, "草稿已失效，请重新输入")
	}
	p.editor = e
	p.echo.setSecret(false)
	// 发帖草稿跳过标题阶段直接进 stageBody；标题行先行回显。
	if d.creating && d.title != "" {
		p.echo.paste(d.title)
		p.echo.newline()
	}
	// 恢复提示必须在正文回显**之前**：writeFixed 自带尾部 \r\n，若放在
	// pasteBlock 之后，光标被甩到提示下一行而 echo.cols 仍描述正文末行，
	// 退格会在错误位置擦除（屏幕正文不消失）；且提示会紧贴正文末字。
	// 前置后最后输出的是正文——光标停在正文末字、cols 与之一一对应，退格正确。
	if err := p.writeHint(sess, i18n.KeyDraftRestored); err != nil {
		return err
	}
	p.echo.pasteBlock(d.body)
	_ = sess.SetState(presence.StateComposing)
	return nil
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
		return p.writeHint(sess, i18n.KeyComposerBodyAfterTitle)
	}

	editing := e.editPostID > 0
	// 编辑用 ownerActor（仅 UserID，不查状态、不给访客 ErrReadOnly），
	// 与前置 CanEditPost、与删除路径同一口径；发帖/回复仍走 p.actor 的写准入。
	var actor forum.Actor
	if editing {
		actor = p.ownerActor(ctx, sess)
	} else {
		var err error
		actor, err = p.actor(ctx, sess)
		if err != nil {
			// P0-1：actor 失败（只读/冻结/不存在账号）同样存草稿并回屏，
			// 走与写失败 B 路径同一套草稿保留文案（§4.2），让用户知道内容没丢。
			if retryableDraftError(err) {
				p.draft = p.saveDraft(e, text)
				p.editor = nil
				_ = sess.SetState(presence.StateBrowsing)
				return p.failWithDraftHint(sess, err)
			}
			return p.cancelEditorWithNotice(sess, err)
		}
	}

	var writeErr error
	var newPostID int64 // 仅回复分支捕获：用于成功后把焦点回填到自己刚发的回复（ADR-068）
	if editing {
		// 编辑：只覆盖既有正文，actor 仅用于 canModify 判权；EDIT 不查账号状态
		// （与删除同口径：作者权不是治理权，前置 CanEditPost 已统一拒绝访客/非作者）。
		writeErr = p.service.EditPost(ctx, actor, forum.EditPostInput{
			PostID: e.editPostID, Body: text,
		})
	} else if e.creating {
		_, writeErr = p.service.CreateThread(ctx, actor, forum.CreateThreadInput{
			NodeID: e.nodeID, Title: e.title, Body: text,
		})
	} else {
		newPostID, writeErr = p.service.CreatePost(ctx, actor, forum.CreatePostInput{
			ThreadID: e.threadID, Body: text,
		})
	}

	threadID, nodeID, nodeSlug := e.threadID, e.nodeID, p.nodeSlug
	creating := e.creating

	if writeErr != nil {
		if retryableDraftError(writeErr) {
			p.draft = p.saveDraft(e, text)
			if rl, ok := asRateLimit(writeErr); ok {
				// A：限流原地重建，保持 Composing，不整屏重绘、不摘编辑器。
				return p.rebuildComposerFromDraft(sess, rl)
			}
			// B：ErrReadOnly / ErrForbidden 回浏览屏，草稿保留。
			p.editor = nil
			_ = sess.SetState(presence.StateBrowsing)
			return p.failWithDraftHint(sess, writeErr)
		}
		// 不可重试：维持现状。
		p.editor = nil
		_ = sess.SetState(presence.StateBrowsing)
		return p.fail(sess, writeErr)
	}

	p.editor = nil
	p.draft = nil // 成功提交清除草稿
	_ = sess.SetState(presence.StateBrowsing)
	if editing {
		p.notice = viewmodel.Notice{
			Kind: viewmodel.NoticeSuccess, Code: "edited", Message: "已保存修改",
		}
		// 编辑不推进 last_reply_at：刷新详情页即可，列表排序不受影响。
		return p.showDetail(ctx, sess, threadID)
	}
	p.notice = viewmodel.Notice{
		Kind: viewmodel.NoticeSuccess, Code: "saved", Message: "已发布",
	}
	if creating {
		return p.showThreads(ctx, sess, nodeID, nodeSlug, "", 0)
	}
	// 回复成功：showDetail 把焦点重置为首条回复，按新 postID 回填到自己刚发的回复
	//（ADR-068，复用 ADR-066 restoreFocus，按 Row.ID 定位）。未命中（软删 / 超
	// MaxPostsPerThread 不在返回集合）由 restoreFocus 自然回退首行，不劣化现状。
	if err := p.showDetail(ctx, sess, threadID); err != nil {
		return err
	}
	p.restoreFocus(newPostID)
	return p.render(sess)
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
		// 账号被删（ErrNotFound）与事务内 checkWritePermission 把"用户不存在"
		// 收敛为 ErrForbidden 的口径一致（§4.2），可留草稿；真实读取故障原样上抛，
		// 不伪装成权限拒绝。
		if errors.Is(err, forum.ErrNotFound) {
			return forum.Actor{}, forum.ErrForbidden
		}
		return forum.Actor{}, err
	}
	return forum.ActorFromPrincipal(principal, status), nil
}
