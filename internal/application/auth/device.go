package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Device Flow 常量（评审稿 §4.4.1；2026-11-18 对照 GitHub 官方文档复核）。
const (
	// DeviceScope 是登录所需的最小权限集，固定在这里，不允许调用方放大。
	DeviceScope = "read:user user:email"
	// DeviceGrantType 是 OAuth 2.0 Device Authorization Grant 的固定 grant_type。
	DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// DefaultPollInterval 是 GitHub 未返回 interval 时的兜底秒数（官方示例 5s）。
	DefaultPollInterval = 5
	// SlowDownStep 是 slow_down 时在上次 interval 基础上累加的秒数（官方明确 5 秒）。
	SlowDownStep = 5
)

// ErrClientIDMissing 表示未配置 MOTD_GITHUB_CLIENT_ID。
// 客户端允许在缺配置下构造（本机开发 / httptest 测试不需要真实凭据），
// 但每次请求都会返回这个哨兵：绝不静默发出 client_id 为空的请求，也不 panic
// （OPS §3 的「缺值时给明确提示，不 panic、不静默失败」）。
var ErrClientIDMissing = errors.New("未配置 MOTD_GITHUB_CLIENT_ID，无法发起 GitHub 登录")

// DeviceCode 是 Device Flow 第一步的产物（§4.4.1）。
// 字段与 GitHub POST /login/device/code 的 JSON 响应一一对应。
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int // 秒，官方默认 900
	Interval        int // 秒，两次轮询之间的最小间隔
}

// PollInterval 返回合法的最小轮询间隔；非正数一律回落到官方默认 5 秒。
// 验收项「轮询间隔不得小于 interval」的第一道保证：调用方只能用这个值，
// 不能自己写一个更小的常数。
func (d DeviceCode) PollInterval() int {
	if d.Interval > 0 {
		return d.Interval
	}
	return DefaultPollInterval
}

// AccessToken 是轮询成功后拿到的令牌。P0 只在服务端进程内使用，
// 绝不写回终端、绝不进日志（Q3 / P0-19）。
type AccessToken struct {
	Token     string
	TokenType string
	Scope     string
}

func (t AccessToken) Valid() bool { return t.Token != "" }

// GitHubUser 是 GET /user 的最小投影。id 是 GitHub 数字 ID，
// 按 §4.3 作为身份主键，禁止用 login / email（两者都可能被用户更改）。
type GitHubUser struct {
	ID    int64
	Login string
	Email string
}

// GitHubEmail 是 GET /user/emails 的单项。P0-17 只接受 Primary && Verified。
type GitHubEmail struct {
	Email    string
	Primary  bool
	Verified bool
}

// PollStatus 是轮询结果的决策分类，供 P0-16 状态机消费。
type PollStatus uint8

const (
	PollPending  PollStatus = iota + 1 // authorization_pending：继续轮询
	PollSlowDown                       // slow_down：延长间隔后继续
	PollExpired                        // expired_token / token_expired：重新申请设备码
	PollTerminal                       // 终止类错误
	PollUnknown                        // 未识别错误码
)

func (s PollStatus) String() string {
	switch s {
	case PollPending:
		return "pending"
	case PollSlowDown:
		return "slow_down"
	case PollExpired:
		return "expired"
	case PollTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// PollError 把 GitHub 错误码映射成「可读中文提示 + 决策分类」。
// Error() 只返回 Message，绝不带响应体 / token / client_id，避免凭据泄漏进日志或终端。
type PollError struct {
	Code     string     // 原始错误码，仅用于日志与测试
	Status   PollStatus // 状态机的决策依据
	Interval int        // slow_down 响应携带的新 interval（0 表示未携带）
	Message  string     // 可直接展示在终端的中文提示
	Endpoint string     // 出错端点：device/code 或 access_token
}

func (e *PollError) Error() string { return e.Message }

// ClassifyDeviceError 是错误码映射的唯一事实来源，实现方不得自行发明分类。
//
// 8 个错误码来自 GitHub 官方文档（§7 R3 补 3 个，2026-11-18 复核又补
// unsupported_grant_type）。expired_token 与官方正文里的 token_expired 都接受：
// 同一份文档的表格与正文用了两个名字，双兼容才不会在真实链路上漏判。
// 未知码不吞、不降级，返回 PollUnknown 让状态机终止本次登录。
func ClassifyDeviceError(code, endpoint string, newInterval int) *PollError {
	pe := &PollError{Code: code, Endpoint: endpoint, Interval: newInterval}
	switch code {
	case "authorization_pending":
		pe.Status = PollPending
		pe.Message = "等待用户在浏览器完成授权"
	case "slow_down":
		pe.Status = PollSlowDown
		pe.Message = "轮询过快，已按 GitHub 要求延长间隔"
	case "expired_token", "token_expired":
		pe.Status = PollExpired
		pe.Message = "设备码已过期，请重新发起登录"
	case "access_denied":
		pe.Status = PollTerminal
		pe.Message = "用户已取消授权"
	case "device_flow_disabled":
		pe.Status = PollTerminal
		pe.Message = "OAuth App 未启用 Device Flow，请在 GitHub App 设置中开启"
	case "incorrect_client_credentials":
		pe.Status = PollTerminal
		pe.Message = "client_id 无效，请检查 MOTD_GITHUB_CLIENT_ID"
	case "incorrect_device_code":
		pe.Status = PollTerminal
		pe.Message = "设备码无效，请重新发起登录"
	case "unsupported_grant_type":
		pe.Status = PollTerminal
		pe.Message = "GitHub 拒绝了 grant_type，属客户端实现错误"
	default:
		pe.Status = PollUnknown
		pe.Message = fmt.Sprintf("GitHub 返回未识别错误（%s），已终止本次登录", safeCode(code))
	}
	return pe
}

// safeCode 只保留 ASCII 字母数字与 _ -，避免未识别错误码把控制字符 / ANSI
// 带进终端消息。P0-23 的分层渲染是展示侧防线，这里是错误构造侧防线。
func safeCode(code string) string {
	var b strings.Builder
	n := 0
	for _, r := range code {
		if r > 127 || n >= 64 {
			break
		}
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			n++
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// NextInterval 根据本次轮询结果给出下一次的最小间隔（秒）。
//
// 规则（官方文档 / §7 R2）：slow_down 优先取响应携带的新 interval；响应未带时
// 在上次基础上加 5 秒；其余情况不缩小间隔。返回值恒 >= DefaultPollInterval 且
// 恒 >= last，杜绝「越轮越快」把 GitHub 限流打死。
//
// 客户端本身不 sleep：轮询节奏由 P0-16 状态机持有并驱动，本函数只是纯计算，
// 便于在没有真实网络的情况下把退避语义测死。
func NextInterval(last int, pollErr error) int {
	base := last
	if base < DefaultPollInterval {
		base = DefaultPollInterval
	}
	var pe *PollError
	if !errors.As(pollErr, &pe) || pe.Status != PollSlowDown {
		return base
	}
	if pe.Interval > base {
		return pe.Interval
	}
	if pe.Interval > 0 {
		// 响应给的新值反而比当前小：不缩小，避免实现缺陷或异常响应打死限流。
		return base
	}
	return base + SlowDownStep
}

// DeviceFlowClient 是 P0-16 状态机唯一需要的外部协议端口。
//
// 端口定义在使用方（ADR-001 / ADR-020 同一口径）：application 不碰 HTTP，
// 实现落在 infrastructure/github，由 cmd 装配注入。
// Q3：P0 全链路进程内调用，不开放任何业务 HTTP 端点。
// scope 与 grant_type 由实现固定为最小集，接口不暴露这两个参数，
// 调用方无法放大权限。
type DeviceFlowClient interface {
	// RequestDeviceCode 发起 Device Flow，返回供终端展示的 user_code / verification_uri。
	RequestDeviceCode(ctx context.Context) (DeviceCode, error)
	// PollAccessToken 单次轮询；pending / slow_down / 终止类错误以 *PollError 返回。
	PollAccessToken(ctx context.Context, deviceCode string) (AccessToken, error)
	// GetUser 用 access_token 取数字 id（身份主键）。
	GetUser(ctx context.Context, token string) (GitHubUser, error)
	// GetEmails 取邮箱列表，供 P0-17 选 primary && verified。
	GetEmails(ctx context.Context, token string) ([]GitHubEmail, error)
}
