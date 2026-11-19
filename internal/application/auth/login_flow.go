package auth

// P0-19 登录交互的领域事实（评审稿 §4.4.1 / §4.6 / §3.5，偏差见 docs/DEVIATIONS.md R18~R20）。
//
// 本文件只定义三类东西：**可呈现的白名单投影**、**轮询结果 → 下一步指令**、
// **服务端固定文案**。全部是纯函数 / 常量，不碰 IO、不碰时间循环、不碰终端，
// 也不持有任何 Session 或 device_code。
//
// 两条硬边界：
//  1. LoginView / LoginDirective **刻意不含 DeviceCode 与 Token**。
//     device_code 只在服务端会话内存里保存用于 Poll，access_token 只在
//     LoginFlow.finish 里进程内交 LoginService；两者一旦进入呈现结构，
//     就可能被写回终端（§4.4 / ADR-021 / Q3）。
//  2. user_code / verification_uri 来自 GitHub HTTP 响应，属**外部数据**。
//     必须过 ProjectLoginView 的格式白名单才允许进入呈现层，且呈现时仍
//     走 safe.RenderUser 净化路径（服务端固定文本才走 RenderSystem）。

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// LoginMinIntervalSeconds 是两次 Poll 之间的最小间隔，与 P0-15 的
	// DefaultPollInterval 对齐；任何来自响应的更小值都会被 ClampInterval 抬上来，
	// 防止异常 interval 把 GitHub 限流打死（ADR-022 / §7 R2）。
	LoginMinIntervalSeconds = DefaultPollInterval
	// LoginMaxIntervalSeconds 是单次等待上限。slow_down 会持续累加，
	// 若响应给出荒谬的大值，用户会以为进程卡死，因此封顶 60 秒。
	LoginMaxIntervalSeconds = 60
	// LoginMaxNetworkRetries 是连续网络错误后的重试上限。网络抖动不是
	// 「用户拒绝授权」，不能直接终止；但也不能无限轮询，到顶后转终态提示。
	LoginMaxNetworkRetries = 3

	maxUserCodeRunes        = 64
	maxVerificationURIRunes = 256
)

var (
	// ErrUnsafeLoginView 表示 GitHub 响应里的呈现字段不合法，整个 Start 作废。
	// 失败关闭：宁可让用户重新 /login，也不把外部原文交给终端。
	ErrUnsafeLoginView = errors.New("设备码响应的呈现字段不合法")
	// ErrLoginViewExpired 表示设备码在呈现前就已过期。
	ErrLoginViewExpired = errors.New("设备码已过期，拒绝呈现")
	// ErrSessionUpgradeRejected 是 transport/ssh.Session 升级被拒的哨兵：
	// 已登录会话不可重复升级、不可横向改成另一个账号。
	ErrSessionUpgradeRejected = errors.New("会话升级被拒绝")
)

// ---------------------------------------------------------------------------
// 服务端固定文案
//
// 全部是编译期常量，**不含任何外部数据**。cmd 的 loginPresenter 只渲染这些
// 常量或经 ProjectLoginView 白名单投影过的字段；archtest 会静态禁止把
// PollResult.Message / err.Error() / Username 直接拼进 RenderSystem 的输入。
// ---------------------------------------------------------------------------

const (
	LoginTextStart         = "正在发起 GitHub 登录（Device Flow）"
	LoginTextOpenHint      = "请在浏览器打开下面的地址并输入验证码："
	LoginTextCancelHint    = "按 Esc 取消本次登录。"
	LoginTextWaiting       = "等待授权中，服务端正在轮询…"
	LoginTextRetry         = "连接 GitHub 暂时失败，稍后自动重试…"
	LoginTextNetworkGiveUp = "多次连接 GitHub 失败，请稍后重新 /login github"
	LoginTextCancelled     = "已取消登录。"
	LoginTextExpired       = "设备码已过期，请重新 /login github"
	LoginTextDenied        = "登录已取消或被拒绝。"
	LoginTextAuthorizing   = "授权成功，正在建立账号…"
	LoginTextLoginFailed   = "登录失败，请稍后重试。"
	// 本机没有 client_id 时给可操作提示，而不是笼统的「登录失败」。
	// 依据 OPS §3「缺值时给明确提示，不 panic、不静默失败」。
	LoginTextClientIDMissing       = "未配置 MOTD_GITHUB_CLIENT_ID，无法发起 GitHub 登录；请在服务端注入该环境变量后重试（见 docs/OPS.md §3）。"
	LoginTextBindFailed            = "账号已建立，但当前连接的公钥未能绑定；请换一把公钥或先解绑后重试。"
	LoginTextUpgradeFailed         = "登录已完成，但本会话未能升级为登录态，请重新连接。"
	LoginTextSuccessLogin          = "登录成功。"
	LoginTextSuccessCreate         = "账号已创建并登录。"
	LoginTextWelcome               = "当前账号："
	LoginTextWriteReady            = "已获得写权限（当前连接公钥已自动绑定）。"
	LoginTextAlreadyUser           = "当前会话已登录，无需重复 /login。"
	LoginTextUnsupportedProvider   = "仅支持 /login github。"
	LoginTextNotReady              = "登录服务尚未就绪。"
	LoginTextDeviceCodeInvalidated = "登录已中止（设备码响应不合法）。"
	// LoginTextRateLimited 是命中认证限流时的固定文案；剩余秒数由本地数值拼出，
	// 只走 RenderSystem / 固定输出路径，不回显 IP、邮箱或底层错误。
	LoginTextRateLimited = "登录请求过于频繁，已暂时拒绝。"
)

// LoginView 是 /login 发起屏可呈现的白名单投影。
//
// DeviceCode 不在这里：它由 LoginFlow 在进程内保存用于后续 Poll。
// Token 不在这里：它只在授权成功后由 LoginFlow.finish 直接消费。
type LoginView struct {
	UserCode         string
	VerificationURI  string
	ExpiresAt        time.Time
	IntervalSeconds  int
	RemainingSeconds int
}

// ProjectLoginView 把 StartResult 收紧成可呈现视图（失败关闭）。
//
// user_code 与 verification_uri 是 GitHub 响应字段，可能被代理 / 中间人
// 注入控制字符。这里做**格式白名单**而不是简单净化：不合法直接拒绝整个
// Start，避免「先展示再净化」把脏数据放进错误提示或日志。
func ProjectLoginView(s StartResult, now time.Time) (LoginView, error) {
	uc := strings.TrimSpace(s.UserCode)
	if !validUserCode(uc) {
		return LoginView{}, fmt.Errorf("%w: user_code 格式不合法", ErrUnsafeLoginView)
	}
	uri, err := normalizeVerificationURI(s.VerificationURI)
	if err != nil {
		return LoginView{}, err
	}
	exp := s.ExpiresAt
	if !exp.After(now) {
		return LoginView{}, ErrLoginViewExpired
	}
	remaining := int(exp.Sub(now).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return LoginView{
		UserCode:         uc,
		VerificationURI:  uri,
		ExpiresAt:        exp,
		IntervalSeconds:  ClampInterval(s.Interval),
		RemainingSeconds: remaining,
	}, nil
}

// validUserCode 只允许 GitHub 设备码的字符集：大写字母 / 数字 / 连字符。
// 长度 4..64，且必须含至少一个字母或数字，避免 "----" 这类退化值。
func validUserCode(s string) bool {
	n := 0
	hasAlnum := false
	for _, r := range s {
		n++
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasAlnum = true
		case r == '-':
		default:
			return false
		}
	}
	return n >= 4 && n <= maxUserCodeRunes && hasAlnum
}

// normalizeVerificationURI 校验并原样返回验证地址。
//
// 只接受 https，字符集收敛到 URI 安全子集，明确排除空白、控制字符、
// 尖括号、引号与反斜杠（这些是终端注入与钓鱼地址拼接的常见载体）。
// 失败返回 ErrUnsafeLoginView，由调用方作废设备码。
func normalizeVerificationURI(raw string) (string, error) {
	uri := strings.TrimSpace(raw)
	if len(uri) > maxVerificationURIRunes {
		return "", fmt.Errorf("%w: verification_uri 过长", ErrUnsafeLoginView)
	}
	if !strings.HasPrefix(strings.ToLower(uri), "https://") {
		return "", fmt.Errorf("%w: verification_uri 必须是 https", ErrUnsafeLoginView)
	}
	for _, r := range uri {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(":/.-_?=&%#+~@", r):
		default:
			return "", fmt.Errorf("%w: verification_uri 含非法字符", ErrUnsafeLoginView)
		}
	}
	return uri, nil
}

// ClampInterval 把轮询间隔（秒）收敛到 [5, 60]，是会话循环唯一允许使用的
// 节奏来源：调用方不得自己写一个更小的常数（ADR-021 验收项）。
func ClampInterval(seconds int) int {
	if seconds < LoginMinIntervalSeconds {
		return LoginMinIntervalSeconds
	}
	if seconds > LoginMaxIntervalSeconds {
		return LoginMaxIntervalSeconds
	}
	return seconds
}

// ---------------------------------------------------------------------------
// 轮询结果 → 下一步指令
// ---------------------------------------------------------------------------

// LoginDirectiveKind 是登录交互对「本次 Poll 结论」的决策分类。
type LoginDirectiveKind uint8

const (
	// DirectiveContinue：authorization_pending / slow_down，继续等待。
	DirectiveContinue LoginDirectiveKind = iota + 1
	// DirectiveAuthorized：拿到令牌，执行 Login → Bind → Upgrade。
	// 指令**不携带 token**，调用方直接从 PollResult.Token 取用。
	DirectiveAuthorized
	// DirectiveTerminal：denied / expired / 已是终态，结束本次登录。
	DirectiveTerminal
	// DirectiveRetry：网络 / 数据库瞬时错误，保持状态并重试，
	// 绝不把一次网络抖动误判成用户拒绝（ADR-022）。
	DirectiveRetry
)

func (k LoginDirectiveKind) String() string {
	switch k {
	case DirectiveContinue:
		return "continue"
	case DirectiveAuthorized:
		return "authorized"
	case DirectiveTerminal:
		return "terminal"
	case DirectiveRetry:
		return "retry"
	default:
		return "unknown"
	}
}

// LoginDirective 是一次轮询对会话循环的结论。
// 同样刻意不含 Token / DeviceCode / 任何来自响应的原文。
type LoginDirective struct {
	Kind         LoginDirectiveKind
	DelaySeconds int    // 仅 Continue / Retry 有效，已经过 ClampInterval
	Message      string // 服务端固定文案
}

// ClassifyPoll 把 DeviceFlowService.Poll 的结果映射成会话循环指令。
//
// 这是「网络错误不终止、终态不重试、令牌不进指令」三条约束的唯一事实来源：
//   - err != nil（网络 / 状态机内部错误）→ Retry，保持 device_auth_states 原状态；
//   - OutcomeContinue → Continue，间隔取 NextInterval（为 0 时回落 state 的 interval）；
//   - OutcomeAuthorized → Authorized，令牌由调用方从 PollResult.Token 直接消费；
//   - OutcomeTerminal / 未知枚举 → Terminal，失败关闭。
func ClassifyPoll(r PollResult, err error) LoginDirective {
	if err != nil {
		return LoginDirective{
			Kind:         DirectiveRetry,
			DelaySeconds: ClampInterval(r.State.PollInterval()),
			Message:      LoginTextRetry,
		}
	}
	switch r.Outcome {
	case OutcomeAuthorized:
		return LoginDirective{Kind: DirectiveAuthorized, Message: LoginTextAuthorizing}
	case OutcomeContinue:
		next := r.NextInterval
		if next <= 0 {
			next = r.State.PollInterval()
		}
		return LoginDirective{
			Kind:         DirectiveContinue,
			DelaySeconds: ClampInterval(next),
			Message:      LoginTextWaiting,
		}
	case OutcomeTerminal:
		return LoginDirective{Kind: DirectiveTerminal, Message: TerminalLoginText(r.State.Status)}
	default:
		// 枚举被扩展出未知值时失败关闭，不静默降级成「继续轮询」。
		return LoginDirective{Kind: DirectiveTerminal, Message: LoginTextDenied}
	}
}

// TerminalLoginText 把终态映射成固定文案。刻意不透传 PollResult.Message：
// 那条消息虽已由 P0-15 的 safeCode / terminalMessage 收紧过，但仍是响应侧
// 字符串，呈现层只应消费编译期常量（§3.5 分层渲染的错误构造侧防线）。
func TerminalLoginText(st DeviceAuthStatus) string {
	switch st {
	case StatusExpired:
		return LoginTextExpired
	case StatusDenied:
		return LoginTextDenied
	default:
		return LoginTextDenied
	}
}
