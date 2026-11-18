package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Device Flow 状态机（评审稿 v0.2 §4.4.1 / P0-16）。
//
// 状态跨请求存活 → 必须持久化在 device_auth_states，不复用 oauth_states。
// 本文件只定义领域事实：四态枚举、合法迁移表、轮询门禁，全部是纯函数，
// 不碰数据库、不碰网络、不碰终端，便于把「非法路径被拒绝」「过期码不再轮询」
// 两条验收测死。

// DeviceAuthStatus 与 0001_init.sql 里 device_auth_states.status 的 CHECK 约束
// 逐字对应。archtest 会校验两侧不漂移。
type DeviceAuthStatus string

const (
	StatusPending    DeviceAuthStatus = "pending"
	StatusAuthorized DeviceAuthStatus = "authorized"
	StatusDenied     DeviceAuthStatus = "denied"
	StatusExpired    DeviceAuthStatus = "expired"
)

// DeviceProviderGitHub 是 P0 唯一支持的身份源（§4.3）。DB 存字符串，领域用常量。
const DeviceProviderGitHub = "github"

// AllDeviceAuthStatuses 是枚举全集，供守卫与测试穷举，不对外暴露可变切片。
func AllDeviceAuthStatuses() []DeviceAuthStatus {
	return []DeviceAuthStatus{StatusPending, StatusAuthorized, StatusDenied, StatusExpired}
}

func (s DeviceAuthStatus) String() string { return string(s) }

func (s DeviceAuthStatus) Valid() bool {
	switch s {
	case StatusPending, StatusAuthorized, StatusDenied, StatusExpired:
		return true
	}
	return false
}

// Terminal 表示终态：不再允许任何迁移，也不再允许轮询。
func (s DeviceAuthStatus) Terminal() bool { return s.Valid() && s != StatusPending }

// 状态机阶段的哨兵错误。transport/ssh 与 P0-19 用这些做可读提示，
// 不依赖错误字符串内容。
var (
	ErrInvalidTransition   = errors.New("非法的设备码状态迁移")
	ErrInvalidStatus       = errors.New("设备码状态值非法")
	ErrDeviceAuthNotFound  = errors.New("设备码状态不存在")
	ErrStateConflict       = errors.New("设备码状态已被并发修改，请重试")
	ErrSessionRefEmpty     = errors.New("会话标识为空")
	ErrDeviceCodeEmpty     = errors.New("设备码为空")
	ErrMalformedDeviceCode = errors.New("GitHub 返回的设备码响应不完整")
	ErrMalformedToken      = errors.New("GitHub 返回的访问令牌为空")
	ErrInvalidInterval     = errors.New("轮询间隔非法")
)

// CanTransition 是状态迁移的唯一事实来源（验收：非法路径被拒绝）。
//
// 合法路径仅三条：pending → authorized / denied / expired。
// 其余全部拒绝，包括：
//   - 任何终态出发的迁移（authorized / denied / expired 不可变）；
//   - pending → pending（无状态变化，slow_down 改间隔走 UpdateInterval，不算迁移）；
//   - 未知状态值。
//
// 同会话重复发起「作废旧码」是批量作废，走 SupersedeSessionPending，不经此表。
func CanTransition(from, to DeviceAuthStatus) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: from=%q to=%q", ErrInvalidTransition, from, to)
	}
	if from == StatusPending && to.Terminal() {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
}

// PollEligibility 是一次轮询前的门禁结论。
type PollEligibility uint8

const (
	// GateReady：pending 且未到 expires_at，允许发起本次网络轮询。
	GateReady PollEligibility = iota + 1
	// GateTerminal：已是终态，不再轮询，也不再迁移。
	// 刻意不用 PollTerminal：该名字已被 P0-15 的 PollStatus（错误码分类）占用。
	GateTerminal
	// GateExpired：仍是 pending 但已过 expires_at，应先迁移为 expired 再拒绝。
	GateExpired
)

func (e PollEligibility) String() string {
	switch e {
	case GateReady:
		return "ready"
	case GateTerminal:
		return "terminal"
	case GateExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// EvaluatePoll 落实验收「过期码不再轮询」。
// 只有 pending 且严格早于 ExpiresAt 才可轮询；到点（now == ExpiresAt）即视为过期，
// 不留「恰好相等仍可请求」的边界窗口。
func EvaluatePoll(st DeviceAuthState, now time.Time) PollEligibility {
	if st.Status.Terminal() || !st.Status.Valid() {
		return GateTerminal
	}
	if !now.Before(st.ExpiresAt) {
		return GateExpired
	}
	return GateReady
}

// DeviceAuthState 是 device_auth_states 一行的领域投影。
//
// 刻意不含 device_code 之外的任何令牌：access_token 只在服务端进程内
// 流转给 P0-17，绝不落库、绝不写终端（Q3 / §4.4 / ADR-021）。
type DeviceAuthState struct {
	ID              int64
	Provider        string
	DeviceCode      string
	UserCode        string
	PollingInterval int
	ExpiresAt       time.Time
	Status          DeviceAuthStatus
	SessionRef      string
	CreatedAt       time.Time
}

// PollInterval 返回合法的最小轮询间隔，非正数回落到官方默认 5 秒。
func (s DeviceAuthState) PollInterval() int {
	if s.PollingInterval > 0 {
		return s.PollingInterval
	}
	return DefaultPollInterval
}

// NewDeviceAuthState 是一次 Insert 的写入参数（不含 ID，由 DB 生成）。
type NewDeviceAuthState struct {
	Provider        string
	DeviceCode      string
	UserCode        string
	PollingInterval int
	ExpiresAt       time.Time
	Status          DeviceAuthStatus
	SessionRef      string
	CreatedAt       time.Time
}

// StartResult 是一次 Start 的产物，供 P0-19 的终端呈现消费。
//
// DeviceCode 只在服务端会话内保存用于后续 Poll，绝不写回终端；
// 终端只展示 UserCode / VerificationURI / ExpiresAt / Interval（§4.4.1）。
type StartResult struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	Interval        int
}

// PollOutcome 是一次轮询对上层（P0-19）的决策结论。
type PollOutcome uint8

const (
	// OutcomeContinue：继续等待用户授权（authorization_pending / slow_down）。
	OutcomeContinue PollOutcome = iota + 1
	// OutcomeAuthorized：拿到令牌，交给 P0-17 做账号创建/登录。
	OutcomeAuthorized
	// OutcomeTerminal：本次登录结束（denied / expired / 已是终态），Message 说明原因。
	OutcomeTerminal
)

func (o PollOutcome) String() string {
	switch o {
	case OutcomeContinue:
		return "continue"
	case OutcomeAuthorized:
		return "authorized"
	case OutcomeTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// PollResult 是一次轮询的完整结论。
// Token 仅在 OutcomeAuthorized 时有值；它不来自持久化层，也不写回终端。
type PollResult struct {
	Outcome      PollOutcome
	State        DeviceAuthState
	Token        AccessToken
	Message      string
	NextInterval int
}

// terminalMessage 按终态给出固定中文提示，不携带任何来自响应的原文。
func terminalMessage(st DeviceAuthStatus) string {
	switch st {
	case StatusAuthorized:
		return "已完成授权"
	case StatusDenied:
		return "登录已取消或被拒绝"
	case StatusExpired:
		return "设备码已过期，请重新发起登录"
	default:
		return "登录流程已结束"
	}
}

// normalizeSessionRef 去掉首尾空白；空白会话标识会让「作废旧码」误伤所有
// 空 session_ref 的记录，因此直接拒绝（失败关闭）。
func normalizeSessionRef(s string) (string, error) {
	ref := strings.TrimSpace(s)
	if ref == "" {
		return "", ErrSessionRefEmpty
	}
	return ref, nil
}
