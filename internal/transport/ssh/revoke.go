package ssh

import (
	"context"

	"github.com/jeeinn/motd/internal/application/auth"
)

// Server 实现 application/auth.SessionRevoker（P0-18）。
//
// 端口定义在使用方（application），实现落在 transport：只有 SSH Server 握有
// 全部 *Session 且能关闭底层 channel。会话撤销是纯内存动作，不写库、不占
// 写队列（§2.6 / ADR-016 同一口径）。
var _ auth.SessionRevoker = (*Server)(nil)

// RevokeSessions 关闭所有仍在使用该指纹的在线会话，返回关闭数量。
//
// 调用时机由 application 层保证：**只在解绑事务提交成功之后**。若放进写事务
// 闭包，事务回滚时会话已经被踢掉且无法复原（archtest 对此有静态守卫）。
// 先在锁内收集目标，再在锁外 Close，避免 Close 触发的 handler 退出路径
// 反过来调用 trackSession 时与锁形成重入。
func (s *Server) RevokeSessions(_ context.Context, fingerprint string) int {
	if fingerprint == "" {
		return 0
	}
	s.sessMu.Lock()
	var targets []*Session
	for _, sess := range s.sessions {
		if sess.Fingerprint() == fingerprint {
			targets = append(targets, sess)
		}
	}
	s.sessMu.Unlock()

	n := 0
	for _, sess := range targets {
		_ = sess.Close() // 幂等；同时唤醒 handler 的 <-sess.Done()
		n++
	}
	return n
}

// trackSession 登记 / 注销会话撤销索引。与 conns 分开一把锁，避免锁环。
func (s *Server) trackSession(sess *Session, add bool) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if add {
		s.sessions[sess.ID()] = sess
	} else {
		delete(s.sessions, sess.ID())
	}
}
