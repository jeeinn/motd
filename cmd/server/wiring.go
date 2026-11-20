package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jeeinn/motd/internal/application/admin"
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	_ "github.com/jeeinn/motd/internal/plugin/demo" // 编译期注册：首次发帖成就 + /fortune 彩蛋
	"github.com/jeeinn/motd/internal/plugin/events"
	"github.com/jeeinn/motd/internal/plugin/registry"
	_ "github.com/jeeinn/motd/internal/plugin/social" // 编译期注册：夜行者 / 首次被投票成就
	"github.com/jeeinn/motd/internal/presentation/terminal"
)

// eggRegistry 把编译期注册的插件命令转换成 command.Command 并合并进内置注册表。
// 这是 cmd 装配层的职责：command 包不 import plugin，plugin 不 import application/command
// （ADR-015），只有 cmd 同时看到两者。转换时剥掉插件命令的前导 "/"，因为
// command.validateCommand 明确拒绝 "/"（P3-M6 / v0.5.1）。
func eggRegistry(plugins []registry.Plugin) (*command.Registry, error) {
	var eggs []command.Command
	for _, p := range plugins {
		for _, pc := range p.Commands {
			name := strings.TrimPrefix(pc.Name, "/")
			if name == "" {
				continue
			}
			eggs = append(eggs, command.Command{
				Name:    name,
				Usage:   pc.Usage,
				Summary: pc.Description,
				MinArgs: 0,
				MaxArgs: 0,
			})
		}
	}
	// 兜底：把 EggCommands 里声明但插件元数据未登记的命令一并合入，
	// 避免两处清单漂移（demo 插件当前只登记 /fortune）。
	seen := make(map[string]bool, len(eggs))
	for _, e := range eggs {
		seen[e.Name] = true
	}
	for _, c := range command.EggCommands() {
		if !seen[c.Name] {
			eggs = append(eggs, c)
		}
	}
	return command.WithCommands(eggs)
}

// forumRuntime 是一次装配的产物，抽出来便于端到端集成测试直接复用真实链路。
type forumRuntime struct {
	Service  *forum.Service
	Bus      *events.DomainBus
	Outbox   *forum.Outbox
	Store    *sqlite.AchievementStore // v0.5.0：成就持久化；内存占位实现已移除
	Presence *presence.Registry
	Plugins  []registry.Plugin
}

// newForumRuntime 把「Service → 事件总线 → 插件 → 成就校验」接起来。
//
// 装配边界：cmd 是唯一可以同时看到 application / plugin / infrastructure 的地方；
// application 不 import plugin（DomainEventSink 由 DomainBus 结构化匹配实现），
// plugin 不 import infrastructure（受限上下文由 cmd 注入）。
func newForumRuntime(pools *sqlite.Pools, writer *sqlite.Writer,
	onPublishError func(event.DomainEvent, error),
	adminFingerprints map[string]struct{}, auditLogger *auth.AuditLogger,
) (*forumRuntime, error) {
	repos := sqlite.NewForumRepos(pools, writer)

	// v0.5.0：成就落 SQLite，进程重启不丢（ADR-056）。写经唯一写队列，读走读池。
	store := sqlite.NewAchievementStore(pools, writer)
	gate := appuser.NewAchievementService(store, sqlite.NewUserStatusReader(pools), time.Now)
	pluginCtx := registry.Context{
		Read:         forum.NewPublicQuery(repos, repos),
		Achievements: gate,
		// v0.5.1 patch：注入 MOTD_DISPLAY_TIMEZONE，供 night_owl
		// 按用户可见的本地时段判断（main 已在 newForumRuntime 之前 SetDisplayLocation）。
		Location: terminal.DisplayLocation(),
	}

	bus := events.NewDomainBus()
	// P2-M1：治理事件经 outbox 提交后投递到 auth_events 的异步审计通道。
	// 审计器由进程持有（ticker 在 main），这里只注册提交后的桥接订阅者。
	if auditLogger != nil {
		if err := bus.Subscribe(event.EventAdminAction, admin.AuditBridge(auditLogger)); err != nil {
			return nil, fmt.Errorf("装配治理审计订阅者: %w", err)
		}
	}
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
	// R12：事件先随业务写落 outbox，提交后由调度器投递给同一条总线。
	// cmd 是唯一能同时看到 application / plugin / infrastructure 的装配点。
	outbox, err := forum.NewOutbox(sqlite.NewOutboxStore(writer), bus, obs)
	if err != nil {
		return nil, fmt.Errorf("装配 outbox 调度器: %w", err)
	}
	svc := forum.NewService(repos,
		forum.WithEventSink(bus),
		forum.WithPublishObserver(obs),
		forum.WithOutbox(outbox),
		forum.WithAdminWhitelist(adminFingerprints),
	)
	return &forumRuntime{
		Service:  svc,
		Bus:      bus,
		Outbox:   outbox,
		Store:    store,
		Presence: presence.New(), // 纯内存；SSH 入口在 M3 才往里登记连接
		Plugins:  plugins,
	}, nil
}
