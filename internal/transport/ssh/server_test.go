package ssh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// ---- 测试替身 ----

type testResolver struct {
	err error
	p   auth.Principal
}

func (r testResolver) Resolve(_ context.Context, fp string) (auth.Principal, error) {
	if r.err != nil {
		return auth.Principal{}, r.err
	}
	if r.p.KeyFingerprint != "" {
		return r.p, nil
	}
	return auth.Guest(fp), nil
}

type fakeConnMeta struct{ user string }

func (f fakeConnMeta) User() string          { return f.user }
func (f fakeConnMeta) SessionID() []byte     { return nil }
func (f fakeConnMeta) ClientVersion() []byte { return nil }
func (f fakeConnMeta) ServerVersion() []byte { return nil }
func (f fakeConnMeta) RemoteAddr() net.Addr  { return nil }
func (f fakeConnMeta) LocalAddr() net.Addr   { return nil }

// scriptedHandler 输出一行固定文本、登记浏览态、周期上报窗口尺寸，
// 并持续读输入以刷新心跳（心跳不写库，ADR-016）。
type scriptedHandler struct {
	started   chan struct{}
	exited    chan struct{}
	window    chan [2]uint32
	startOnce sync.Once
	exitOnce  sync.Once
}

func newScriptedHandler() *scriptedHandler {
	return &scriptedHandler{
		started: make(chan struct{}),
		exited:  make(chan struct{}),
		window:  make(chan [2]uint32, 8),
	}
}

func (h *scriptedHandler) Serve(ctx context.Context, sess *Session) error {
	defer h.exitOnce.Do(func() { close(h.exited) })
	_, _ = io.WriteString(sess, "motd test\r\n")
	_ = sess.SetState(presence.StateBrowsing)
	h.startOnce.Do(func() { close(h.started) })

	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := sess.Read(buf); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w, ht, _ := sess.Window()
			select {
			case h.window <- [2]uint32{w, ht}:
			default:
			}
		case <-ctx.Done():
			return nil
		case <-sess.Done():
			return nil
		}
	}
}

func newClientSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func clientConfig(t *testing.T) *gossh.ClientConfig {
	t.Helper()
	return &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(newClientSigner(t))},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // 测试用自签 host key
		Timeout:         3 * time.Second,
	}
}

func startServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	cfg.DataDir = t.TempDir()
	cfg.Addr = "127.0.0.1:0"
	if cfg.Resolver == nil {
		cfg.Resolver = testResolver{}
	}
	if cfg.Presence == nil {
		cfg.Presence = presence.New()
	}
	if cfg.Handler == nil {
		cfg.Handler = newScriptedHandler()
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Second
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	addr := s.Addr()
	if addr == "" {
		t.Fatal("监听未就绪")
	}
	t.Cleanup(func() {
		cancel()
		s.Close()
	})
	return s, addr
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待条件超时")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitWindow(t *testing.T, ch <-chan [2]uint32, want [2]uint32) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatalf("未收到期望窗口尺寸 %v", want)
		}
	}
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("读取欢迎行: %v", res.err)
		}
		return res.line
	case <-time.After(3 * time.Second):
		t.Fatal("读取欢迎行超时")
		return ""
	}
}

// ---- 配置与认证策略 ----

func TestNewValidatesDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("缺少 DataDir 必须拒绝")
	}
	if _, err := New(Config{DataDir: t.TempDir()}); err == nil {
		t.Fatal("缺少 Resolver 必须拒绝")
	}
	if _, err := New(Config{DataDir: t.TempDir(), Resolver: testResolver{}}); err == nil {
		t.Fatal("缺少 Presence 必须拒绝")
	}
}

func TestServerConfigOnlyPublicKeyAuth(t *testing.T) {
	s, err := New(Config{
		DataDir: t.TempDir(), Addr: "127.0.0.1:0",
		Resolver: testResolver{}, Presence: presence.New(), Handler: newScriptedHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.serverConfig.PasswordCallback != nil {
		t.Error("PasswordCallback 必须为 nil（禁密码登录）")
	}
	if s.serverConfig.KeyboardInteractiveCallback != nil {
		t.Error("KeyboardInteractiveCallback 必须为 nil（仅公钥认证）")
	}
	if s.serverConfig.NoClientAuth {
		t.Error("NoClientAuth 必须为 false（匿名连接直接拒绝）")
	}
	if s.serverConfig.PublicKeyCallback == nil {
		t.Error("PublicKeyCallback 必须设置")
	}
}

func TestAnonymousClientRejected(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "tester",
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err == nil {
		_ = cl.Close()
		t.Fatal("不带公钥的客户端必须被拒绝（仅公钥认证）")
	}
}

func TestPublicKeyCallbackRejectsRootAndRevoked(t *testing.T) {
	key := newClientSigner(t).PublicKey()
	base := Config{
		DataDir: t.TempDir(), Addr: "127.0.0.1:0",
		Presence: presence.New(), Handler: newScriptedHandler(),
	}

	s, err := New(baseWithResolver(base, testResolver{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.publicKeyCallback(fakeConnMeta{user: "root"}, key); !errors.Is(err, ErrRootLogin) {
		t.Fatalf("root 登录必须被拒，实际 %v", err)
	}
	perm, err := s.publicKeyCallback(fakeConnMeta{user: "tester"}, key)
	if err != nil {
		t.Fatalf("普通用户应通过: %v", err)
	}
	if perm.Extensions["fingerprint"] != gossh.FingerprintSHA256(key) {
		t.Fatal("Permissions 必须带上服务端算出的指纹，供 P0-18 绑定使用")
	}

	revoked, err := New(baseWithResolver(base, testResolver{err: auth.ErrKeyRevoked}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := revoked.publicKeyCallback(fakeConnMeta{user: "tester"}, key); !errors.Is(err, ErrRevokedKey) {
		t.Fatalf("已吊销公钥必须映射为 ErrRevokedKey，实际 %v", err)
	}

	failing, err := New(baseWithResolver(base, testResolver{err: errors.New("db down")}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.publicKeyCallback(fakeConnMeta{user: "tester"}, key); err == nil || errors.Is(err, ErrRevokedKey) {
		t.Fatal("普通解析失败不能谎报为「公钥已吊销」")
	}
}

func baseWithResolver(c Config, r auth.Resolver) Config {
	c.Resolver = r
	return c
}

func TestPrincipalFromPermissions(t *testing.T) {
	p, fp := principalFromPermissions(&gossh.Permissions{Extensions: map[string]string{
		"fingerprint": "SHA256:x", "user_id": "42",
	}})
	if fp != "SHA256:x" || !p.IsUser() || p.UserID != 42 {
		t.Fatalf("登录态还原错误: %+v fp=%q", p, fp)
	}
	g, _ := principalFromPermissions(nil)
	if !g.IsGuest() {
		t.Fatal("空 Permissions 必须回落为访客")
	}
}

// ---- 端到端链路 ----

func TestEndToEndSessionRegistersPresence(t *testing.T) {
	reg := presence.New()
	h := newScriptedHandler()
	_, addr := startServer(t, Config{
		Presence: reg, Handler: h, IdleTimeout: 5 * time.Second,
	})

	cl, err := gossh.Dial("tcp", addr, clientConfig(t))
	if err != nil {
		t.Fatalf("SSH 拨号: %v", err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if line := readLine(t, out); !strings.Contains(line, "motd test") {
		t.Fatalf("未收到服务端欢迎行: %q", line)
	}
	<-h.started
	waitUntil(t, func() bool { return reg.Online() == 1 })

	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].Kind != presence.KindSSH {
		t.Fatalf("presence 应登记一条 SSH 会话: %+v", snap)
	}
	if snap[0].State != presence.StateBrowsing {
		t.Fatalf("handler 应把会话置为 Browsing，实际 %v", snap[0].State)
	}
	if reg.Active() != 1 {
		t.Fatal("Browsing 会话应计入 Active")
	}

	waitWindow(t, h.window, [2]uint32{80, 24})
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}
	waitWindow(t, h.window, [2]uint32{100, 30})

	_ = cl.Close()
	<-h.exited
	waitUntil(t, func() bool { return reg.Online() == 0 })
}

func TestIdleTimeoutClosesSession(t *testing.T) {
	reg := presence.New()
	h := newScriptedHandler()
	_, addr := startServer(t, Config{
		Presence: reg, Handler: h, IdleTimeout: 150 * time.Millisecond,
	})

	cl, err := gossh.Dial("tcp", addr, clientConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	_ = readLine(t, out)
	<-h.started
	waitUntil(t, func() bool { return reg.Online() == 1 })

	// 不发任何输入，空闲计时器必须自行收掉会话并注销 presence。
	waitUntil(t, func() bool { return reg.Online() == 0 })
	<-h.exited
}

func TestServerRejectsWhenFull(t *testing.T) {
	s, addr := startServer(t, Config{MaxConns: 1})

	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	waitUntil(t, func() bool { return s.full() })

	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if err := c2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c2).ReadString('\n')
	if err != nil {
		t.Fatalf("满员拒绝时未收到可读提示: %v", err)
	}
	if !strings.Contains(line, "连接数已满") {
		t.Fatalf("满员提示内容不正确: %q", line)
	}
}
