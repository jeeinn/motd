package main

// P0-19 交互编排回归：用真实 SQLite + 假 GitHub Device Flow 端点跑通
// D2 全链路（Start → Poll → Login → Bind → Upgrade），并覆盖 Esc 取消、
// 网络重试、失败关闭、R19 双事务边界。
//
// 说明：Serve 的异步 Poll goroutine 与这里的直接 f.device.Poll 等价，
// 差异只在节奏由谁持有；决策逻辑全在 loginFlow 里，故不需要起真实 SSH。

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

const (
	loginTestDeviceCode = "dc_PRIVATE_ONLY"
	loginTestToken      = "gho_TEST_SECRET_TOKEN"
)

// 43 字符 body 由 Repeat 生成，避免手写计数出错。
var loginTestFingerprint = "SHA256:" + strings.Repeat("C", 43)

type loginFakeClient struct {
	mu          sync.Mutex
	polls       int
	authorizeAt int // 第几次 Poll 返回令牌；0 = 永不
	networkErrs int // 前几次 Poll 返回非协议网络错误
	deviceCode  auth.DeviceCode
	token       string
}

func newLoginFakeClient() *loginFakeClient {
	return &loginFakeClient{
		authorizeAt: 2,
		token:       loginTestToken,
		deviceCode: auth.DeviceCode{
			DeviceCode:      loginTestDeviceCode,
			UserCode:        "ABCD-EFGH",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		},
	}
}

func (c *loginFakeClient) RequestDeviceCode(context.Context) (auth.DeviceCode, error) {
	return c.deviceCode, nil
}

func (c *loginFakeClient) PollAccessToken(_ context.Context, _ string) (auth.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
	if c.polls <= c.networkErrs {
		// 非 *PollError：状态机原样返回，ClassifyPoll 必须判成 Retry 而不是拒绝。
		return auth.AccessToken{}, errors.New("fake network down")
	}
	if c.authorizeAt > 0 && c.polls >= c.authorizeAt {
		return auth.AccessToken{Token: c.token, TokenType: "bearer"}, nil
	}
	return auth.AccessToken{}, &auth.PollError{
		Status: auth.PollPending, Message: "等待用户在浏览器完成授权",
	}
}

func (c *loginFakeClient) GetUser(_ context.Context, tok string) (auth.GitHubUser, error) {
	if tok == "" {
		return auth.GitHubUser{}, errors.New("空令牌")
	}
	return auth.GitHubUser{ID: 8801, Login: "alice"}, nil
}

func (c *loginFakeClient) GetEmails(_ context.Context, _ string) ([]auth.GitHubEmail, error) {
	return []auth.GitHubEmail{{Email: "alice@example.com", Primary: true, Verified: true}}, nil
}

type fakeLoginConn struct {
	id, fp, pk string
	principal  auth.Principal
	upgrades   []int64
	upgradeErr error
}

func newFakeLoginConn() *fakeLoginConn {
	return &fakeLoginConn{
		id:        "s-login-test",
		fp:        loginTestFingerprint,
		pk:        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample",
		principal: auth.Guest(loginTestFingerprint),
	}
}

func (c *fakeLoginConn) ID() string                { return c.id }
func (c *fakeLoginConn) Fingerprint() string       { return c.fp }
func (c *fakeLoginConn) PublicKey() string         { return c.pk }
func (c *fakeLoginConn) Principal() auth.Principal { return c.principal }
func (c *fakeLoginConn) UpgradeToUser(uid int64) error {
	if c.upgradeErr != nil {
		return c.upgradeErr
	}
	c.upgrades = append(c.upgrades, uid)
	c.principal = auth.Principal{Kind: auth.KindUser, UserID: uid, KeyFingerprint: c.fp}
	return nil
}

type loginNoopRevoker struct{}

func (loginNoopRevoker) RevokeSessions(context.Context, string) int { return 0 }

func setupLoginFlow(t *testing.T, client auth.DeviceFlowClient) (*loginFlow, *fakeLoginConn, *bytes.Buffer, *sqlite.Pools) {
	t.Helper()
	ctx := context.Background()
	pools, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "motd.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(writer.Stop)

	device := auth.NewDeviceFlowService(sqlite.NewDeviceAuthRepos(pools, writer), client)
	login := auth.NewLoginService(client, sqlite.NewLoginRepos(writer))
	keys := auth.NewKeyBindingService(sqlite.NewKeyBindingRepos(pools, writer), loginNoopRevoker{})
	return &loginFlow{device: device, login: login, keys: keys, now: time.Now},
		newFakeLoginConn(), &bytes.Buffer{}, pools
}

// pollOnce 模拟 Serve 的一次异步 Poll 结果回传。
func pollOnce(t *testing.T, f *loginFlow, conn *fakeLoginConn, out *bytes.Buffer) (bool, error) {
	t.Helper()
	r, err := f.device.Poll(context.Background(), f.pollCode())
	done, _, aerr := f.applyPoll(context.Background(), conn, out, r, err)
	return done, aerr
}

func countRows(t *testing.T, pools *sqlite.Pools, query string) int {
	t.Helper()
	var n int
	if err := pools.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("查询 %q: %v", query, err)
	}
	return n
}

func deviceStateStatus(t *testing.T, pools *sqlite.Pools) string {
	t.Helper()
	var s string
	if err := pools.QueryRowContext(context.Background(), `SELECT status FROM device_auth_states LIMIT 1`).Scan(&s); err != nil {
		t.Fatalf("读取设备码状态: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// D2：新用户 /login github 走完 Device Flow，会话升级且当前连接公钥自动绑定
// ---------------------------------------------------------------------------

func TestLoginFlowD2EndToEnd(t *testing.T) {
	client := newLoginFakeClient()
	f, conn, out, pools := setupLoginFlow(t, client)
	ctx := context.Background()

	delay, active, err := f.begin(ctx, conn, out)
	if err != nil || !active {
		t.Fatalf("begin 失败: active=%v err=%v", active, err)
	}
	if delay != 5*time.Second {
		t.Fatalf("首次轮询间隔应为 5s，实际 %v", delay)
	}
	if countRows(t, pools, `SELECT COUNT(*) FROM users`) != 0 {
		t.Fatal("发起阶段不得建号")
	}

	// 第一次 Poll：authorization_pending → 继续等待，不终止不建号。
	done, err := pollOnce(t, f, conn, out)
	if err != nil || done {
		t.Fatalf("pending 应继续，实际 done=%v err=%v", done, err)
	}
	if !f.active || conn.principal.IsUser() {
		t.Fatal("pending 阶段必须保持登录中且仍为访客")
	}
	if deviceStateStatus(t, pools) != string(auth.StatusPending) {
		t.Fatal("pending 阶段状态仍应是 pending")
	}

	// 第二次 Poll：授权成功 → Login → Bind → Upgrade 全链路。
	done, err = pollOnce(t, f, conn, out)
	if err != nil || !done {
		t.Fatalf("授权后应结束流程: done=%v err=%v", done, err)
	}
	if !conn.principal.IsUser() || conn.principal.UserID <= 0 {
		t.Fatal("D2 要求会话升级为登录态")
	}
	if len(conn.upgrades) != 1 {
		t.Fatalf("会话升级必须恰好 1 次，实际 %d", len(conn.upgrades))
	}
	if deviceStateStatus(t, pools) != string(auth.StatusAuthorized) {
		t.Fatal("设备码应进入 authorized 终态")
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM users`); n != 1 {
		t.Fatalf("应恰好建 1 个账号，实际 %d", n)
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM identities`); n != 1 {
		t.Fatalf("应恰好建 1 条身份绑定，实际 %d", n)
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM ssh_keys WHERE revoked_at IS NULL`); n != 1 {
		t.Fatalf("D2 要求当前连接公钥自动绑定，实际 %d 条", n)
	}

	text := out.String()
	for _, want := range []string{"ABCD-EFGH", "https://github.com/login/device", auth.LoginTextSuccessCreate, "alice", auth.LoginTextWriteReady} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q", want)
		}
	}
	// 令牌与 device_code 绝不能出现在终端输出里。
	if strings.Contains(text, loginTestDeviceCode) || strings.Contains(text, loginTestToken) {
		t.Fatalf("终端输出泄露了 device_code / access_token: %q", text)
	}
}

// ---------------------------------------------------------------------------
// Esc 取消：pending → denied，不留未绑定半成品
// ---------------------------------------------------------------------------

func TestLoginFlowCancelWritesDeniedAndNoHalfBoundState(t *testing.T) {
	f, conn, out, pools := setupLoginFlow(t, newLoginFakeClient())
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: active=%v err=%v", active, err)
	}
	f.cancel(ctx, out)

	if deviceStateStatus(t, pools) != string(auth.StatusDenied) {
		t.Fatal("Esc 取消后 device_auth_states 必须进入 denied")
	}
	if conn.principal.IsUser() || len(conn.upgrades) != 0 {
		t.Fatal("取消后会话必须保持访客态")
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM users`); n != 0 {
		t.Fatalf("取消不得建号，实际 %d", n)
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM ssh_keys`); n != 0 {
		t.Fatalf("取消不得留下公钥绑定，实际 %d", n)
	}
	if !strings.Contains(out.String(), auth.LoginTextCancelled) {
		t.Fatal("取消应输出固定文案")
	}
	// 取消后迟到的 Poll 结果不得复活登录态。
	if f.active {
		t.Fatal("取消后 flow 必须失活")
	}
}

// ---------------------------------------------------------------------------
// 网络错误：Retry 而不是误判为用户拒绝；到顶后失败关闭
// ---------------------------------------------------------------------------

func TestLoginFlowNetworkErrorRetriesWithoutDenying(t *testing.T) {
	client := newLoginFakeClient()
	client.networkErrs = 1
	client.authorizeAt = 3
	f, conn, out, pools := setupLoginFlow(t, client)
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: %v", err)
	}
	done, _ := pollOnce(t, f, conn, out)
	if done || !f.active {
		t.Fatal("一次网络抖动不得终止登录")
	}
	if deviceStateStatus(t, pools) != string(auth.StatusPending) {
		t.Fatal("网络错误不得改动设备码状态")
	}
	if !strings.Contains(out.String(), auth.LoginTextRetry) {
		t.Fatal("应输出固定重试文案")
	}
	if conn.principal.IsUser() {
		t.Fatal("重试阶段不得升级会话")
	}
}

func TestLoginFlowNetworkRetriesAreBounded(t *testing.T) {
	client := newLoginFakeClient()
	client.networkErrs = 99
	client.authorizeAt = 0
	f, conn, out, _ := setupLoginFlow(t, client)
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: %v", err)
	}
	done := false
	for i := 0; i <= auth.LoginMaxNetworkRetries+1; i++ {
		done, _ = pollOnce(t, f, conn, out)
		if done {
			break
		}
	}
	if !done || f.active {
		t.Fatal("连续网络错误到上限后必须终止，不能无限轮询")
	}
	if !strings.Contains(out.String(), auth.LoginTextNetworkGiveUp) {
		t.Fatal("应输出固定放弃文案")
	}
}

// ---------------------------------------------------------------------------
// 失败关闭与 R19 双事务边界
// ---------------------------------------------------------------------------

func TestLoginFlowRejectsUnsafeDeviceCodeResponse(t *testing.T) {
	client := newLoginFakeClient()
	client.deviceCode.UserCode = "ABCD-EFGH\x1b[2J"
	f, conn, out, pools := setupLoginFlow(t, client)
	_, active, err := f.begin(context.Background(), conn, out)
	if err == nil || active {
		t.Fatalf("不合法响应必须拒绝且不进入登录态: active=%v err=%v", active, err)
	}
	if !errors.Is(err, auth.ErrUnsafeLoginView) {
		t.Fatalf("错误类型不符: %v", err)
	}
	if deviceStateStatus(t, pools) != string(auth.StatusDenied) {
		t.Fatal("响应不合法时必须顺带作废旧码")
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("输出不得含原始 ESC: %q", out.String())
	}
}

func TestLoginFlowBindFailureKeepsGuestSession(t *testing.T) {
	client := newLoginFakeClient()
	f, conn, out, pools := setupLoginFlow(t, client)
	conn.fp = "not-a-fingerprint" // Bind 会因格式非法失败
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < client.authorizeAt-1; i++ {
		if done, _ := pollOnce(t, f, conn, out); done {
			t.Fatal("提前结束")
		}
	}
	done, _ := pollOnce(t, f, conn, out)
	if !done {
		t.Fatal("授权后应结束流程")
	}
	if conn.principal.IsUser() || len(conn.upgrades) != 0 {
		t.Fatal("R19：绑定失败绝不升级会话")
	}
	if !strings.Contains(out.String(), auth.LoginTextBindFailed) {
		t.Fatal("应输出固定绑定失败文案")
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM users`); n != 1 {
		t.Fatalf("账号事务已提交（R19 诚实边界），实际 %d 个账号", n)
	}
	if n := countRows(t, pools, `SELECT COUNT(*) FROM ssh_keys`); n != 0 {
		t.Fatalf("绑定失败不得落行，实际 %d", n)
	}
	if deviceStateStatus(t, pools) != string(auth.StatusAuthorized) {
		t.Fatal("授权成功即 authorized 终态，不是 denied")
	}
}

func TestLoginFlowUpgradeFailureDoesNotClaimSuccess(t *testing.T) {
	client := newLoginFakeClient()
	f, conn, out, _ := setupLoginFlow(t, client)
	conn.upgradeErr = errors.New("fake upgrade failure")
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < client.authorizeAt-1; i++ {
		_, _ = pollOnce(t, f, conn, out)
	}
	if done, _ := pollOnce(t, f, conn, out); !done {
		t.Fatal("应结束流程")
	}
	if conn.principal.IsUser() {
		t.Fatal("升级失败时会话必须保持访客态")
	}
	if !strings.Contains(out.String(), auth.LoginTextUpgradeFailed) {
		t.Fatal("应输出固定升级失败文案，不得谎报登录成功")
	}
	if strings.Contains(out.String(), auth.LoginTextWriteReady) {
		t.Fatal("升级失败不得输出已获得写权限")
	}
}

func TestLoginFlowLocalExpiryStopsPolling(t *testing.T) {
	client := newLoginFakeClient()
	client.authorizeAt = 0 // 永远 pending
	f, conn, out, pools := setupLoginFlow(t, client)
	ctx := context.Background()
	if _, active, err := f.begin(ctx, conn, out); err != nil || !active {
		t.Fatalf("begin: %v", err)
	}
	r, err := f.device.Poll(ctx, f.pollCode())
	if err != nil {
		t.Fatal(err)
	}
	// 把 flow 的时钟推到设备码过期之后，模拟会话循环迟到的 Continue。
	f.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	done, _, _ := f.applyPoll(ctx, conn, out, r, nil)
	if !done || f.active {
		t.Fatal("到点后必须终止，不得继续发网络请求")
	}
	if !strings.Contains(out.String(), auth.LoginTextExpired) {
		t.Fatal("应输出固定过期文案")
	}
	if deviceStateStatus(t, pools) != string(auth.StatusDenied) {
		t.Fatal("本地到点应作废旧码（Cancel → denied）")
	}
}

// ---------------------------------------------------------------------------
// 命令白名单与取消键
// ---------------------------------------------------------------------------

func TestLoginCommandProviderWhitelist(t *testing.T) {
	reg := command.Default()
	for _, line := range []string{"/login github", "/signin github"} {
		inv, err := reg.Parse(line)
		if err != nil || !isGitHubLogin(inv) {
			t.Errorf("%q 应被接受，实际 inv=%+v err=%v", line, inv, err)
		}
	}
	for _, line := range []string{"/login gitlab", "/login", "/login github extra", "/keys"} {
		inv, err := reg.Parse(line)
		if err == nil && isGitHubLogin(inv) {
			t.Errorf("%q 不应被判定为 github 登录", line)
		}
	}
}

func TestLoginCancelKeysAreEscAndCtrlCOnly(t *testing.T) {
	for _, k := range []input.Kind{input.KeyEsc, input.KeyCtrlC} {
		if !isLoginCancelKey(input.KeyMsg{Kind: k}) {
			t.Errorf("%v 应可取消登录", k)
		}
	}
	for _, k := range []input.Kind{input.KeyEnter, input.KeyRune, input.KeyCtrlD, input.KeyPaste, input.KeyArrowUp} {
		if isLoginCancelKey(input.KeyMsg{Kind: k, Text: "\x1b[2J"}) {
			t.Errorf("%v 不得取消登录（也不得回显）", k)
		}
	}
}

func TestAlreadyLoggedInSessionCannotStartAnotherLogin(t *testing.T) {
	client := newLoginFakeClient()
	f, conn, out, _ := setupLoginFlow(t, client)
	conn.principal = auth.Principal{Kind: auth.KindUser, UserID: 7}
	_, active, err := f.begin(context.Background(), conn, out)
	if err != nil || active {
		t.Fatalf("已登录会话不应再进入登录态: active=%v err=%v", active, err)
	}
	if !strings.Contains(out.String(), auth.LoginTextAlreadyUser) {
		t.Fatal("应输出固定已登录提示")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.polls != 0 {
		t.Fatal("已登录会话不得发起任何网络请求")
	}
}
