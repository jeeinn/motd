// 成就申请是插件唯一的写路径：插件提交申请，由本层校验账号状态、
// 成就白名单与重复解锁，再经 AchievementStore 落地（评审稿 §2.4）。
// P0 的 Store 是内存占位，不写 SQLite —— 成就系统属 P1 范围。
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

var (
	ErrInvalidAchievement = errors.New("成就代码不合法")
	ErrAchievementDenied  = errors.New("成就申请被拒绝")
)

// AchievementRequest 是插件提交的申请。Evidence 只作诊断记录，不参与判定。
type AchievementRequest struct {
	UserID   int64
	Code     domainuser.AchievementCode
	Evidence string
}

// AchievementDecision 是校验结果。业务拒绝用 Reason 表达，不伪装成系统错误。
type AchievementDecision struct {
	Granted bool
	Already bool
	Reason  string
}

// AchievementGate 是暴露给插件的写路径接口。
// plugin/registry.Context 直接持有它，插件拿不到 Store 或任何数据库句柄。
type AchievementGate interface {
	Apply(ctx context.Context, req AchievementRequest) (AchievementDecision, error)
}

// AchievementStore 是成就落地的端口（接口定义在使用方）。
// P0 用内存实现，P1 换成 achievements 表的持久化实现，本层逻辑不变。
type AchievementStore interface {
	Has(ctx context.Context, userID int64, code domainuser.AchievementCode) (bool, error)
	Grant(ctx context.Context, userID int64, code domainuser.AchievementCode, at time.Time) error
	GrantedCount() int
}

// StatusReader 读取账号当前状态。权限判断必须基于 DB 现值，不用会话快照。
type StatusReader interface {
	Status(ctx context.Context, userID int64) (domainuser.Status, error)
}

// KarmaReader 读取用户 karma（v0.5.0 / ADR-057）。独立端口而非扩 StatusReader，
// 避免所有只关心状态的调用方与测试 fake 被迫实现无关方法。karma 只用于显示等级。
type KarmaReader interface {
	Karma(ctx context.Context, userID int64) (int64, error)
}

// AchievementService 校验并执行成就申请。
type AchievementService struct {
	store AchievementStore
	users StatusReader
	now   func() time.Time
}

func NewAchievementService(store AchievementStore, users StatusReader, now func() time.Time) *AchievementService {
	if now == nil {
		now = time.Now
	}
	return &AchievementService{store: store, users: users, now: now}
}

// Apply 的四道校验：代码白名单 → 用户 ID → 账号 active → 未解锁。
// 任一不过都不写 Store；重复申请返回 Already=true 而非错误（幂等）。
func (s *AchievementService) Apply(ctx context.Context, req AchievementRequest) (AchievementDecision, error) {
	if !req.Code.Valid() {
		return AchievementDecision{}, fmt.Errorf("%w: %q", ErrInvalidAchievement, req.Code)
	}
	if req.UserID <= 0 {
		return AchievementDecision{}, fmt.Errorf("%w: 非法用户 ID", ErrAchievementDenied)
	}
	status, err := s.users.Status(ctx, req.UserID)
	if err != nil {
		return AchievementDecision{}, fmt.Errorf("读取账号状态: %w", err)
	}
	if !status.CanPost() {
		return AchievementDecision{Reason: "账号状态 " + string(status) + " 不允许解锁成就"}, nil
	}
	has, err := s.store.Has(ctx, req.UserID, req.Code)
	if err != nil {
		return AchievementDecision{}, fmt.Errorf("查询成就: %w", err)
	}
	if has {
		return AchievementDecision{Already: true, Reason: "成就已解锁"}, nil
	}
	if err := s.store.Grant(ctx, req.UserID, req.Code, s.now().UTC()); err != nil {
		return AchievementDecision{}, fmt.Errorf("写入成就: %w", err)
	}
	return AchievementDecision{Granted: true}, nil
}
