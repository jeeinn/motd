package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	_ "github.com/jeeinn/motd/internal/plugin/demo" // 编译期注册：init() 把彩蛋插件放进注册表
	"github.com/jeeinn/motd/internal/plugin/events"
	"github.com/jeeinn/motd/internal/plugin/registry"
)

// forumRuntime 是一次装配的产物，抽出来便于端到端集成测试直接复用真实链路。
type forumRuntime struct {
	Service *forum.Service
	Bus     *events.DomainBus
	Store   *appuser.MemoryAchievementStore
	Plugins []registry.Plugin
}

// newForumRuntime 把「Service → 事件总线 → 插件 → 成就校验」接起来。
//
// 装配边界：cmd 是唯一可以同时看到 application / plugin / infrastructure 的地方；
// application 不 import plugin（DomainEventSink 由 DomainBus 结构化匹配实现），
// plugin 不 import infrastructure（受限上下文由 cmd 注入）。
func newForumRuntime(pools *sqlite.Pools, writer *sqlite.Writer, onPublishError func(event.DomainEvent, error)) (*forumRuntime, error) {
	repos := sqlite.NewForumRepos(pools, writer)

	store := appuser.NewMemoryAchievementStore() // P0 占位：不写 SQLite
	gate := appuser.NewAchievementService(store, sqlite.NewUserStatusReader(pools), time.Now)
	pluginCtx := registry.Context{
		Read:         forum.NewPublicQuery(repos),
		Achievements: gate,
	}

	bus := events.NewDomainBus()
	plugins := registry.All()
	for _, p := range plugins {
		p := p
		for _, t := range p.DomainSubscriptions {
			if err := bus.Subscribe(t, func(ctx context.Context, e event.DomainEvent) error {
				return p.Handler.Handle(ctx, e, pluginCtx)
			}); err != nil {
				return nil, fmt.Errorf("装配插件 %q: %w", p.Name, err)
			}
		}
	}

	var obs forum.PublishObserver
	if onPublishError != nil {
		obs = func(e event.DomainEvent, err error) { onPublishError(e, err) }
	}
	svc := forum.NewService(repos,
		forum.WithEventSink(bus),
		forum.WithPublishObserver(obs),
	)
	return &forumRuntime{Service: svc, Bus: bus, Store: store, Plugins: plugins}, nil
}
