package auth

import (
	"context"
	"errors"
)

// Kind 是一次会话的身份类别。
type Kind uint8

const (
	KindGuest Kind = iota
	KindUser
)

// ErrKeyRevoked 表示指纹命中但公钥已被吊销（设计稿 §4.6 三状态之一）。
// P0-21 的 SSH 入口靠这个哨兵把「拒绝认证」与「解析失败」区分开。
var ErrKeyRevoked = errors.New("公钥已吊销")

// Principal 是认证完成后交给上层用例的身份快照。
//
// P0-14 只做「服务端持有的公钥指纹 → Principal」的解析；Device Flow 登录、
// 账号创建与公钥绑定属于 P0-15 ~ P0-18，不在这个文件里。
// KeyFingerprint 必须来自认证阶段服务端持有的 ssh.PublicKey，绝不接受
// 客户端自行提交的公钥文本（§4.6 / P0-18 的核心约束）。
type Principal struct {
	Kind           Kind
	UserID         int64
	KeyFingerprint string
}

// IsUser 是写权限判定的唯一入口：访客 UserID 恒为 0。
func (p Principal) IsUser() bool { return p.Kind == KindUser && p.UserID > 0 }

func (p Principal) IsGuest() bool { return !p.IsUser() }

// Guest 构造访客身份。
func Guest(fingerprint string) Principal {
	return Principal{Kind: KindGuest, KeyFingerprint: fingerprint}
}

// Resolver 把公钥指纹解析为身份。
//
// 端口定义在 application（使用方），实现由 P0-14 落到 infrastructure/sqlite。
// transport/ssh 只依赖这个接口，因此第三波先做 SSH 入口不需要等 M2，
// M2 换实现时也不用改动协议层（ADR-017）。
type Resolver interface {
	Resolve(ctx context.Context, fingerprint string) (Principal, error)
}
