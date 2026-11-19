package auth

// 公钥使用痕迹：认证成功（指纹命中已绑定钥匙）后记录一次 last_used_at（P1-M4）。
//
// 边界：application 只定义「记录一次使用」的端口与 Resolver 装饰器，
// 不认识 SQLite / 写入队列 / ticker。具体实现在 infrastructure/sqlite，
// 负责节流（每把钥匙 ≥5min 一次）与异步落库，任何失败都被吞掉——
// 认证链路绝不因为「写使用记录」而变慢或失败。

import "context"

// KeyUsageRecorder 记录「某指纹刚刚认证成功」这一事实。
// 实现必须非阻塞、best-effort：调用点在 SSH 公钥认证热路径上。
type KeyUsageRecorder interface {
	NoteUse(fingerprint string)
}

// usageRecordingResolver 在身份解析成功后打一个使用标记。
// 只有「命中已绑定钥匙、得到真实用户身份」才算使用；访客（未绑定）
// 与解析失败都不记录，避免无关流量放大写库。
type usageRecordingResolver struct {
	inner Resolver
	usage KeyUsageRecorder
}

var _ Resolver = (*usageRecordingResolver)(nil)

// NewUsageRecordingResolver 给身份解析器挂上使用记录装饰器。
// 依赖为空属于装配期错误，立即 panic，不留到运行期静默丢记录。
func NewUsageRecordingResolver(inner Resolver, usage KeyUsageRecorder) Resolver {
	if inner == nil {
		panic("auth.NewUsageRecordingResolver: Resolver 不能为空")
	}
	if usage == nil {
		panic("auth.NewUsageRecordingResolver: KeyUsageRecorder 不能为空")
	}
	return &usageRecordingResolver{inner: inner, usage: usage}
}

// Resolve 保持被装饰解析器的全部语义（三状态、错误原样返回），
// 仅在成功得到用户身份时异步记一次使用。
func (r *usageRecordingResolver) Resolve(ctx context.Context, fingerprint string) (Principal, error) {
	p, err := r.inner.Resolve(ctx, fingerprint)
	if err == nil && p.Kind == KindUser && p.KeyFingerprint != "" {
		r.usage.NoteUse(p.KeyFingerprint)
	}
	return p, err
}
