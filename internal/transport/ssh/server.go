package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

const (
	DefaultAddr        = ":2222"
	DefaultMaxConns    = 100
	DefaultIdleTimeout = 10 * time.Minute
)

var (
	ErrServerFull = errors.New("motd: 连接数已满，请稍后重试")
	ErrRootLogin  = errors.New("motd: 禁止以 root 登录")
	ErrRevokedKey = errors.New("motd: 公钥已吊销，拒绝认证")
)

// sshLoginBanner 是服务端生成的固定引导文案（ADR-019：固定文本路径，
// 不含任何用户数据 / 凭据 / 指纹）。始终随 SSH banner 下发，使被拒的
// root 用户或密码登录用户知道正确姿势；两行 CRLF 收口，1 Mbps 带宽友好。
const sshLoginBanner = "motd: 请用任意非 root 用户名 + 本机 SSH 公钥登录（禁 root、禁密码认证）。\r\n" +
	"motd: 未绑定公钥将以访客进入；访客可浏览并使用 /register 完成注册。\r\n"

// Config 是 SSH 入口的全部外部依赖。
// Resolver / Presence / Handler 都由 cmd 装配注入：transport 不碰数据库、
// 不碰渲染实现，也不自己决定身份策略（§2.2 禁止表）。
type Config struct {
	Addr        string
	DataDir     string
	MaxConns    int
	IdleTimeout time.Duration
	Resolver    auth.Resolver
	Presence    *presence.Registry
	Handler     Handler
	Logger      *slog.Logger
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = DefaultAddr
	}
	if c.MaxConns <= 0 {
		c.MaxConns = DefaultMaxConns
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return c
}

func (c Config) validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("Config.DataDir 不能为空")
	}
	if c.Resolver == nil {
		return fmt.Errorf("Config.Resolver 不能为空")
	}
	if c.Presence == nil {
		return fmt.Errorf("Config.Presence 不能为空")
	}
	if c.Handler == nil {
		return fmt.Errorf("Config.Handler 不能为空")
	}
	if c.MaxConns < 1 {
		return fmt.Errorf("Config.MaxConns 必须 >= 1")
	}
	return nil
}

// Server 是单进程 SSH 入口。一个连接一个 goroutine 树，退出必须回收。
type Server struct {
	cfg             Config
	signer          gossh.Signer
	hostFingerprint string
	serverConfig    *gossh.ServerConfig

	slots chan struct{}
	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	// sessions 是 P0-18 的会话撤销索引：解绑公钥后要关掉所有仍用该指纹的
	// 在线会话。独立锁，避免与 conns 的互斥形成锁环。
	sessMu   sync.Mutex
	sessions map[string]*Session
	wg       sync.WaitGroup
	closed   chan struct{}
	once     sync.Once
}

func New(cfg Config) (*Server, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	signer, fp, err := LoadOrCreateHostKey(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	// 端口占用在 New 阶段就暴露，不必等 Serve 的第一个 Accept；
	// 同时让 Addr() 成为纯读操作，消除「先 Addr 后 Serve」的自锁陷阱
	// （从零启动实测发现过一次）。
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s: %w", cfg.Addr, err)
	}
	s := &Server{
		cfg:             cfg,
		signer:          signer,
		hostFingerprint: fp,
		slots:           make(chan struct{}, cfg.MaxConns),
		conns:           make(map[net.Conn]struct{}),
		sessions:        make(map[string]*Session),
		ln:              ln,
		closed:          make(chan struct{}),
	}

	// 仅公钥认证：PasswordCallback / KeyboardInteractiveCallback 保持 nil，
	// NoClientAuth 保持 false（匿名连接直接拒绝）。
	sc := &gossh.ServerConfig{
		ServerVersion:     "SSH-2.0-motd",
		MaxAuthTries:      3,
		PublicKeyCallback: s.publicKeyCallback,
		BannerCallback:    s.bannerCallback,
	}
	sc.AddHostKey(signer)
	s.serverConfig = sc
	return s, nil
}

func (s *Server) HostKeyFingerprint() string { return s.hostFingerprint }

// Addr 返回实际监听地址；Addr 配 :0 时供测试取随机端口。
// listener 在 New 阶段已绑定，因此这里是纯读，不会阻塞。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) full() bool { return len(s.slots) >= cap(s.slots) }

// fullLine 是满员提示的唯一生成点，rejectFull 与 bannerCallback 共用。
func (s *Server) fullLine() string {
	return fmt.Sprintf("motd: 连接数已满（%d/%d），请稍后重试\r\n", len(s.slots), cap(s.slots))
}

// bannerCallback 在 SSH 用户认证之前下发：始终带登录引导，满员时再叠加满员提示。
// root / 密码认证被拒的客户端通常只看到通用的 Permission denied (publickey)，
// 这段 banner 给出可操作的提示（禁 root、仅公钥、访客放行、可 /register）。
func (s *Server) bannerCallback(_ gossh.ConnMetadata) string {
	var b strings.Builder
	if s.full() {
		b.WriteString(s.fullLine())
	}
	b.WriteString(sshLoginBanner)
	return b.String()
}

func (s *Server) publicKeyCallback(conn gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
	// 设计稿 §1.2：禁 root、禁密码。SSH 用户名只是客户端声明，root 一律拒绝。
	if conn.User() == "root" {
		return nil, ErrRootLogin
	}
	if s.full() {
		return nil, ErrServerFull
	}
	fp := gossh.FingerprintSHA256(key)
	// P0-18：把服务端持有的 ssh.PublicKey 序列化文本一并放进 Permissions。
	// 客户端必须先持有私钥通过公钥挑战，服务端才拿得到 key；这不是「客户端
	// 提交的公钥文本」。用 application 的纯校验函数兜底，脏文本不入库不进内存。
	pk := ""
	if serialized := gossh.MarshalAuthorizedKey(key); len(serialized) > 0 {
		candidate := strings.TrimSpace(string(serialized))
		if auth.ValidatePublicKeyText(candidate) == nil {
			pk = candidate
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := s.cfg.Resolver.Resolve(ctx, fp)
	if err != nil {
		if errors.Is(err, auth.ErrKeyRevoked) {
			return nil, ErrRevokedKey
		}
		return nil, fmt.Errorf("motd: 身份解析失败: %w", err)
	}
	return &gossh.Permissions{Extensions: map[string]string{
		"fingerprint": fp,
		"public_key":  pk,
		"kind":        strconv.FormatUint(uint64(p.Kind), 10),
		"user_id":     strconv.FormatInt(p.UserID, 10),
	}}, nil
}

func principalFromPermissions(perm *gossh.Permissions) (auth.Principal, string) {
	if perm == nil {
		return auth.Guest(""), ""
	}
	fp := perm.Extensions["fingerprint"]
	p := auth.Guest(fp)
	if uid, err := strconv.ParseInt(perm.Extensions["user_id"], 10, 64); err == nil && uid > 0 {
		p.Kind = auth.KindUser
		p.UserID = uid
	}
	return p, fp
}

// publicKeyFromPermissions 与 principalFromPermissions 分开，保持后者的
// 双返回值签名不变（既有契约测试零改动）。值同样只来自服务端认证阶段。
func publicKeyFromPermissions(perm *gossh.Permissions) string {
	if perm == nil {
		return ""
	}
	return perm.Extensions["public_key"]
}

// Serve 阻塞直到 ctx 取消或 Close 调用；返回时连接 goroutine 已全部退出。
// listener 由 New 绑定，这里只负责 Accept（端口占用不会拖到运行时才发现）。
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return fmt.Errorf("SSH listener 未初始化")
	}

	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-s.closed:
			_ = ln.Close()
		}
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			s.cfg.Logger.Warn("接受连接失败", "err", err)
			continue
		}
		// 满员在握手前就拒绝，不把资源耗在 SSH 协商上。
		if s.full() {
			s.rejectFull(c)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(ctx, c)
		}()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) rejectFull(c net.Conn) {
	_, _ = io.WriteString(c, s.fullLine())
	_ = c.Close()
}

// Close 主动停服（测试用）；生产路径由 Serve 的 ctx 取消触发。
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closed)
		s.mu.Lock()
		ln := s.ln
		conns := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		for _, c := range conns {
			_ = c.Close()
		}
	})
}

func (s *Server) trackConn(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

func (s *Server) serveConn(ctx context.Context, c net.Conn) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		// accept 检查与占槽之间的竞态窗口，仍要给出明确拒绝。
		s.rejectFull(c)
		return
	}
	s.trackConn(c, true)
	defer func() {
		s.trackConn(c, false)
		_ = c.Close()
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close() // 也打断仍在进行握手的连接
		case <-s.closed:
			_ = c.Close()
		case <-done:
		}
	}()

	sc, chans, reqs, err := gossh.NewServerConn(c, s.serverConfig)
	if err != nil {
		s.cfg.Logger.Debug("SSH 握手失败", "remote", c.RemoteAddr(), "err", err)
		return
	}
	defer sc.Close()
	go gossh.DiscardRequests(reqs)

	p, fp := principalFromPermissions(sc.Permissions)
	pk := publicKeyFromPermissions(sc.Permissions)
	remoteIP := ""
	if c.RemoteAddr() != nil {
		remoteIP = c.RemoteAddr().String()
	}
	// 连接级关闭信号：chans 关闭即 SSH 连接已断，该连接上的所有 session 一起收掉。
	// 不能用「channel 读到 EOF」代替——客户端只 CloseWrite 输入方向时，
	// 服务端仍应继续输出，那是合法状态而非连接断开。
	connDone := make(chan struct{})
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(gossh.UnknownChannelType, "motd: 只支持 session 通道")
			continue
		}
		s.wg.Add(1)
		go func(nch gossh.NewChannel) {
			defer s.wg.Done()
			s.handleSession(ctx, nch, p, fp, pk, connDone, remoteIP)
		}(nch)
	}
	close(connDone)
}

func (s *Server) handleSession(ctx context.Context, nch gossh.NewChannel, p auth.Principal, fp, pk string, connDone <-chan struct{}, remoteIP string) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	id := newSessionID()
	if err := s.cfg.Presence.Connect(id, presence.KindSSH, p.UserID); err != nil {
		_ = ch.Close()
		return
	}
	defer s.cfg.Presence.Disconnect(id)

	sess := newSession(id, ch, fp, pk, p, s.cfg.Presence, s.cfg.IdleTimeout, remoteIP)
	s.trackSession(sess, true)
	defer s.trackSession(sess, false)
	defer sess.Close()

	go func() {
		for req := range reqs {
			switch req.Type {
			case "pty-req":
				var r ptyRequest
				if err := gossh.Unmarshal(req.Payload, &r); err != nil {
					_ = req.Reply(false, nil)
					continue
				}
				sess.setWindow(r.Term, r.Columns, r.Rows)
				_ = req.Reply(true, nil)
			case "window-change":
				var r windowChange
				if err := gossh.Unmarshal(req.Payload, &r); err != nil {
					_ = req.Reply(false, nil)
					continue
				}
				sess.setSize(r.Columns, r.Rows)
				_ = req.Reply(true, nil)
			case "shell":
				_ = req.Reply(true, nil)
				go s.runHandler(ctx, sess)
			default:
				_ = req.Reply(false, nil)
			}
		}
	}()

	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-s.closed:
			_ = sess.Close()
		case <-connDone:
			_ = sess.Close()
		case <-sess.Done():
		}
	}()

	<-sess.Done()
}

func (s *Server) runHandler(ctx context.Context, sess *Session) {
	defer sess.Close()
	err := s.cfg.Handler.Serve(ctx, sess)
	// Close 之前回退出码，客户端才能以 0 结束而不是 255。
	sess.sendExitStatus(0)
	if err != nil && !errors.Is(err, io.EOF) {
		s.cfg.Logger.Debug("会话 handler 退出", "session", sess.ID(), "err", err)
	}
}
