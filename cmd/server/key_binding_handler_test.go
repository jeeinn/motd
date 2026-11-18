package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
)

// P0-18 命令分发回归：/key add 不接受公钥文本、帮助由注册表生成、
// 结果文案全是服务端固定文本，绝不回显命令参数或错误原文。

func TestKeyCommandTableArity(t *testing.T) {
	c, ok := command.Default().Lookup("key")
	if !ok {
		t.Fatal("命令表必须有 /key")
	}
	if c.MinArgs != 1 || c.MaxArgs != 2 {
		t.Fatalf("/key 参数区间应为 [1,2]（add 1 个 / rm 2 个），实际 [%d,%d]", c.MinArgs, c.MaxArgs)
	}
	if _, ok := command.Default().Lookup("keys"); !ok {
		t.Fatal("命令表必须保留 /keys")
	}
}

func TestKeyAddRejectsForgedPublicKeyText(t *testing.T) {
	reg := command.Default()
	forged := []string{
		"/key add ssh-rsa",
		"/key add ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQ...",
		"/key add ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...",
		"/key add SHA256:AAAA",
	}
	for _, line := range forged {
		inv, err := reg.Parse(line)
		if errors.Is(err, command.ErrArgCount) {
			continue // 命令表粗筛已挡下（3+ 参数）
		}
		if err != nil {
			t.Fatalf("Parse(%q) 意外失败: %v", line, err)
		}
		// 两参数形态（如 "/key add ssh-rsa"）由业务层严格收口。
		if _, err := auth.ParseKeyCommand(inv.Args); !errors.Is(err, auth.ErrKeySubcommand) {
			t.Errorf("伪造公钥文本 %q 必须被子命令解析拒绝，实际 %v", line, err)
		}
	}
}

func TestKeyCommandHelpIsGeneratedFromRegistry(t *testing.T) {
	reg := command.Default()
	inv, err := reg.Parse("/help")
	if err != nil {
		t.Fatal(err)
	}
	help := formatResult(reg, inv, nil)
	for _, want := range []string{"/keys", "/key add", "/key rm <fingerprint>"} {
		if !strings.Contains(help, want) {
			t.Errorf("帮助缺少 %q", want)
		}
	}
}

func TestFormatResultNeverEchoesKeyArguments(t *testing.T) {
	reg := command.Default()
	malicious := []string{
		"/key rm \x1b[2J\x1b]0;pwned\x07",
		"/keys \x1b]52;c;YWJj\x07",
		"/key add pwned",
	}
	for _, line := range malicious {
		inv, err := reg.Parse(line)
		out := formatResult(reg, inv, err)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") || strings.Contains(out, "YWJj") {
			t.Errorf("回显泄露了用户提交的原始文本: line=%q out=%q", line, out)
		}
	}
}

func TestBindResultTextIsFixedAndNeverEchoesErrors(t *testing.T) {
	cases := []struct {
		res auth.BindResult
		err error
	}{
		{auth.BindResult{Created: true}, nil},
		{auth.BindResult{}, nil},
		{auth.BindResult{}, auth.ErrBindingRequiresLogin},
		{auth.BindResult{}, auth.ErrKeyBoundToOtherUser},
		{auth.BindResult{}, auth.ErrKeyRevokedBinding},
		{auth.BindResult{}, auth.ErrInvalidPublicKey},
		{auth.BindResult{}, auth.ErrInvalidFingerprint},
		{auth.BindResult{}, errors.New("\x1b[2J pwned ssh-rsa AAAA")},
		{auth.BindResult{}, fmt.Errorf("wrap: %w", auth.ErrKeyRetriesExhausted)},
	}
	for _, c := range cases {
		out := bindResultText(c.res, c.err)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") ||
			strings.Contains(out, "ssh-rsa") || strings.Contains(out, "wrap:") {
			t.Errorf("结果文案泄露错误原文: %q", out)
		}
		if !strings.HasSuffix(out, "\r\n") {
			t.Errorf("终端输出必须以 CRLF 收尾: %q", out)
		}
	}
}

func TestUnbindResultTextIsFixedAndNeverEchoesErrors(t *testing.T) {
	cases := []struct {
		res auth.UnbindResult
		err error
	}{
		{auth.UnbindResult{Revoked: true, SessionsClosed: 2}, nil},
		{auth.UnbindResult{SessionsClosed: 0}, nil},
		{auth.UnbindResult{}, auth.ErrBindingRequiresLogin},
		{auth.UnbindResult{}, auth.ErrKeyNotFound},
		{auth.UnbindResult{}, auth.ErrKeyBoundToOtherUser},
		{auth.UnbindResult{}, errors.New("\x1b]52;c;YWJj\x07")},
	}
	for _, c := range cases {
		out := unbindResultText(c.res, c.err)
		if strings.Contains(out, "\x1b") || strings.Contains(out, "YWJj") {
			t.Errorf("结果文案泄露错误原文: %q", out)
		}
		if !strings.HasSuffix(out, "\r\n") {
			t.Errorf("终端输出必须以 CRLF 收尾: %q", out)
		}
	}
	if out := unbindResultText(auth.UnbindResult{Revoked: true, SessionsClosed: 3}, nil); !strings.Contains(out, "3 个") {
		t.Errorf("应带回被撤销的会话数: %q", out)
	}
}
