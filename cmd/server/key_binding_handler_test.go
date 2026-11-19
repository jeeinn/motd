package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

type noopKeyRevoker struct{}

func (noopKeyRevoker) RevokeSessions(context.Context, string) int { return 0 }

// P1-M4：/keys <fingerprint> 展示单钥元数据；跨用户与不存在共用固定拒绝文案，
// 命令参数里的控制字符绝不回显。
func TestRunKeysDetailAndOwnership(t *testing.T) {
	ctx, _, _, pools, writer := setupMVPRuntime(t)
	fp := "SHA256:" + strings.Repeat("G", 43)
	created := time.Unix(1_700_000_000, 0).UTC()
	lastUsed := created.Add(time.Hour)

	err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at, last_used_at)
			VALUES (1, ?, 'ssh-ed25519 AAAAExample wei@host', ?, ?)`,
			fp, created.Unix(), lastUsed.Unix())
		return err
	})
	if err != nil {
		t.Fatalf("预置公钥: %v", err)
	}

	h := newInputHandler().withKeyBinding(newKeyBindingService(pools, writer, noopKeyRevoker{}))
	alice := auth.Principal{Kind: auth.KindUser, UserID: 1, KeyFingerprint: fp}
	bob := auth.Principal{Kind: auth.KindUser, UserID: 2, KeyFingerprint: "SHA256:" + strings.Repeat("H", 43)}

	inv := command.Invocation{Name: "keys", Args: []string{fp}}
	out := h.runKeys(ctx, alice, inv)
	for _, want := range []string{"公钥详情", fp, "标签：", "创建：", "最近使用：", "2023-11-14"} {
		if !strings.Contains(out, want) {
			t.Errorf("详情缺少 %q: %q", want, out)
		}
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Errorf("详情输出须以 CRLF 收尾: %q", out)
	}

	// 跨用户：固定拒绝文案，且不泄露指纹。
	foreign := h.runKeys(ctx, bob, inv)
	if !strings.Contains(foreign, "未找到该公钥绑定") || strings.Contains(foreign, fp) {
		t.Fatalf("跨用户详情应拒绝且不回显指纹: %q", foreign)
	}

	// 恶意参数不回显。
	malicious := h.runKeys(ctx, alice, command.Invocation{Name: "keys", Args: []string{"\x1b[2Jpwned"}})
	if strings.Contains(malicious, "\x1b") || strings.Contains(malicious, "pwned") {
		t.Fatalf("恶意指纹参数被回显: %q", malicious)
	}

	// 无参数仍走列表路径。
	list := h.runKeys(ctx, alice, command.Invocation{Name: "keys"})
	if !strings.Contains(list, fp) || !strings.Contains(list, "已绑定公钥") {
		t.Fatalf("无参数应列公钥: %q", list)
	}
}
