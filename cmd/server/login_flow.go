package main

// P0-19 登录交互编排（docs/DEVIATIONS.md R18 / R19 / R20）。
//
// 这是**最小交互层**：不改 transport/ssh 协议层、不改命令注册表、不改
// application/auth 的四个用例，只在 cmd 装配层把 P0-15/16/17/18 串成
// `/login github` 的完整链路。论坛页面控制器已落地（page_controller.go），
// 但 /login 的呈现职责仍留在本文件与 login_view.go，作为独立替换点保留
// （DEVIATIONS.md R18/R20）；命令表与协议层零改动。
//
// 三条硬边界：
//  1. device_code 只保存在本结构体的进程内字段，绝不写终端 / 日志 / 库；
//  2. access_token 只在 finish 里进程内交 LoginService，不进任何呈现结构；
//  3. 授权成功后顺序 Login → Bind → UpgradeToUser，**三步全成功才升级会话**；
//     任何一步失败都保持访客态，不落半成品（§4.6 验收 / ADR-024 第 6 条）。

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

// loginConn 是 /login 对「当前连接」的受限视图。
// *sshd.Session 天然实现；测试用 fake 实现，避免起真实 SSH 就能回归全链路。
type loginConn interface {
	ID() string
	Fingerprint() string
	PublicKey() string
	Principal() auth.Principal
	UpgradeToUser(userID int64) error
}

// loginPollMsg 是一次异步 Poll 的结果。网络调用最长 10s，若放在会话循环里
// 同步执行，Esc 就无法即时取消；因此由 Serve 起 goroutine、经 channel 回传。
// 取消后迟到的结果由主循环丢弃（ADR-022 的 CAS 保证「取消优先」）。
type loginPollMsg struct {
	result auth.PollResult
	err    error
}

// loginFlow 只做「事件 → 动作」决策，不持有时间循环：节奏由
// inputHandler.Serve 的 timer 驱动，状态机（P0-16）也不 sleep。
type loginFlow struct {
	device *auth.DeviceFlowService
	login  *auth.LoginService
	keys   *auth.KeyBindingService
	gate   *auth.SecurityGate // P1-M2：失败留痕 + 按 IP 限流；nil 时零影响
	view   loginPresenter

	// onLogin 在会话升级成功后调用的签到钩子（v0.6.0），nil 时跳过。
	onLogin func(context.Context, int64)

	active     bool
	deviceCode string // 进程内唯一副本；绝不呈现、不落库
	ipHash     string // 对端 IP 的 SHA-256；只作限流/审计键，不呈现
	expiresAt  time.Time
	retries    int
	now        func() time.Time
}

// begin 发起登录并输出发起屏，返回首次轮询延迟与是否进入登录态。
func (f *loginFlow) begin(ctx context.Context, conn loginConn, out io.Writer) (time.Duration, bool, error) {
	if f.active {
		return 0, false, nil
	}
	if conn.Principal().IsUser() {
		_ = f.view.line(out, auth.LoginTextAlreadyUser, safe.StylePlain)
		return 0, false, nil
	}

	st, err := f.device.Start(ctx, conn.ID())
	if err != nil {
		_ = f.view.line(out, loginStartFailureText(err), safe.StylePlain)
		return 0, false, err
	}
	v, err := auth.ProjectLoginView(st, f.now().UTC())
	if err != nil {
		// 失败关闭：响应字段不合法 → 立即作废旧码，不留可轮询的脏状态。
		_ = f.device.Cancel(ctx, st.DeviceCode)
		_ = f.view.line(out, auth.LoginTextDeviceCodeInvalidated, safe.StylePlain)
		return 0, false, err
	}
	// 终端宽度只来自连接自身的 pty-req；loginConn 不暴露 Window，
	// 用可选接口适配，测试 fake 无需实现新方法（0 时 Renderer 按 80 列降级）。
	if w, ok := conn.(interface {
		Window() (uint32, uint32, string)
	}); ok {
		width, _, _ := w.Window()
		f.view.width = int(width)
	}
	if err := f.view.showStart(out, v); err != nil {
		_ = f.device.Cancel(ctx, st.DeviceCode)
		return 0, false, err
	}

	f.active = true
	f.deviceCode = st.DeviceCode
	f.expiresAt = st.ExpiresAt
	f.retries = 0
	return time.Duration(v.IntervalSeconds) * time.Second, true, nil
}

// loginStartFailureText 把「无法起流」的原因映射成可操作提示。
// 刻意不回显 err.Error()：它来自外部依赖，拼进输出就是渲染旁路（P0-22/P0-23）。
func loginStartFailureText(err error) string {
	if errors.Is(err, auth.ErrClientIDMissing) {
		return auth.LoginTextClientIDMissing
	}
	return auth.LoginTextLoginFailed
}

// pollCode 返回本次轮询用的 device_code，供 Serve 的异步 Poll goroutine 使用。
func (f *loginFlow) pollCode() string { return f.deviceCode }

// applyPoll 把一次 Poll 结果映射成「是否结束 + 下次延迟」。
func (f *loginFlow) applyPoll(ctx context.Context, conn loginConn, out io.Writer, r auth.PollResult, pollErr error) (bool, time.Duration, error) {
	if !f.active {
		return true, 0, nil
	}
	d := auth.ClassifyPoll(r, pollErr)
	switch d.Kind {
	case auth.DirectiveRetry:
		f.retries++
		if f.retries > auth.LoginMaxNetworkRetries {
			f.active = false
			_ = f.view.line(out, auth.LoginTextNetworkGiveUp, safe.StylePlain)
			return true, 0, nil
		}
		_ = f.view.line(out, auth.LoginTextRetry, safe.StyleDim)
		return false, time.Duration(d.DelaySeconds) * time.Second, nil

	case auth.DirectiveContinue:
		f.retries = 0
		// 本地到点保护：状态机 GateExpired 会转 expired，但会话循环也不该在
		// expires_at 之后再发网络请求。
		if !f.now().UTC().Before(f.expiresAt) {
			_ = f.device.Cancel(ctx, f.deviceCode)
			f.active = false
			_ = f.view.line(out, auth.LoginTextExpired, safe.StylePlain)
			return true, 0, nil
		}
		_ = f.view.line(out, auth.LoginTextWaiting, safe.StyleDim)
		return false, time.Duration(d.DelaySeconds) * time.Second, nil

	case auth.DirectiveTerminal:
		f.active = false
		// 终止态（拒绝 / 过期）是一次安全相关事件：先留审计再渲染固定文案。
		// 审计环满时返回错误，但终止态已经确定，仍要给用户明确提示。
		_ = f.gate.NoteLoginFailure(f.ipHash, "")
		_ = f.view.line(out, auth.TerminalLoginText(r.State.Status), safe.StylePlain)
		return true, 0, nil

	case auth.DirectiveAuthorized:
		return f.finish(ctx, conn, out, r.Token.Token)

	default:
		// 未知指令失败关闭，不静默降级成「继续轮询」。
		f.active = false
		_ = f.view.line(out, auth.LoginTextDenied, safe.StylePlain)
		return true, 0, nil
	}
}

// finish 是 OutcomeAuthorized 后的三步：Login → Bind → UpgradeToUser。
//
// R19：账号事务与公钥绑定是两个独立写事务，中间隔着两次网络调用，无法原子提交。
// 因此这里显式处理「账号已建但绑定失败」——**不升级会话**，返回固定文案提示
// 换钥；device_auth_states 此时已是 authorized 终态，不是 denied（denied 只由
// Esc 取消 / 用户拒绝产生）。
func (f *loginFlow) finish(ctx context.Context, conn loginConn, out io.Writer, token string) (bool, time.Duration, error) {
	f.active = false
	_ = f.view.line(out, auth.LoginTextAuthorizing, safe.StylePlain)

	res, err := f.login.Login(ctx, token)
	if err != nil {
		// 账号创建失败同样是登录失败：审计入队失败时以它的错误为准上报，
		// 但终端只输出固定文案，不回显底层错误原文。
		if gateErr := f.gate.NoteLoginFailure(f.ipHash, ""); gateErr != nil {
			err = gateErr
		}
		_ = f.view.line(out, auth.LoginTextLoginFailed, safe.StylePlain)
		return true, 0, err
	}

	// D2：登录成功后当前连接公钥自动绑定，直接复用 KeyBindingService.Bind，
	// **不经命令表**（ADR-024 第 5 条）。指纹 / 公钥只来自 conn —— 即认证阶段
	// 服务端持有的 ssh.PublicKey，绝不是客户端提交的文本。
	_, err = f.keys.Bind(ctx, auth.BindRequest{
		UserID:      res.UserID,
		Fingerprint: conn.Fingerprint(),
		PublicKey:   conn.PublicKey(),
	})
	if err != nil {
		_ = f.view.line(out, auth.LoginTextBindFailed, safe.StylePlain)
		return true, 0, err
	}

	// 绑定成功后才升级会话；失败一律保持访客态。
	if err := conn.UpgradeToUser(res.UserID); err != nil {
		_ = f.view.line(out, auth.LoginTextUpgradeFailed, safe.StylePlain)
		return true, 0, err
	}
	// 签到是登录成功的副作用（v0.6.0）：失败不影响登录结果，只记日志由调用方处理。
	if f.onLogin != nil {
		f.onLogin(ctx, res.UserID)
	}

	notice := ""
	if res.Notice == auth.EmailOccupiedNotice {
		notice = auth.EmailOccupiedNotice // 只有白名单常量允许进 RenderSystem
	}
	if err := f.view.showSuccess(out, res.Username, notice, res.Created); err != nil {
		return true, 0, err
	}
	return true, 0, nil
}

// cancel 由 Esc / 连接关闭调用：pending → denied，不留下未绑定半成品。
func (f *loginFlow) cancel(ctx context.Context, out io.Writer) {
	if !f.active {
		return
	}
	_ = f.device.Cancel(ctx, f.deviceCode)
	f.active = false
	_ = f.view.line(out, auth.LoginTextCancelled, safe.StylePlain)
}

// isGitHubLogin 是 /login 的参数白名单：P0 只支持 github，其余 provider
// 一律拒绝，绝不把 inv.Args 原文写回终端（P0-22 零回显约束）。
func isGitHubLogin(inv command.Invocation) bool {
	return inv.Name == "login" && len(inv.Args) == 1 && inv.Args[0] == "github"
}

// isEmailLogin 是 /login 的第二个白名单 provider（P1-M5）。
// 参数必须精确等于 email，其余一律落到 UnsupportedProvider，绝不回显原文。
func isEmailLogin(inv command.Invocation) bool {
	return inv.Name == "login" && len(inv.Args) == 1 && inv.Args[0] == "email"
}

// isLoginCancelKey 只把 Esc / Ctrl-C 当作取消；其余按键在登录态下一律忽略
// 且零回显（P0-22 的用户文本不得写回终端）。
func isLoginCancelKey(m input.KeyMsg) bool {
	return m.Kind == input.KeyEsc || m.Kind == input.KeyCtrlC
}
