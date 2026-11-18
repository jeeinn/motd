package ssh

// P0-19：Session.UpgradeToUser 是 guest → user 的唯一升级入口。
// 用同包未导出的 newSession 构造真实 Session，ch 用最小 stub 顶替。

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

type upgradeStubChannel struct{}

var _ gossh.Channel = upgradeStubChannel{}

func (upgradeStubChannel) Read([]byte) (int, error)                       { return 0, io.EOF }
func (upgradeStubChannel) Write(p []byte) (int, error)                    { return len(p), nil }
func (upgradeStubChannel) Close() error                                   { return nil }
func (upgradeStubChannel) CloseWrite() error                              { return nil }
func (upgradeStubChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (upgradeStubChannel) Stderr() io.ReadWriter                          { return nil }

// 用 Repeat 生成 43 字符 body，避免手写计数出错（P0-14 曾踩过 42 位的坑）。
var upgradeTestFP = "SHA256:" + strings.Repeat("B", 43)

func newUpgradeSession(t *testing.T, reg *presence.Registry, id string) *Session {
	t.Helper()
	s := newSession(id, upgradeStubChannel{}, upgradeTestFP, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
		auth.Guest(upgradeTestFP), reg, time.Hour)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpgradeToUserSucceedsOnce(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-1", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newUpgradeSession(t, reg, "s-1")
	if s.Principal().IsUser() {
		t.Fatal("初始必须是访客")
	}
	if err := s.UpgradeToUser(42); err != nil {
		t.Fatalf("访客升级应成功: %v", err)
	}
	p := s.Principal()
	if !p.IsUser() || p.UserID != 42 || p.KeyFingerprint != upgradeTestFP {
		t.Fatalf("升级后的身份快照错误: %+v", p)
	}
	ps, ok := reg.Get("s-1")
	if !ok || ps.UserID != 42 {
		t.Fatalf("presence 未同步 UserID: %+v", ps)
	}
}

func TestUpgradeToUserRejectsRepeatAndLateralMove(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-1", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newUpgradeSession(t, reg, "s-1")
	if err := s.UpgradeToUser(42); err != nil {
		t.Fatal(err)
	}
	// 重复升级、横向改成另一个账号都必须拒绝，且身份快照不变。
	for _, uid := range []int64{42, 99} {
		if err := s.UpgradeToUser(uid); !errors.Is(err, auth.ErrSessionUpgradeRejected) {
			t.Errorf("二次升级 %d 应被拒绝，实际 %v", uid, err)
		}
	}
	if s.Principal().UserID != 42 {
		t.Fatalf("被拒绝的升级不得改动身份，实际 %d", s.Principal().UserID)
	}
}

func TestUpgradeToUserRejectsInvalidUserID(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-1", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newUpgradeSession(t, reg, "s-1")
	for _, uid := range []int64{0, -1} {
		if err := s.UpgradeToUser(uid); !errors.Is(err, auth.ErrSessionUpgradeRejected) {
			t.Errorf("非法 UserID %d 应被拒绝，实际 %v", uid, err)
		}
	}
	if s.Principal().IsUser() {
		t.Fatal("失败的升级不得留下已登录态")
	}
}

func TestUpgradeToUserFailsClosedWhenPresenceUnknown(t *testing.T) {
	reg := presence.New() // 故意不 Connect
	s := newUpgradeSession(t, reg, "s-missing")
	if err := s.UpgradeToUser(42); err == nil {
		t.Fatal("presence 未登记时升级必须失败")
	}
	if s.Principal().IsUser() {
		t.Fatal("presence 绑定失败不得先写 principal")
	}
}

func TestUpgradeToUserConcurrentOnlyOneWins(t *testing.T) {
	reg := presence.New()
	if err := reg.Connect("s-1", presence.KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	s := newUpgradeSession(t, reg, "s-1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			if err := s.UpgradeToUser(uid); err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(int64(i + 1))
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("并发升级应恰好 1 次成功，实际 %d", success)
	}
	if !s.Principal().IsUser() {
		t.Fatal("并发后必须处于已登录态")
	}
}

func TestUpgradeTestFingerprintIsValid(t *testing.T) {
	if !auth.ValidFingerprint(upgradeTestFP) {
		t.Fatalf("测试指纹格式不合法（长度 %d）", len(strings.TrimPrefix(upgradeTestFP, "SHA256:")))
	}
}
