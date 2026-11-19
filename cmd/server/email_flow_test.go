package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
)

// fakeEmailConn 实现 emailSession，仅用于 cmd 层测试，不触网、不起 SSH。
type fakeEmailConn struct {
	bytes.Buffer
	upgraded   int64
	ipHash     string
	upgradeErr error
}

func (c *fakeEmailConn) ID() string                { return "conn-test" }
func (c *fakeEmailConn) Principal() auth.Principal { return auth.Principal{} }
func (c *fakeEmailConn) RemoteIPHash() string      { return c.ipHash }
func (c *fakeEmailConn) UpgradeToUser(id int64) error {
	if c.upgradeErr != nil {
		return c.upgradeErr
	}
	c.upgraded = id
	return nil
}

func TestEmailCommandsParseFromRegistry(t *testing.T) {
	cases := []struct {
		line string
		name string
		args []string
	}{
		{"/register", "register", nil},
		{"/recover", "recover", nil},
		{"/resend", "resend", nil},
		{"/verify 123456", "verify", []string{"123456"}},
		{"/login email", "login", []string{"email"}},
	}
	for _, c := range cases {
		inv, err := command.Default().Parse(c.line)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.line, err)
		}
		if inv.Name != c.name || len(inv.Args) != len(c.args) {
			t.Fatalf("%q 解析结果不对: %+v", c.line, inv)
		}
		for i, a := range c.args {
			if inv.Args[i] != a {
				t.Errorf("%q 参数 %d 应为 %q，实际 %q", c.line, i, a, inv.Args[i])
			}
		}
	}
}

func TestIsEmailLoginWhitelist(t *testing.T) {
	good, _ := command.Default().Parse("/login email")
	if !isEmailLogin(good) || isGitHubLogin(good) {
		t.Fatal("/login email 必须命中 email 白名单且不是 github")
	}
	for _, line := range []string{"/login", "/login github", "/login Email", "/login email extra"} {
		inv, err := command.Default().Parse(line)
		if err != nil {
			continue
		}
		if isEmailLogin(inv) {
			t.Errorf("%q 不得命中 email 白名单", line)
		}
	}
	if !strings.Contains(auth.LoginTextUnsupportedProvider, "/login email") {
		t.Fatal("无参 /login 提示必须给出 email 用法")
	}
}

// 密码步骤必须隐藏回显；邮箱步骤必须明文回显（ADR-026）。
func TestEmailFlowSecretEchoByStep(t *testing.T) {
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pending := "alice@example.com"
	purpose := auth.EmailPurposeRecover

	verify := newEmailFlow(emailFlowVerify, nil, conn, echo, &pending, &purpose, "123456", true)
	p, secret := verify.currentPrompt()
	if !secret || !strings.Contains(p, "新密码") {
		t.Fatalf("找回 verify 应隐藏回显新密码: prompt=%q secret=%v", p, secret)
	}
	if !echo.secret {
		t.Fatal("密码步骤必须 setSecret(true)")
	}
	// 输入数字只应回显 '*'，绝不回显原文。
	echo.rune('1')
	echo.rune('2')
	if strings.Contains(conn.String(), "12") {
		t.Fatalf("密码原文绝不能回显: %q", conn.String())
	}

	conn2 := &fakeEmailConn{ipHash: "iphash"}
	echo2 := newLineEcho(conn2)
	login := newEmailFlow(emailFlowLogin, nil, conn2, echo2, &pending, &purpose, "", false)
	p, secret = login.currentPrompt()
	if secret || !strings.Contains(p, "邮箱") {
		t.Fatalf("登录邮箱步骤应明文回显: prompt=%q secret=%v", p, secret)
	}
	if echo2.secret {
		t.Fatal("邮箱步骤不得隐藏回显")
	}
	login.stage = 1
	p, secret = login.currentPrompt()
	if !secret || !strings.Contains(p, "密码") {
		t.Fatalf("登录密码步骤应隐藏回显: prompt=%q secret=%v", p, secret)
	}
}

// 错误到文案的映射绝不回显底层错误原文（可能含外部数据 / AUTH 串）。
func TestWriteEmailErrorNeverLeaksUnderlyingError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{auth.ErrInvalidEmail, emailTextBadEmail},
		{auth.ErrEmailOccupied, auth.EmailOccupiedHint},
		{fmt.Errorf("wrap: %w", auth.ErrEmailDelivery), emailTextSendFailed},
		{auth.ErrInvalidCode, emailTextBadCode},
		{auth.ErrWeakPassword, emailTextBadPassword},
		{auth.ErrInvalidCredentials, emailTextBadLogin},
		{auth.ErrNoPendingToken, emailTextNoPending},
		{&auth.RateLimitError{Kind: auth.LimitVerify, Reason: "verify", RetryAfter: 61 * time.Second}, "邮箱操作过于频繁"},
	}
	for _, c := range cases {
		var b bytes.Buffer
		writeEmailError(&b, c.err)
		if !strings.Contains(b.String(), c.want) {
			t.Errorf("错误 %v 应映射为 %q，实际 %q", c.err, c.want, b.String())
		}
	}

	leaky := errors.New("上游 AUTH_SECRET_SHOULD_NOT_APPEAR 邮箱 alice@example.com")
	var b bytes.Buffer
	writeEmailError(&b, leaky)
	if strings.Contains(b.String(), "AUTH_SECRET") || strings.Contains(b.String(), "alice@example.com") {
		t.Fatalf("外部错误原文不得写回终端: %q", b.String())
	}
}

// 分步输入：Esc 取消不调用任何服务；Enter 推进 step；密码缓冲只收字符不改回显模式。
func TestEmailFlowInputStateMachine(t *testing.T) {
	conn := &fakeEmailConn{ipHash: "iphash"}
	echo := newLineEcho(conn)
	pending := ""
	purpose := ""
	f := newEmailFlow(emailFlowLogin, nil, conn, echo, &pending, &purpose, "", false)
	conn.Reset()

	done, cancelled := f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyRune, Rune: 'a'})
	if done || cancelled || string(f.buf) != "a" {
		t.Fatalf("普通字符应进缓冲: buf=%q done=%v cancelled=%v", string(f.buf), done, cancelled)
	}
	done, cancelled = f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyBackspace})
	if done || cancelled || len(f.buf) != 0 {
		t.Fatalf("退格应删一个字符: buf=%q", string(f.buf))
	}
	done, cancelled = f.step(context.Background(), nil, input.KeyMsg{Kind: input.KeyEsc})
	if !done || !cancelled {
		t.Fatal("Esc 应取消流程")
	}
	if !strings.Contains(conn.String(), emailTextCancelled) {
		t.Fatalf("取消应给固定文案: %q", conn.String())
	}
}
