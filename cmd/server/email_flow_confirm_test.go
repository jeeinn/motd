package main

// 邮箱流程的回归测试：
//  1. secret 掩码在所有流程出口（成功 / 失败 / 取消 / 被新命令取代）复位；
//  2. verify 密码两次确认：一致提交、不一致回退重输且验证码不被消费；
//  3. /login email 仍只需一次密码。
// 全部在进程内 fake 端口跑，不触网、不碰 SQLite、不碰生产 data/。

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
)

// ---- 进程内邮箱端口 fake（cmd 层专用，实现 auth 的端口接口）----

type cmdMemToken struct {
	id        int64
	email     string
	codeHash  string
	purpose   string
	expiresAt time.Time
	attempts  int
	consumed  bool
}

type cmdMemStore struct {
	tokens     []*cmdMemToken
	identities map[string]int64
	creds      map[int64]string
	nextID     int64
	committed  bool
}

func newCmdMemStore() *cmdMemStore {
	return &cmdMemStore{identities: map[string]int64{}, creds: map[int64]string{}, nextID: 100}
}

func (s *cmdMemStore) Write(_ context.Context, fn func(auth.EmailWriteTx) error) error {
	s.committed = false
	if err := fn(&cmdMemTx{s: s}); err != nil {
		return err
	}
	s.committed = true
	return nil
}

func (s *cmdMemStore) FindCredentialByEmail(_ context.Context, email string) (auth.CredentialView, error) {
	uid, ok := s.identities[email]
	if !ok {
		return auth.CredentialView{}, auth.ErrLoginNotFound
	}
	return auth.CredentialView{UserID: uid, Username: "seeduser", PasswordHash: s.creds[uid]}, nil
}

type cmdMemTx struct{ s *cmdMemStore }

func (t *cmdMemTx) UsernameExists(context.Context, string) (bool, error) { return false, nil }
func (t *cmdMemTx) InsertUser(_ context.Context, a auth.NewAccount) (int64, error) {
	t.s.nextID++
	id := t.s.nextID
	if a.PrimaryEmail != "" {
		t.s.identities[a.PrimaryEmail] = id
	}
	return id, nil
}
func (t *cmdMemTx) InsertIdentity(_ context.Context, i auth.NewIdentity) error {
	t.s.identities[i.ProviderUID] = i.UserID
	return nil
}
func (t *cmdMemTx) FindAccountByEmail(context.Context, string) (auth.AccountView, error) {
	return auth.AccountView{}, auth.ErrLoginNotFound
}
func (t *cmdMemTx) InsertEmailToken(_ context.Context, n auth.NewEmailToken) (int64, error) {
	t.s.nextID++
	id := t.s.nextID
	t.s.tokens = append(t.s.tokens, &cmdMemToken{
		id: id, email: n.Email, codeHash: n.CodeHash,
		purpose: n.Purpose, expiresAt: n.ExpiresAt,
	})
	return id, nil
}
func (t *cmdMemTx) FindLatestActiveToken(_ context.Context, email string) (auth.EmailTokenView, error) {
	for i := len(t.s.tokens) - 1; i >= 0; i-- {
		tok := t.s.tokens[i]
		if tok.email == email && !tok.consumed {
			return auth.EmailTokenView{
				ID: tok.id, Email: tok.email, CodeHash: tok.codeHash,
				Purpose: tok.purpose, ExpiresAt: tok.expiresAt, Attempts: tok.attempts,
			}, nil
		}
	}
	return auth.EmailTokenView{}, auth.ErrNoPendingToken
}
func (t *cmdMemTx) ConsumeEmailToken(_ context.Context, id int64, _ time.Time) error {
	for _, tok := range t.s.tokens {
		if tok.id == id {
			if tok.consumed {
				return auth.ErrTokenConsumed
			}
			tok.consumed = true
			return nil
		}
	}
	return auth.ErrTokenConsumed
}
func (t *cmdMemTx) NoteEmailTokenAttempt(_ context.Context, id int64) error {
	for _, tok := range t.s.tokens {
		if tok.id == id {
			tok.attempts++
			return nil
		}
	}
	return nil
}
func (t *cmdMemTx) FindUserByEmailIdentity(_ context.Context, email string) (auth.AccountView, error) {
	uid, ok := t.s.identities[email]
	if !ok {
		return auth.AccountView{}, auth.ErrLoginNotFound
	}
	return auth.AccountView{UserID: uid, Username: "seeduser"}, nil
}
func (t *cmdMemTx) UpsertLocalCredential(_ context.Context, userID int64, hash string, _ time.Time) error {
	t.s.creds[userID] = hash
	return nil
}

type cmdMemSender struct{}

func (cmdMemSender) Send(context.Context, string, string, string) error { return nil }

type cmdMemRevoker struct{}

func (cmdMemRevoker) RevokeUserSessions(context.Context, int64) int { return 0 }

func newConfirmTestSvc(t *testing.T) (*auth.EmailAuthService, *cmdMemStore, string) {
	t.Helper()
	store := newCmdMemStore()
	code := "135790"
	svc := auth.NewEmailAuthService(store, cmdMemSender{}, cmdMemRevoker{},
		auth.WithEmailCodeFunc(func() (string, error) { return code, nil }))
	return svc, store, code
}

// ---- 测试辅助 ----

func feedRunes(t *testing.T, f *emailFlow, s string) {
	t.Helper()
	for _, r := range s {
		f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyRune, Rune: r})
	}
}

func emailEnter(t *testing.T, f *emailFlow) (done bool, cancelled bool) {
	t.Helper()
	return f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyEnter})
}

// assertEchoPlain 断言 echo 已退出掩码模式：随后输入的 "/login" 必须逐字回显，
// 而不是被写成 "******"（用户实测 bug 的直接复现）。
func assertEchoPlain(t *testing.T, echo *lineEcho, out *bytes.Buffer, label string) {
	t.Helper()
	if echo.secret {
		t.Fatalf("%s：echo 仍处于 secret 掩码模式", label)
	}
	before := out.Len()
	for _, r := range "/login" {
		echo.rune(r)
	}
	got := out.String()[before:]
	if got != "/login" {
		t.Fatalf("%s：后续命令应明文回显 %q，实际 %q", label, "/login", got)
	}
}

// ---- 1. secret 掩码复位：成功出口 ----

func TestVerifyConfirmSuccessResetsSecret(t *testing.T) {
	svc, _, code := newConfirmTestSvc(t)
	ctx := context.Background()
	pending := "alice@example.com"
	if _, err := svc.RequestCode(ctx, auth.EmailPurposeRegister, pending, "iphash"); err != nil {
		t.Fatal(err)
	}
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	purpose := auth.EmailPurposeRegister
	f := newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, code, false)
	conn.Reset()

	feedRunes(t, f, "password123")
	if done, _ := emailEnter(t, f); done {
		t.Fatal("第一次输入后不应结束流程")
	}
	if !echo.secret || !strings.Contains(conn.String(), emailHintConfirmPwd) {
		t.Fatalf("应进入二次确认且仍掩码: out=%q secret=%v", conn.String(), echo.secret)
	}

	feedRunes(t, f, "password123")
	done, cancelled := emailEnter(t, f)
	if !done || cancelled {
		t.Fatalf("两次一致应完成流程: done=%v cancelled=%v", done, cancelled)
	}
	if !strings.Contains(conn.String(), emailTextRegistered) {
		t.Fatalf("成功应给注册完成文案: %q", conn.String())
	}
	assertEchoPlain(t, echo, &conn.Buffer, "verify 成功出口")
}

// 失败出口：两次一致但 service 拒绝（此处无待验证 token → ErrInvalidCode）。
func TestVerifyConfirmFailureResetsSecret(t *testing.T) {
	svc, _, _ := newConfirmTestSvc(t)
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pending := "nobody@example.com"
	purpose := auth.EmailPurposeRegister
	// 未先 RequestCode → Verify 必然失败，但仍须复位 secret。
	f := newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, "999999", false)
	conn.Reset()

	feedRunes(t, f, "password123")
	emailEnter(t, f)
	feedRunes(t, f, "password123")
	done, cancelled := emailEnter(t, f)
	if !done || cancelled {
		t.Fatalf("service 失败也应结束流程: done=%v cancelled=%v", done, cancelled)
	}
	if !strings.Contains(conn.String(), emailTextBadCode) {
		t.Fatalf("应给固定错误文案: %q", conn.String())
	}
	assertEchoPlain(t, echo, &conn.Buffer, "verify 失败出口")
}

// ---- 2. 取消出口：Esc（第一次输入阶段与二次确认阶段都要复位）----

func TestVerifyConfirmEscResetsSecret(t *testing.T) {
	for _, atConfirm := range []bool{false, true} {
		svc, _, code := newConfirmTestSvc(t)
		conn := &fakeEmailConn{ipHash: "iphash"}
		echo := newLineEcho(conn)
		pending := "alice@example.com"
		purpose := auth.EmailPurposeRegister
		f := newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, code, false)
		conn.Reset()

		if atConfirm {
			feedRunes(t, f, "password123")
			emailEnter(t, f)
		}
		done, cancelled := f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyEsc})
		if !done || !cancelled {
			t.Fatalf("atConfirm=%v：Esc 应取消流程", atConfirm)
		}
		if !strings.Contains(conn.String(), emailTextCancelled) {
			t.Fatalf("atConfirm=%v：取消应给固定文案: %q", atConfirm, conn.String())
		}
		assertEchoPlain(t, echo, &conn.Buffer, "Esc 取消出口")
	}
}

// ---- 3. 被新命令取代的出口：clearEmailFlow + handleEmailCommand 都要复位 ----

func TestClearEmailFlowResetsSecret(t *testing.T) {
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pc := &pageController{echo: echo}
	pending := "alice@example.com"
	purpose := auth.EmailPurposeRegister
	pc.emailFlow = newEmailFlow(emailFlowVerify, nil, conn, echo, &pending, &purpose, "135790", false)
	if !echo.secret {
		t.Fatal("前置条件：verify 密码阶段应为掩码")
	}
	pc.clearEmailFlow()
	if pc.emailFlow != nil {
		t.Fatal("clearEmailFlow 应摘掉流程")
	}
	assertEchoPlain(t, echo, &conn.Buffer, "clearEmailFlow 出口")
	// 幂等：重复调用不得 panic 或再次改写回显。
	pc.clearEmailFlow()
}

func TestNewCommandReplacingActiveFlowResetsSecret(t *testing.T) {
	svc, _, code := newConfirmTestSvc(t)
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pc := &pageController{echo: echo}
	pending := "alice@example.com"
	purpose := auth.EmailPurposeRegister
	// 旧流程停在 verify 密码阶段（掩码开启）。
	pc.emailFlow = newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, code, false)
	conn.Reset()

	// 用户在流程未结束时又发 /register（新命令直接覆盖旧 flow）。
	h := &inputHandler{email: svc}
	inv, err := command.Default().Parse("/register")
	if err != nil {
		t.Fatal(err)
	}
	h.handleEmailCommand(context.Background(), conn, pc, inv)
	if pc.emailFlow == nil || pc.emailFlow.kind != emailFlowRegister {
		t.Fatalf("应挂上新的 register 流程: %+v", pc.emailFlow)
	}
	assertEchoPlain(t, echo, &conn.Buffer, "新命令取代旧流程出口")
}

// ---- 4. 两次密码不一致：回退重输、验证码不被消费、不落计数 ----

func TestVerifyConfirmMismatchRetriesWithoutConsumingCode(t *testing.T) {
	svc, store, code := newConfirmTestSvc(t)
	ctx := context.Background()
	pending := "alice@example.com"
	if _, err := svc.RequestCode(ctx, auth.EmailPurposeRegister, pending, "iphash"); err != nil {
		t.Fatal(err)
	}
	if len(store.tokens) != 1 || store.tokens[0].consumed {
		t.Fatalf("前置条件：应有一条未消费 token: %+v", store.tokens)
	}

	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	purpose := auth.EmailPurposeRegister
	f := newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, code, false)
	conn.Reset()

	// 第一次 "password123"，第二次 "different1" → 不一致。
	feedRunes(t, f, "password123")
	emailEnter(t, f)
	feedRunes(t, f, "different1")
	done, cancelled := emailEnter(t, f)
	if done || cancelled {
		t.Fatalf("不一致不应结束流程: done=%v cancelled=%v", done, cancelled)
	}
	out := conn.String()
	if !strings.Contains(out, emailTextPwdMismatch) {
		t.Fatalf("应给固定不一致文案: %q", out)
	}
	if !strings.Contains(out, emailHintSetPwd) {
		t.Fatalf("应回到第一次输入提示: %q", out)
	}
	if f.confirm || f.firstPwd != "" {
		t.Fatalf("不一致应重置到第一次输入状态: confirm=%v firstPwd=%q", f.confirm, f.firstPwd)
	}
	if !echo.secret {
		t.Fatal("重输阶段仍须掩码")
	}
	if store.tokens[0].consumed {
		t.Fatal("不一致时验证码不得被消费")
	}
	if store.tokens[0].attempts != 0 {
		t.Fatalf("不一致时不应调用 service 计数: attempts=%d", store.tokens[0].attempts)
	}
	// 底层错误原文绝不能出现（这里也断言固定文案不含用户数据）。
	if strings.Contains(out, "password123") || strings.Contains(out, "different1") {
		t.Fatalf("密码原文绝不能回显: %q", out)
	}

	// 回到第一次输入后重输两次一致 → 成功，且这次才消费验证码。
	conn.Reset()
	feedRunes(t, f, "password123")
	emailEnter(t, f)
	feedRunes(t, f, "password123")
	done, cancelled = emailEnter(t, f)
	if !done || cancelled {
		t.Fatalf("重输一致应完成: done=%v cancelled=%v", done, cancelled)
	}
	if !strings.Contains(conn.String(), emailTextRegistered) {
		t.Fatalf("重输一致应给注册完成文案: %q", conn.String())
	}
	if !store.tokens[0].consumed {
		t.Fatal("成功时验证码必须被消费")
	}
	assertEchoPlain(t, echo, &conn.Buffer, "不一致重输后成功出口")
}

// 找回流程同样要确认两次，二次输入阶段沿用「请再次输入密码确认」文案，
// 成功后走 EmailPurposeRecover 分支给重置完成文案。
func TestRecoverConfirmUsesNewPwdPromptAndResetsSecret(t *testing.T) {
	svc, store, code := newConfirmTestSvc(t)
	ctx := context.Background()
	pending := "alice@example.com"
	// 找回需要已有身份，预置一条，否则 RequestCode 不发 token。
	store.identities[pending] = 42
	if _, err := svc.RequestCode(ctx, auth.EmailPurposeRecover, pending, "iphash"); err != nil {
		t.Fatal(err)
	}

	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	purpose := auth.EmailPurposeRecover
	f := newEmailFlow(emailFlowVerify, svc, conn, echo, &pending, &purpose, code, true)
	conn.Reset()

	p, secret := f.currentPrompt()
	if !secret || !strings.Contains(p, "新密码") {
		t.Fatalf("找回首输应提示新密码且掩码: prompt=%q secret=%v", p, secret)
	}
	feedRunes(t, f, "newpass123")
	emailEnter(t, f)
	p, secret = f.currentPrompt()
	if !secret || p != emailHintConfirmPwd {
		t.Fatalf("找回二次确认提示不对: prompt=%q secret=%v", p, secret)
	}
	feedRunes(t, f, "newpass123")
	done, cancelled := emailEnter(t, f)
	if !done || cancelled {
		t.Fatalf("找回两次一致应完成: done=%v cancelled=%v", done, cancelled)
	}
	if !strings.Contains(conn.String(), emailTextRecovered) {
		t.Fatalf("找回成功应给重置完成文案: %q", conn.String())
	}
	assertEchoPlain(t, echo, &conn.Buffer, "找回成功出口")
}

// ---- 5. /login email 不加确认：一次密码即提交 ----

func TestEmailLoginPasswordHasNoConfirm(t *testing.T) {
	svc, _, _ := newConfirmTestSvc(t)
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pending, purpose := "", ""
	f := newEmailFlow(emailFlowLogin, svc, conn, echo, &pending, &purpose, "", false)
	conn.Reset()

	feedRunes(t, f, "alice@example.com")
	if done, _ := emailEnter(t, f); done {
		t.Fatal("邮箱步骤后不应结束")
	}
	feedRunes(t, f, "whatever123")
	done, cancelled := emailEnter(t, f)
	if !done || cancelled {
		t.Fatalf("登录一次密码即应提交（无确认阶段）: done=%v cancelled=%v", done, cancelled)
	}
	out := conn.String()
	if strings.Contains(out, emailHintConfirmPwd) || strings.Contains(out, emailTextPwdMismatch) {
		t.Fatalf("/login email 不得出现确认步骤: %q", out)
	}
	assertEchoPlain(t, echo, &conn.Buffer, "登录出口")
}
