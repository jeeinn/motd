package main

// /verify 命令在收密码前做只读预检的回归（v0.3.1 体验项）。
// 进程内 fake 端口：不触网、不起 SSH、不碰生产 data/。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
)

func setupVerifyPrecheck(t *testing.T, opts ...auth.EmailServiceOption) (*auth.EmailAuthService, *cmdMemStore, string) {
	t.Helper()
	store := newCmdMemStore()
	code := "135790"
	all := append([]auth.EmailServiceOption{
		auth.WithEmailCodeFunc(func() (string, error) { return code, nil }),
	}, opts...)
	svc := auth.NewEmailAuthService(store, cmdMemSender{}, cmdMemRevoker{}, all...)
	return svc, store, code
}

func runVerifyCommand(t *testing.T, h *inputHandler, pc *pageController, conn *fakeEmailConn, line string) {
	t.Helper()
	inv, err := command.Default().Parse(line)
	if err != nil {
		t.Fatalf("解析 %q: %v", line, err)
	}
	h.handleEmailCommand(context.Background(), conn, pc, inv)
}

func precheckFixture(t *testing.T, svc *auth.EmailAuthService) (*pageController, *fakeEmailConn) {
	t.Helper()
	if _, err := svc.RequestCode(context.Background(), auth.EmailPurposeRegister, "alice@example.com", "iphash"); err != nil {
		t.Fatal(err)
	}
	conn := &fakeEmailConn{ipHash: "iphash"}
	// /register 会把 pendingEmail/pendingPurpose 留在 pageController 上跨命令存活；
	// 测试直接调 RequestCode，故这里手工补齐同一前置状态。
	pc := &pageController{
		echo:           newLineEcho(conn),
		pendingEmail:   "alice@example.com",
		pendingPurpose: auth.EmailPurposeRegister,
	}
	return pc, conn
}

// 无效码：直接固定文案，不进入密码交互（不提示设密码、不开掩码）。
func TestVerifyBadCodeStopsBeforePasswordPrompt(t *testing.T) {
	svc, store, _ := setupVerifyPrecheck(t)
	pc, conn := precheckFixture(t, svc)
	writesBefore := store.writes

	runVerifyCommand(t, &inputHandler{email: svc}, pc, conn, "/verify 000000")

	if pc.emailFlow != nil {
		t.Fatal("无效码不得挂上 verify 密码流程")
	}
	out := conn.String()
	if !strings.Contains(out, emailTextBadCode) {
		t.Fatalf("无效码应给固定文案: %q", out)
	}
	if strings.Contains(out, emailHintSetPwd) || strings.Contains(out, emailHintConfirmPwd) {
		t.Fatalf("无效码不得提示输密码: %q", out)
	}
	if pc.echo.secret {
		t.Fatal("无效码不得开启密码掩码")
	}
	if store.writes != writesBefore || store.reads != 1 {
		t.Fatalf("预检必须只读一次、零写库: writes=%d(+%d) reads=%d", writesBefore, store.writes-writesBefore, store.reads)
	}
	if store.tokens[0].consumed || store.tokens[0].attempts != 0 {
		t.Fatalf("预检不得消费 token 或计数: %+v", store.tokens[0])
	}
}

// 有效码：正常进入密码交互（首提示 + 掩码开启），且预检只读。
func TestVerifyGoodCodeEntersPasswordPrompt(t *testing.T) {
	svc, store, code := setupVerifyPrecheck(t)
	pc, conn := precheckFixture(t, svc)
	writesBefore := store.writes

	runVerifyCommand(t, &inputHandler{email: svc}, pc, conn, "/verify "+code)

	if pc.emailFlow == nil || pc.emailFlow.kind != emailFlowVerify {
		t.Fatalf("有效码应挂上 verify 流程，实际 %+v", pc.emailFlow)
	}
	if !strings.Contains(conn.String(), emailHintSetPwd) {
		t.Fatalf("有效码应提示设置密码: %q", conn.String())
	}
	if !pc.echo.secret {
		t.Fatal("密码步骤必须开启掩码")
	}
	if store.writes != writesBefore || store.reads != 1 {
		t.Fatalf("预检必须只读一次、零写库: writes=%d(+%d) reads=%d", writesBefore, store.writes-writesBefore, store.reads)
	}
}

// 过期 / 已消费 / 尝试用尽的码都不进入密码交互。
func TestVerifyStaleCodesStopBeforePasswordPrompt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*cmdMemStore)
	}{
		{"已过期", func(s *cmdMemStore) { s.tokens[0].expiresAt = time.Now().Add(-time.Minute) }},
		{"已消费", func(s *cmdMemStore) { s.tokens[0].consumed = true }},
		{"尝试用尽", func(s *cmdMemStore) { s.tokens[0].attempts = 5 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, store, code := setupVerifyPrecheck(t)
			pc, conn := precheckFixture(t, svc)
			c.mutate(store)

			runVerifyCommand(t, &inputHandler{email: svc}, pc, conn, "/verify "+code)

			if pc.emailFlow != nil {
				t.Fatalf("%s：不得进入密码交互", c.name)
			}
			out := conn.String()
			if !strings.Contains(out, emailTextBadCode) || strings.Contains(out, emailHintSetPwd) {
				t.Fatalf("%s：应只给固定错误文案: %q", c.name, out)
			}
		})
	}
}

// 预检限流命中也给固定文案（等待秒数），同样不进入密码交互——不留下爆破 oracle。
func TestVerifyPrecheckRateLimitStopsBeforePasswordPrompt(t *testing.T) {
	clock := time.Now()
	limiter := auth.NewRateLimiter(
		auth.WithRateLimitClock(func() time.Time { return clock }),
		auth.WithRateLimitConfig(auth.RateLimitConfig{
			MaxAttempts: 1, Window: time.Hour, Lockout: 15 * time.Minute, MaxKeys: 16,
		}),
	)
	svc, store, _ := setupVerifyPrecheck(t, auth.WithEmailPrecheckLimiter(limiter))
	pc, conn := precheckFixture(t, svc)

	h := &inputHandler{email: svc}
	runVerifyCommand(t, h, pc, conn, "/verify 000000") // 第一次错码：放行到预检，码无效
	if pc.emailFlow != nil {
		t.Fatal("第一次错码不应进入密码交互")
	}
	if !strings.Contains(conn.String(), emailTextBadCode) {
		t.Fatalf("第一次应给码无效文案: %q", conn.String())
	}
	conn.Reset()
	writesBefore := store.writes
	runVerifyCommand(t, h, pc, conn, "/verify 000000") // 第二次：命中预检限流
	out := conn.String()
	if pc.emailFlow != nil || !strings.Contains(out, "邮箱操作过于频繁") {
		t.Fatalf("第二次应被限流且不进入密码交互: %q", out)
	}
	if store.writes != writesBefore {
		t.Fatal("限流拒绝不得写库")
	}
}
