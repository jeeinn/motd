package forum

// 管理员白名单的线程安全容器（R-15 / ADR-079）。
//
// 背景：SIGHUP 热重载会在运行中替换白名单，而多个 SSH 会话并发读它做治理判定。
// 直接用 map 字段会数据竞争。这里用 atomic.Pointer 持有不可变快照：
// 写方构造新 map 后一次 Store 换入，读方 Load 拿到的是永不修改的快照，
// 因此可以安全传给 admin.CanAdmin（其签名与 archtest 单点守卫保持不变）。
//
// 失败关闭：未注入（nil 快照）视为空名单，所有治理用例一律拒绝。

import "github.com/jeeinn/motd/internal/application/auth"

// adminWhitelistSnapshot 是不可变的白名单快照。发布后绝不修改其中的 map。
type adminWhitelistSnapshot struct {
	fingerprints map[string]struct{}
}

// currentAdminWhitelist 返回当前白名单的只读 map；未注入时返回 nil。
// CanAdmin 对 nil map 返回 false（fail-closed）。
func (s *Service) currentAdminWhitelist() map[string]struct{} {
	snap := s.adminWhitelist.Load()
	if snap == nil {
		return nil
	}
	return snap.fingerprints
}

// SetAdminWhitelist 原子换入一份新白名单。
// 传入的 map 会被复制，调用方后续修改不会影响已发布快照。
// 返回 error 仅为与配置重载链路对齐（校验在 config 层已完成），
// 本层不接受非法指纹——空 map 合法（表示撤销全部管理员）。
func (s *Service) SetAdminWhitelist(whitelist map[string]struct{}) error {
	next := make(map[string]struct{}, len(whitelist))
	for fp := range whitelist {
		if !auth.ValidFingerprint(fp) {
			return ErrAdminDenied
		}
		next[fp] = struct{}{}
	}
	s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: next})
	return nil
}
