// Package events 实现评审稿 v0.2 §2.4 的两条事件通道。
//
// 领域事件：同步、可靠、事务提交后发布；订阅者失败会被收集并返回，
// 不静默吞掉。交互事件：带缓冲 channel、非阻塞投递、满即丢弃，允许丢失。
// 两条通道不共用总线、不共用类型、不共用投递语义。
package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jeeinn/motd/internal/domain/event"
)

// Handler 是领域事件的订阅者。用函数类型而非接口，让装配层可以把
// plugin/registry 的受限上下文通过闭包绑定进去，events 包因此不必依赖 registry。
type Handler func(ctx context.Context, e event.DomainEvent) error

// DomainBus 是领域事件总线：同步派发，进程内不丢事件。
// P0 的「可靠」边界是单进程内；崩溃导致的丢失需 P1 补事务内 outbox 表（见 ADR-015）。
type DomainBus struct {
	mu        sync.RWMutex
	handlers  map[event.DomainEventType][]Handler
	published atomic.Int64
	failures  atomic.Int64
}

func NewDomainBus() *DomainBus {
	return &DomainBus{handlers: make(map[event.DomainEventType][]Handler)}
}

// Subscribe 注册一个订阅者。非法事件种类与 nil handler 直接拒绝，避免运行时静默失效。
func (b *DomainBus) Subscribe(t event.DomainEventType, h Handler) error {
	if !t.Valid() {
		return fmt.Errorf("非法领域事件种类: %d", t)
	}
	if h == nil {
		return fmt.Errorf("订阅者不能为空")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[t] = append(b.handlers[t], h)
	return nil
}

// Publish 同步派发给该类型的全部订阅者。
// 它满足 application/forum.DomainEventSink（结构化接口匹配，无需 import application）。
func (b *DomainBus) Publish(ctx context.Context, e event.DomainEvent) error {
	if err := e.Validate(); err != nil {
		b.failures.Add(1)
		return err
	}
	b.published.Add(1)

	b.mu.RLock()
	list := append([]Handler(nil), b.handlers[e.Type]...)
	b.mu.RUnlock()

	var errs []error
	for _, h := range list {
		if err := h(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		b.failures.Add(1)
		return errors.Join(errs...)
	}
	return nil
}

// Published 是成功校验并派发的事件数；Failures 是派发/校验失败次数。
// 两者用于运维观测：事件投递失败不能改变已提交事务的结果，但必须可见。
func (b *DomainBus) Published() int64 { return b.published.Load() }
func (b *DomainBus) Failures() int64  { return b.failures.Load() }

// SubscriberCount 供装配自检使用。
func (b *DomainBus) SubscriberCount(t event.DomainEventType) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.handlers[t])
}
