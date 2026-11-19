package auth

import (
	"strings"
	"testing"
)

func TestGenerateCodeIsSixDigits(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		code, err := generateCode()
		if err != nil {
			t.Fatal(err)
		}
		if !validCode(code) {
			t.Fatalf("生成了非法验证码 %q", code)
		}
		seen[code] = true
	}
	if len(seen) < 180 {
		t.Fatalf("验证码分布异常，200 次仅 %d 个不同值", len(seen))
	}
}

func TestValidCodeRejectsJunk(t *testing.T) {
	for _, code := range []string{"", "12345", "1234567", "12345a", "12 456", "１２３４５６", "-12345"} {
		if validCode(code) {
			t.Errorf("非法验证码 %q 被接受", code)
		}
	}
}

func TestHashCodeAndConstantTimeCompare(t *testing.T) {
	pepper := []byte("test-pepper-not-a-real-secret")
	code := "012345"
	hash := hashCode(pepper, code)
	if strings.Contains(hash, code) {
		t.Fatal("散列里不得出现明文码")
	}
	if len(hash) != 64 {
		t.Fatalf("HMAC-SHA256 hex 应为 64 字符，实际 %d", len(hash))
	}
	if !codeMatches(pepper, code, hash) {
		t.Fatal("正确码应校验通过")
	}
	if codeMatches(pepper, "012346", hash) {
		t.Fatal("错误码不得通过")
	}
	if codeMatches([]byte("different-pepper"), code, hash) {
		t.Fatal("pepper 不同不得通过")
	}
}

func TestPasswordHashUsesArgon2idParams(t *testing.T) {
	hash, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, passwordHashPref) {
		t.Fatalf("散列前缀应写明 argon2id 参数，实际 %q", hash[:min(len(hash), 40)])
	}
	if !verifyPassword(hash, "correct horse battery staple") {
		t.Fatal("正确密码应通过")
	}
	if verifyPassword(hash, "wrong password") {
		t.Fatal("错误密码不得通过")
	}
	if !strings.Contains(hash, "m=19456,t=2,p=1") {
		t.Fatal("参数必须是 19MiB/t=2/p=1")
	}
}

func TestPasswordStrengthFloor(t *testing.T) {
	for _, pw := range []string{"", "1234567", "短密码"} {
		if err := validatePassword(pw); err == nil {
			t.Errorf("弱密码 %q 应被拒绝", pw)
		}
	}
	if err := validatePassword("12345678"); err != nil {
		t.Fatalf("8 位应通过: %v", err)
	}
}

func TestVerifyPasswordRejectsTamperedEncoding(t *testing.T) {
	for _, bad := range []string{
		"",
		"plaintext",
		passwordHashPref + "short",
		passwordHashPref + "!!!notbase64!!!$abc",
		"$argon2id$v=19$m=1024,t=1,p=1$c2FsdA$a2V5", // 低参数散列应被拒
	} {
		if verifyPassword(bad, "whatever123") {
			t.Errorf("被篡改的散列不得通过: %q", bad)
		}
	}
}

func TestEmailNormalizationAndLocalPart(t *testing.T) {
	if got := normalizeEmail("  User@Example.COM "); got != "user@example.com" {
		t.Fatalf("归一化失败: %q", got)
	}
	if got := emailLocalPart("user@example.com"); got != "user" {
		t.Fatalf("本地段提取失败: %q", got)
	}
	if got := emailLocalPart("bad-address"); got != "" {
		t.Fatalf("无 @ 不应有本地段: %q", got)
	}
	for _, email := range []string{"user@example.com", "a@b.co"} {
		if !validEmailAddress(email) {
			t.Errorf("合法邮箱被拒: %q", email)
		}
	}
	for _, email := range []string{"", "no-at-sign", "a@@b.com", "a b@c.com"} {
		if validEmailAddress(email) {
			t.Errorf("非法邮箱被接受: %q", email)
		}
	}
}
