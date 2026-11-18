package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DeviceFlowService 编排 Device Flow 状态机（P0-16）。
//
// 两条硬边界：
//  1. **网络调用一律在写事务之外**。RequestDeviceCode / PollAccessToken 单次最长
//     10s（ADR-021），绝不能占着 §2.6 的唯一写连接；因此「读状态 → 网络 →
//     CAS 写回」分三步，迟到结果靠 WriteTx 的条件更新兜底，不会覆盖终态。
//  2. **状态机不 sleep、不起 ticker**。轮询节奏以 PollResult.NextInterval 交给
//     P0-19 的会话循环决定，本层只做纯决策，便于在没有真实时间的情况下测死。
//
// token 只在 PollResult 里进程内流转给 P0-17，不写库、不进日志、不回终端。
type DeviceFlowService struct {
	store  DeviceAuthStore
	client DeviceFlowClient
	now    func() time.Time
}

// DeviceFlowOption 注入时钟等测试替身；限流/过期判定必须可测，不能靠真实等待。
type DeviceFlowOption func(*DeviceFlowService)

func WithDeviceFlowClock(f func() time.Time) DeviceFlowOption {
	return func(s *DeviceFlowService) {
		if f != nil {
			s.now = f
		}
	}
}

// NewDeviceFlowService 装配状态机。store / client 为 nil 属装配期错误，直接 panic，
// 不留到运行期退化成"永远 pending"。
func NewDeviceFlowService(store DeviceAuthStore, client DeviceFlowClient, opts ...DeviceFlowOption) *DeviceFlowService {
	if store == nil || client == nil {
		panic("auth.NewDeviceFlowService: store 与 client 不能为空")
	}
	s := &DeviceFlowService{store: store, client: client, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start 发起一次登录：向 GitHub 申请设备码，然后**在一个事务内**作废旧码 + 插入新码。
//
// 网络请求在事务外先做；拿到响应后才开事务。同一 session_ref 的旧 pending 记录
// 批量迁移为 expired（"以最后一次为准并作废旧码"），与新码插入是同一原子单元，
// 不会出现"旧的还没作废、新的已可见"的中间态。
func (s *DeviceFlowService) Start(ctx context.Context, sessionRef string) (StartResult, error) {
	ref, err := normalizeSessionRef(sessionRef)
	if err != nil {
		return StartResult{}, err
	}

	dc, err := s.client.RequestDeviceCode(ctx)
	if err != nil {
		return StartResult{}, fmt.Errorf("申请设备码: %w", err)
	}
	if strings.TrimSpace(dc.DeviceCode) == "" || strings.TrimSpace(dc.UserCode) == "" {
		return StartResult{}, ErrMalformedDeviceCode
	}

	now := s.now().UTC()
	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = DefaultExpiresIn
	}
	interval := dc.PollInterval()
	st := NewDeviceAuthState{
		Provider:        DeviceProviderGitHub,
		DeviceCode:      dc.DeviceCode,
		UserCode:        dc.UserCode,
		PollingInterval: interval,
		ExpiresAt:       now.Add(time.Duration(expiresIn) * time.Second),
		Status:          StatusPending,
		SessionRef:      ref,
		CreatedAt:       now,
	}

	// 作废旧码与插入新码必须同一事务：任何一步失败都整体回滚，不留半成品。
	err = s.store.Write(ctx, func(tx DeviceAuthWriteTx) error {
		if _, err := tx.SupersedeSessionPending(ctx, ref, StatusExpired); err != nil {
			return fmt.Errorf("作废旧设备码: %w", err)
		}
		if _, err := tx.Insert(ctx, st); err != nil {
			return fmt.Errorf("写入新设备码状态: %w", err)
		}
		return nil
	})
	if err != nil {
		return StartResult{}, err
	}
	return StartResult{
		DeviceCode:      dc.DeviceCode,
		UserCode:        dc.UserCode,
		VerificationURI: dc.VerificationURI,
		ExpiresAt:       st.ExpiresAt,
		Interval:        interval,
	}, nil
}

// Poll 执行一次轮询决策。
//
// 顺序刻意固定：只读查状态 → 门禁（过期/终态直接返回，不发网络）→ 网络轮询
// → CAS 写回。门禁拦下的路径完全不碰 GitHub，兑现「过期码不再轮询」。
// 网络错误（超时/5xx，非 *PollError）不改状态，原样返回让 P0-19 决定重试，
// 绝不把一次网络抖动误判成用户拒绝。
func (s *DeviceFlowService) Poll(ctx context.Context, deviceCode string) (PollResult, error) {
	code := strings.TrimSpace(deviceCode)
	if code == "" {
		return PollResult{}, ErrDeviceCodeEmpty
	}

	st, err := s.store.GetByDeviceCode(ctx, code)
	if err != nil {
		return PollResult{}, err
	}
	switch EvaluatePoll(st, s.now().UTC()) {
	case GateTerminal:
		return PollResult{
			Outcome: OutcomeTerminal,
			State:   st,
			Message: terminalMessage(st.Status),
		}, nil
	case GateExpired:
		// pending 但已过期：迁移为 expired 后拒绝，且本次不发网络请求。
		if err := s.transition(ctx, st, StatusExpired); err != nil {
			return PollResult{}, err
		}
		st.Status = StatusExpired
		return PollResult{
			Outcome: OutcomeTerminal,
			State:   st,
			Message: terminalMessage(StatusExpired),
		}, nil
	case GateReady:
		// 唯一允许继续向下走网络轮询的分支。
	default:
		// 失败关闭：门禁枚举若被扩展出未知值，绝不能静默降级成「发起网络请求」。
		return PollResult{}, fmt.Errorf("%w: 未知轮询门禁结论", ErrInvalidStatus)
	}

	// 网络调用在写事务之外。
	token, pollErr := s.client.PollAccessToken(ctx, code)
	if pollErr == nil {
		if !token.Valid() {
			return PollResult{}, ErrMalformedToken
		}
		// 拿到令牌：pending → authorized。CAS 失败说明期间被取消或重复发起，
		// 令牌随即作废（不落库），取消优先。
		if err := s.transition(ctx, st, StatusAuthorized); err != nil {
			return PollResult{}, err
		}
		st.Status = StatusAuthorized
		return PollResult{
			Outcome: OutcomeAuthorized,
			State:   st,
			Token:   token,
			Message: "授权成功",
		}, nil
	}

	var pe *PollError
	if !errors.As(pollErr, &pe) {
		// 非协议错误：保持原状态，交给上层重试。
		return PollResult{}, fmt.Errorf("轮询 GitHub 失败: %w", pollErr)
	}

	switch pe.Status {
	case PollPending:
		return s.continueResult(st, pe.Message, pollErr), nil
	case PollSlowDown:
		next := NextInterval(st.PollInterval(), pollErr)
		// 把退避后的间隔持久化（跨请求存活），CAS 保证不覆盖已终态的记录。
		if err := s.updateInterval(ctx, st, next); err != nil {
			return PollResult{}, err
		}
		st.PollingInterval = next
		return PollResult{
			Outcome:      OutcomeContinue,
			State:        st,
			Message:      pe.Message,
			NextInterval: next,
		}, nil
	case PollExpired:
		return s.terminalTransition(ctx, st, StatusExpired, pe.Message)
	default: // PollTerminal / PollUnknown
		return s.terminalTransition(ctx, st, StatusDenied, pe.Message)
	}
}

// Cancel 由 P0-19 的 Esc 取消调用：pending → denied，不留下未绑定半成品。
// 已是终态时幂等返回（重复按 Esc 不报错）。
func (s *DeviceFlowService) Cancel(ctx context.Context, deviceCode string) error {
	code := strings.TrimSpace(deviceCode)
	if code == "" {
		return ErrDeviceCodeEmpty
	}
	st, err := s.store.GetByDeviceCode(ctx, code)
	if err != nil {
		return err
	}
	if st.Status.Terminal() {
		return nil
	}
	return s.transition(ctx, st, StatusDenied)
}

// transition 把「校验路径 + CAS 写回」收口，所有状态迁移都经这里。
func (s *DeviceFlowService) transition(ctx context.Context, st DeviceAuthState, to DeviceAuthStatus) error {
	if err := CanTransition(st.Status, to); err != nil {
		return err
	}
	return s.store.Write(ctx, func(tx DeviceAuthWriteTx) error {
		return tx.Transition(ctx, st.ID, st.Status, to)
	})
}

func (s *DeviceFlowService) updateInterval(ctx context.Context, st DeviceAuthState, interval int) error {
	return s.store.Write(ctx, func(tx DeviceAuthWriteTx) error {
		return tx.UpdateInterval(ctx, st.ID, st.Status, interval)
	})
}

func (s *DeviceFlowService) continueResult(st DeviceAuthState, msg string, pollErr error) PollResult {
	return PollResult{
		Outcome:      OutcomeContinue,
		State:        st,
		Message:      msg,
		NextInterval: NextInterval(st.PollInterval(), pollErr),
	}
}

func (s *DeviceFlowService) terminalTransition(ctx context.Context, st DeviceAuthState, to DeviceAuthStatus, msg string) (PollResult, error) {
	if err := s.transition(ctx, st, to); err != nil {
		return PollResult{}, err
	}
	st.Status = to
	if msg == "" {
		msg = terminalMessage(to)
	}
	return PollResult{Outcome: OutcomeTerminal, State: st, Message: msg}, nil
}
