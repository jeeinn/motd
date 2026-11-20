package ssh

// v0.4.2 会话生存期回归：idle timer（MOTD_SSH_IDLE_TIMEOUT）已移除——它自
// v0.4.1 起由心跳持续重置、实际永不触发，是给用户虚假控制感的死配置。
// 会话生存期现在只由绝对上限（6h）约束。
// 覆盖：无按键且不心跳也不关闭（idle 路径已消失）、硬上限兜底静默消失、
// 关闭序列下发固定文案与 exit-status、正常退出无提示且幂等。

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
// 读方向恒阻塞（模拟"用户无任何输入"），让绝对上限成为唯一收口触发者。
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

func newLifetimeSession(t *testing.T, reg *presence.Registry, id string, ch gossh.Channel) *Session {
	t.Helper()
	s := newSession(id, ch, upgradeTestFP,
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample", auth.Guest(upgradeTestFP), reg)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestNoIdlePathWithoutInputOrHeartbeat 证明 idle 路径确实消失：
// 既无用户输入、也不调心跳，会话在远超过旧默认 10m 的时长内仍不被关闭
// （测试用短绝对上限排除干扰，观察窗口内的关闭只能是 idle 残留——不应发生）。
func TestNoIdlePathWithoutInputOrHeartbeat(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-no-idle", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	orig := sessionMaxLifetime
	sessionMaxLifetime = 10 * time.Second // 远大于观察窗口，排除硬上限
	t.Cleanup(func() { sessionMaxLifetime = orig })

	ch := newRecordingChannel()
	s := newLifetimeSession(t, reg, "s-no-idle", ch)

	// 不发任何输入、不调 heartbeat，观察 300ms——旧 idle 若残留会在此触发。
	select {
	case <-s.Done():
		t.Fatal("无输入且无心跳时会话不应关闭：idle 路径应已移除")
	case <-time.After(300 * time.Millisecond):
	}
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

	s := newLifetimeSession(t, reg, "s-lifetime", ch)

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
	reqs := ch.reqList()
	if len(reqs) == 0 || reqs[0] != "exit-status" {
		t.Fatalf("关闭前应回 exit-status，实际请求 %v", reqs)
	}
}

func TestNormalCloseSendsNoNotice(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-normal", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	ch := newRecordingChannel()
	s := newLifetimeSession(t, reg, "s-normal", ch)

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
