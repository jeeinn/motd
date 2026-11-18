package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

// inputHandler 把 SSH 字节流归一化后交给命令注册表的唯一入口（§3.2）。
// P0-19 起在 handle 分发里接入 /login 最小交互层（docs/DEVIATIONS.md R18）。
// P0-24 的页面控制器只替换 loginPresenter / loginFlow 的呈现职责，
// 解码器、命令表与 transport/ssh 协议层保持不变。
//
// 安全约束：绝不把 KeyMsg.Raw / KeyMsg.Text 写回终端。用户提交的文本
// 只能经 P0-23 的净化 + 白名单渲染后才能出现；在此之前一律零回显。
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
}

func newInputHandler() *inputHandler {
	return &inputHandler{reg: command.Default()}
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
	return &loginFlow{
		device: h.deviceAuth,
		login:  h.login,
		keys:   h.keys,
		now:    time.Now,
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

// commandLine 是斜杠命令的行编辑缓冲。P0-22 只做收集与提交，不回显原文。
type commandLine struct {
	active bool
	runes  []rune
}

type cmdAction uint8

const (
	actionNone cmdAction = iota
	actionSubmit
	actionCancel
	actionQuit
	actionHelp
	actionCommandStart
	actionPasteNotice
	actionEscNotice
)

func (h *inputHandler) Serve(ctx context.Context, sess *sshd.Session) error {
	_, _ = fmt.Fprintf(sess, "motd %s\r\n", version)
	_, _ = io.WriteString(sess, "访客只读。按 ? 查看命令；页面将在 P0-24 接入。\r\n")
	_ = sess.SetState(presence.StateBrowsing)

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
			}
			return false
		}
		switch h.handle(ctx, sess, m, &cl) {
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

func (h *inputHandler) handle(ctx context.Context, sess *sshd.Session, m input.KeyMsg, cl *commandLine) handleOutcome {
	action, line, pasteLen := applyKey(cl, m)
	switch action {
	case actionQuit:
		return outcomeQuit
	case actionCancel:
		_, _ = io.WriteString(sess, "（已取消命令输入）\r\n")
	case actionHelp:
		_, _ = io.WriteString(sess, h.reg.Help())
	case actionCommandStart:
		_, _ = io.WriteString(sess, "命令输入：以 / 开头，Enter 提交，Esc 取消。\r\n")
	case actionPasteNotice:
		_, _ = fmt.Fprintf(sess, "（已收到粘贴块 %d 字节，P0-24 接入编辑器）\r\n", pasteLen)
	case actionEscNotice:
		_, _ = io.WriteString(sess, "（取消）\r\n")
	case actionSubmit:
		inv, err := h.reg.Parse(line)
		out := formatResult(h.reg, inv, err)
		outcome := outcomeContinue
		if err == nil {
			// P0-18：/keys 与 /key 走真实业务动作；P0-19：/login github 起
			// 最小交互层；其余仍由 P0-24 页面控制器接入。解析失败时只输出
			// 固定文案，绝不把 inv.Args 原文写回终端。
			switch inv.Name {
			case "keys":
				out = h.runKeys(ctx, sess)
			case "key":
				out = h.runKey(ctx, sess, inv)
			case "login":
				if isGitHubLogin(inv) {
					outcome = outcomeStartLogin
					out = "" // 发起屏由 loginFlow.begin 统一输出
				} else {
					out = auth.LoginTextUnsupportedProvider + "\r\n"
				}
			}
		}
		if out != "" {
			_, _ = io.WriteString(sess, out)
		}
		if inv.Name == "quit" && err == nil {
			return outcomeQuit
		}
		return outcome
	}
	return outcomeContinue
}

// runKeys 列出当前账号未吊销的公钥。指纹来自 ssh_keys 表（服务端数据源），
// 且逐条再过一次 ValidFingerprint 白名单，脏数据也不会把控制字符写进终端。
func (h *inputHandler) runKeys(ctx context.Context, sess *sshd.Session) string {
	if h.keys == nil {
		return "公钥管理尚未就绪\r\n"
	}
	p := sess.Principal()
	if !p.IsUser() {
		return "请先 /login github 登录后管理公钥\r\n"
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
	case input.KeyRune:
		switch m.Rune {
		case '?':
			return actionHelp, "", 0
		case '/':
			cl.active = true
			cl.runes = []rune{'/'}
			return actionCommandStart, "", 0
		}
	case input.KeyEsc:
		return actionEscNotice, "", 0
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
		return fmt.Sprintf("已收到 /%s（P0-24 接入业务动作）\r\n", inv.Name)
	}
}
