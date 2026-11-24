package user

import (
	"context"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// ProfileView 是个人资料的展示投影（ADR-065：/whoami 与 /who 共用）。
// 只放展示所需字段：不含凭证、令牌、身份源明细、邮箱等隐私字段（二期 /finger 再议）。
type ProfileView struct {
	Username  string
	UserID    int64
	Karma     int64
	Status    domainuser.Status
	CreatedAt int64 // unix 秒，展示经 formatKeyTime 时区口径转换
}

// ProfileReader 批量读取账号展示投影。一次 IN 查询零 N+1；查不到的 ID
// 不出现在返回 map 中，调用方按缺省回退 #ID 处理（与 fillRowAuthors 同口径）。
// 独立于 StatusReader / KarmaReader：只关心状态或只关心 karma 的调用方与
// 测试 fake 不必被迫实现本方法（沿用 KarmaReader 的拆分理由，ADR-057）。
type ProfileReader interface {
	ProfilesByIDs(ctx context.Context, ids []int64) (map[int64]ProfileView, error)
}
