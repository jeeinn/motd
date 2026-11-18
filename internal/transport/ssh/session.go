package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// Handler 消费一条已建立 PTY 的会话。
//
// P0-21 注入静态欢迎页；P0-22 换成输入归一化；P0-24 换成页面控制器。
// 协议层不解析按键、不渲染 ViewModel，替换实现不需要改 transport/ssh。
type Handler interface {
	Serve(ctx context.Context, sess *Session) error
}

// Session 是 transport/ssh 暴露给 Handler 的会话门面。
// 它实现 io.ReadWriter，但不暴露底层 ssh.Channel，避免 Handler 绕过
// 心跳刷新与空闲超时控制。
type Session struct {
	id          string
	ch          gossh.Channel
	fingerprint string
	publicKey   string // 认证阶段服务端持有的 ssh.PublicKey 序列化文本（P0-18）
	principal   auth.Principal
	reg         *presence.Registry
	idle        time.Duration

	mu     sync.Mutex
	term   string
	width  uint32
	height uint32

	timer *time.Timer
	hb    *time.Ticker
	done  chan struct{}
	once  sync.Once
}

// sessionHeartbeatInterval 是连接级心跳周期，取 presence 默认超时的一半。
//
// Registry 的 20s 超时本意是清理「连接已断但没走 Disconnect」的僵尸条目，
// 但 SSH 是长连接：用户等待 GitHub Device Flow 授权时可能几十秒没有任何输入，
// 单靠 Session.Read 刷新心跳会把仍活着的连接误删，进而导致
// 「授权成功、账号已建、公钥已绑，但 UpgradeToUser 失败仍是访客」。
// 心跳只在内存 Touch，不写 SQLite（§2.6 / ADR-016）。
const sessionHeartbeatInterval = 10 * time.Second

// reRegisterSession 把被 Reap 清掉、但连接仍然活着的会话放回注册表。
// 注册表只保存内存状态，无法知道 TCP/SSH 连接是否还在；只有 Session 能判断，
// 因此补登记放在这一层，而不是放宽 Reap 的超时口径（ADR-016 保持不变）。
func reRegisterSession(reg *presence.Registry, id string, userID int64) error {
	if err := reg.Connect(id, presence.KindSSH, userID); err != nil {
		return err
	}
	return reg.SetState(id, presence.StateBrowsing)
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "s-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "s-" + hex.EncodeToString(b[:])
}

// newSession 的 fp / pk 都来自认证阶段服务端持有的 ssh.PublicKey，
// 不是客户端自由提交的文本（§4.6 / P0-18）。Handler 只能读、不能改。
func newSession(id string, ch gossh.Channel, fp, pk string, p auth.Principal, reg *presence.Registry, idle time.Duration) *Session {
	s := &Session{
		id: id, ch: ch, fingerprint: fp, publicKey: pk, principal: p, reg: reg, idle: idle,
		done: make(chan struct{}),
	}
	s.timer = time.AfterFunc(idle, func() { _ = s.Close() })
	s.hb = time.NewTicker(sessionHeartbeatInterval)
	go s.heartbeatLoop()
	return s
}

// heartbeatLoop 让活着的连接持续刷新注册表心跳；Close 关闭 done 后即退出。
func (s *Session) heartbeatLoop() {
	for {
		select {
		case <-s.hb.C:
			s.heartbeat()
		case <-s.done:
			return
		}
	}
}

// heartbeat 只刷新内存状态，抽成方法便于用注入时钟回归「长等待不误删」。
func (s *Session) heartbeat() { _ = s.reg.Touch(s.id) }

// Read 把每次输入视为一次心跳：刷新空闲计时器与 presence.LastSeenAt，
// 但不产生任何持久化写入（§2.6 / ADR-016）。
//
// 注意：读到 io.EOF 不等于连接断开——客户端可以只 CloseWrite 自己的输入方向，
// 此时服务端仍要继续输出。会话的收口由 Server 的连接级 connDone 信号负责。
func (s *Session) Read(p []byte) (int, error) {
	n, err := s.ch.Read(p)
	if n > 0 {
		s.resetIdle()
		if terr := s.reg.Touch(s.id); terr != nil && errors.Is(terr, presence.ErrUnknownSession) {
			// Reap 会按 20s 心跳超时把「长时间只读不输入」的连接移出注册表，
			// 但连接本身没断、Read 仍会继续收到按键。若不重新登记，在线数会
			// 永久归零，后续 SetState / AttachUser 也全部失败（本机实测发现）。
			_ = reRegisterSession(s.reg, s.id, s.Principal().UserID)
		}
	}
	return n, err
}

func (s *Session) Write(p []byte) (int, error) { return s.ch.Write(p) }

// sendExitStatus 在会话结束前回一个 exit-status，否则 OpenSSH 客户端
// 只看到连接被关闭，会以 255 退出，看起来像登录失败。
func (s *Session) sendExitStatus(code uint32) {
	_, _ = s.ch.SendRequest("exit-status", false, gossh.Marshal(&struct{ Status uint32 }{code}))
}

func (s *Session) resetIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Reset(s.idle)
	}
}

// Close 幂等：handler 结束、空闲超时、ctx 取消三条路径都会调它。
func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.hb != nil {
			s.hb.Stop()
		}
		s.mu.Unlock()
		err = s.ch.Close()
	})
	return err
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) ID() string          { return s.id }
func (s *Session) Fingerprint() string { return s.fingerprint }

// Principal 返回身份快照。读写都走 s.mu：P0-19 引入了一次运行期写入
// （UpgradeToUser），若这里无锁读会与升级形成数据竞争。
func (s *Session) Principal() auth.Principal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principal
}

// UpgradeToUser 是 guest → user 的**唯一**升级入口（P0-19 / ADR-024 第 6 条）。
//
// 三道门同时生效：
//   - userID 必须 > 0；
//   - 当前必须是访客（已登录会话不可重复升级、更不可横向改成别人的账号）；
//   - presence 注册表里的 UserID 也必须成功从 0 绑定（与内存在线状态一致）。
//
// 成功后才写 principal；任一门失败都保持访客态，不留「已登录却未绑定」
// 的半成品会话（§4.6 验收）。本方法只改身份快照，不触碰协议层与 channel。
func (s *Session) UpgradeToUser(userID int64) error {
	if userID <= 0 {
		return auth.ErrSessionUpgradeRejected
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.principal.IsUser() {
		return auth.ErrSessionUpgradeRejected
	}
	// 失败关闭保持不变（ADR-024 第 6 条）：会话不在注册表里就拒绝升级。
	// 活连接由下面的连接级心跳保证不会被 Reap 清掉，因此这里不需要、
	// 也不允许放宽成「自动补登记后升级」。
	if err := s.reg.AttachUser(s.id, userID); err != nil {
		return err
	}
	s.principal = auth.Principal{
		Kind:           auth.KindUser,
		UserID:         userID,
		KeyFingerprint: s.fingerprint,
	}
	return nil
}

// PublicKey 返回认证阶段服务端序列化出的公钥文本，供 P0-18 的 /key add 使用。
// 值来自 gossh.MarshalAuthorizedKey(key)，已在 publicKeyCallback 里过
// auth.ValidatePublicKeyText；不解析、不回显，只作 ssh_keys.public_key 落库。
func (s *Session) PublicKey() string { return s.publicKey }

func (s *Session) SetState(state presence.State) error { return s.reg.SetState(s.id, state) }

// Window 返回最近一次 pty-req / window-change 的终端尺寸。
func (s *Session) Window() (width, height uint32, term string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.width, s.height, s.term
}

func (s *Session) setWindow(term string, width, height uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.term, s.width, s.height = term, width, height
}

func (s *Session) setSize(width, height uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width, s.height = width, height
}

// ptyRequest / windowChange 是 RFC 4254 的载荷结构，字段名供 ssh.Unmarshal 使用。
type ptyRequest struct {
	Term        string
	Columns     uint32
	Rows        uint32
	PixelWidth  uint32
	PixelHeight uint32
	Modes       string
}

type windowChange struct {
	Columns     uint32
	Rows        uint32
	PixelWidth  uint32
	PixelHeight uint32
}

var _ io.ReadWriter = (*Session)(nil)
