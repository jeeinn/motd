// Package demo 是一个编译期注册的彩蛋插件（评审稿 v0.2 §3.4）。
//
// 它订阅 PostCreated，通过受限只读接口取主题标题作为 evidence，
// 再向 application/user 申请「首次发帖」成就。插件不接触数据库：
// 唯一写路径是 Context.Achievements，且由 application 层校验后落地。
package demo

import (
	"context"
	"fmt"

	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/plugin/registry"
)

const pluginName = "achievement.first-post"

func init() {
	registry.MustRegister(registry.Plugin{
		Name:    pluginName,
		Version: "0.1.0",
		Commands: []registry.Command{
			{Name: "/fortune", Usage: "/fortune", Description: "随机输出一句箴言（M3 接入命令分发）"},
		},
		DomainSubscriptions: []event.DomainEventType{event.EventPostCreated},
		Handler:             firstPostHandler{},
	})
}

type firstPostHandler struct{}

func (firstPostHandler) Handle(ctx context.Context, e event.DomainEvent, c registry.Context) error {
	if e.Type != event.EventPostCreated {
		return nil
	}
	if c.Achievements == nil {
		return fmt.Errorf("插件 %s 缺少成就申请通道", pluginName)
	}
	if c.Read == nil {
		return fmt.Errorf("插件 %s 缺少只读查询通道", pluginName)
	}

	// 演示受限只读接口：只取标题这一公开语义字段，拿不到用户邮箱、凭证或连接句柄。
	title, err := c.Read.ThreadTitle(ctx, e.ThreadID)
	if err != nil {
		return fmt.Errorf("读取主题标题: %w", err)
	}
	_, err = c.Achievements.Apply(ctx, appuser.AchievementRequest{
		UserID:   e.ActorID,
		Code:     domainuser.AchievementFirstPost,
		Evidence: fmt.Sprintf("post:%d in %q", e.PostID, title),
	})
	return err
}
