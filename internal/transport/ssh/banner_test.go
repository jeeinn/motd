package ssh

// SSH 登录引导 banner 的回归测试：
//   - 非满员也要有引导（root / 公钥 / 访客）；
//   - 满员时引导 + 满员提示同时出现；
//   - 固定文本，不含任何用户数据 / 凭据，CRLF 收口且控制行数。

import (
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestBannerCallbackAlwaysGuidesLogin(t *testing.T) {
	s := &Server{slots: make(chan struct{}, 2)}
	got := s.bannerCallback(fakeConnMeta{user: "alice"})
	if got == "" {
		t.Fatal("banner 不得为空：被拒用户需要看到登录引导")
	}
	for _, kw := range []string{"root", "公钥", "访客", "register"} {
		if !strings.Contains(got, kw) {
			t.Errorf("banner 缺少关键引导词 %q: %q", kw, got)
		}
	}
	if !strings.HasSuffix(got, "\r\n") {
		t.Fatalf("banner 必须以 CRLF 收口: %q", got)
	}
	lines := strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n")
	if len(lines) < 2 || len(lines) > 4 {
		t.Fatalf("banner 应控制在 2-4 行，实际 %d 行: %q", len(lines), got)
	}
	for _, line := range lines {
		if strings.Contains(line, "\n") {
			t.Fatalf("banner 不得含裸 LF: %q", line)
		}
	}
}

func TestBannerCallbackWhenFullIncludesBoth(t *testing.T) {
	s := &Server{slots: make(chan struct{}, 1)}
	s.slots <- struct{}{} // 占满，模拟 rejectFull 之外的握手期满员
	if !s.full() {
		t.Fatal("前置条件：连接数应已满")
	}
	got := s.bannerCallback(fakeConnMeta{user: "root"})
	if !strings.Contains(got, "连接数已满") {
		t.Fatalf("满员时须叠加满员提示: %q", got)
	}
	for _, kw := range []string{"root", "公钥", "访客"} {
		if !strings.Contains(got, kw) {
			t.Errorf("满员时仍须带登录引导词 %q: %q", kw, got)
		}
	}
	lines := strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n")
	if len(lines) < 3 || len(lines) > 4 {
		t.Fatalf("满员 banner 应为 3-4 行，实际 %d 行: %q", len(lines), got)
	}
}

// 端到端：root 用户被拒时，客户端也必须真的收到引导 banner——
// 这正是用户实测 "Permission denied (publickey)" 看不到任何提示的场景。
func TestRootLoginReceivesGuidanceBannerOverWire(t *testing.T) {
	_, addr := startServer(t, Config{})
	var banner string
	cfg := clientConfig(t)
	cfg.User = "root"
	cfg.BannerCallback = func(msg string) error {
		banner = msg
		return nil
	}
	cl, err := gossh.Dial("tcp", addr, cfg)
	if err == nil {
		_ = cl.Close()
		t.Fatal("root 登录必须被拒")
	}
	if banner == "" {
		t.Fatal("被拒的 root 客户端应收到登录引导 banner")
	}
	for _, kw := range []string{"root", "公钥", "访客"} {
		if !strings.Contains(banner, kw) {
			t.Errorf("线上 banner 缺少引导词 %q: %q", kw, banner)
		}
	}
}

func TestBannerCallbackIsFixedNoCredentials(t *testing.T) {
	s := &Server{slots: make(chan struct{}, 2)}
	// banner 对任何 ConnMetadata 都必须返回完全相同的固定文本——
	// 一旦输出随用户名变化，就说明把用户数据写进了 banner（ADR-019 禁止）。
	a := s.bannerCallback(fakeConnMeta{user: "SENSITIVE_USER_XYZ"})
	b := s.bannerCallback(fakeConnMeta{user: "root"})
	c := s.bannerCallback(fakeConnMeta{user: ""})
	if a != b || b != c {
		t.Fatalf("banner 必须固定，不随用户名变化:\n%q\n%q\n%q", a, b, c)
	}
	for _, leak := range []string{
		"SENSITIVE_USER_XYZ", "PRIVATE KEY", "BEGIN ", "SHA256:",
		"@", "/home/", "password=", "secret", "AUTH_",
	} {
		if strings.Contains(strings.ToLower(a), strings.ToLower(leak)) {
			t.Errorf("banner 不得含凭据 / 用户数据 %q: %q", leak, a)
		}
	}
}
