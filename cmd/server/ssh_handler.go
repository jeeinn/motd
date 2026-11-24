package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/domain/node"
	"github.com/jeeinn/motd/internal/presentation/terminal"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

// inputHandler 把 SSH 字节流归一化后交给命令注册表的唯一入口（§3.2）。
// P0-19 起在 handle 分发里接入 /login 最小交互层（docs/DEVIATIONS.md R18）。
// MVP 收尾把 P0-24/P0-25 的终端页面控制器接进同一分发链：解码器、命令表
// 与 transport/ssh 协议层保持不变，页面状态每会话独立（pageController）。
//
// 安全约束（ADR-026）：绝不把 KeyMsg.Raw / KeyMsg.Text 原始字节写回终端。
// 需要回显的输入一律经 safe.SanitizeText 净化（ESC/CSI/OSC 转可见图示）；
// 编辑控制（\b、\r\n）由服务端常量生成，与用户字节分属不同信任域（§3.5）。
type inputHandler struct {
	reg *command.Registry

	// deviceFlow 由 P0-15 装配，deviceAuth 由 P0-16 装配，login 由 P0-17 装配；
	// 三者都供 P0-19 的 /login 会话循环消费。端口类型在 application/auth，
	// cmd 不碰 HTTP / SQL 细节。
	deviceFlow auth.DeviceFlowClient
	deviceAuth *auth.DeviceFlowService
	login      *auth.LoginService
	// keys 由 P0-18 装配。构造顺序上它依赖 SSH Server（会话撤销实现），
	// 而 Server 又持有本 handler，因此用 setter 后填，避免装配循环。
	keys *auth.KeyBindingService

	// P1-M2 认证安全：安全门与注销用例都是进程级单例，每会话只引用不复制。
	gate   *auth.SecurityGate
	logout *auth.LogoutService
	// P1-M5 邮箱认证用例，进程级单例。
	email *auth.EmailAuthService

	// MVP 页面控制器依赖：进程级共享，每会话只复制状态不复制依赖。
	forum    *forum.Service
	users    appuser.StatusReader
	presence *presence.Registry
	// onLogin 是登录成功后的副作用钩子（签到 + login_success 审计），由 cmd 装配层注入（v0.6.0 / v0.7.1）。
	// 只在会话升级成功后调用；nil 时零影响，保持既有测试零改动。
	onLogin loginHook
	// onUnlock / unlockedNodes 是隐藏节点解锁的写/读钩子（v0.6.0），
	// 由 cmd 装配层注入；nil 时 Konami 码与列表注入都零影响。
	onUnlock      func(context.Context, int64) error
	unlockedNodes func(context.Context, int64) ([]node.Node, error)
	// stats 是 /stats 的统计用例（v0.7.1），nil 时命令只给固定提示。
	stats *appuser.StatsService
}

// withStats 注入社区统计用例（v0.7.1 / ADR-064）。读端口在 application/user，
// 命令层只做排版，端口注入沿用 withCommands / withLoginHook 的既有先例。
func (h *inputHandler) withStats(svc *appuser.StatsService) *inputHandler {
	h.stats = svc
	return h
}

// loginHook 是「登录动作成功」之后的副作用钩子：v0.6.0 只做签到，
// v0.7.1 起多带一个 provider，供 login_success 审计事件记录来源（ADR-064）。
//
// 为什么用命名类型：签名从 (ctx, userID) 扩到 (ctx, userID, provider) 会波及
// loginFlow / emailFlow / pageController / 装配闭包共 9 处，命名类型让编译器
// 一次性把所有调用点标出来，而不是靠人肉搜。
//
// 语义边界：只在**主动登录**成功（UpgradeToUser 之后）时调用——
// GitHub Device Flow 与邮箱密码两条路径。重连凭公钥恢复登录态在 transport/ssh
// 解析身份，根本不走登录流程，因此天然不计入「登录人次」。
type loginHook func(ctx context.Context, userID int64, provider string)

// withSecurity 注入认证安全门（限流 + 审计的 fail-closed 入口）。
func (h *inputHandler) withSecurity(gate *auth.SecurityGate) *inputHandler {
	h.gate = gate
	return h
}

// withLogout 注入注销边界用例。
func (h *inputHandler) withLogout(svc *auth.LogoutService) *inputHandler {
	h.logout = svc
	return h
}

// withEmail 注入 P1-M5 邮箱认证用例；保持无参构造，既有会话层测试零改动。
func (h *inputHandler) withEmail(svc *auth.EmailAuthService) *inputHandler {
	h.email = svc
	return h
}

// withForum 注入终端页面所需的用例与会话上下文。同样保持无参构造，
// 让既有会话层测试零改动。
func (h *inputHandler) withForum(svc *forum.Service, users appuser.StatusReader, reg *presence.Registry) *inputHandler {
	h.forum, h.users, h.presence = svc, users, reg
	return h
}

func newInputHandler() *inputHandler {
	return &inputHandler{reg: command.Default()}
}

// withUnlockHooks 注入隐藏节点的解锁写钩子与已解锁读钩子（v0.6.0）。
func (h *inputHandler) withUnlockHooks(
	onUnlock func(context.Context, int64) error,
	unlocked func(context.Context, int64) ([]node.Node, error),
) *inputHandler {
	h.onUnlock, h.unlockedNodes = onUnlock, unlocked
	return h
}

// withLoginHook 注入登录成功后的副作用钩子（签到 + login_success 审计，v0.6.0 / v0.7.1）。
func (h *inputHandler) withLoginHook(fn loginHook) *inputHandler {
	h.onLogin = fn
	return h
}

// withCommands 注入合并了彩蛋命令的注册表（v0.5.1 / P3-M6）。
// 测试的无参构造仍用 defaultRegistry，彩蛋只在真实装配中生效。
func (h *inputHandler) withCommands(reg *command.Registry) *inputHandler {
	h.reg = reg
	return h
}

// withLogin 注入 P0-17 登录事务。P0-19 拿到状态机的 PollResult.Token 后调用
// LoginService.Login，再用返回的 UserID 升级会话；P0 本项只装配不接线。
func (h *inputHandler) withLogin(svc *auth.LoginService) *inputHandler {
	h.login = svc
	return h
}

// withDeviceFlow 注入 GitHub Device Flow 客户端，保持 newInputHandler() 的无参
// 签名不变（现有会话层测试零改动）。P0-16 之后由页面控制器直接读取该字段。
func (h *inputHandler) withDeviceFlow(df auth.DeviceFlowClient) *inputHandler {
	h.deviceFlow = df
	return h
}

// withDeviceAuth 注入 P0-16 状态机。同样保持无参构造，会话层测试零改动。
// deviceAuth 的生命周期由进程持有（内部不 sleep、不起 goroutine），
// 轮询节奏在 P0-19 由 SSH 会话循环按 PollResult.NextInterval 驱动。
func (h *inputHandler) withDeviceAuth(svc *auth.DeviceFlowService) *inputHandler {
	h.deviceAuth = svc
	return h
}

// withKeyBinding 注入 P0-18 公钥绑定用例。同样保持无参构造，会话层测试零改动。
func (h *inputHandler) withKeyBinding(svc *auth.KeyBindingService) *inputHandler {
	h.keys = svc
	return h
}

// newLoginFlow 为一次 /login 创建独立的交互编排器。四个依赖（状态机、登录
// 事务、公钥绑定、呈现）都来自已装配的进程级单例；flow 本身是每会话对象，
// 持有 device_code 但绝不持有 token（token 只在 finish 的栈上短暂存在）。
func (h *inputHandler) newLoginFlow() (*loginFlow, bool) {
	if h.deviceAuth == nil || h.login == nil || h.keys == nil {
		return nil, false
	}
	// /login 与论坛四页共用同一份 StatusBar 语义：在线/活跃取 presence 快照，
	// 登录发起时用户必为访客，身份与等级固定。
	status := viewmodel.StatusBar{Identity: viewmodel.Identity{Guest: true}, Level: 1}
	if h.presence != nil {
		status.Online = h.presence.Online()
		status.Active = h.presence.Active()
	}
	return &loginFlow{
		device:  h.deviceAuth,
		login:   h.login,
		keys:    h.keys,
		gate:    h.gate,
		view:    loginPresenter{status: status},
		now:     time.Now,
		onLogin: h.onLogin,
	}, true
}

// handleOutcome 是一次输入事件对会话循环的结论。P0-19 把「启动登录」显式
// 建模出来，避免把长生命周期的登录循环塞进 handle 的返回值里。
type handleOutcome uint8

const (
	outcomeContinue handleOutcome = iota
	outcomeQuit
	outcomeStartLogin
)

// commandLine 是斜杠命令的行编辑缓冲，只做收集与提交（纯状态机）。
// 回显由 lineEcho 在调用侧完成，applyKey 不碰 IO（ADR-026）。
type commandLine struct {
	active bool
	runes  []rune
}

// 进入命令态时 cl.runes 已预置 '/'，必须说清楚「不用再打一次」；
// ADR-026 起输入会正常回显，不再需要解释「看不到输入」。
const commandEntryHint = "已进入命令输入（/ 已自动填入，无需再打一次）：直接输入命令名与参数，Enter 提交，Esc 取消。\r\n"

type cmdAction uint8

const (
	actionNone cmdAction = iota
	actionSubmit
	actionCancel
	actionQuit
	actionHelp
	actionCommandStart
	actionPasteNotice
	// MVP 页面导航（P0-25）：键盘与斜杠命令最终都落到同一个 pageController，
	// 不产生第二条业务路径（§3.2）。
	actionMoveDown
	actionMoveUp
	actionOpen
	actionBack
	actionReply
	actionNextPage
	actionPrevPage
)

// actionNotice 返回「产生文本、但不承载业务」的动作对应的固定提示文案。
// 空串表示该动作不写终端（导航键与静默动作）。
// 抽成纯函数是为了让「浏览态 Esc 输出为零」可直接断言，不必构造 SSH 会话。
// 注意：编辑态取消（已取消编辑）走 pageController.editorKey，与此无关。
func actionNotice(action cmdAction, pasteLen int) string {
	switch action {
	case actionCancel:
		return "（已取消命令输入）\r\n"
	case actionPasteNotice:
		return fmt.Sprintf("（已收到粘贴块 %d 字节；请先在编辑态内粘贴）\r\n", pasteLen)
	default:
		return ""
	}
}

func (h *inputHandler) Serve(ctx context.Context, sess *sshd.Session) error {
	// 页面状态每会话独立：inputHandler 是进程级单例，绝不能把光标 / 当前节点
	// 之类的 UI 状态放在 handler 上（原则二：内存状态只是运行时状态）。
	pc := h.newPageController(sess)
	// 版本行不再写在这里：首帧的整屏重绘（ESC[H + ESC[2J）会立即把它擦掉，
	// 且 2J 不进 scrollback，等于整场会话都看不到。版本改由 /stats 提供。
	_ = sess.SetState(presence.StateBrowsing)
	_ = pc.showNodes(ctx, sess)

	dec := sshd.NewKeyDecoder()
	msgs := make(chan input.KeyMsg, 64)
	buf := make([]byte, 4096)
	go func() {
		defer close(msgs)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				for _, m := range dec.Feed(buf[:n]) {
					select {
					case msgs <- m:
					case <-ctx.Done():
						return
					case <-sess.Done():
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var cl commandLine
	var flow *loginFlow
	var pollTimer *time.Timer
	var pollC <-chan time.Time
	var pollResultC <-chan loginPollMsg

	// stopPoll 收掉定时器与两个 channel；登录结束 / 取消 / 会话退出都要调。
	stopPoll := func() {
		if pollTimer != nil {
			pollTimer.Stop()
			pollTimer = nil
		}
		pollC = nil
		pollResultC = nil
	}
	defer stopPoll()

	// cancelActive 在连接退出时作废未完成的设备码。此时 ctx 可能已取消，
	// 因此另起一个短超时上下文，保证 Cancel 真的能写回 denied。
	cancelActive := func() {
		if flow == nil || !flow.active {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		flow.cancel(cctx, sess)
		flow = nil
		stopPoll()
	}

	// handleOne 是主循环里唯一处理单条 KeyMsg 的入口：登录态下只认 Esc / Ctrl-C，
	// 其余按键零回显；非登录态走原有的命令表分发。
	handleOne := func(m input.KeyMsg) bool {
		if flow != nil && flow.active {
			if isLoginCancelKey(m) {
				flow.cancel(ctx, sess)
				flow = nil
				stopPoll()
				_ = pc.refresh(ctx, sess)
			}
			return false
		}
		if pc.emailFlow != nil {
			// 邮箱分步输入优先于编辑态与命令表；密码字段隐藏回显（ADR-026）。
			done, cancelled := pc.emailFlow.step(ctx, pc, m)
			if cancelled || done {
				pc.clearEmailFlow() // 复位 secret 掩码（step 内已 finish，这里保证 flow 一定被摘掉）
				_ = pc.refresh(ctx, sess)
			}
			return false
		}
		if pc.editor != nil {
			// 编辑态优先于命令表；输入经 lineEcho 净化后回显（ADR-026）。
			_ = pc.editorKey(ctx, sess, m)
			return false
		}
		switch h.handle(ctx, sess, pc, m, &cl) {
		case outcomeQuit:
			return true
		case outcomeStartLogin:
			f, ok := h.newLoginFlow()
			if !ok {
				_, _ = io.WriteString(sess, auth.LoginTextNotReady+"\r\n")
				return false
			}
			delay, active, err := f.begin(ctx, sess, sess)
			if err != nil || !active {
				return false
			}
			// IP 摘要只作安全门键，绝不经 loginPresenter 呈现（ADR-021）。
			f.ipHash = sess.RemoteIPHash()
			flow = f
			pollTimer = time.NewTimer(delay)
			pollC = pollTimer.C
		}
		return false
	}

	ticker := time.NewTicker(sshd.EscPollInterval)
	defer ticker.Stop()
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				cancelActive()
				return nil
			}
			if handleOne(m) {
				return nil
			}
		case <-ticker.C:
			for _, m := range dec.Poll() {
				if handleOne(m) {
					return nil
				}
			}
		case <-pollC:
			if flow == nil || !flow.active {
				stopPoll()
				continue
			}
			// 网络调用最长 10s，异步执行才能让 Esc 即时取消；取消后迟到的结果
			// 由下面的 flow.active 判定丢弃（ADR-022 的 CAS 保证取消优先）。
			code := flow.pollCode()
			ch := make(chan loginPollMsg, 1)
			go func() {
				r, err := h.deviceAuth.Poll(ctx, code)
				select {
				case ch <- loginPollMsg{result: r, err: err}:
				case <-ctx.Done():
				case <-sess.Done():
				}
			}()
			pollResultC = ch
			pollC = nil // 等待本次结果期间不再触发新的 Poll
		case pm := <-pollResultC:
			if flow == nil || !flow.active {
				stopPoll()
				continue
			}
			pollResultC = nil
			done, delay, _ := flow.applyPoll(ctx, sess, sess, pm.result, pm.err)
			if done || !flow.active {
				flow = nil
				stopPoll()
				// 登录成功后状态栏要立刻从「访客」变成「已登录」。
				_ = pc.refresh(ctx, sess)
			} else {
				pollTimer.Reset(delay)
				pollC = pollTimer.C
			}
		case <-ctx.Done():
			cancelActive()
			return nil
		case <-sess.Done():
			cancelActive()
			return nil
		}
	}
}

func (h *inputHandler) handle(ctx context.Context, sess *sshd.Session, pc *pageController, m input.KeyMsg, cl *commandLine) handleOutcome {
	// v0.6.0：浏览态监听 Konami 码（↑↑↓↓←→←→BA）。命令输入/编辑态不拦截，
	// 避免吞掉方向键的原有语义；访客也不解锁（解锁需落用户记录）。
	if !cl.active && pc.editor == nil && sess.Principal().IsUser() {
		if pc.konami.Feed(m) {
			if pc.onUnlock != nil {
				if err := pc.onUnlock(ctx, sess.Principal().UserID); err == nil {
					// 固定文案（不含用户数据），经安全渲染路径输出。
					writeEgg(sess, "隐藏版块已解锁：/dev/null\r\n")
				}
			}
			pc.konami.Reset()
		}
	}
	// applyKey 保持纯状态机；回显在调用侧按「前后长度」推导是否真的删除了字符。
	wasEditing, beforeLen := cl.active, len(cl.runes)
	action, line, pasteLen := applyKey(cl, m)
	echoCommandKey(pc.echo, m, wasEditing, beforeLen, len(cl.runes), action)
	// 固定提示统一从纯函数取；空串即「零输出」（浏览态 Esc 走 actionNone）。
	if notice := actionNotice(action, pasteLen); notice != "" {
		_, _ = io.WriteString(sess, notice)
	}
	switch action {
	case actionQuit:
		return outcomeQuit
	case actionHelp:
		_, _ = io.WriteString(sess, h.reg.Help())
	case actionCommandStart:
		// 提示语若说「以 / 开头」，用户补打一个 / 会得到 //login → 未知命令
		// （本机实测反馈）。具体措辞见 commandEntryHint 的回归测试。
		// v0.3.1：每会话只打一次，后续进入命令输入静默，避免反复滚动。
		pc.maybeShowCommandEntryHint(sess)
		// 提示换行后再回显预置的 '/'，保证用户输入行连续（ADR-026）。
		pc.echo.preloadedSlash()
	case actionMoveDown, actionMoveUp, actionOpen, actionBack, actionReply,
		actionNextPage, actionPrevPage:
		// 键盘与斜杠命令汇聚到同一个页面控制器，不产生第二条业务路径。
		_ = pc.key(ctx, sess, action)
	case actionSubmit:
		inv, err := h.reg.Parse(line)
		if err != nil {
			// 解析失败只输出固定文案，绝不把用户提交的原文写回终端。
			_, _ = io.WriteString(sess, formatResult(h.reg, inv, err))
			return outcomeContinue
		}
		switch inv.Name {
		case "quit":
			// v0.5.1：约 12% 概率在退出时多说一句固定彩蛋文案（不回显任何用户数据）。
			if rand.IntN(8) == 0 {
				writeEgg(sess, command.Farewell())
			}
			_, _ = io.WriteString(sess, "再见\r\n")
			return outcomeQuit
		case "help":
			_, _ = io.WriteString(sess, h.reg.Help())
		case "matrix":
			// v0.6.0：滚动雨帘。限帧率、固定行数，只用 SGR 绿色 + 普通字符，
			// 不触碰被 ADR-027 禁止的 ED 清屏 / CUP 定位 / 备用屏序列。
			h.runMatrix(ctx, sess)
		case "fortune", "cowsay", "xkcd", "blame", "sudo":
			// v0.5.1 / P3-M6：彩蛋都是固定语料，经安全渲染路径输出。
			// 参数（如 /blame @user）只做个数校验，绝不回显原文（ADR-026）。
			writeEgg(sess, h.eggText(inv.Name)+"\r\n")
		case "whoami":
			// ADR-065：本人资料（账号 / 注册时间 / 声望等级），分段渲染。
			h.runWhoami(ctx, sess, sess.Principal())
		case "who":
			// ADR-065：在线人员名单（参数分页），所有人可见。
			h.runWho(ctx, sess, inv)
		case "stats":
			// v0.7.1：社区统计看板。所有人可见（含访客），输出全是固定文案 + 本地数字。
			writeSystem(sess, h.runStats(ctx)+"\r\n")
		case "keys":
			_, _ = io.WriteString(sess, h.runKeys(ctx, sess.Principal(), inv))
		case "key":
			_, _ = io.WriteString(sess, h.runKey(ctx, sess, inv))
		case "login":
			if isGitHubLogin(inv) {
				// 发起前先按 hash(ip) 限流；命中只给固定文案 + 本地算出的秒数，
				// 不回显 IP、邮箱或底层错误原文（P0-22 零回显）。
				if err := h.gate.AllowDeviceStart(sess.RemoteIPHash()); err != nil {
					var rl *auth.RateLimitError
					if errors.As(err, &rl) {
						_, _ = fmt.Fprintf(sess, "%s（请 %d 秒后重试）\r\n",
							auth.LoginTextRateLimited, rl.RetryAfterSeconds())
					} else {
						_, _ = io.WriteString(sess, auth.LoginTextRateLimited+"\r\n")
					}
					return outcomeContinue
				}
				return outcomeStartLogin // 发起屏由 loginFlow.begin 统一输出
			}
			if isEmailLogin(inv) {
				if h.email == nil {
					_, _ = io.WriteString(sess, emailTextNotReady+"\r\n")
				} else {
					pc.clearEmailFlow() // 若已有邮箱流程未结束，先复位其 secret 掩码
					pc.emailFlow = newEmailFlow(emailFlowLogin, h.email, sess, pc.echo, &pc.pendingEmail, &pc.pendingPurpose, "", false, h.onLogin)
				}
				return outcomeContinue
			}
			_, _ = io.WriteString(sess, auth.LoginTextUnsupportedProvider+"\r\n")
		case "register", "recover", "verify", "resend":
			h.handleEmailCommand(ctx, sess, pc, inv)
		case "logout":
			_, _ = io.WriteString(sess, h.runLogout(ctx, sess))
		default:
			// top / open / new / reply 由页面控制器接管；命令表仍是唯一入口。
			_ = pc.command(ctx, sess, inv)
		}
	}
	return outcomeContinue
}

// runLogout 执行主动注销：撤销该账号全部在线会话并留审计。
// 确认行在撤销之前写出，保证当前会话被关闭前用户能看到反馈；
// 撤销之后的返回值是 best-effort（连接可能已被自己踢掉）。
func (h *inputHandler) runLogout(ctx context.Context, sess *sshd.Session) string {
	if h.logout == nil {
		return "注销服务尚未就绪\r\n"
	}
	p := sess.Principal()
	if !p.IsUser() {
		return "访客无需注销\r\n"
	}
	_, _ = io.WriteString(sess, "正在注销，撤销该账号全部在线会话…\r\n")
	res, err := h.logout.Logout(ctx, p.UserID, sess.RemoteIPHash())
	switch {
	case err != nil:
		return fmt.Sprintf("注销已生效（关闭 %d 个会话），但审计记录失败，请联系运维\r\n", res.SessionsClosed)
	case res.SessionsClosed > 0:
		return fmt.Sprintf("已注销，撤销在线会话 %d 个\r\n", res.SessionsClosed)
	default:
		return "已注销当前账号\r\n"
	}
}

// writeSystem 经安全渲染路径输出**服务端固定文案**（ADR-019 / ADR-026）。
// 适用于彩蛋语料、/stats、/who 汇总行等「全由常量与本地数字拼成」的多行文本。
// 含用户数据的行（/whoami 账号、/who 名单）分段走 segSystem/segUser，不进这里。
//
// 传入文本里的 \n 由 RenderSystem 校验（它拒绝 \r），故先规范化换行再渲染，
// 最后统一转 CRLF。渲染失败时退化为不输出——绝不把原始文本裸写到终端。
func writeSystem(sess io.Writer, text string) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	rendered, err := safe.RenderSystem(normalized, safe.StylePlain)
	if err == nil {
		_, _ = io.WriteString(sess, toCRLF(rendered))
	}
}

// writeEgg 是彩蛋语料的入口，保留旧名以固定调用点语义（内部即 writeSystem）。
// v0.7.1 把实现抽为 writeSystem 供 /stats 复用，调用点不必区分「彩蛋」与「系统文案」。
func writeEgg(sess io.Writer, text string) { writeSystem(sess, text) }

// runMatrix 输出固定行数的滚动雨帘（v0.6.0）。
// 合规约束：只经 safe.RenderSystem 的 SGR 样式白名单上色，帧内容是 MatrixFrame()
// 生成的固定字符集，不含 ESC；不用 ED 清屏 / CUP 光标定位 / 备用屏（ADR-027）。
// 帧率固定 ~50ms/行、总行数固定，ctx 取消（连接断开）立即停。
func (h *inputHandler) runMatrix(ctx context.Context, sess io.Writer) {
	const (
		lines    = 24
		frameGap = 50 * time.Millisecond
	)
	ticker := time.NewTicker(frameGap)
	defer ticker.Stop()
	for i := 0; i < lines; i++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		rendered, err := safe.RenderSystem(command.MatrixFrame(), safe.StyleCode)
		if err != nil {
			return // 安全渲染失败立即停，绝不裸写
		}
		if _, err := io.WriteString(sess, rendered+"\r\n"); err != nil {
			return
		}
	}
}

// eggText 返回彩蛋固定文案（v0.5.1 / P3-M6）。纯常量语料，不含用户数据。
func (h *inputHandler) eggText(name string) string {
	switch name {
	case "fortune":
		return command.Fortune()
	case "cowsay":
		return command.Cowsay()
	case "xkcd":
		return command.XKCD()
	case "blame":
		return command.Blame()
	case "sudo":
		return command.Sudo()
	default:
		return "" // 不可达：switch 分支只把已知彩蛋名传进来
	}
}

// karmaLevel 与 domain/user.Level 的阈值对齐（L2=10 / L3=50 / L4=200）。
// next=0 表示已达最高等级。ADR-065：/karma 命令入口删除后，本函数由 /whoami 复用，
// 与 user.Level 的对拍见 TestKarmaLevelMatchesDomainLevel。
func karmaLevel(karma int64) (level int, next int64) {
	switch {
	case karma < 10:
		return 1, 10
	case karma < 50:
		return 2, 50
	case karma < 200:
		return 3, 200
	default:
		return 4, 0
	}
}

func progressDesc(level int, next int64) string {
	if next == 0 {
		return "已达最高等级"
	}
	return fmt.Sprintf("距 L%d 还需 %d 声望", level+1, next)
}

// runKeys 不带参数时列出当前账号未吊销的公钥；带一个指纹参数时查单钥详情
// （P1-M4）。指纹来自 ssh_keys 表（服务端数据源），且逐条再过一次
// ValidFingerprint 白名单，脏数据也不会把控制字符写进终端。
func (h *inputHandler) runKeys(ctx context.Context, p auth.Principal, inv command.Invocation) string {
	if h.keys == nil {
		return "公钥管理尚未就绪\r\n"
	}
	if !p.IsUser() {
		return "请先 /login github 登录后管理公钥\r\n"
	}
	if len(inv.Args) > 0 {
		return h.runKeyDetail(ctx, p.UserID, inv.Args[0])
	}
	list, err := h.keys.List(ctx, p.UserID)
	if err != nil {
		return "读取公钥列表失败\r\n"
	}
	if len(list) == 0 {
		return "当前账号未绑定公钥\r\n"
	}
	var b strings.Builder
	b.WriteString("已绑定公钥：\r\n")
	for _, k := range list {
		if !auth.ValidFingerprint(k.Fingerprint) {
			continue
		}
		_, _ = fmt.Fprintf(&b, "  %s\r\n", k.Fingerprint)
	}
	return b.String()
}

// formatKeyTime 按进程展示时区格式化公钥时间戳：绝对时间 + 该时刻的时区缩写。
// 时区缩写取自时间戳自身所在时刻（而非 time.Now），因此跨 DST 的历史时间也正确；
// 固定偏移制时区可能给出空缩写，此时回退到完整时区名，绝不硬编码 "UTC" 误导用户。
// 展示时区的唯一来源是 terminal.DisplayLocation()，与论坛列表/详情完全一致。
func formatKeyTime(sec int64) string {
	loc := terminal.DisplayLocation()
	t := time.Unix(sec, 0).In(loc)
	zone := t.Format("MST")
	if zone == "" {
		zone = loc.String()
	}
	return t.Format("2006-01-02 15:04:05") + " " + zone
}

// runKeyDetail 输出单把钥匙的 label / created_at / last_used_at。
// 输出全是服务端固定模板：指纹经白名单校验，label 经 SanitizeText 净化
// （当前无输入口、恒为空；保留净化是为了将来加 label 时不产生渲染旁路）。
// 归属过滤在用例/仓储层完成，跨用户与不存在都返回同一句固定文案。
func (h *inputHandler) runKeyDetail(ctx context.Context, userID int64, rawFingerprint string) string {
	fp, err := auth.NormalizeFingerprint(rawFingerprint)
	if err != nil {
		return "指纹格式不合法（用法 /keys <fingerprint>）\r\n"
	}
	k, err := h.keys.Get(ctx, userID, fp)
	if err != nil {
		return "未找到该公钥绑定\r\n"
	}
	if !auth.ValidFingerprint(k.Fingerprint) {
		return "公钥记录不合法，未展示\r\n"
	}
	label := safe.SanitizeText(k.Label)
	if label == "" {
		label = "（未设置）" // label 恒空：P1 不提供设置入口
	}
	var b strings.Builder
	b.WriteString("公钥详情：\r\n")
	_, _ = fmt.Fprintf(&b, "  指纹：%s\r\n", k.Fingerprint)
	_, _ = fmt.Fprintf(&b, "  标签：%s\r\n", label)
	_, _ = fmt.Fprintf(&b, "  创建：%s\r\n", formatKeyTime(k.CreatedAtUnix))
	if k.HasLastUsed {
		_, _ = fmt.Fprintf(&b, "  最近使用：%s\r\n", formatKeyTime(k.LastUsedUnix))
	} else {
		b.WriteString("  最近使用：从未\r\n")
	}
	return b.String()
}

// runKey 分发 /key add 与 /key rm <fingerprint>。
//
// 安全边界（§4.6 / P0-18 验收）：
//   - 子命令由 auth.ParseKeyCommand 严格收口，add 恰好 1 个参数，
//     伪造的公钥字符串连用例入口都进不去；
//   - BindRequest 的 Fingerprint / PublicKey **只从 Session 读取**
//     （服务端认证阶段产物），绝不来自 inv.Args；
//   - 输出全是服务端固定文案，不回显指纹 / 公钥 / 错误信息原文。
func (h *inputHandler) runKey(ctx context.Context, sess *sshd.Session, inv command.Invocation) string {
	if h.keys == nil {
		return "公钥管理尚未就绪\r\n"
	}
	cmd, err := auth.ParseKeyCommand(inv.Args)
	if err != nil {
		return "用法：/key add 或 /key rm <fingerprint>（不接受公钥文本参数）\r\n"
	}
	p := sess.Principal()
	if !p.IsUser() {
		return "请先 /login github 登录后管理公钥\r\n"
	}

	switch cmd.Kind {
	case auth.KeySubAdd:
		res, err := h.keys.Bind(ctx, auth.BindRequest{
			UserID:      p.UserID,
			Fingerprint: sess.Fingerprint(),
			PublicKey:   sess.PublicKey(),
		})
		return bindResultText(res, err)
	case auth.KeySubRemove:
		res, err := h.keys.Unbind(ctx, auth.UnbindRequest{
			UserID:      p.UserID,
			Fingerprint: cmd.Fingerprint,
		})
		return unbindResultText(res, err)
	default:
		return "用法：/key add 或 /key rm <fingerprint>\r\n"
	}
}

// bindResultText / unbindResultText 把结果映射成**固定文案**。
// 刻意不用 err.Error()：错误信息可能带外部数据，一旦写回终端就是渲染旁路
// （P0-22 / P0-23 的零回显与分层渲染约束提前落地）。
func bindResultText(res auth.BindResult, err error) string {
	switch {
	case err == nil && res.Created:
		return "已绑定当前连接的公钥\r\n"
	case err == nil:
		return "当前连接的公钥已绑定，无需重复操作\r\n"
	case errors.Is(err, auth.ErrBindingRequiresLogin):
		return "请先登录后再绑定公钥\r\n"
	case errors.Is(err, auth.ErrKeyBoundToOtherUser):
		return "该公钥已属于其他账号，无法绑定\r\n"
	case errors.Is(err, auth.ErrKeyRevokedBinding):
		return "该公钥已吊销，无法重新绑定\r\n"
	case errors.Is(err, auth.ErrInvalidPublicKey), errors.Is(err, auth.ErrInvalidFingerprint),
		errors.Is(err, auth.ErrEmptyFingerprint):
		return "当前连接的公钥信息不合法，无法绑定\r\n"
	default:
		return "绑定公钥失败，请稍后重试\r\n"
	}
}

func unbindResultText(res auth.UnbindResult, err error) string {
	switch {
	case err == nil && res.Revoked:
		return fmt.Sprintf("已解绑公钥，撤销在线会话 %d 个\r\n", res.SessionsClosed)
	case err == nil:
		return fmt.Sprintf("该公钥此前已解绑（撤销在线会话 %d 个）\r\n", res.SessionsClosed)
	case errors.Is(err, auth.ErrBindingRequiresLogin):
		return "请先登录后再解绑公钥\r\n"
	case errors.Is(err, auth.ErrKeyNotFound):
		return "未找到该公钥绑定\r\n"
	case errors.Is(err, auth.ErrKeyBoundToOtherUser):
		return "无法解绑其他账号的公钥\r\n"
	default:
		return "解绑公钥失败，请稍后重试\r\n"
	}
}

// applyKey 是纯状态机，不碰 IO，便于在没有 SSH 会话的情况下回归。
// 返回动作、提交时的命令行文本、粘贴块字节数。
func applyKey(cl *commandLine, m input.KeyMsg) (cmdAction, string, int) {
	if cl.active {
		switch m.Kind {
		case input.KeyEnter:
			line := string(cl.runes)
			cl.active, cl.runes = false, nil
			return actionSubmit, line, 0
		case input.KeyEsc, input.KeyCtrlC:
			cl.active, cl.runes = false, nil
			return actionCancel, "", 0
		case input.KeyCtrlD:
			return actionQuit, "", 0
		case input.KeyBackspace:
			if len(cl.runes) > 1 {
				cl.runes = cl.runes[:len(cl.runes)-1]
			}
			return actionNone, "", 0
		case input.KeyRune:
			cl.runes = append(cl.runes, m.Rune)
			return actionNone, "", 0
		case input.KeyPaste:
			for _, r := range m.Text {
				cl.runes = append(cl.runes, r)
			}
			return actionNone, "", 0
		default:
			return actionNone, "", 0
		}
	}

	switch m.Kind {
	case input.KeyCtrlC, input.KeyCtrlD:
		return actionQuit, "", 0
	case input.KeyArrowDown:
		return actionMoveDown, "", 0
	case input.KeyArrowUp:
		return actionMoveUp, "", 0
	case input.KeyArrowRight, input.KeyEnter:
		return actionOpen, "", 0
	case input.KeyArrowLeft:
		return actionBack, "", 0
	case input.KeyRune:
		switch m.Rune {
		case '?':
			return actionHelp, "", 0
		case '/':
			cl.active = true
			cl.runes = []rune{'/'}
			return actionCommandStart, "", 0
		case 'j':
			return actionMoveDown, "", 0
		case 'k':
			return actionMoveUp, "", 0
		case 'l':
			return actionOpen, "", 0
		case 'h', 'q':
			// §3.1 / P0-25：q 是「返回上一层」，不是退出；
			// 退出统一走 /quit 或 Ctrl-C / Ctrl-D。
			return actionBack, "", 0
		case 'r':
			return actionReply, "", 0
		case 'n':
			return actionNextPage, "", 0
		case 'p':
			return actionPrevPage, "", 0
		}
	case input.KeyEsc:
		// 浏览态没有可取消对象：静默（actionNone），不输出「（取消）」也不让
		// 屏幕失效。编辑态 Esc 由 editorKey 处理，命令输入态 Esc 仍是 actionCancel。
		return actionNone, "", 0
	case input.KeyPaste:
		return actionPasteNotice, "", len(m.Text)
	}
	return actionNone, "", 0
}

// formatResult 只输出服务端生成的文本或注册表白名单里的命令名，
// 绝不把用户提交的原始字节（含 ESC / CSI / OSC）写回终端。
// 参数 inv.Args 也刻意不输出：它来自用户输入，不是注册表常量。
func formatResult(reg *command.Registry, inv command.Invocation, err error) string {
	switch {
	case errors.Is(err, command.ErrUnknownCommand):
		return "未知命令（按 ? 查看全部命令）\r\n"
	case errors.Is(err, command.ErrArgCount):
		return "参数个数不符（按 ? 查看用法）\r\n"
	case errors.Is(err, command.ErrEmptyCommand), errors.Is(err, command.ErrNotCommand):
		return "（未识别的命令输入）\r\n"
	case err != nil:
		return "命令无法解析\r\n"
	}
	switch inv.Name {
	case "help":
		return reg.Help()
	case "quit":
		return "再见\r\n"
	default:
		return fmt.Sprintf("已收到 /%s\r\n", inv.Name)
	}
}
