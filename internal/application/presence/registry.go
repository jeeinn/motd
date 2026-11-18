// Package presence 维护内存连接注册表（评审稿 v0.2 §2.6 / §2.7）。
//
// 边界：在线人数、每会话状态与心跳超时全部只在内存中；
// 20 秒心跳不得逐次写 SQLite —— 为维护在线状态耗尽唯一写通道是本末倒置。
// 本包不 import infrastructure 或任何 SQL 驱动，由 archtest 守卫强制。
package presence

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultHeartbeatTimeout 是设计稿 §2.6 给的心跳超时。
const DefaultHeartbeatTimeout = 20 * time.Second

var (
	ErrDuplicateSession = errors.New("会话已存在")
	ErrUnknownSession   = errors.New("会话不存在")
	ErrInvalidSession   = errors.New("会话参数不合法")
)

// Kind 是连接入口类型。P0 只有 SSH；Web / CLI 为 P1 预留。
type Kind uint8

const (
	KindUnknown Kind = iota
	KindSSH
	KindWeb
	KindCLI
)

func (k Kind) Valid() bool { return k >= KindSSH && k <= KindCLI }

// State 是会话的交互状态。Idle 不计入 Active（同时活跃独立统计）。
type State uint8

const (
	StateUnknown State = iota
	StateAuthenticating
	StateBrowsing
	StateComposing
	StateIdle
)

func (s State) Valid() bool { return s >= StateAuthenticating && s <= StateIdle }

// Active 只把「真正在浏览或编辑」算作活跃；认证中与空闲都不计入，
// 这样 Active 才能与 Online（连接总数）形成有意义的独立统计。
func (s State) Active() bool { return s == StateBrowsing || s == StateComposing }

// Session 是一条在线连接的内存状态。ID 是连接标识，不是数据库键；
// UserID 为 0 表示访客。
type Session struct {
	ID          string
	Kind        Kind
	UserID      int64
	State       State
	ConnectedAt time.Time
	LastSeenAt  time.Time
}

// Registry 是纯内存的连接注册表。所有方法并发安全。
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]Session
	now      func() time.Time
	timeout  time.Duration
	peak     int
}

type Option func(*Registry)

// WithClock 注入时钟，使心跳超时测试不必真等 20 秒。
func WithClock(f func() time.Time) Option {
	return func(r *Registry) {
		if f != nil {
			r.now = f
		}
	}
}

// WithTimeout 调整心跳超时；<=0 时回落到 20s。
func WithTimeout(d time.Duration) Option {
	return func(r *Registry) {
		if d > 0 {
			r.timeout = d
		}
	}
}

func New(opts ...Option) *Registry {
	r := &Registry{
		sessions: make(map[string]Session),
		now:      time.Now,
		timeout:  DefaultHeartbeatTimeout,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Connect 登记新连接。重复 ID 视为协议错误，直接拒绝。
func (r *Registry) Connect(id string, kind Kind, userID int64) error {
	if id == "" || !kind.Valid() {
		return ErrInvalidSession
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sessions[id]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateSession, id)
	}
	at := r.now().UTC()
	r.sessions[id] = Session{
		ID: id, Kind: kind, UserID: userID,
		State: StateAuthenticating, ConnectedAt: at, LastSeenAt: at,
	}
	if len(r.sessions) > r.peak {
		r.peak = len(r.sessions)
	}
	return nil
}

// Touch 是一次心跳：只更新内存时间，绝不产生任何持久化写入。
func (r *Registry) Touch(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	s.LastSeenAt = r.now().UTC()
	r.sessions[id] = s
	return nil
}

// SetState 更新交互状态；状态变化本身也是活跃信号，顺带刷新心跳时间。
func (r *Registry) SetState(id string, state State) error {
	if !state.Valid() {
		return ErrInvalidSession
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	s.State = state
	s.LastSeenAt = r.now().UTC()
	r.sessions[id] = s
	return nil
}

// Disconnect 移除会话，返回它此前是否存在。
func (r *Registry) Disconnect(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[id]
	delete(r.sessions, id)
	return ok
}

// Online 是连接总数。
func (r *Registry) Online() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// Active 是「在心跳窗口内且非 Idle」的会话数，与 Online 独立统计（§2.7）。
func (r *Registry) Active() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.now()
	n := 0
	for _, s := range r.sessions {
		if s.State.Active() && now.Sub(s.LastSeenAt) <= r.timeout {
			n++
		}
	}
	return n
}

// Reap 清理心跳超时的会话，返回被清理的 ID（排序保证结果可判定）。
func (r *Registry) Reap() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var out []string
	for id, s := range r.sessions {
		if now.Sub(s.LastSeenAt) > r.timeout {
			delete(r.sessions, id)
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot 返回按 ID 排序的副本，外部修改不影响内部状态。
func (r *Registry) Snapshot() []Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 返回单条会话副本。
func (r *Registry) Get(id string) (Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[id]
	return s, ok
}

// Peak 是历史最大在线数；Timeout 暴露当前心跳超时配置。
func (r *Registry) Peak() int              { r.mu.RLock(); defer r.mu.RUnlock(); return r.peak }
func (r *Registry) Timeout() time.Duration { return r.timeout }
