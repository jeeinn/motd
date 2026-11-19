// Package auth 的认证限流（P1-M2）。
//
// 纯内存滑动窗口 + 锁定时长。零 DB、零 infrastructure import，由 archtest 守卫。
// 计数只在进程内存里：进程重启即清零，这是已知且接受的限制（水平扩容后各进程
// 独立计数，需要共享计数时另配外部存储，不在 MVP 范围）。
//
// 三类作用域：hash(ip) / userID / hash(email)。作用域字符串由调用方构造，
// 本层只接受已经哈希过的标识，绝不接触原始 IP / 邮箱明文。
package auth

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// DefaultRateLimitConfig 是限流参数兜底。config.Load 提供同名环境变量默认值，
// 两处必须保持一致：一处给运维看，一处保证直接构造用例时也有安全值。
var DefaultRateLimitConfig = RateLimitConfig{
	MaxAttempts: 5,
	Window:      5 * time.Minute,
	Lockout:     15 * time.Minute,
	MaxKeys:     4096,
}

// LimitKind 是限流场景。不同场景各自计数，互不串号。
type LimitKind uint8

const (
	LimitUnknown      LimitKind = iota
	LimitLoginFailure           // 登录失败：键 = hash(ip) + hash(email)
	LimitDeviceStart            // Device Flow 发起：键 = hash(ip)
	LimitKeyBinding             // 公钥绑定尝试：键 = userID
	LimitVerify                 // 邮箱验证码：键 = hash(email)；M5 才落地，本里程碑只留接口
)

func (k LimitKind) String() string {
	switch k {
	case LimitLoginFailure:
		return "login_failure"
	case LimitDeviceStart:
		return "device_start"
	case LimitKeyBinding:
		return "key_binding"
	case LimitVerify:
		return "verify"
	default:
		return "unknown"
	}
}

// Valid 拒绝未登记场景，避免调用方传 0 把所有动作挤进同一条计数。
func (k LimitKind) Valid() bool { return k >= LimitLoginFailure && k <= LimitVerify }

// RateLimitConfig 是限流器的全部可调参数；非正值一律回落到 DefaultRateLimitConfig。
type RateLimitConfig struct {
	MaxAttempts int
	Window      time.Duration
	Lockout     time.Duration
	MaxKeys     int
}

func (c RateLimitConfig) normalized() RateLimitConfig {
	d := DefaultRateLimitConfig
	if c.MaxAttempts > 0 {
		d.MaxAttempts = c.MaxAttempts
	}
	if c.Window > 0 {
		d.Window = c.Window
	}
	if c.Lockout > 0 {
		d.Lockout = c.Lockout
	}
	if c.MaxKeys > 0 {
		d.MaxKeys = c.MaxKeys
	}
	return d
}

// ErrInvalidRateLimitScope 表示调用方没有给出可追踪的作用域。
// 空作用域不计入限流（否则所有匿名请求会共用一把锁），但必须让调用方知道。
var ErrInvalidRateLimitScope = errors.New("认证限流缺少作用域标识")

// RateLimitError 命中限流时的明确拒绝，形态与 forum.RateLimitError 对齐：
// RetryAfter 供终端显示「请 N 秒后重试」，Reason 只含固定场景名，不含原始 IP / 邮箱。
type RateLimitError struct {
	Kind       LimitKind
	Reason     string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("操作过于频繁（%s），请 %s 后重试", e.Reason, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("操作过于频繁（%s）", e.Reason)
}

// RetryAfterSeconds 是终端展示用的向上取整秒数，至少为 1。
func (e *RateLimitError) RetryAfterSeconds() int64 {
	if e == nil {
		return 0
	}
	sec := int64((e.RetryAfter + time.Second - 1) / time.Second)
	if sec < 1 {
		return 1
	}
	return sec
}

type limitState struct {
	attempts    []time.Time
	lockedUntil time.Time
	lastSeen    time.Time
}

// RateLimiter 是并发安全的有界内存限流器。
type RateLimiter struct {
	mu     sync.Mutex
	cfg    RateLimitConfig
	now    func() time.Time
	states map[string]*limitState
}

// RateLimitOption 用于注入时钟与参数；注入时钟让锁定/恢复测试无需真实等待。
type RateLimitOption func(*RateLimiter)

func WithRateLimitClock(f func() time.Time) RateLimitOption {
	return func(r *RateLimiter) {
		if f != nil {
			r.now = f
		}
	}
}

func WithRateLimitConfig(c RateLimitConfig) RateLimitOption {
	return func(r *RateLimiter) { r.cfg = c.normalized() }
}

func NewRateLimiter(opts ...RateLimitOption) *RateLimiter {
	r := &RateLimiter{
		cfg:    DefaultRateLimitConfig.normalized(),
		now:    time.Now,
		states: make(map[string]*limitState),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// check 是该限流器唯一的计数入口。
//
// 语义：窗口内累计到 MaxAttempts 次即锁定 Lockout，并清空窗口；
// 锁定期内每次调用都返回带 RetryAfter 的 *RateLimitError；锁定到期后重新计数。
// 四类场景共用同一套状态机，但作用域字符串带 kind 前缀，互不干扰。
func (r *RateLimiter) check(kind LimitKind, scope string) error {
	if !kind.Valid() || scope == "" {
		return ErrInvalidRateLimitScope
	}
	key := kind.String() + ":" + scope

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now().UTC()
	st := r.states[key]
	if st == nil {
		if len(r.states) >= r.cfg.MaxKeys {
			r.evictOldest()
		}
		st = &limitState{}
		r.states[key] = st
	}
	st.lastSeen = now

	if now.Before(st.lockedUntil) {
		return &RateLimitError{
			Kind: kind, Reason: kind.String(),
			RetryAfter: st.lockedUntil.Sub(now),
		}
	}

	cutoff := now.Add(-r.cfg.Window)
	kept := st.attempts[:0]
	for _, at := range st.attempts {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	st.attempts = kept

	if len(st.attempts) >= r.cfg.MaxAttempts {
		st.attempts = nil
		st.lockedUntil = now.Add(r.cfg.Lockout)
		return &RateLimitError{
			Kind: kind, Reason: kind.String(), RetryAfter: r.cfg.Lockout,
		}
	}
	st.attempts = append(st.attempts, now)
	return nil
}

// evictOldest 只在容量打满时按 lastSeen 淘汰一个最旧作用域，
// 保证 map 不会随随机 IP 无限增长。调用方必须已持有 r.mu。
func (r *RateLimiter) evictOldest() {
	var oldestKey string
	var oldestAt time.Time
	for key, st := range r.states {
		if oldestKey == "" || st.lastSeen.Before(oldestAt) {
			oldestKey, oldestAt = key, st.lastSeen
		}
	}
	if oldestKey != "" {
		delete(r.states, oldestKey)
	}
}

// scopeOf 用分隔符拼两个哈希，避免 ("ab","c") 与 ("a","bc") 撞同一个键。
// 参数应当已经哈希过；本函数不做哈希，防止把哈希职责散落到使用方。
func scopeOf(ipHash, emailHash string) string {
	return ipHash + "|" + emailHash
}

// NoteLoginFailure 记一次登录失败；emailHash 为空时退化为纯 IP 作用域。
// M2 没有邮箱密码流程（M5 才有），因此调用方只能给 ipHash。
func (r *RateLimiter) NoteLoginFailure(ipHash, emailHash string) error {
	if ipHash == "" && emailHash == "" {
		return ErrInvalidRateLimitScope
	}
	return r.check(LimitLoginFailure, scopeOf(ipHash, emailHash))
}

// AllowDeviceStart 是 Device Flow 发起的按 IP 限流。
func (r *RateLimiter) AllowDeviceStart(ipHash string) error {
	return r.check(LimitDeviceStart, ipHash)
}

// AllowKeyBinding 是公钥绑定尝试的按用户限流。
func (r *RateLimiter) AllowKeyBinding(userID int64) error {
	if userID <= 0 {
		return ErrInvalidRateLimitScope
	}
	return r.check(LimitKeyBinding, strconv.FormatInt(userID, 10))
}

// AllowVerify 是邮箱验证码校验的按邮箱限流。M2 不实现邮箱流程，
// 本方法只是预留端口，供 M5 直接接线，不臆造发送/校验逻辑。
func (r *RateLimiter) AllowVerify(emailHash string) error {
	return r.check(LimitVerify, emailHash)
}

// AllowScope 按**本实例的配置**检查任意作用域字符串。邮箱流程需要三种不同
// 窗口（60s 重发冷却 / 每小时 5 封 / 5 次尝试），各自构造一个带独立
// WithRateLimitConfig 的 RateLimiter 即可复用同一套状态机，不必另起一套实现。
// scope 由调用方编目的（如 "e:<hash>" / "i:<hash>"），本方法不接触明文。
func (r *RateLimiter) AllowScope(scope string) error {
	if scope == "" {
		return ErrInvalidRateLimitScope
	}
	return r.check(LimitVerify, scope)
}

// Reset 清掉一个作用域的计数，供运维处置误锁与测试使用。
func (r *RateLimiter) Reset(kind LimitKind, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, kind.String()+":"+scope)
}

// Keys 返回当前保留的作用域数量，供 /healthz 与内存观测。
func (r *RateLimiter) Keys() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.states)
}
