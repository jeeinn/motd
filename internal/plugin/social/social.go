// Package social 是 v0.5.1 / P3-M6 的状态类彩蛋插件。
// 订阅 PostCreated（深夜发帖 → night_owl）与 VoteCast（首次被投票 → first_vote）。
// 遵守评审稿 §2.4 权限表：插件只有只读查询 + 申请成就两条能力，
// 不碰数据库、不改计分；成就白名单由 application/user 校验。
package social

import (
	"context"
	"fmt"
	"time"

	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/plugin/registry"
)

const pluginName = "achievement.social"

// 深夜时段：02:00–05:00（含 02、03、04 点），按 MOTD_DISPLAY_TIMEZONE 计算。
// 事件时间 OccurredAt 是 UTC 时刻，转到展示时区后取小时；这样「深夜」与用户
// 在终端看到的时间一致（v0.5.1 patch，用户建议）。Location 由 cmd 装配层注入；
// 零值为 nil 时回落 UTC（保持测试零值安全与改动前行为）。
const (
	nightStartHour = 2
	nightEndHour   = 5
)

func init() {
	registry.MustRegister(registry.Plugin{
		Name:    pluginName,
		Version: "0.1.0",
		DomainSubscriptions: []event.DomainEventType{
			event.EventPostCreated,
			event.EventVoteCast,
		},
		Handler: socialHandler{},
	})
}

type socialHandler struct{}

func (socialHandler) Handle(ctx context.Context, e event.DomainEvent, c registry.Context) error {
	switch e.Type {
	case event.EventPostCreated:
		return handleNightOwl(ctx, e, c)
	case event.EventVoteCast:
		return handleFirstVote(ctx, e, c)
	default:
		return nil
	}
}

// handleNightOwl：展示时区 02:00–05:00 发帖解锁 night_owl。
func handleNightOwl(ctx context.Context, e event.DomainEvent, c registry.Context) error {
	loc := c.Location
	if loc == nil {
		loc = time.UTC
	}
	hour := e.OccurredAt.In(loc).Hour()
	if hour < nightStartHour || hour >= nightEndHour {
		return nil
	}
	return apply(ctx, c, e.ActorID, domainuser.AchievementNightOwl, "post:%d", e.PostID)
}

// handleFirstVote：主题/回复首次被投票时给**内容作者**解锁 first_vote。
// 注意 VoteCast 的 ActorID 是投票者，作者要从只读查询取；给自己投票不解锁。
func handleFirstVote(ctx context.Context, e event.DomainEvent, c registry.Context) error {
	if c.Read == nil {
		return fmt.Errorf("插件 %s 缺少只读查询通道", pluginName)
	}
	var owner int64
	var err error
	if e.PostID != 0 {
		owner, err = c.Read.PostOwnerID(ctx, e.PostID)
	} else {
		owner, err = c.Read.ThreadOwnerID(ctx, e.ThreadID)
	}
	if err != nil {
		// 内容可能已被软删/隐藏，查不到作者时静默跳过——成就不是关键路径，
		// 不能因为插件读不到而让投票事件投递失败（outbox 会重试整条事件）。
		return nil
	}
	if owner == 0 || owner == e.ActorID {
		return nil
	}
	return apply(ctx, c, owner, domainuser.AchievementFirstVote, "vote:%s", e.ID)
}

func apply(ctx context.Context, c registry.Context, userID int64, code domainuser.AchievementCode, evidenceFmt string, args ...any) error {
	if c.Achievements == nil {
		return fmt.Errorf("插件 %s 缺少成就申请通道", pluginName)
	}
	_, err := c.Achievements.Apply(ctx, appuser.AchievementRequest{
		UserID:   userID,
		Code:     code,
		Evidence: fmt.Sprintf(evidenceFmt, args...),
	})
	return err
}
