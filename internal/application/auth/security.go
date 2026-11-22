package auth

import (
	"errors"
	"fmt"
)

// SecurityGate 把「审计 fail-closed」与「限流判定」收在一个入口，
// 避免每个认证用例各自决定先记审计还是先判限流。
//
// 顺序固定：审计先入队（队列满 → 直接拒绝本次动作），再走限流计数。
// 这样审计缓冲溢出时不会有人绕过审计继续尝试认证（D-a 拍板结论）。
//
// 可选组件：任一依赖为 nil 时对应能力静默关闭，保证 P0 既有测试与
// 未装配限流的部署路径零改动；生产由 cmd 显式注入。
type SecurityGate struct {
	limiter  *RateLimiter
	audit    *AuditLogger
	provider string // 只放白名单 provider 常量（如 github），不接用户输入
}

func NewSecurityGate(limiter *RateLimiter, audit *AuditLogger, provider string) *SecurityGate {
	return &SecurityGate{limiter: limiter, audit: audit, provider: provider}
}

// Enabled 报告是否真的有安全能力在岗，供装配日志与探针展示。
func (g *SecurityGate) Enabled() bool { return g != nil && (g.limiter != nil || g.audit != nil) }

// loginSuccessProviders 是 login_success 允许的 provider 白名单。
// provider 是**审计列**，只接受编译期常量，绝不接用户输入（ADR-019 / ADR-026）。
var loginSuccessProviders = map[string]struct{}{
	ProviderGitHub: {},
	ProviderEmail:  {},
}

// NoteLoginSuccess 记录一次**成功登录动作**（v0.7.1 / ADR-064）。
//
// 与 NoteLoginFailure / NoteKeyAttempt 的 fail-closed 语义**刻意不同**：
// 登录此刻已经成功、会话已升级，事后统计事件写不进去不能回滚登录结果，
// 因此 Record 失败（ErrAuditQueueFull）只由调用方记 Warn，绝不拒绝登录。
// 这是 audit.go 包注释里「Record 返回错误 → 调用方必须拒绝本次认证动作」的
// **唯一显式例外**，理由与边界写在 ADR-064。
//
// provider 必须来自白名单常量；非法值返回错误（调用方 bug，不是用户输入）。
// 注意：这里直接调 audit.Record 并显式传 provider，**不走** g.record()——
// g.record 固定用 g.provider（装配时恒为 github），会把邮箱登录也记成 github。
func (g *SecurityGate) NoteLoginSuccess(userID int64, provider string) error {
	if _, ok := loginSuccessProviders[provider]; !ok {
		return fmt.Errorf("login_success 的 provider 不在白名单: %q", provider)
	}
	if g == nil || g.audit == nil {
		return nil
	}
	return g.audit.Record(AuditLoginSuccess, userID, provider, "", `{}`)
}

func (g *SecurityGate) record(event string, userID int64, ipHash, detail string) error {
	if g == nil || g.audit == nil {
		return nil
	}
	return g.audit.Record(event, userID, g.provider, ipHash, detail)
}

// AllowDeviceStart 是 Device Flow 发起的按 IP 限流。不写审计：
// 高频正常发起会把审计环冲掉，真正需要留痕的是失败与状态变更。
func (g *SecurityGate) AllowDeviceStart(ipHash string) error {
	if g == nil || g.limiter == nil {
		return nil
	}
	return g.limiter.AllowDeviceStart(ipHash)
}

// NoteLoginFailure 先留审计再记失败次数；两道门任一拒绝即拒绝本次动作。
func (g *SecurityGate) NoteLoginFailure(ipHash, emailHash string) error {
	if err := g.record(AuditLoginFailure, 0, ipHash, `{}`); err != nil {
		return err
	}
	if g == nil || g.limiter == nil {
		return nil
	}
	return g.limiter.NoteLoginFailure(ipHash, emailHash)
}

// NoteKeyAttempt 覆盖公钥绑定 / 解绑的「尝试」统计。
// event 必须是 audit.go 的固定常量之一，detail 只放本地拼出的 JSON。
func (g *SecurityGate) NoteKeyAttempt(event string, userID int64, ipHash string) error {
	if err := g.record(event, userID, ipHash, `{}`); err != nil {
		return err
	}
	if g == nil || g.limiter == nil {
		return nil
	}
	return g.limiter.AllowKeyBinding(userID)
}

// NoteSessionRevoked 记录一次会话撤销。撤销已经发生、不可回滚，
// 审计失败只作为错误上报，不改变已关闭的会话数。
func (g *SecurityGate) NoteSessionRevoked(userID int64, ipHash string, closed int) error {
	detail := fmt.Sprintf(`{"sessions_closed":%d}`, closed)
	if err := g.record(AuditSessionRevoked, userID, ipHash, detail); err != nil {
		return fmt.Errorf("会话撤销已生效，但审计入队失败: %w", err)
	}
	return nil
}

// NoteLogout 记录注销。校验与错误形态见 LogoutService：会话撤销先行，
// 审计失败只上报不回滚（回滚撤销等于把已踢出的会话放回来，更危险）。
func (g *SecurityGate) NoteLogout(userID int64, ipHash string, closed int) error {
	detail := fmt.Sprintf(`{"sessions_closed":%d}`, closed)
	if err := g.record(AuditLogout, userID, ipHash, detail); err != nil {
		return fmt.Errorf("注销已生效，但审计入队失败: %w", err)
	}
	return nil
}

// ErrLogoutRequiresLogin 是注销用例的入口校验错误。
var ErrLogoutRequiresLogin = errors.New("访客无需注销")
