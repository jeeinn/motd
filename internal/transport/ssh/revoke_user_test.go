package ssh

import (
	"context"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// P1-M2：注销按 userID 撤销全部在线会话，且不得误伤其他账号 / 访客。

type userByFingerprintResolver map[string]int64

func (r userByFingerprintResolver) Resolve(_ context.Context, fp string) (auth.Principal, error) {
	if uid, ok := r[fp]; ok {
		return auth.Principal{Kind: auth.KindUser, UserID: uid, KeyFingerprint: fp}, nil
	}
	return auth.Guest(fp), nil
}

func TestRevokeUserSessionsByUserID(t *testing.T) {
	reg := presence.New()
	signerA := newClientSigner(t)
	signerB := newClientSigner(t)
	signerC := newClientSigner(t)
	fpA := gossh.FingerprintSHA256(signerA.PublicKey())
	fpB := gossh.FingerprintSHA256(signerB.PublicKey())
	fpC := gossh.FingerprintSHA256(signerC.PublicKey())

	s, addr := startServer(t, Config{
		Presence: reg,
		Resolver: userByFingerprintResolver{fpA: 100, fpB: 100, fpC: 200},
		Handler:  newScriptedHandler(),
	})

	c1 := dialShell(t, addr, signerA)
	defer c1.Close()
	c2 := dialShell(t, addr, signerB)
	defer c2.Close()
	c3 := dialShell(t, addr, signerC)
	defer c3.Close()
	waitUntil(t, func() bool { return reg.Online() == 3 })

	if n := s.RevokeUserSessions(context.Background(), 100); n != 2 {
		t.Fatalf("应撤销账号 100 的 2 个会话，实际 %d", n)
	}
	waitUntil(t, func() bool { return reg.Online() == 1 })

	// 边界：无效 userID / 不存在账号不得误伤剩余连接。
	if n := s.RevokeUserSessions(context.Background(), 0); n != 0 {
		t.Fatalf("userID=0 不得撤销任何会话，实际 %d", n)
	}
	if n := s.RevokeUserSessions(context.Background(), -1); n != 0 {
		t.Fatalf("负 userID 不得撤销任何会话，实际 %d", n)
	}
	if n := s.RevokeUserSessions(context.Background(), 999); n != 0 {
		t.Fatalf("未知账号不得撤销任何会话，实际 %d", n)
	}
	if reg.Online() != 1 {
		t.Fatalf("无关账号不得被影响，实际 online=%d", reg.Online())
	}
}

func TestSessionRemoteIPHashIsStableAndNeverPlaintext(t *testing.T) {
	s := newSession("s-ip", &reapStubChannel{}, "fp", "pk", auth.Guest("fp"),
		presence.New(), "203.0.113.7:54321")
	defer s.Close()
	h := s.RemoteIPHash()
	if h == "" || len(h) != 64 {
		t.Fatalf("IP 摘要应为 64 位十六进制，实际 %q", h)
	}
	if h != s.RemoteIPHash() {
		t.Fatal("同一对端 IP 的摘要必须稳定")
	}
	if h == "203.0.113.7:54321" {
		t.Fatal("绝不能把明文 IP 当哈希交出去")
	}
	// 无对端（单测 stub / 进程内连接）时返回空，调用方据此跳过限流。
	none := newSession("s-none", &reapStubChannel{}, "fp", "pk", auth.Guest("fp"),
		presence.New())
	defer none.Close()
	if got := none.RemoteIPHash(); got != "" {
		t.Fatalf("空对端应返回空摘要，实际 %q", got)
	}
}
