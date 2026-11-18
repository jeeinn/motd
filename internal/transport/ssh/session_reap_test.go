package ssh

// 本机实测反馈：用户停顿超过 20s 后，main.go 的 Reap 会把连接从注册表清掉，
// 但 SSH 连接本身没断，随后按 j/k/Enter 仍会持续收到字节。改前 Touch 失败被
// 静默忽略，导致状态栏与 /healthz 的 online/active 永久归零，且 SetState 全失败。

import (
	"io"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

type reapStubChannel struct{ served bool }

var _ gossh.Channel = (*reapStubChannel)(nil)

func (c *reapStubChannel) Read(p []byte) (int, error) {
	if c.served {
		return 0, io.EOF
	}
	c.served = true
	return copy(p, []byte("j")), nil
}
func (*reapStubChannel) Write(p []byte) (int, error)                    { return len(p), nil }
func (*reapStubChannel) Close() error                                   { return nil }
func (*reapStubChannel) CloseWrite() error                              { return nil }
func (*reapStubChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (*reapStubChannel) Stderr() io.ReadWriter                          { return nil }

func newReapSession(t *testing.T, reg *presence.Registry, id string, ch gossh.Channel) *Session {
	t.Helper()
	s := newSession(id, ch, upgradeTestFP, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
		auth.Guest(upgradeTestFP), reg, time.Hour)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionReadReRegistersAfterReap(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-reap", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newReapSession(t, reg, "s-reap", &reapStubChannel{})

	// 等价于 Reap()：把这条仍活着的连接移出注册表。
	if !reg.Disconnect("s-reap") {
		t.Fatal("测试前置：会话应存在")
	}
	if reg.Online() != 0 {
		t.Fatal("测试前置：注册表应已清空")
	}

	buf := make([]byte, 8)
	n, err := s.Read(buf)
	if err != nil || n != 1 {
		t.Fatalf("读输入失败: n=%d err=%v", n, err)
	}
	if got := reg.Online(); got != 1 {
		t.Fatalf("连接仍活时收到输入应重新登记在线会话，实际 online=%d", got)
	}
	if got := reg.Active(); got != 1 {
		t.Fatalf("重新登记后应回到 Browsing 并计入活跃，实际 active=%d", got)
	}
	snap, ok := reg.Get("s-reap")
	if !ok || snap.Kind != presence.KindSSH || snap.State != presence.StateBrowsing {
		t.Fatalf("重新登记的会话属性不对: %+v ok=%v", snap, ok)
	}
}

// 本机实测复现：Device Flow 等待授权 > 20s 且期间用户无输入。
// 修复方式是连接级心跳保住注册表条目，而不是放宽 UpgradeToUser 的失败关闭。
func TestHeartbeatKeepsLongOAuthWaitUpgradable(t *testing.T) {
	now := time.Now()
	reg := presence.New(presence.WithClock(func() time.Time { return now }))
	if err := reg.Connect("s-oauth", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newSession("s-oauth", &reapStubChannel{}, upgradeTestFP,
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
		auth.Guest(upgradeTestFP), reg, time.Hour)
	t.Cleanup(func() { _ = s.Close() })

	// 用户打开浏览器输验证码的 30 秒里，没有任何 Read，只有连接级心跳。
	now = now.Add(30 * time.Second)
	s.heartbeat()

	if reaped := reg.Reap(); len(reaped) != 0 {
		t.Fatalf("活连接不应被 Reap 清掉: %v", reaped)
	}
	if err := s.UpgradeToUser(42); err != nil {
		t.Fatalf("长时间等待授权后升级应成功，实际: %v", err)
	}
	if !s.Principal().IsUser() || s.Principal().UserID != 42 {
		t.Fatalf("会话未升级为登录态: %+v", s.Principal())
	}
	snap, ok := reg.Get("s-oauth")
	if !ok || snap.UserID != 42 {
		t.Fatalf("注册表状态不对: %+v ok=%v", snap, ok)
	}
	if err := s.UpgradeToUser(99); err == nil {
		t.Fatal("仍不得二次升级 / 横向改账号")
	}
}

func TestClosedSessionIsStillReaped(t *testing.T) {
	now := time.Now()
	reg := presence.New(presence.WithClock(func() time.Time { return now }))
	if err := reg.Connect("s-dead", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	// 没有心跳、连接已断：超过 20s 后 Reap 必须仍能清理僵尸条目。
	now = now.Add(30 * time.Second)
	if reaped := reg.Reap(); len(reaped) != 1 || reaped[0] != "s-dead" {
		t.Fatalf("僵尸条目应被清理，实际 %v", reaped)
	}
}

func TestSessionReadWithoutDataDoesNotInventSession(t *testing.T) {
	reg := presence.New()
	s := newReapSession(t, reg, "s-idle", &upgradeStubChannel{}) // Read 立刻返回 EOF

	buf := make([]byte, 8)
	if _, err := s.Read(buf); err != io.EOF {
		t.Fatalf("期望 EOF，实际 %v", err)
	}
	if got := reg.Online(); got != 0 {
		t.Fatalf("没有输入就不得凭空登记会话，实际 online=%d", got)
	}
}
