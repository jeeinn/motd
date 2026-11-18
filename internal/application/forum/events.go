package forum

import (
	"context"

	"github.com/jeeinn/motd/internal/domain/event"
)

// DomainEventSink 是「事务提交后」的领域事件出口。
//
// 接口定义在使用方（application）：plugin/events.DomainBus 通过结构化接口
// 匹配实现它，application 因此不需要 import plugin（§2.2 单向依赖）。
// 若改用「在事务内发布」，事件可能先于提交对订阅者可见，违背 §2.4 流程图。
type DomainEventSink interface {
	Publish(ctx context.Context, e event.DomainEvent) error
}

// PublishObserver 观察事件投递失败。事务已提交，失败不能回滚业务结果，
// 因此不把错误回传给调用方，只做计数与可观测上报。
type PublishObserver func(e event.DomainEvent, err error)
