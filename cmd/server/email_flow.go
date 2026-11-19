package main

// 邮箱认证的分步输入（P1-M5）：只做「提示 → 收集 → 调用例 → 输出固定文案」。
// 三条硬边界：
//  1. 密码字段一律 setSecret(true)，只回显 '*'（ADR-026）；
//  2. 用户输入经 lineEcho 净化回显，绝不把 KeyMsg.Raw/Text 写回终端（P0-23）；
//  3. 错误一律映射成服务端固定文案，绝不回显底层 err（可能含外部数据）。
//
// 与 loginFlow 一样，flow 是每会话对象；pendingEmail 放在 pageController
// 里跨命令存活（/register → /verify），重连即失效，符合短 TTL 预期。

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
)

type emailFlowKind uint8

const (
	emailFlowRegister emailFlowKind = iota
	emailFlowRecover
	emailFlowLogin
	emailFlowVerify
)

// 固定的提示与结果文案（走 RenderSystem / 固定文本路径，不含任何用户数据）。
const (
	emailHintRegister   = "邮箱注册：请输入邮箱后按 Enter 确认（Esc 取消）"
	emailHintRecover    = "密码找回：请输入邮箱后按 Enter 确认（Esc 取消）"
	emailHintLoginEmail = "邮箱登录：请输入邮箱后按 Enter 确认（Esc 取消）"
	emailHintPassword   = "请输入密码后按 Enter 确认（Esc 取消）"
	emailHintSetPwd     = "请设置密码（至少 8 位）后按 Enter 确认（Esc 取消）"
	emailHintNewPwd     = "请输入新密码（至少 8 位）后按 Enter 确认（Esc 取消）"
	emailHintConfirmPwd = "请再次输入密码确认（Esc 取消）"

	emailTextRegisterSent = "验证码已发送至邮箱，请在 10 分钟内输入 /verify <验证码> 完成注册。"
	emailTextRecoverSent  = "若邮箱已注册，验证码已发送；请在 10 分钟内输入 /verify <验证码> 重置密码。"
	emailTextRegistered   = "注册完成，账号已创建；请输入 /login email 使用邮箱密码登录。"
	emailTextRecovered    = "密码已重置，旧会话已全部失效；请用 /login email 重新登录。"
	emailTextLoggedin     = "邮箱登录成功。"
	emailTextCancelled    = "（已取消邮箱操作）"
	emailTextNotReady     = "邮箱服务尚未就绪。"
	emailTextNoPending    = "请先执行 /register 或 /recover，再输入 /verify <验证码>。"
	emailTextBadEmail     = "邮箱格式不合法，请重新执行 /register 或 /recover。"
	emailTextBadCode      = "验证码无效、已使用或已过期，请重新 /register 或 /recover。"
	emailTextBadPassword  = "密码至少 8 位，请重新输入 /verify <验证码>。"
	emailTextPwdMismatch  = "两次密码不一致，请重新输入 /verify <验证码>。"
	emailTextBadLogin     = "邮箱或密码错误。"
	emailTextSendFailed   = "验证码邮件发送失败，请稍后重试或输入 /resend。"
	emailTextFailed       = "邮箱操作失败，请稍后重试。"
	emailTextResendOK     = "验证码已重发，请在 10 分钟内输入 /verify <验证码>。"
)

// emailSession 是邮箱流程对当前连接的受限视图；*sshd.Session 天然实现，
// 测试可用 fake，无需起真实 SSH。
type emailSession interface {
	io.Writer
	ID() string
	Principal() auth.Principal
	RemoteIPHash() string
	UpgradeToUser(userID int64) error
}

// emailFlow 是单条邮箱命令的分步输入状态。
type emailFlow struct {
	kind    emailFlowKind
	svc     *auth.EmailAuthService
	sess    emailSession
	echo    *lineEcho
	pending *string // 指向 pageController.pendingEmail，跨命令存活
	purpose *string // 指向 pageController.pendingPurpose（register/recover）
	ipHash  string

	code    string // verify 的验证码（来自命令行参数，不回显原文到结果）
	email   string
	recover bool // verify：pending 是找回流程 → 提示输入新密码
	buf     []rune
	stage   int // login: 0=email 1=password；其余只有 stage 0
	// verify 的密码二次确认（仅 register/recover 的 verify，login 不使用）：
	// 第一次输入存 firstPwd，confirm=true 时进入二次输入；两次一致才提交 service。
	firstPwd string
	confirm  bool
}

// newEmailFlow 创建一条流程并输出第一步提示。svc 为空由调用方拦截，
// 这里不做静默降级。
func newEmailFlow(kind emailFlowKind, svc *auth.EmailAuthService, sess emailSession, echo *lineEcho, pending, purpose *string, code string, recover bool) *emailFlow {
	f := &emailFlow{kind: kind, svc: svc, sess: sess, echo: echo, pending: pending, purpose: purpose, code: code, recover: recover, ipHash: sess.RemoteIPHash()}
	f.showStep()
	return f
}

// showStep 输出当前步骤提示并切换回显模式。
func (f *emailFlow) showStep() {
	prompt, secret := f.currentPrompt()
	_, _ = io.WriteString(f.sess, prompt+"\r\n")
	f.echo.setSecret(secret)
	f.buf = nil
}

func (f *emailFlow) currentPrompt() (string, bool) {
	switch f.kind {
	case emailFlowRegister:
		return emailHintRegister, false
	case emailFlowRecover:
		return emailHintRecover, false
	case emailFlowVerify:
		if f.confirm {
			return emailHintConfirmPwd, true
		}
		if f.recover {
			return emailHintNewPwd, true
		}
		return emailHintSetPwd, true
	case emailFlowLogin:
		if f.stage == 0 {
			return emailHintLoginEmail, false
		}
		return emailHintPassword, true
	default:
		return emailHintPassword, true
	}
}

// finish 复位本流程对 echo 的副作用：secret 掩码必须随流程结束一起消失，
// 否则下一条斜杠命令的回显会被永久打成 '*'（用户实测 /login 被掩码）。
// 幂等：finish 与 clearEmailFlow 可能都会调到。
func (f *emailFlow) finish() {
	f.echo.setSecret(false)
}

// step 处理一个按键。done=true 表示流程结束（成功或失败），调用方应清掉 flow。
// 流程一旦结束（done/cancelled）立即复位回显掩码，覆盖所有出口，
// 调用方无论在哪里清 flow 都不会遗漏 secret 泄漏。
func (f *emailFlow) step(ctx context.Context, _ *pageController, m input.KeyMsg) (done bool, cancelled bool) {
	defer func() {
		if done || cancelled {
			f.finish()
		}
	}()
	switch m.Kind {
	case input.KeyEsc, input.KeyCtrlC:
		f.echo.newline()
		_, _ = io.WriteString(f.sess, emailTextCancelled+"\r\n")
		return true, true
	case input.KeyBackspace:
		if len(f.buf) > 0 {
			f.buf = f.buf[:len(f.buf)-1]
			f.echo.backspace(true)
		}
		return false, false
	case input.KeyPaste:
		clean := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return -1
			}
			return r
		}, m.Text)
		for _, r := range clean {
			f.buf = append(f.buf, r)
		}
		f.echo.paste(clean)
		return false, false
	case input.KeyRune:
		f.buf = append(f.buf, m.Rune)
		f.echo.rune(m.Rune)
		return false, false
	case input.KeyEnter:
		f.echo.newline()
		value := strings.TrimSpace(string(f.buf))
		return f.submit(ctx, value), false
	default:
		return false, false
	}
}

// submit 在 Enter 时推进流程；所有服务错误都映射成固定文案。
func (f *emailFlow) submit(ctx context.Context, value string) (done bool) {
	switch f.kind {
	case emailFlowRegister, emailFlowRecover:
		purpose := auth.EmailPurposeRegister
		if f.kind == emailFlowRecover {
			purpose = auth.EmailPurposeRecover
		}
		_, err := f.svc.RequestCode(ctx, purpose, value, f.ipHash)
		if err != nil {
			f.writeError(err)
			return true
		}
		*f.pending = strings.ToLower(strings.TrimSpace(value))
		if f.purpose != nil {
			*f.purpose = purpose
		}
		if f.kind == emailFlowRegister {
			_, _ = io.WriteString(f.sess, emailTextRegisterSent+"\r\n")
		} else {
			_, _ = io.WriteString(f.sess, emailTextRecoverSent+"\r\n")
		}
		return true

	case emailFlowVerify:
		// 注册设密码 / 找回新密码：两次输入确认。验证码只在两次一致时
		// 才交给 service 消费；不一致时回第一次输入重来，码不消费、不计数。
		if !f.confirm {
			f.firstPwd = value
			f.confirm = true
			f.showStep()
			return false
		}
		if value != f.firstPwd {
			f.firstPwd = ""
			f.confirm = false
			_, _ = io.WriteString(f.sess, emailTextPwdMismatch+"\r\n")
			f.showStep()
			return false
		}
		f.firstPwd = ""
		res, err := f.svc.Verify(ctx, *f.pending, f.code, value, f.ipHash)
		if err != nil {
			f.writeError(err)
			return true
		}
		if res.Purpose == auth.EmailPurposeRecover {
			_, _ = io.WriteString(f.sess, emailTextRecovered+"\r\n")
		} else {
			_, _ = io.WriteString(f.sess, emailTextRegistered+"\r\n")
		}
		return true

	case emailFlowLogin:
		if f.stage == 0 {
			f.email = value
			f.stage = 1
			f.showStep()
			return false
		}
		res, err := f.svc.Login(ctx, f.email, value, f.ipHash)
		if err != nil {
			f.writeError(err)
			return true
		}
		if err := f.sess.UpgradeToUser(res.UserID); err != nil {
			_, _ = io.WriteString(f.sess, emailTextFailed+"\r\n")
			return true
		}
		_, _ = io.WriteString(f.sess, emailTextLoggedin+"\r\n")
		return true
	}
	return true
}

// clearEmailFlow 结束并摘掉当前邮箱流程，复位回显掩码。
// 所有流程出口（done/cancelled/被新命令取代）都经此收口。
func (p *pageController) clearEmailFlow() {
	if p.emailFlow == nil {
		return
	}
	p.emailFlow.finish()
	p.emailFlow = nil
}

// handleEmailCommand 把四条邮箱命令接到分步流程 / 直接动作上。
// 只使用注册表白名单解析出的 inv，绝不把用户原文写回终端。
func (h *inputHandler) handleEmailCommand(ctx context.Context, sess emailSession, pc *pageController, inv command.Invocation) {
	if h.email == nil {
		_, _ = io.WriteString(sess, emailTextNotReady+"\r\n")
		return
	}
	switch inv.Name {
	case "register", "recover":
		kind := emailFlowRegister
		if inv.Name == "recover" {
			kind = emailFlowRecover
		}
		pc.invalidateScreen()
		pc.clearEmailFlow() // 若旧流程尚未结束就被新命令取代，先复位其 secret 掩码
		pc.emailFlow = newEmailFlow(kind, h.email, sess, pc.echo, &pc.pendingEmail, &pc.pendingPurpose, "", false)
	case "verify":
		if pc.pendingEmail == "" {
			_, _ = io.WriteString(sess, emailTextNoPending+"\r\n")
			return
		}
		pc.invalidateScreen()
		recover := pc.pendingPurpose == auth.EmailPurposeRecover
		pc.clearEmailFlow() // 同上：新命令取代旧流程时先复位
		pc.emailFlow = newEmailFlow(emailFlowVerify, h.email, sess, pc.echo, &pc.pendingEmail, &pc.pendingPurpose, inv.Args[0], recover)
	case "resend":
		if pc.pendingEmail == "" {
			_, _ = io.WriteString(sess, emailTextNoPending+"\r\n")
			return
		}
		pc.invalidateScreen()
		if err := h.email.Resend(ctx, pc.pendingEmail, sess.RemoteIPHash()); err != nil {
			writeEmailError(sess, err)
			return
		}
		_, _ = io.WriteString(sess, emailTextResendOK+"\r\n")
	}
}

// writeError 委托给统一映射点：绝不输出 err.Error()。
func (f *emailFlow) writeError(err error) { writeEmailError(f.sess, err) }

// writeEmailError 是错误到固定文案的唯一映射点：绝不输出 err.Error()
// （底层错误可能含外部数据，一旦写回终端就是渲染旁路，P0-23）。
func writeEmailError(out io.Writer, err error) {
	var rl *auth.RateLimitError
	text := emailTextFailed
	switch {
	case errors.As(err, &rl):
		text = "邮箱操作过于频繁，请 " + strconv.FormatInt(rl.RetryAfterSeconds(), 10) + " 秒后重试。"
	case errors.Is(err, auth.ErrInvalidEmail):
		text = emailTextBadEmail
	case errors.Is(err, auth.ErrEmailOccupied):
		text = auth.EmailOccupiedHint
	case errors.Is(err, auth.ErrEmailDelivery):
		text = emailTextSendFailed
	case errors.Is(err, auth.ErrInvalidCode):
		text = emailTextBadCode
	case errors.Is(err, auth.ErrWeakPassword):
		text = emailTextBadPassword
	case errors.Is(err, auth.ErrInvalidCredentials):
		text = emailTextBadLogin
	case errors.Is(err, auth.ErrNoPendingToken):
		text = emailTextNoPending
	}
	_, _ = io.WriteString(out, text+"\r\n")
}
