package ssh

import (
	"context"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	gossh "golang.org/x/crypto/ssh"
)

// P0-18：认证阶段必须把服务端持有的 ssh.PublicKey 序列化文本带进会话，
// 且解绑后能按指纹撤销所有相关在线会话。

func TestPublicKeyCallbackEmbedsAuthorizedKey(t *testing.T) {
	s, err := New(Config{
		DataDir: t.TempDir(), Addr: "127.0.0.1:0",
		Resolver: testResolver{}, Presence: presence.New(), Handler: newScriptedHandler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	key := newClientSigner(t).PublicKey()
	perm, err := s.publicKeyCallback(fakeConnMeta{user: "tester"}, key)
	if err != nil {
		t.Fatalf("公钥挑战应通过: %v", err)
	}
	pk := perm.Extensions["public_key"]
	if pk == "" {
		t.Fatal("Permissions 必须带上服务端序列化的 public_key，供 P0-18 落库")
	}
	if err := auth.ValidatePublicKeyText(pk); err != nil {
		t.Fatalf("public_key 必须通过 application 层校验: %v", err)
	}
	parsed, _, _, _, err := gossh.ParseAuthorizedKey([]byte(pk))
	if err != nil {
		t.Fatalf("public_key 必须是可解析的 authorized key: %v", err)
	}
	if gossh.FingerprintSHA256(parsed) != perm.Extensions["fingerprint"] {
		t.Fatal("public_key 与 fingerprint 必须来自同一把服务端公钥")
	}
}

func TestPublicKeyFromPermissions(t *testing.T) {
	if publicKeyFromPermissions(nil) != "" {
		t.Fatal("空 Permissions 应回落为空公钥文本")
	}
	perm := &gossh.Permissions{Extensions: map[string]string{
		"fingerprint": "SHA256:x", "public_key": "ssh-ed25519 AAAA",
	}}
	if publicKeyFromPermissions(perm) != "ssh-ed25519 AAAA" {
		t.Fatal("public_key 未被取出")
	}
	// principalFromPermissions 的双返回值签名保持不变（既有契约测试零改动）。
	p, fp := principalFromPermissions(perm)
	if fp != "SHA256:x" || p.IsUser() {
		t.Fatalf("principal 解析被意外改变: %+v fp=%q", p, fp)
	}
}

func dialShell(t *testing.T, addr string, signer gossh.Signer) *gossh.Client {
	t.Helper()
	cl, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("SSH 拨号: %v", err)
	}
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
	_ = readLine(t, out) // 等 handler 起来并完成会话登记
	return cl
}

func TestRevokeSessionsByFingerprint(t *testing.T) {
	reg := presence.New()
	s, addr := startServer(t, Config{
		Presence: reg, Handler: newScriptedHandler(),
	})

	signerA := newClientSigner(t)
	signerB := newClientSigner(t)
	fpA := gossh.FingerprintSHA256(signerA.PublicKey())

	c1 := dialShell(t, addr, signerA)
	defer c1.Close()
	c2 := dialShell(t, addr, signerA)
	defer c2.Close()
	c3 := dialShell(t, addr, signerB)
	defer c3.Close()
	waitUntil(t, func() bool { return reg.Online() == 3 })

	if n := s.RevokeSessions(context.Background(), fpA); n != 2 {
		t.Fatalf("应撤销 2 个使用同指纹的会话，实际 %d", n)
	}
	waitUntil(t, func() bool { return reg.Online() == 1 })

	// 空指纹 / 不存在的指纹不得误伤剩余会话。
	if n := s.RevokeSessions(context.Background(), ""); n != 0 {
		t.Fatalf("空指纹不得撤销任何会话，实际 %d", n)
	}
	if n := s.RevokeSessions(context.Background(), "SHA256:unknown"); n != 0 {
		t.Fatalf("未登记指纹不得撤销任何会话，实际 %d", n)
	}
	if reg.Online() != 1 {
		t.Fatalf("无关指纹不得影响剩余连接，实际 online=%d", reg.Online())
	}
}
