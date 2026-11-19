package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryAuditSink struct {
	mu      sync.Mutex
	batches [][]AuditEvent
	fail    error
}

func (s *memoryAuditSink) Flush(_ context.Context, events []AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.batches = append(s.batches, append([]AuditEvent(nil), events...))
	return nil
}

func (s *memoryAuditSink) all() []AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuditEvent
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

func TestAuditRingOverflowFailsClosedAndCountsDropped(t *testing.T) {
	sink := &memoryAuditSink{}
	l := NewAuditLogger(sink, WithAuditCapacity(3))
	for i := 0; i < 3; i++ {
		if err := l.Record(AuditLoginFailure, int64(i+1), "github", "ip-hash", `{}`); err != nil {
			t.Fatalf("第 %d 条应入队: %v", i+1, err)
		}
	}
	if err := l.Record(AuditLoginFailure, 99, "github", "ip-hash", `{}`); !errors.Is(err, ErrAuditQueueFull) {
		t.Fatalf("缓冲满必须 fail-closed，实际 %v", err)
	}
	depth, dropped, _, _ := l.Stats()
	if depth != 3 || dropped != 1 {
		t.Fatalf("depth=%d dropped=%d", depth, dropped)
	}
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := sink.all()
	if len(all) != 3 {
		t.Fatalf("最多落库 3 条，实际 %d", len(all))
	}
	for i, ev := range all {
		if ev.UserID != int64(i+1) || ev.Event != AuditLoginFailure {
			t.Fatalf("第 %d 条事件顺序/内容错误: %+v", i, ev)
		}
	}
	if depth, _, flushed, _ := l.Stats(); depth != 0 || flushed != 3 {
		t.Fatalf("flush 后 depth=%d flushed=%d", depth, flushed)
	}
}

func TestAuditFlushFailureRetainsEventsUntilRetry(t *testing.T) {
	sink := &memoryAuditSink{fail: errors.New("db down")}
	l := NewAuditLogger(sink, WithAuditCapacity(4))
	if err := l.Record(AuditLogout, 7, "github", "ip", `{"sessions_closed":2}`); err != nil {
		t.Fatal(err)
	}
	if err := l.Flush(context.Background()); err == nil {
		t.Fatal("落库失败应返回错误")
	}
	if depth, _, _, failures := l.Stats(); depth != 1 || failures != 1 {
		t.Fatalf("失败时事件必须留在队列里: depth=%d failures=%d", depth, failures)
	}
	sink.fail = nil
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.all()) != 1 {
		t.Fatalf("重试后应落库 1 条，实际 %d", len(sink.all()))
	}
}

func TestAuditUsesInjectedClockAndClampsFields(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	sink := &memoryAuditSink{}
	l := NewAuditLogger(sink, WithAuditClock(func() time.Time { return at }))
	longDetail := string(make([]byte, maxAuditDetailBytes+64))
	if err := l.Record(AuditKeyBound, 1, "abcdefghij", "ip", longDetail); err != nil {
		t.Fatal(err)
	}
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := sink.all()[0]
	if !ev.CreatedAt.Equal(at) {
		t.Fatalf("CreatedAt 应使用注入时钟: %v", ev.CreatedAt)
	}
	if len(ev.DetailJSON) != maxAuditDetailBytes {
		t.Fatalf("detail 应截断到 %d 字节，实际 %d", maxAuditDetailBytes, len(ev.DetailJSON))
	}
	if ev.Provider != "abcdefghij" {
		t.Fatalf("短 provider 不应被截断: %q", ev.Provider)
	}
}

func TestAuditStopDrainsFinalBatch(t *testing.T) {
	sink := &memoryAuditSink{}
	l := NewAuditLogger(sink, WithAuditFlushInterval(time.Hour)) // 不依赖真实等待，只靠 Stop 排空
	for i := 0; i < 5; i++ {
		if err := l.Record(AuditDeviceStart, 0, "github", "ip", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	l.Stop()
	if got := len(sink.all()); got != 5 {
		t.Fatalf("Stop 应排空最后一批，实际落库 %d 条", got)
	}
	// Stop 幂等，重复调用不 panic、不重复落库。
	l.Stop()
	if got := len(sink.all()); got != 5 {
		t.Fatalf("重复 Stop 不应重复落库，实际 %d", got)
	}
}

func TestAuditTickFlushesBatchAndIntervalIsConfigurable(t *testing.T) {
	sink := &memoryAuditSink{}
	l := NewAuditLogger(sink, WithAuditFlushInterval(250*time.Millisecond))
	if l.Interval() != 250*time.Millisecond {
		t.Fatalf("Interval=%v", l.Interval())
	}
	if err := l.Record(AuditSessionRevoked, 3, "github", "ip", `{}`); err != nil {
		t.Fatal(err)
	}
	l.Tick(context.Background())
	if got := len(sink.all()); got != 1 {
		t.Fatalf("Tick 应批量落库一条，实际 %d", got)
	}
	// 空队列的 Tick 是幂等的，不重复投递。
	l.Tick(context.Background())
	if got := len(sink.all()); got != 1 {
		t.Fatalf("空队列 Tick 不应重复落库，实际 %d", got)
	}
}

func TestAuditNilSinkPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("空 sink 属于装配错误，必须立即 panic")
		}
	}()
	_ = NewAuditLogger(nil)
}

func TestAuditSecurityGateFailClosedOrder(t *testing.T) {
	sink := &memoryAuditSink{}
	audit := NewAuditLogger(sink, WithAuditCapacity(1))
	limiter := NewRateLimiter()
	gate := NewSecurityGate(limiter, audit, "github")

	if err := gate.NoteLoginFailure("ip-a", ""); err != nil {
		t.Fatal(err)
	}
	// 审计环满 → 后续认证动作即使限流额度还在也必须被拒。
	if err := gate.NoteLoginFailure("ip-b", ""); !errors.Is(err, ErrAuditQueueFull) {
		t.Fatalf("审计溢出应 fail-closed，实际 %v", err)
	}
	if err := gate.AllowDeviceStart("ip-a"); err != nil {
		t.Fatalf("不限流的动作不应被审计计数牵连: %v", err)
	}
	// 一个 login_failure:ip-a 键 + 一个 device_start:ip-a 键；
	// 被审计拒绝的 login_failure:ip-b 不应产生第三个键。
	if keys := limiter.Keys(); keys != 2 {
		t.Fatalf("被审计拒绝的动作不应计入限流，实际 keys=%d", keys)
	}
	limiter.Reset(LimitLoginFailure, scopeOf("ip-b", ""))
	if keys := limiter.Keys(); keys != 2 {
		t.Fatalf("被拒绝的 ip-b 本就不该有键，Reset 后不应变化: %d", keys)
	}
	if _, dropped, _, _ := audit.Stats(); dropped != 1 {
		t.Fatalf("dropped 应为 1，实际 %d", dropped)
	}
}

func TestAuditSecurityGateOptionalDependencies(t *testing.T) {
	gate := NewSecurityGate(nil, nil, "github")
	if gate.Enabled() {
		t.Fatal("两个依赖都为空时不应声称已启用")
	}
	if err := gate.NoteLoginFailure("ip", ""); err != nil {
		t.Fatal(err)
	}
	if err := gate.NoteKeyAttempt(AuditKeyBound, 1, "ip"); err != nil {
		t.Fatal(err)
	}
	if err := gate.NoteLogout(1, "ip", 0); err != nil {
		t.Fatal(err)
	}
	if err := gate.AllowDeviceStart("ip"); err != nil {
		t.Fatal(err)
	}
	if err := gate.NoteSessionRevoked(1, "ip", 0); err != nil {
		t.Fatal(err)
	}
}
