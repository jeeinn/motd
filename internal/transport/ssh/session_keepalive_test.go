package ssh

// v0.4.1 会话保活回归：空闲语义由「无按键」改为「连接不存活」。
// 覆盖：心跳保活不被 idle 断、硬上限兜底静默消失、关闭序列下发固定文案与
// exit-status、正常退出无提示、四条收口路径幂等不重复发 exit-status。

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// recordingChannel 记录 Write 内容与 SendRequest 调用，便于断言关闭序列。
// 读方向恒返回阻塞（用阻塞通道模拟"用户不按键但不 EOF"），让 idle timer 成为
// 唯一收口触发者。
type recordingChannel struct {
	mu        sync.Mutex
	writes    strings.Builder
	requests  []string
	readBlock chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

var _ gossh.Channel = (*recordingChannel)(nil)

func newRecordingChannel() *recordingChannel {
	return &recordingChannel{
		readBlock: make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (c *recordingChannel) Read([]byte) (int, error) {
	<-c.readBlock
	select {
	case <-c.closed:
		return 0, io.EOF
	default:
		return 0, nil
	}
}
func (c *recordingChannel) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes.Write(p)
	return len(p), nil
}
func (c *recordingChannel) SendRequest(name string, _ bool, _ []byte) (bool, error) {
	c.mu.Lock()
	c.requests = append(c.requests, name)
	c.mu.Unlock()
	return false, nil
}
func (c *recordingChannel) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *recordingChannel) CloseWrite() error     { return nil }
func (c *recordingChannel) Stderr() io.ReadWriter { return nil }
func (c *recordingChannel) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes.String()
}
func (c *recordingChannel) reqList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func newKeepaliveSession(t *testing.T, reg *presence.Registry, id string, ch gossh.Channel, idle time.Duration) *Session {
	t.Helper()
	s := newSession(id, ch, upgradeTestFP,
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample", auth.Guest(upgradeTestFP), reg, idle)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHeartbeatKeepsIdleSessionAlive(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-keepalive", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	ch := newRecordingChannel()
	// idle 很短（120ms），但心跳周期远短于它——持续心跳应让会话一直存活。
	s := newKeepaliveSession(t, reg, "s-keepalive", ch, 120*time.Millisecond)

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.heartbeat()
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-s.Done():
		t.Fatal("心跳持续刷新时，会话不应被 idle timer 关闭")
	default:
	}
}

func TestIdleTimeoutSendsNoticeAndExitStatus(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-idle-notice", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	ch := newRecordingChannel()
	_ = newKeepaliveSession(t, reg, "s-idle-notice", ch, 100*time.Millisecond)

	select {
	case <-ch.closed: // 等 channel 真正关闭，确保文案与 exit-status 已写出
	case <-time.After(2 * time.Second):
		t.Fatal("idle timer 未在预期时间内关闭会话")
	}

	if got := ch.written(); !strings.Contains(got, idleCloseNotice) {
		t.Fatalf("超时关闭未下发固定提示文案，实际写出 %q", got)
	}
	reqs := ch.reqList()
	if len(reqs) == 0 || reqs[0] != "exit-status" {
		t.Fatalf("关闭前应回 exit-status，实际请求 %v", reqs)
	}
	// presence 注销由 server.handleSession 的 defer 负责（见 server.go），
	// 本测试直接构造 Session、不经过该层；集成覆盖见 TestIdleTimeoutClosesSession。
}

func TestMaxLifetimeClosesDespiteHeartbeat(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-lifetime", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	ch := newRecordingChannel()

	// 临时缩短硬上限，测试后恢复（生产值 6h 不可测）。
	orig := sessionMaxLifetime
	sessionMaxLifetime = 150 * time.Millisecond
	t.Cleanup(func() { sessionMaxLifetime = orig })

	s := newKeepaliveSession(t, reg, "s-lifetime", ch, time.Hour) // idle 设很大，排除 idle 路径

	// 持续心跳证明连接"活着"，但硬上限仍应到点收口。
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				s.heartbeat()
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	select {
	case <-ch.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("硬上限未在预期时间内关闭会话")
	}
	if got := ch.written(); !strings.Contains(got, lifetimeCloseNotice) {
		t.Fatalf("硬上限关闭未下发固定提示文案，实际写出 %q", got)
	}
}

func TestNormalCloseSendsNoNotice(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-normal", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	ch := newRecordingChannel()
	s := newKeepaliveSession(t, reg, "s-normal", ch, time.Hour)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-ch.closed
	if got := ch.written(); got != "" {
		t.Fatalf("正常退出不应下发超时提示文案，实际写出 %q", got)
	}
	reqs := ch.reqList()
	if len(reqs) != 1 || reqs[0] != "exit-status" {
		t.Fatalf("正常退出应恰好发一次 exit-status，实际 %v", reqs)
	}
	// 幂等：重复 Close 不重复发 exit-status。
	_ = s.Close()
	if reqs := ch.reqList(); len(reqs) != 1 {
		t.Fatalf("重复 Close 不应重复发 exit-status，实际 %v", reqs)
	}
}
