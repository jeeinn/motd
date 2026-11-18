package auth

import (
	"context"
	"fmt"
	"strings"
)

// 指纹格式与 golang.org/x/crypto/ssh 的 FingerprintSHA256 输出一致：
// "SHA256:" + base64.RawStdEncoding(sha256(pubkey.Marshal()))，43 字符、无填充。
// 这里只用标准库校验，application 层不依赖 SSH 库，也就无法从客户端提交的
// 公钥文本里自行算指纹（§4.6）；格式一致性由 transport/ssh 的契约测试保证。
const (
	fingerprintPrefix  = "SHA256:"
	fingerprintBodyLen = 43
)

// keyResolver 是 Resolver 的默认实现：查 ssh_keys 表做三状态映射。
type keyResolver struct {
	lookup KeyLookup
}

var _ Resolver = (*keyResolver)(nil)

// NewResolver 构造查库实现的身份解析器。
// lookup 为 nil 属于装配期错误，直接 panic，不能留到运行期退化成访客放行。
func NewResolver(lookup KeyLookup) Resolver {
	if lookup == nil {
		panic("auth.NewResolver: KeyLookup 不能为空")
	}
	return &keyResolver{lookup: lookup}
}

// Resolve 实现 §4.6 的三状态：
//   - 未命中 ssh_keys        → 访客只读
//   - 命中且未吊销           → 对应用户身份
//   - 命中但已吊销           → ErrKeyRevoked（拒绝认证）
//
// fingerprint 由 transport/ssh 在认证阶段从服务端持有的 ssh.PublicKey 算出，
// 已经过公钥挑战，绝不接受客户端自行提交的公钥文本（P0-18 约束提前落地）。
func (r *keyResolver) Resolve(ctx context.Context, fingerprint string) (Principal, error) {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return Principal{}, ErrEmptyFingerprint
	}
	if !ValidFingerprint(fp) {
		return Principal{}, ErrInvalidFingerprint
	}

	st, err := r.lookup.Lookup(ctx, fp)
	if err != nil {
		// 查库失败必须拒绝认证：一次 DB 抖动不能让已绑定用户「降级」成访客，
		// 更不能让实现缺陷把吊销态漏成放行。
		return Principal{}, fmt.Errorf("查询公钥绑定状态: %w", err)
	}
	if !st.Found {
		return Guest(fp), nil
	}
	if st.Revoked {
		return Principal{}, ErrKeyRevoked
	}
	if st.UserID <= 0 {
		return Principal{}, fmt.Errorf("公钥 %s 绑定了非法用户 ID %d", fp, st.UserID)
	}
	return Principal{Kind: KindUser, UserID: st.UserID, KeyFingerprint: fp}, nil
}

// ValidFingerprint 只做格式校验，不做密码学验证。
// 导出给 transport/ssh 的契约测试使用：确保认证层算出的真实指纹一定被接受，
// 避免格式规则写错导致所有连接被拒之门外。
func ValidFingerprint(fp string) bool {
	if !strings.HasPrefix(fp, fingerprintPrefix) {
		return false
	}
	body := fp[len(fingerprintPrefix):]
	if len(body) != fingerprintBodyLen {
		return false
	}
	for _, c := range body {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
		default:
			return false
		}
	}
	return true
}
