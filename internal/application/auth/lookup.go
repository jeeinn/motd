package auth

import (
	"context"
	"errors"
)

// 指纹解析阶段的哨兵错误。
// transport/ssh 把 ErrKeyRevoked 映射成「拒绝认证」，其余错误一律当作解析失败；
// 任何错误都不会被降级成访客放行（ADR-017）。
var (
	ErrEmptyFingerprint   = errors.New("公钥指纹为空")
	ErrInvalidFingerprint = errors.New("公钥指纹格式非法")
)

// KeyStatus 是一次「指纹 → 绑定状态」查询的结果快照。
// Found=false 即未绑定；Found=true 时 Revoked 区分三状态的后两种。
//
// 刻意不携带账号状态：账号可用性由写用例在事务内二次复核（P0-10 的
// UserStatusReader），认证层不缓存它，避免快照过期造成越权。
type KeyStatus struct {
	Found   bool
	Revoked bool
	UserID  int64
}

// KeyLookup 是身份解析唯一需要的只读端口，实现落在 infrastructure/sqlite。
// 端口定义在使用方（与 ADR-012 / ADR-017 同一口径），application 不碰 SQL。
type KeyLookup interface {
	Lookup(ctx context.Context, fingerprint string) (KeyStatus, error)
}
