package forum

import (
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/domain/user"
)

// ActorFromPrincipal 把认证快照映射成用例层 Actor（P0-14）。
//
// status 由调用方从 application/user.StatusReader 取得；认证层不缓存账号状态。
// 写用例仍会在事务内用 UserStatusReader 二次复核（P0-10），因此这里只是会话
// 建立时的快照，不构成权限判定的最终依据。
func ActorFromPrincipal(p auth.Principal, status user.Status) Actor {
	if !p.IsUser() {
		return Actor{Guest: true}
	}
	return Actor{UserID: p.UserID, Status: status}
}
