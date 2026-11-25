package main

// ADR-077 / v0.9.3：终态文案可见性回归。
//
// 本文件专门捕获一类"字节流里有文案、用户屏幕上看不到"的缺陷：
// email / GitHub 登录的终态文案先 append 到终端，随后主循环的 pc.refresh()
// 用 ESC[H+ESC[2J 整屏重绘把它擦掉（v0.9.0 ADR-072 引入，生产实测"回车无反应"）。
//
// 判据不是 strings.Contains（那只能证明"写出了"），而是：终态文案必须出现在
// **最后一次整屏清屏（2J）之后**的字节段里——因为 C1a 把文案纳入屏幕 notice，
// 它随整屏帧一起输出，天然位于 2J 之后，2J 擦不掉。
//
// 若退回 bug 实现（文案只 append、refresh 擦屏），最后一次 2J 必在文案之后，
// 本判据必然失败。这比完整 VT100 屏幕模型更轻，但对本缺陷同样严格。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

// waitQuiet 等待输出静止，确保 done → pc.refresh() 的整屏帧已完整落盘。
// 只 waitSince(want) 会在文案 append 后、refresh 之前抢跑，可能假绿。
func waitQuiet(t *testing.T, o *mvpOutput, quiet time.Duration) {
	t.Helper()
	deadline := time.Now().Add(e2eWaitTimeout)
	var last int
	stable := time.Now()
	for {
		n := len(o.text())
		if n != last {
			last, stable = n, time.Now()
		}
		if time.Since(stable) >= quiet {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待输出静止超时（最后长度 %d）", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertVisibleAfterSettle 断言：等输出静止后，want 出现在最后一次 2J 之后的字节段。
//
// 这是"用户可见"的判据：2J 会清掉当前屏，任何在 2J 之前写出的屏内文本都会被擦；
// C1a 之后终态文案随整屏帧输出，位于最后一个 2J 的同一帧内（ clearer 之后）。
func assertVisibleAfterSettle(t *testing.T, c *mvpClient, from int, want string) {
	t.Helper()
	c.out.waitSince(t, from, want)            // 先确认文案确实写出
	waitQuiet(t, c.out, 120*time.Millisecond) // 再等 refresh 整屏帧完成
	seg := c.out.text()[from:]
	lastClear := strings.LastIndex(seg, "\x1b[2J")
	if lastClear < 0 {
		// 没有整屏清屏：文案直接可见（例如无 refresh 的命令路径）。
		if !strings.Contains(seg, want) {
			t.Fatalf("文案不可见（无 2J 但仍缺失）: want=%q seg=%q", want, seg)
		}
		return
	}
	after := seg[lastClear+len("\x1b[2J"):]
	if !strings.Contains(after, want) {
		t.Fatalf("文案被整屏清屏擦除（写出但不在最后一次 2J 之后）: want=%q seg=%q", want, seg)
	}
}

// startVisibilityServer 装配一台带内存邮件服务的真实 SSH 服务器，
// 返回一个访客 client。新客户端公钥未写入 ssh_keys → 访客（登录/注册的前置态）。
func startVisibilityServer(t *testing.T) (*mvpClient, string) {
	t.Helper()
	_, h, _, pools, _ := setupMVPRuntime(t)
	svc, _, code := newConfirmTestSvc(t)
	h.withEmail(svc)

	srv, err := sshd.New(sshd.Config{
		Addr: "127.0.0.1:0", DataDir: t.TempDir(), MaxConns: 10,
		Resolver: auth.NewResolver(sqlite.NewSSHKeyLookup(pools)),
		Presence: h.presence, Handler: h,
	})
	if err != nil {
		t.Fatalf("启动 SSH 服务: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(serveCtx) }()
	t.Cleanup(func() { cancel(); srv.Close() })
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("SSH 监听未就绪")
	}
	c := dialMVP(t, addr, newMVPKey(t))
	t.Cleanup(c.close)
	return c, code
}

// TestMVPEmailLoginErrorTextVisibleAfterRender 是本次 bug 的直接回归：
// 错误密码 → "邮箱或密码错误。" 必须在 refresh 后仍可见（ADR-077）。
func TestMVPEmailLoginErrorTextVisibleAfterRender(t *testing.T) {
	c, _ := startVisibilityServer(t)

	m := c.out.mark()
	c.send(t, "/login email\r")
	c.out.waitSince(t, m, emailHintLoginEmail)

	m = c.out.mark()
	c.send(t, "nobody@example.com\r")
	c.out.waitSince(t, m, emailHintPassword)

	m = c.out.mark()
	c.send(t, "wrongpassword\r")
	assertVisibleAfterSettle(t, c, m, emailTextBadLogin)
}

// TestMVPEmailVerifySuccessTextVisibleAfterRender 覆盖成功路径：
// /register → /verify 设密码两次 → "注册完成..." 必须在 refresh 后仍可见。
// 与失败路径一起，验证 NoticeSuccess/NoticeError 两类终态都能穿过整屏重绘。
func TestMVPEmailVerifySuccessTextVisibleAfterRender(t *testing.T) {
	c, code := startVisibilityServer(t)

	m := c.out.mark()
	c.send(t, "/register\r")
	c.out.waitSince(t, m, emailHintRegister)

	m = c.out.mark()
	c.send(t, "alice@example.com\r")
	assertVisibleAfterSettle(t, c, m, emailTextRegisterSent)

	m = c.out.mark()
	c.send(t, "/verify "+code+"\r")
	c.out.waitSince(t, m, emailHintSetPwd)

	m = c.out.mark()
	c.send(t, "correct-horse-9\r")
	c.out.waitSince(t, m, emailHintConfirmPwd)

	m = c.out.mark()
	c.send(t, "correct-horse-9\r")
	assertVisibleAfterSettle(t, c, m, emailTextRegistered)
}
