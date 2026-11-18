package auth

import (
	"errors"
	"strings"
	"testing"
)

// kbTestFingerprint 是合法格式（SHA256: + 43 个 RawStdEncoding 字符），
// 与 P0-14 的 ValidFingerprint 及 gossh.FingerprintSHA256 输出一致。
var kbTestFingerprint = "SHA256:" + strings.Repeat("B", 43)

func TestParseKeyCommandAdd(t *testing.T) {
	cmd, err := ParseKeyCommand([]string{"add"})
	if err != nil || cmd.Kind != KeySubAdd {
		t.Fatalf("/key add 应解析为 Add，实际 %+v err=%v", cmd, err)
	}
	if cmd.Fingerprint != "" {
		t.Fatal("add 不得携带指纹（指纹只能来自 Session）")
	}

	// P0-18 验收核心：伪造提交「别人的公钥字符串」连子命令解析都过不去。
	forged := [][]string{
		{"add", "ssh-rsa"},
		{"add", "ssh-rsa", "AAAAB3NzaC1yc2EAAAADAQABAAACAQ..."},
		{"add", "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAI...", "comment"},
		{"add", "SHA256:" + strings.Repeat("C", 43)},
		{"add", "\x1b[2J"},
	}
	for _, args := range forged {
		if _, err := ParseKeyCommand(args); !errors.Is(err, ErrKeySubcommand) {
			t.Errorf("伪造公钥文本 %q 必须被拒绝，实际 err=%v", args, err)
		}
	}
}

func TestParseKeyCommandRemove(t *testing.T) {
	cmd, err := ParseKeyCommand([]string{"rm", kbTestFingerprint})
	if err != nil || cmd.Kind != KeySubRemove || cmd.Fingerprint != kbTestFingerprint {
		t.Fatalf("/key rm 解析错误: %+v err=%v", cmd, err)
	}
	// 前后空白允许 trim，但内容必须仍是合法指纹。
	cmd, err = ParseKeyCommand([]string{"rm", "  " + kbTestFingerprint + "  "})
	if err != nil || cmd.Fingerprint != kbTestFingerprint {
		t.Fatalf("指纹应被 trim 归一化: %+v err=%v", cmd, err)
	}

	bad := [][]string{
		{"rm"},
		{"rm", kbTestFingerprint, "extra"},
		{"rm", "ssh-rsa AAAAB3NzaC1yc2E..."},
		{"rm", "SHA256:short"},
		{"rm", "\x1b[2J" + kbTestFingerprint},
		{"rm", strings.Repeat("A", 43)}, // 缺 SHA256: 前缀
	}
	for _, args := range bad {
		if _, err := ParseKeyCommand(args); !errors.Is(err, ErrKeySubcommand) {
			t.Errorf("非法 /key rm 参数 %q 必须被拒绝，实际 err=%v", args, err)
		}
	}
}

func TestParseKeyCommandRejectsUnknown(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"link"}, {"ADD"}} {
		if _, err := ParseKeyCommand(args); !errors.Is(err, ErrKeySubcommand) {
			t.Errorf("未知子命令 %q 必须被拒绝，实际 err=%v", args, err)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	fp, err := NormalizeFingerprint("  " + kbTestFingerprint + " ")
	if err != nil || fp != kbTestFingerprint {
		t.Fatalf("合法指纹应被接受，实际 %q err=%v", fp, err)
	}
	bad := []string{
		"", "   ",
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQ...", // 公钥文本不是指纹
		"SHA256:short",
		"SHA256:" + strings.Repeat("B", 42),
		"SHA256:" + strings.Repeat("B", 44),
		"SHA256:" + strings.Repeat("B", 42) + "!",
		"\x1b[2J" + kbTestFingerprint,
	}
	for _, raw := range bad {
		if _, err := NormalizeFingerprint(raw); err == nil {
			t.Errorf("非法指纹 %q 不应通过", raw)
		}
	}
}

func TestValidatePublicKeyText(t *testing.T) {
	good := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyMaterial wei@host",
		"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQExample",
	}
	for _, s := range good {
		if err := ValidatePublicKeyText(s); err != nil {
			t.Errorf("服务端序列化公钥文本应通过: %q: %v", s, err)
		}
	}
	bad := []string{
		"", "   ",
		"ssh-ed25519 AAAA\nssh-rsa BBBB",
		"ssh-ed25519 AAAA\rssh-rsa BBBB",
		"ssh-ed25519 AAAA\x1b[2JBBBB",
		"ssh-ed25519 AAAA\x00BBBB",
		strings.Repeat("A", maxPublicKeyTextRunes+1),
	}
	for _, s := range bad {
		if err := ValidatePublicKeyText(s); !errors.Is(err, ErrInvalidPublicKey) {
			t.Errorf("非法公钥文本应被拒绝: %q: %v", s, err)
		}
	}
}

func TestKeyBindingSentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		ErrBindingRequiresLogin, ErrInvalidPublicKey, ErrKeyBoundToOtherUser,
		ErrKeyRevokedBinding, ErrKeyNotFound, ErrKeyConflict,
		ErrKeyRetriesExhausted, ErrKeySubcommand,
	}
	seen := make(map[string]bool)
	for _, e := range sentinels {
		if seen[e.Error()] {
			t.Errorf("哨兵文案重复: %q", e.Error())
		}
		seen[e.Error()] = true
	}
}
