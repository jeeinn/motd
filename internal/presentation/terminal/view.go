package terminal

// 终端 Renderer（评审稿 v0.2 §2.3 / §3.1 / §3.5 / ADR-002 / ADR-019）。
//
// 本文件只做一件事：把 application/viewmodel 的语义 Screen 翻译成终端文本。
// 它不读数据库、不认识 SSH、不持有会话状态，也不产生任何光标 / 清屏 / OSC 控制。
//
// 信任域严格分离：
//   - 编译期常量与本地数值（序号、计数、样式标签）→ safe.RenderSystem；
//   - 一切来自 DB 或用户提交的文本（标题、正文、slug、提示文案）→ safe.RenderUser。
// 两类文本绝不拼进同一次渲染调用，避免外部数据绕过净化（§3.5）。
//
// 输出统一用 LF；PTY 需要的 LF→CRLF 由页面层完成（ADR-019 已知边界 1）。

import (
	"fmt"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

const (
	defaultWidth  = 80
	minContentCol = 20
	prefixCol     = 12 // "> 12. [123] " 之类的固定前缀预留列数
)

// RenderScreen 渲染一屏。width <= 0 时按 80 列降级（未收到 pty-req 的情况）。
// 返回错误表示 ViewModel 或服务端固定文本不自洽，调用方应失败关闭而不是原样输出。
//
// 确定性边界：输出只由 (Screen, width) 与进程展示时区决定（见 display_location.go）。
// 后者在启动期由 config.DisplayLocation 注入一次、之后只读；本函数不读时钟、
// 不读 DB、不持有会话状态。时间字段按该时区格式化，格式串固定为 MM-DD HH:MM。
func RenderScreen(s viewmodel.Screen, width int) (string, error) {
	if err := s.Validate(); err != nil {
		return "", fmt.Errorf("ViewModel 非法: %w", err)
	}
	if width <= 0 {
		width = defaultWidth
	}
	contentWidth := width - prefixCol
	if contentWidth < minContentCol {
		contentWidth = minContentCol
	}

	var b strings.Builder
	if err := sys(&b, screenTitle(s.Kind), safe.StyleHeader); err != nil {
		return "", err
	}
	b.WriteByte('\n')

	if s.Kind == viewmodel.ScreenThreadDetail {
		if err := ext(&b, s.Detail.Title, contentWidth); err != nil {
			return "", err
		}
		b.WriteByte('\n')
		// v0.2.1：标题与正文之间的固定虚线，与下方「── 回复 ──」上下对称。
		// 服务端固定文案走 RenderSystem：零用户内容、零光标控制，不碰 ADR-019 / ADR-027。
		if err := sys(&b, "──", safe.StyleDim); err != nil {
			return "", err
		}
		b.WriteByte('\n')
		if s.Detail.Body != "" {
			if err := ext(&b, s.Detail.Body, width); err != nil {
				return "", err
			}
			b.WriteByte('\n')
		}
		// P2-M2 / ADR-051：正文下方「已编辑 MM-DD HH:MM」标记，仅详情页出现。
		// 文案前缀是编译期固定串，时间由服务端从 EditedAt（本地数值）按展示时区
		// 格式化（formatTime，与列表同一口径），整段走 sys()→RenderSystem，不进用户
		// 内容净化链（ADR-014 三层守卫）；EditedAt<=0（从未编辑）时整行不输出。
		if s.Detail.EditedAt > 0 {
			if err := sys(&b, "已编辑 "+formatTime(s.Detail.EditedAt), safe.StyleDim); err != nil {
				return "", err
			}
			b.WriteByte('\n')
		}
		if err := sys(&b, "── 回复 ──", safe.StyleDim); err != nil {
			return "", err
		}
		b.WriteByte('\n')
	}

	// Device Flow 浮层自带内容，不需要「暂无内容」占位行。
	if len(s.Rows) == 0 && s.Overlay.Kind != viewmodel.OverlayDeviceCode {
		if err := sys(&b, emptyHint(s.Kind), safe.StyleDim); err != nil {
			return "", err
		}
		b.WriteByte('\n')
	}

	// 浮层：固定标题/正文/标签/动作走 RenderSystem，
	// 外部字段（user_code / verification_uri）走 RenderUser，两条路径不混用。
	if err := renderOverlay(&b, s.Overlay, contentWidth); err != nil {
		return "", err
	}

	for _, r := range s.Rows {
		mark := "  "
		if r.Index == s.Selection.FocusedIndex {
			mark = "> "
		}
		if err := sys(&b, fmt.Sprintf("%s%2d. ", mark, r.Index), safe.StylePlain); err != nil {
			return "", err
		}
		if s.Kind == viewmodel.ScreenThreads {
			if err := sys(&b, fmt.Sprintf("[%d] ", r.Score), safe.StyleDim); err != nil {
				return "", err
			}
		}

		text := r.Title
		if text == "" {
			text = r.Body
		}
		if err := ext(&b, text, contentWidth); err != nil {
			return "", err
		}
		// 元信息分信任域渲染：id=/时间/回复数是本地数值与固定字符，走 sys()；
		// 作者名来自 users 表，是用户数据，必须走 ext()（ADR-014/019）。
		// 两段固定文本夹住作者段，保持「id= → 作者 → 时间/回复数」的既有顺序。
		if fixed := metaID(r, s.Kind); fixed != "" {
			if err := sys(&b, fixed, safe.StyleDim); err != nil {
				return "", err
			}
		}
		if err := ext(&b, authorSegment(r), contentWidth); err != nil {
			return "", err
		}
		if fixed := metaTail(r, s.Kind); fixed != "" {
			if err := sys(&b, fixed, safe.StyleDim); err != nil {
				return "", err
			}
		}
		// slug 来自 nodes 表，按外部数据处理，不进 RenderSystem。
		if r.Meta.Node != "" {
			if err := ext(&b, r.Meta.Node, contentWidth); err != nil {
				return "", err
			}
		}
		b.WriteByte('\n')
	}

	if err := renderStatusBar(&b, s, width); err != nil {
		return "", err
	}
	if err := sys(&b, screenFooter(s.Kind), safe.StyleDim); err != nil {
		return "", err
	}
	b.WriteByte('\n')
	if s.Pagination.HasMore {
		if err := sys(&b, "（还有更多，按 n 加载下一页，p 回第一页）", safe.StyleDim); err != nil {
			return "", err
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// renderOverlay 渲染登录浮层。信任域约定（写进函数契约，防止后续误用）：
//   - Title / Body / Field.Label / Action.Key / Action.Label 是服务端权威文本，走 sys；
//   - Field.Value / Progress.UserCode 是外部字段（GitHub 响应投影），走 ext；
//   - 时间由服务端用本地数值预先格式化进 Body，Renderer 不自己读时钟。
func renderOverlay(b *strings.Builder, o viewmodel.Overlay, contentWidth int) error {
	if o.Kind != viewmodel.OverlayDeviceCode {
		return nil
	}
	if o.Title != "" {
		if err := sys(b, o.Title, safe.StyleBold); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	if o.Body != "" {
		if err := sys(b, o.Body, safe.StylePlain); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	for _, f := range o.Fields {
		if f.Label != "" {
			if err := sys(b, f.Label, safe.StylePlain); err != nil {
				return err
			}
		}
		if err := ext(b, f.Value, contentWidth); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	p := o.Progress
	if p.State != viewmodel.DeviceFlowIdle && p.UserCode != "" {
		if err := sys(b, "验证码：", safe.StylePlain); err != nil {
			return err
		}
		if err := ext(b, p.UserCode, contentWidth); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	for _, a := range o.Actions {
		if err := sys(b, "["+a.Key+"] "+a.Label, safe.StyleDim); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	return nil
}

func screenFooter(k viewmodel.ScreenKind) string {
	if k == viewmodel.ScreenLogin {
		return "Esc 取消登录 · 浏览器完成授权后自动继续"
	}
	return "j/k 移动 · Enter 打开 · h/q 返回 · r 回复 · n 下一页 · / 命令 · ? 帮助"
}

func renderStatusBar(b *strings.Builder, s viewmodel.Screen, width int) error {
	sb := s.StatusBar
	identity := "访客"
	if !sb.Identity.Guest {
		identity = "已登录"
	}
	// P4-T1：状态栏只在 L3/L4 时拆段着色，面积仅覆盖等级两字符。等级档位由
	// application 用 domain/user.Level(karma) 派生后存进 StatusBar.Level（1–4），
	// Renderer 只读这个语义值，不在这里重算领域规则、不引入 domain 依赖。
	// L1/L2 保持 Dim 且整行一次渲染（与既有输出逐字节一致）；L3/L4 的前半段
	// 仍走 Dim，等级标签走 Accent（深蓝）。"L%d" 文字标签始终保留——
	// 颜色不单独承载语义。
	levelText := fmt.Sprintf("L%d", sb.Level)
	if sb.Level >= 3 {
		if err := sys(b, fmt.Sprintf("online=%d active=%d | %s | karma=%d ",
			sb.Online, sb.Active, identity, sb.Karma), safe.StyleDim); err != nil {
			return err
		}
		if err := sys(b, levelText, safe.StyleAccent); err != nil {
			return err
		}
	} else {
		if err := sys(b, fmt.Sprintf("online=%d active=%d | %s | karma=%d %s",
			sb.Online, sb.Active, identity, sb.Karma, levelText), safe.StyleDim); err != nil {
			return err
		}
	}
	b.WriteByte('\n')

	if sb.Context != "" {
		if err := sys(b, "位置: ", safe.StyleDim); err != nil {
			return err
		}
		if err := ext(b, sb.Context, width); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	if sb.Notice.Kind != viewmodel.NoticeNone && sb.Notice.Message != "" {
		prefix, style := noticePrefix(sb.Notice.Kind)
		if err := sys(b, prefix, style); err != nil {
			return err
		}
		// 消息正文来自外部数据，必须继续走 ext()/RenderUser 净化链。
		// 配色只落在固定前缀上，用户文本即使含 SGR 注入也不会选中 UI 色。
		if err := ext(b, sb.Notice.Message, width); err != nil {
			return err
		}
		b.WriteByte('\n')
	}
	return nil
}

// noticePrefix 按提示语义返回固定前缀文字与样式（P4-T1）。
// 只处理服务端编译期常量，不接收任何用户文本，因此用户无法借 Kind 染色正文。
func noticePrefix(kind viewmodel.NoticeKind) (string, safe.Style) {
	switch kind {
	case viewmodel.NoticeError:
		return "[错误] ", safe.StyleError
	case viewmodel.NoticeWarning:
		return "[提示] ", safe.StyleNotice
	default:
		return "[提示] ", safe.StyleDim
	}
}

// sys 只接服务端固定文本或由本地数值拼成的文本。
func sys(b *strings.Builder, text string, style safe.Style) error {
	rendered, err := safe.RenderSystem(text, style)
	if err != nil {
		return err
	}
	b.WriteString(rendered)
	return nil
}

// ext 接一切外部数据（DB 文本 / 用户提交内容），必须经过净化与白名单渲染。
func ext(b *strings.Builder, text string, width int) error {
	if text == "" {
		return nil
	}
	b.WriteString(safe.RenderUser(text, safe.Options{Width: width}))
	return nil
}

func screenTitle(k viewmodel.ScreenKind) string {
	switch k {
	case viewmodel.ScreenNodes:
		return "motd · 版块列表"
	case viewmodel.ScreenThreads:
		return "motd · 主题列表"
	case viewmodel.ScreenThreadDetail:
		return "motd · 主题详情"
	case viewmodel.ScreenComposer:
		return "motd · 编辑"
	case viewmodel.ScreenLogin:
		return "motd · 登录"
	case viewmodel.ScreenHelp:
		return "motd · 帮助"
	case viewmodel.ScreenSearchResults:
		return "motd · 检索结果"
	default:
		return "motd"
	}
}

func emptyHint(k viewmodel.ScreenKind) string {
	switch k {
	case viewmodel.ScreenNodes:
		return "（暂无版块）"
	case viewmodel.ScreenThreads:
		// 不能写「该版块」：同一个 ScreenThreads 也承载跨版块列表（/latest），
		// 而 /latest 没有版块上下文，/new 在那里也不可用（需先进入版块）。
		// v0.7.1 / ADR-063。
		return "（暂无主题，登录后可用 /new 发第一帖）"
	case viewmodel.ScreenThreadDetail:
		return "（暂无回复，登录后可用 /reply 回复）"
	case viewmodel.ScreenSearchResults:
		return "（没有匹配的正文）"
	default:
		return "（暂无内容）"
	}
}

// 元信息被刻意拆成「固定前缀 + 用户文本 + 固定后缀」，在渲染循环里交错输出：
//   - metaID / metaTail 只由本地数值与固定字符拼成，走 sys()（RenderSystem）；
//   - authorSegment 含来自 users 表的 username，必须走 ext()（RenderUser）。
//
// v0.3.3：作者从 "#<ID>"（本地数值）改为 "@<username>"（用户数据），
// 因此不能再整段塞进 RenderSystem——那既破坏信任域分离，也会让带控制字符的
// 用户名被 RenderSystem 拒绝渲染。回退时仍显示 "#<ID>"，但同样经 ext() 路径。

const maxAuthorCols = 10

// metaID 返回行实体 ID 段（纯本地数值，RenderSystem 路径）。
// 格式固定为 " id=<十进制>"：既不带 # 也不带方括号，可直接当 /open / /delete 参数。
func metaID(r viewmodel.Row, k viewmodel.ScreenKind) string {
	if showRowID(k) && r.ID > 0 {
		return fmt.Sprintf(" id=%d", r.ID)
	}
	return ""
}

// metaTail 返回时间、回复数与 node 占位（纯本地数值与固定字符，RenderSystem 路径）。
// v0.3.3：回复数由 " N回复" 改为紧凑的 " rN"；其余符号一律不动。
func metaTail(r viewmodel.Row, k viewmodel.ScreenKind) string {
	var parts []string
	if at := formatTime(r.Meta.CreatedAt); at != "" {
		parts = append(parts, " "+at)
	}
	if k == viewmodel.ScreenThreads && r.Meta.Replies > 0 {
		parts = append(parts, fmt.Sprintf(" r%d", r.Meta.Replies))
	}
	if r.Meta.Node != "" {
		parts = append(parts, " ")
	}
	return strings.Join(parts, "")
}

// authorSegment 返回带前导空格的作者段。username 存在时显示 " @<username>"，
// 缺失/查不到时回退到 " #<ID>"。返回值交给 ext() 净化，本函数不产生任何 ANSI。
func authorSegment(r viewmodel.Row) string {
	if r.Meta.Author != "" {
		return " @" + truncateDisplayWidth(r.Meta.Author, maxAuthorCols)
	}
	if r.Meta.AuthorID > 0 {
		return fmt.Sprintf(" #%d", r.Meta.AuthorID)
	}
	return ""
}

// truncateDisplayWidth 按**显示列**截断外部文本（CJK/emoji 算 2 列），
// 超出时以 ASCII '~' 收尾（不引入新的宽字符或多字节省略号）。
// 只在 rune 边界切分，结果是合法 UTF-8；截断严格发生在净化之前，
// 净化与白名单渲染仍由 ext() 统一完成，不在这里碰控制字符。
func truncateDisplayWidth(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	budget := max - 1 // 给 '~' 留 1 列
	if budget < 0 {
		budget = 0
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		w := safe.DisplayWidth(r)
		if col+w > budget {
			break
		}
		b.WriteRune(r)
		col += w
	}
	b.WriteByte('~')
	return b.String()
}

func displayWidth(s string) int {
	col := 0
	for _, r := range s {
		col += safe.DisplayWidth(r)
	}
	return col
}

// showRowID 报告该页面的 Row.ID 是否是用户可操作的帖子/回复 ID。
//   - 主题列表：Row.ID = thread.ID，可用 /open <id> 打开；
//   - 主题详情：Row.ID = post.ID，可用 /delete <id> 删该回复；
//   - 检索结果：Row.ID = thread.ID，回车可直接打开。
//
// 版块列表的 Row.ID 是 node.ID，既不是 /delete 目标也不在本次诉求内，
// 刻意不展示，避免用户拿它去试 /delete 而被拒绝。
func showRowID(k viewmodel.ScreenKind) bool {
	switch k {
	case viewmodel.ScreenThreads, viewmodel.ScreenThreadDetail, viewmodel.ScreenSearchResults:
		return true
	default:
		return false
	}
}

// formatTime 按进程展示时区格式化 Unix 秒。格式串刻意保持 MM-DD HH:MM：
// 不加年份、不加时区后缀，终端窄且 1 Mbps 约束下每一列都算数。
// sec <= 0 视为「无有效时间」，继续返回空串（既有行为不变）。
func formatTime(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(DisplayLocation()).Format("01-02 15:04")
}
