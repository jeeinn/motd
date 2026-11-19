package main

// MVP 端到端冒烟（P0-26）：真实 SSH 客户端 + 真实 SQLite + 真实身份解析。
//
// 与 P0-19 的假 GitHub 端点不同，本测试不碰 Device Flow：登录态通过
// **真实 ssh_keys 绑定**获得 —— 预先把两把客户端公钥写进 ssh_keys，
// 再由 auth.NewResolver(sqlite.NewSSHKeyLookup) 在认证阶段解析指纹。
// 这正是 D3「断开重连免登录、直接具备写权限」的真实链路。

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type mvpKey struct {
	signer gossh.Signer
	fp     string
	pk     string
}

func newMVPKey(t *testing.T) mvpKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := signer.PublicKey()
	return mvpKey{
		signer: signer,
		fp:     gossh.FingerprintSHA256(pub),
		pk:     strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pub))),
	}
}

// mvpOutput 持续读 SSH stdout；waitSince 只检查某个标记之后的新输出，
// 避免累积文本让「已经出现过一次」的字符串造成假绿。
type mvpOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *mvpOutput) start(r io.Reader) {
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				o.mu.Lock()
				_, _ = o.buf.Write(b[:n])
				o.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
}

func (o *mvpOutput) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *mvpOutput) mark() int { return len(o.text()) }

func (o *mvpOutput) waitSince(t *testing.T, from int, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		all := o.text()
		if from <= len(all) && strings.Contains(all[from:], want) {
			return
		}
		if time.Now().After(deadline) {
			tail := all
			if from < len(all) {
				tail = all[from:]
			}
			t.Fatalf("等待输出 %q 超时；本步已收到: %q", want, tail)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type mvpClient struct {
	cl   *gossh.Client
	sess *gossh.Session
	in   io.WriteCloser
	out  *mvpOutput
}

func (c *mvpClient) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(c.in, s); err != nil {
		t.Fatalf("发送按键 %q 失败: %v", s, err)
	}
}

func (c *mvpClient) close() {
	_ = c.sess.Close()
	_ = c.cl.Close()
}

func dialMVP(t *testing.T, addr string, key mvpKey) *mvpClient {
	t.Helper()
	cl, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            "tester",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(key.signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("SSH 拨号: %v", err)
	}
	sess, err := cl.NewSession()
	if err != nil {
		_ = cl.Close()
		t.Fatalf("NewSession: %v", err)
	}
	outPipe, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestPty("xterm", 24, 100, gossh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}
	o := &mvpOutput{}
	o.start(outPipe)
	return &mvpClient{cl: cl, sess: sess, in: in, out: o}
}

// D1 / D3 / D4：陌生客户端进入版块列表 → 发帖 → 回复 → 另一账号可见 →
// 断开重连不再要求登录且直接具备写权限。
func TestMVPSSHClosedLoop(t *testing.T) {
	ctx, h, rt, pools, writer := setupMVPRuntime(t)
	k1, k2 := newMVPKey(t), newMVPKey(t)

	// 真实绑定：把两把公钥分别绑到 alice(1) / bob(2)。
	now := time.Now().Unix()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for i, k := range []mvpKey{k1, k2} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at) VALUES (?, ?, ?, ?)`,
				int64(i+1), k.fp, k.pk, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("预置公钥绑定: %v", err)
	}

	srv, err := sshd.New(sshd.Config{
		Addr:        "127.0.0.1:0",
		DataDir:     t.TempDir(),
		MaxConns:    10,
		IdleTimeout: 30 * time.Second,
		Resolver:    auth.NewResolver(sqlite.NewSSHKeyLookup(pools)),
		Presence:    rt.Presence,
		Handler:     h,
	})
	if err != nil {
		t.Fatalf("启动 SSH 服务: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(serveCtx) }()
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("SSH 监听未就绪")
	}
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})

	// ---- 会话 A：D1 浏览 → 发帖 → 回复 ----
	a := dialMVP(t, addr, k1)
	defer a.close()

	mark := a.out.mark()
	a.out.waitSince(t, mark, "/dev/ai") // D1：访客/已登录首屏都能看到版块列表

	mark = a.out.mark()
	a.send(t, "\r")
	a.out.waitSince(t, mark, "主题列表")

	mark = a.out.mark()
	a.send(t, "/new\r")
	a.out.waitSince(t, mark, "新建主题")

	mark = a.out.mark()
	a.send(t, "MVP 真实 SSH 发帖\r")
	// ADR-026：标题输入必须回显，且这一步的新增输出里只有回显含该文本。
	a.out.waitSince(t, mark, "MVP 真实 SSH 发帖")
	a.out.waitSince(t, mark, "标题已接收")

	mark = a.out.mark()
	// P1-M4：正文 Enter 只换行，Ctrl-D（0x04）才提交。
	a.send(t, "来自真实客户端的首帖\x04")
	a.out.waitSince(t, mark, "来自真实客户端的首帖")
	a.out.waitSince(t, mark, "MVP 真实 SSH 发帖")

	mark = a.out.mark()
	a.send(t, "\r")
	a.out.waitSince(t, mark, "来自真实客户端的首帖")

	page, err := rt.Service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Title != "MVP 真实 SSH 发帖" {
		t.Fatalf("真实 SSH 发帖未落库: rows=%d err=%v", len(page.Rows), err)
	}

	// ---- 会话 B：另一把已绑定公钥（另一个账号）在他人主题下回复 ----
	// 刻意由 B 回复而不是 A：Q6 规定每人 60 秒 1 帖，A 刚发过主题，
	// 连续 /reply 会被正确限流。换账号既符合 D4「在他人主题下回复」，
	// 也顺带验证了限流在真实链路上确实生效。
	b := dialMVP(t, addr, k2)
	defer b.close()
	mark = b.out.mark()
	b.out.waitSince(t, mark, "/dev/ai")
	mark = b.out.mark()
	b.send(t, "\r")
	b.out.waitSince(t, mark, "MVP 真实 SSH 发帖")
	mark = b.out.mark()
	b.send(t, "\r")
	b.out.waitSince(t, mark, "来自真实客户端的首帖")

	mark = b.out.mark()
	b.send(t, "/reply\r")
	b.out.waitSince(t, mark, "回复：")
	mark = b.out.mark()
	b.send(t, "第二个账号能看到\x04")
	b.out.waitSince(t, mark, "第二个账号能看到")

	detail, err := rt.Service.GetThread(ctx, page.Rows[0].ID)
	if err != nil || len(detail.Posts) != 2 || detail.Posts[1].BodyMD != "第二个账号能看到" {
		t.Fatalf("真实 SSH 回复未落库: posts=%d err=%v", len(detail.Posts), err)
	}

	// ---- A 重新进入详情：他人回复可见（D4 的「另一个账号可见」）----
	mark = a.out.mark()
	a.send(t, "q")
	a.out.waitSince(t, mark, "主题列表")
	mark = a.out.mark()
	a.send(t, "\r")
	a.out.waitSince(t, mark, "第二个账号能看到")

	// ---- D3：断开重连，指纹命中 ssh_keys，直接具备写权限 ----
	a.send(t, "/quit\r")
	re := dialMVP(t, addr, k1)
	defer re.close()
	mark = re.out.mark()
	re.out.waitSince(t, mark, "已登录")
	mark = re.out.mark()
	re.send(t, "\r")
	re.out.waitSince(t, mark, "主题列表")
	mark = re.out.mark()
	re.send(t, "/new\r")
	re.out.waitSince(t, mark, "新建主题") // 未再要求 /login，直接可写
}

// 本机 PowerShell + OpenSSH 实测反馈回归：按 / 后只输入 login（不补第二个 /）
// 必须给出「仅支持 /login github」，而不是「未知命令」或「参数个数不符」。
// 同时复现用户看到的现象：多打一个 / 变成 //login 确实是未知命令。
func TestMVPLoginCommandHintOnRealSSH(t *testing.T) {
	_, h, rt, pools, _ := setupMVPRuntime(t)
	srv, err := sshd.New(sshd.Config{
		Addr:        "127.0.0.1:0",
		DataDir:     t.TempDir(),
		MaxConns:    10,
		IdleTimeout: 30 * time.Second,
		Resolver:    auth.NewResolver(sqlite.NewSSHKeyLookup(pools)),
		Presence:    rt.Presence,
		Handler:     h,
	})
	if err != nil {
		t.Fatalf("启动 SSH 服务: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(serveCtx) }()
	addr := srv.Addr()
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	if addr == "" {
		t.Fatal("SSH 监听未就绪")
	}

	c := dialMVP(t, addr, newMVPKey(t))
	defer c.close()
	mark := c.out.mark()
	c.out.waitSince(t, mark, "/dev/ai")

	mark = c.out.mark()
	c.send(t, "/login\r")
	c.out.waitSince(t, mark, auth.LoginTextUnsupportedProvider)
	// 命令态提示必须说明 / 已自动填入。
	c.out.waitSince(t, mark, "无需再打一次")

	mark = c.out.mark()
	c.send(t, "//login\r")
	// ADR-026：正常输入必须回显（净化后），随后才是命令解析结果。
	c.out.waitSince(t, mark, "//login")
	c.out.waitSince(t, mark, "未知命令")
}

// ADR-027：真实 SSH 客户端连按 j/k 时应原地重绘（CUU + EL），
// 不再每次追加一整屏；且清屏 / OSC / 备用屏等禁止序列绝不出现。
func TestMVPNavigationRedrawsInPlace(t *testing.T) {
	_, h, rt, pools, _ := setupMVPRuntime(t)
	srv, err := sshd.New(sshd.Config{
		Addr:        "127.0.0.1:0",
		DataDir:     t.TempDir(),
		MaxConns:    10,
		IdleTimeout: 30 * time.Second,
		Resolver:    auth.NewResolver(sqlite.NewSSHKeyLookup(pools)),
		Presence:    rt.Presence,
		Handler:     h,
	})
	if err != nil {
		t.Fatalf("启动 SSH 服务: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(serveCtx) }()
	addr := srv.Addr()
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})
	if addr == "" {
		t.Fatal("SSH 监听未就绪")
	}

	c := dialMVP(t, addr, newMVPKey(t))
	defer c.close()
	mark := c.out.mark()
	c.out.waitSince(t, mark, "/dev/ai")

	mark = c.out.mark()
	c.send(t, "jjj") // 焦点 1 → 4
	c.out.waitSince(t, mark, ">  4")
	segment := c.out.text()[mark:]
	if !regexp.MustCompile(`\x1b\[\d+A`).MatchString(segment) {
		t.Fatalf("连续导航应原地重绘（CUU 上移），实际 %q", segment)
	}
	if !strings.Contains(segment, "\x1b[K") {
		t.Fatalf("重绘应逐行 EL 清行，实际 %q", segment)
	}
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x1b]0;", "\x1b[?1049h", "\x1b[H"} {
		if strings.Contains(segment, bad) {
			t.Fatalf("重绘泄露禁止序列 %q: %q", bad, segment)
		}
	}
}
