package events

import (
	"sync"
	"sync/atomic"
	"time"
)

// InteractionKind 是交互事件种类。与领域事件完全分开，不共用枚举。
type InteractionKind uint8

const (
	InteractionUnknown InteractionKind = iota
	InteractionConnected
	InteractionDisconnected
	InteractionKeyPressed
	InteractionIdle
)

func (k InteractionKind) Valid() bool {
	return k >= InteractionConnected && k <= InteractionIdle
}

// InteractionEvent 是短生命周期的交互信号。允许丢失，绝不持久化。
type InteractionEvent struct {
	Kind       InteractionKind
	SessionID  string
	OccurredAt time.Time
	Detail     string
}

// InteractionBus 用带缓冲 channel 做非阻塞投递：满了就丢，不去抢写通道，
// 也不会因为消费慢而拖住 SSH 输入循环（§2.6：心跳/在线状态不写 SQLite）。
type InteractionBus struct {
	ch       chan InteractionEvent
	observer func(InteractionEvent)
	stop     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
	sendMu   sync.Mutex // 与 Close 的排空竞态：保证关闭瞬间不会有半途 send
	closed   atomic.Bool
	dropped  atomic.Int64
	handled  atomic.Int64
	accepted atomic.Int64
}

// NewInteractionBus 创建通道；capacity <= 0 时取 64。
// observer 在消费 goroutine 内被调用，可为 nil（此时只计数）。
func NewInteractionBus(capacity int, observer func(InteractionEvent)) *InteractionBus {
	if capacity <= 0 {
		capacity = 64
	}
	b := &InteractionBus{
		ch:       make(chan InteractionEvent, capacity),
		observer: observer,
		stop:     make(chan struct{}),
	}
	b.wg.Add(1)
	go b.loop()
	return b
}

func (b *InteractionBus) loop() {
	defer b.wg.Done()
	handle := func(e InteractionEvent) {
		b.handled.Add(1)
		if b.observer != nil {
			b.observer(e)
		}
	}
	for {
		select {
		case e := <-b.ch:
			handle(e)
		case <-b.stop:
			// 退出前把已投递事件排空，使 Close 之后 Handled() 是确定值。
			for {
				select {
				case e := <-b.ch:
					handle(e)
				default:
					return
				}
			}
		}
	}
}

// TryPublish 非阻塞投递；队列满或已关闭时返回 false 并计入 Dropped。
func (b *InteractionBus) TryPublish(e InteractionEvent) bool {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	if b.closed.Load() {
		b.dropped.Add(1)
		return false
	}
	select {
	case b.ch <- e:
		b.accepted.Add(1)
		return true
	default:
		b.dropped.Add(1)
		return false
	}
}

// Close 停止消费并排空在途事件；重复调用安全。
// 先取 sendMu 再置 closed，确保关闭瞬间不会有「已通过检查但尚未入队」的事件。
func (b *InteractionBus) Close() {
	b.once.Do(func() {
		b.sendMu.Lock()
		b.closed.Store(true)
		b.sendMu.Unlock()
		close(b.stop)
		b.wg.Wait()
	})
}

func (b *InteractionBus) Dropped() int64  { return b.dropped.Load() }
func (b *InteractionBus) Handled() int64  { return b.handled.Load() }
func (b *InteractionBus) Accepted() int64 { return b.accepted.Load() }
func (b *InteractionBus) Capacity() int   { return cap(b.ch) }
