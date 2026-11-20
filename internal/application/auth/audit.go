// 认证安全审计（P1-M2）。
//
// 审计写入绝不在认证临界路径上：Record 只做「入环形缓冲」这一件内存动作，
// 落库由 cmd 持有的 1s ticker 调用 Tick，批量交给 AuditSink
// （infrastructure/sqlite 实现，内部走 Writer.Submit）。application 层自身
// 不起 goroutine / ticker（P0-16 archtest 守卫对全包生效）。
// archtest 静态守卫会检查 Record 体内没有 Flush / Submit / SQL 调用，
// 防止有人把同步落库偷偷塞回认证链路。
//
// 缓冲写满即 fail-closed：Record 返回 ErrAuditQueueFull，调用方必须拒绝本次
// 认证动作；同时 dropped 计数递增供 /healthz 观测。这是 D-a 的拍板结论。
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// 审计事件名是固定常量集，使用方不得散落字符串字面量。
// 新增事件必须在这里登记，便于运维按事件名做告警与报表。
const (
	AuditLoginFailure        = "login_failure"
	AuditVerificationFailure = "verification_failure"
	AuditDeviceStart         = "device_start"
	AuditDeviceDenied        = "device_denied"
	AuditKeyBound            = "key_bound"
	AuditKeyUnbound          = "key_unbound"
	AuditSessionRevoked      = "session_revoked"
	AuditLogout              = "logout"
	// AuditAdminAction 是 P2-M1 节点治理动作的审计事件名。
	// 治理事件经 forum outbox 提交后投递到这里，与其它认证审计共用 auth_events 通道。
	AuditAdminAction = "admin_action"
)

// DefaultAuditCapacity / DefaultAuditFlushInterval 与 config 包同名默认值一致。
const (
	DefaultAuditCapacity      = 512
	DefaultAuditFlushInterval = 1 * time.Second
	maxAuditDetailBytes       = 2048
	maxAuditProviderRunes     = 32
	maxAuditIPHashRunes       = 128
)

// ErrAuditQueueFull 表示审计环形缓冲已满，本次认证动作必须被拒绝。
var ErrAuditQueueFull = errors.New("审计队列已满，认证动作已拒绝")

// AuditEvent 是一条审计记录。CreatedAt 由 AuditLogger 用注入时钟生成，
// 不接受调用方传入，避免外部时间污染排序。
type AuditEvent struct {
	Event      string
	UserID     int64
	Provider   string
	IPHash     string
	DetailJSON string
	CreatedAt  time.Time
}

// AuditSink 是批量落库端口。端口定义在使用方（ADR-001），
// 实现落在 infrastructure/sqlite；application 因此不 import database/sql。
type AuditSink interface {
	Flush(ctx context.Context, events []AuditEvent) error
}

// AuditOption 注入容量 / 周期 / 时钟，全部可测且无须真实等待 1 秒。
type AuditOption func(*AuditLogger)

func WithAuditCapacity(n int) AuditOption {
	return func(l *AuditLogger) {
		if n > 0 {
			l.capacity = n
		}
	}
}

func WithAuditFlushInterval(d time.Duration) AuditOption {
	return func(l *AuditLogger) {
		if d > 0 {
			l.interval = d
		}
	}
}

func WithAuditClock(f func() time.Time) AuditOption {
	return func(l *AuditLogger) {
		if f != nil {
			l.now = f
		}
	}
}

// AuditLogger 是固定容量的环形缓冲。head 指向最早元素，count 为在队元素数，
// 二者都在 mu 下读写。flush 成功后按成功数量推进 head。
type AuditLogger struct {
	sink     AuditSink
	buf      []AuditEvent
	head     int
	count    int
	capacity int
	interval time.Duration
	now      func() time.Time

	mu sync.Mutex
	// flushMu 串行化整批「取快照 → 落库 → 推进队头」，让 cmd 的定时 Tick
	// 与停机时的 Stop 不会把同一批事件重复投递。
	flushMu       sync.Mutex
	stopOnce      sync.Once
	dropped       atomic.Int64
	flushed       atomic.Int64
	flushFailures atomic.Int64
}

// NewAuditLogger 构造审计器；sink 为空直接 panic，避免运行期「静默不审计」。
func NewAuditLogger(sink AuditSink, opts ...AuditOption) *AuditLogger {
	if sink == nil {
		panic("auth.NewAuditLogger: AuditSink 不能为空")
	}
	l := &AuditLogger{
		sink:     sink,
		capacity: DefaultAuditCapacity,
		interval: DefaultAuditFlushInterval,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(l)
	}
	l.buf = make([]AuditEvent, l.capacity)
	return l
}

// Record 只入队、不落库。溢出时 dropped++ 并返回 ErrAuditQueueFull，
// 由调用方（安全门）拒绝本次认证动作。
//
// 注意：本函数体内刻意不出现 sink / Submit / Flush / SQL 相关调用，
// 这是「审计不进认证临界路径」的可机械验证约束（见 archtest/auth_security_test.go）。
func (l *AuditLogger) Record(event string, userID int64, provider, ipHash, detail string) error {
	ev := AuditEvent{
		Event:      event,
		UserID:     userID,
		Provider:   clampRunes(provider, maxAuditProviderRunes),
		IPHash:     clampRunes(ipHash, maxAuditIPHashRunes),
		DetailJSON: clampBytes(detail, maxAuditDetailBytes),
		CreatedAt:  l.now().UTC(),
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count >= l.capacity {
		l.dropped.Add(1)
		return ErrAuditQueueFull
	}
	idx := (l.head + l.count) % l.capacity
	l.buf[idx] = ev
	l.count++
	return nil
}

// Tick 是一次定时批量落库尝试。**节奏由 cmd 进程持有**：application 层不
// 起 goroutine、不起 ticker（P0-16 的 archtest 守卫对全包生效），与 Device Flow
// 状态机「不持有轮询节奏」保持同一口径。cmd 在 ctx 取消后先等 ticker goroutine
// 退出，再调 Stop，保证清理到达静止。
func (l *AuditLogger) Tick(ctx context.Context) { _ = l.Flush(ctx) }

// Interval 返回装配层应使用的 flush 周期。
func (l *AuditLogger) Interval() time.Duration { return l.interval }

// Capacity 环形缓冲容量，供 /healthz 展示水位。
func (l *AuditLogger) Capacity() int64 { return int64(l.capacity) }

// Stop 做停机前的最后一次落库，幂等。调用方必须先让 Tick 循环退出，
// 否则最后一批可能被并发投递两次；flushMu 保证不会数据竞争。
func (l *AuditLogger) Stop() {
	l.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.Flush(ctx)
	})
}

// Flush 把当前队列快照交给 sink。失败时保留队头事件等下次重试：
// 持续失败会把缓冲填满，进而触发 Record 的 fail-closed，这是期望的安全行为。
func (l *AuditLogger) Flush(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()
	if l.count == 0 {
		l.mu.Unlock()
		return nil
	}
	n := l.count
	batch := make([]AuditEvent, n)
	for i := 0; i < n; i++ {
		batch[i] = l.buf[(l.head+i)%l.capacity]
	}
	l.mu.Unlock()

	if err := l.sink.Flush(ctx, batch); err != nil {
		l.flushFailures.Add(1)
		return fmt.Errorf("审计批量落库: %w", err)
	}

	l.mu.Lock()
	remove := n
	if remove > l.count {
		remove = l.count // flush 期间新入队的事件仍留在队头之后
	}
	l.head = (l.head + remove) % l.capacity
	l.count -= remove
	l.mu.Unlock()
	l.flushed.Add(int64(remove))
	return nil
}

// Stats 供 /healthz 观测：depth 当前队列深度，dropped 溢出拒绝数，
// flushed 成功落库条数，failures 落库失败次数。
func (l *AuditLogger) Stats() (depth, dropped, flushed, failures int64) {
	l.mu.Lock()
	depth = int64(l.count)
	l.mu.Unlock()
	return depth, l.dropped.Load(), l.flushed.Load(), l.flushFailures.Load()
}

// clampRunes / clampBytes 防止外部字段把审计行撑爆；IP 与邮箱在调用方已哈希，
// 这里只做长度收口，不做内容改写（审计要忠实记录上送的哈希值）。
func clampRunes(s string, max int) string {
	n := 0
	for i, r := range s {
		if n == max {
			return s[:i]
		}
		_ = r
		n++
	}
	return s
}

func clampBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
