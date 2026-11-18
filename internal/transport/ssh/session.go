package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	done  chan struct{}
	once  sync.Once
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
	return s
}

// Read 把每次输入视为一次心跳：刷新空闲计时器与 presence.LastSeenAt，
// 但不产生任何持久化写入（§2.6 / ADR-016）。
//
// 注意：读到 io.EOF 不等于连接断开——客户端可以只 CloseWrite 自己的输入方向，
// 此时服务端仍要继续输出。会话的收口由 Server 的连接级 connDone 信号负责。
func (s *Session) Read(p []byte) (int, error) {
	n, err := s.ch.Read(p)
	if n > 0 {
		s.resetIdle()
		_ = s.reg.Touch(s.id)
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
