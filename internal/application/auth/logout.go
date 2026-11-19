package auth

import "context"

// LogoutService 是用户主动注销的边界用例（P1-M2）。
//
// 顺序固定且不可颠倒：
//  1. 校验 userID > 0（访客无需注销）；
//  2. 先撤销该账号全部在线会话 —— 这是安全动作，必须先落地；
//  3. 再写注销审计。审计入队失败只作为错误上报，不回滚第 2 步：
//     把已经踢下线的会话「恢复」比少一条审计危险得多。
//
// 密码变更后的旧会话失效在 M2 只预留端口：当前 P0 无本地密码（邮箱包在 M5），
// 不臆造改密流程；届时复用同一个 UserSessionRevoker 即可。
type LogoutService struct {
	revoker UserSessionRevoker
	gate    *SecurityGate
}

func NewLogoutService(revoker UserSessionRevoker, gate *SecurityGate) *LogoutService {
	if revoker == nil {
		panic("auth.NewLogoutService: UserSessionRevoker 不能为空")
	}
	return &LogoutService{revoker: revoker, gate: gate}
}

// LogoutResult 是注销的确定结果。SessionsClosed 含触发注销的当前会话。
type LogoutResult struct {
	UserID         int64
	SessionsClosed int
}

func (s *LogoutService) Logout(ctx context.Context, userID int64, ipHash string) (LogoutResult, error) {
	if userID <= 0 {
		return LogoutResult{}, ErrLogoutRequiresLogin
	}
	closed := s.revoker.RevokeUserSessions(ctx, userID)
	res := LogoutResult{UserID: userID, SessionsClosed: closed}
	if err := s.gate.NoteLogout(userID, ipHash, closed); err != nil {
		return res, err // 会话已撤销，错误仅用于观测与提示
	}
	return res, nil
}
