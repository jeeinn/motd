package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 验证码与密码散列的唯一实现（§2.9：argon2id 19MiB / t=2 / p=1）。
// 这里不引入第三方口令库，也不提供第二种散列算法开关。

const (
	codeDigits   = 6
	codeMaxValue = 1000000 // 6 位：000000..999999
	passwordMin  = 8

	argonMemory      = 19 * 1024 // KiB = 19MiB
	argonTime        = 2
	argonThreads     = 1
	argonKeyLen      = 32
	argonSaltLen     = 16
	passwordHashPref = "$argon2id$v=19$m=19456,t=2,p=1$"
)

// generateCode 用 crypto/rand 生成 6 位数字串（含前导零），绝不取 math/rand。
func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(codeMaxValue))
	if err != nil {
		return "", fmt.Errorf("生成验证码: %w", err)
	}
	return fmt.Sprintf("%0*d", codeDigits, n.Int64()), nil
}

// hashCode 用 HMAC-SHA256(pepper, code) 生成入库散列。
// pepper 只在进程内存中（启动随机生成或测试注入）；即便库泄漏，
// 6 位码空间也无法被离线暴力枚举。返回值是 hex 串，绝不带明文码。
func hashCode(pepper []byte, code string) string {
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(code))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// validCode 只接受恰好 6 位数字：拒绝任何多余字符进入比较路径。
func validCode(code string) bool {
	if len(code) != codeDigits {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// codeMatches 用常量时间比较，避免通过响应耗时逐位猜码。
func codeMatches(pepper []byte, code, storedHash string) bool {
	if !validCode(code) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashCode(pepper, code)), []byte(storedHash)) == 1
}

// validatePassword 只做长度下限；不拼接原文进任何错误文本。
func validatePassword(password string) error {
	if len([]rune(password)) < passwordMin {
		return ErrWeakPassword
	}
	return nil
}

// hashPassword 生成带参数的 argon2id 散列串，可直接落 local_credentials。
func hashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成密码盐: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	var b strings.Builder
	b.WriteString(passwordHashPref)
	b.WriteString(base64.RawStdEncoding.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(key))
	return b.String(), nil
}

// verifyPassword 解析并校验散列。参数写死在编码前缀里，不接受外部传入，
// 避免有人用低参数散列覆盖后「验证通过」形成降级攻击。
func verifyPassword(encoded, password string) bool {
	if !strings.HasPrefix(encoded, passwordHashPref) {
		return false
	}
	rest := strings.TrimPrefix(encoded, passwordHashPref)
	saltB64, keyB64, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) != argonSaltLen {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(keyB64)
	if err != nil || len(want) != argonKeyLen {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// newPepper 生成进程级 HMAC pepper。重启会让在途验证码失效（TTL 很短，
// 可接受），但换来「库泄漏也无法离线爆破 6 位码」。
func newPepper() ([]byte, error) {
	p := make([]byte, sha256.Size)
	if _, err := rand.Read(p); err != nil {
		return nil, fmt.Errorf("生成邮箱 pepper: %w", err)
	}
	return p, nil
}

// normalizeEmail 收敛大小写与首尾空白。邮箱只作恢复通道，不做身份主键，
// 但归一化必须幂等，否则 token / identity / credential 三处会对不上。
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validEmailAddress 与 login.go 的 validEmail 同口径（复用而非复制口径）：
// 长度 3..254、恰好一个 @、无控制 / 空白字符。
func validEmailAddress(normalized string) bool { return validEmail(normalized) }

// emailLocalPart 取 @ 前的本地段，作为用户名派生基准；清洗与冲突处理
// 完全交给 login.go 的 DeriveUsername / AllocateUsername，不新造规则。
func emailLocalPart(normalized string) string {
	at := strings.IndexByte(normalized, '@')
	if at <= 0 {
		return ""
	}
	return normalized[:at]
}
