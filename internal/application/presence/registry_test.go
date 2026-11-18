package presence

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func TestEnumValueDomains(t *testing.T) {
	if KindUnknown.Valid() || !KindSSH.Valid() || !KindWeb.Valid() || !KindCLI.Valid() || (Kind(9)).Valid() {
		t.Error("连接类型取值域错误")
	}
	if StateUnknown.Valid() || !StateBrowsing.Valid() || (State(9)).Valid() {
		t.Error("会话状态取值域错误")
	}
	if StateAuthenticating.Active() || StateIdle.Active() || !StateBrowsing.Active() || !StateComposing.Active() {
		t.Error("活跃判定应只认 Browsing / Composing")
	}
	if New().Timeout() != DefaultHeartbeatTimeout {
		t.Error("默认心跳超时应为 20s")
	}
	if got := New(WithTimeout(0)).Timeout(); got != DefaultHeartbeatTimeout {
		t.Errorf("非正超时应回落默认值，实际 %v", got)
	}
	if got := New(WithTimeout(5 * time.Second)).Timeout(); got != 5*time.Second {
		t.Errorf("自定义超时未生效: %v", got)
	}
}

func TestConnectOnlineAndActiveAreIndependent(t *testing.T) {
	r := New()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("s%d", i)
		if err := r.Connect(id, KindSSH, int64(i+1)); err != nil {
			t.Fatalf("连接 %s: %v", id, err)
		}
	}
	if r.Online() != 3 || r.Peak() != 3 {
		t.Fatalf("在线/峰值错误: online=%d peak=%d", r.Online(), r.Peak())
	}
	// 刚连接处于认证中，不算活跃。
	if r.Active() != 0 {
		t.Fatalf("认证中的会话不应计入活跃: %d", r.Active())
	}
	if err := r.SetState("s0", StateBrowsing); err != nil {
		t.Fatal(err)
	}
	if err := r.SetState("s1", StateComposing); err != nil {
		t.Fatal(err)
	}
	if err := r.SetState("s2", StateIdle); err != nil {
		t.Fatal(err)
	}
	if r.Active() != 2 || r.Online() != 3 {
		t.Fatalf("活跃与在线应独立统计: active=%d online=%d", r.Active(), r.Online())
	}
	if !r.Disconnect("s0") || r.Online() != 2 || r.Peak() != 3 {
		t.Fatalf("下线不应降低峰值: online=%d peak=%d", r.Online(), r.Peak())
	}
}

func TestInvalidOperations(t *testing.T) {
	r := New()
	if err := r.Connect("", KindSSH, 0); !errors.Is(err, ErrInvalidSession) {
		t.Error("空 ID 应拒绝")
	}
	if err := r.Connect("s", KindUnknown, 0); !errors.Is(err, ErrInvalidSession) {
		t.Error("非法连接类型应拒绝")
	}
	if err := r.Connect("s", KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.Connect("s", KindSSH, 0); !errors.Is(err, ErrDuplicateSession) {
		t.Error("重复 ID 应拒绝")
	}
	if err := r.SetState("s", StateUnknown); !errors.Is(err, ErrInvalidSession) {
		t.Error("非法状态应拒绝")
	}
	if err := r.Touch("missing"); !errors.Is(err, ErrUnknownSession) {
		t.Error("未知会话心跳应拒绝")
	}
	if err := r.SetState("missing", StateBrowsing); !errors.Is(err, ErrUnknownSession) {
		t.Error("未知会话改状态应拒绝")
	}
	if r.Disconnect("missing") {
		t.Error("不存在的会话下线应返回 false")
	}
}

func TestHeartbeatReapWithInjectedClock(t *testing.T) {
	clock := newFakeClock()
	r := New(WithClock(clock.Now), WithTimeout(20*time.Second))
	_ = r.Connect("a", KindSSH, 1)
	_ = r.Connect("b", KindSSH, 2)

	clock.Advance(15 * time.Second)
	if got := r.Reap(); len(got) != 0 {
		t.Fatalf("15s 内不应清理: %v", got)
	}
	clock.Advance(10 * time.Second) // 共 25s
	if err := r.Touch("a"); err != nil {
		t.Fatal(err)
	}
	got := r.Reap()
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("只应清理超时且未心跳的会话: %v", got)
	}
	if r.Online() != 1 {
		t.Fatalf("清理后在线数错误: %d", r.Online())
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("心跳过的会话应保留")
	}

	clock.Advance(21 * time.Second)
	if got := r.Reap(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("超时会话应被清理: %v", got)
	}
	if r.Online() != 0 {
		t.Fatal("全部超时后应清零")
	}
}

func TestActiveExcludesExpiredSessions(t *testing.T) {
	clock := newFakeClock()
	r := New(WithClock(clock.Now), WithTimeout(20*time.Second))
	_ = r.Connect("a", KindSSH, 1)
	_ = r.SetState("a", StateBrowsing)
	if r.Active() != 1 {
		t.Fatal("活跃会话应计入")
	}
	clock.Advance(21 * time.Second)
	if r.Active() != 0 {
		t.Fatal("心跳超时的会话不应再计入活跃")
	}
	if r.Online() != 1 {
		t.Fatal("Active 与 Online 独立：超时会话仍在连接表中，等 Reap 清理")
	}
}

func TestSnapshotIsDefensiveCopy(t *testing.T) {
	r := New()
	_ = r.Connect("b", KindSSH, 2)
	_ = r.Connect("a", KindSSH, 1)
	snap := r.Snapshot()
	if len(snap) != 2 || snap[0].ID != "a" || snap[1].ID != "b" {
		t.Fatalf("快照应按 ID 排序: %+v", snap)
	}
	snap[0].ID = "tampered"
	if got, _ := r.Get("a"); got.ID != "a" {
		t.Fatal("快照返回了内部引用")
	}
}

// 验收项：100 个模拟连接上下线，全部在内存完成。
// 写库次数为 0 由 infrastructure/sqlite 的集成测试用真实 Writer.Commits() 验证。
func TestConcurrent100Connections(t *testing.T) {
	r := New()
	const n = 100
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s%03d", i)
			if err := r.Connect(id, KindSSH, int64(i+1)); err != nil {
				t.Errorf("连接 %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	if r.Online() != n || r.Peak() != n {
		t.Fatalf("并发连接后应有 %d 条，实际 online=%d peak=%d", n, r.Online(), r.Peak())
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s%03d", i)
			_ = r.Touch(id)
			_ = r.SetState(id, StateBrowsing)
		}(i)
	}
	wg.Wait()
	if r.Active() != n {
		t.Fatalf("并发心跳后活跃数应为 %d，实际 %d", n, r.Active())
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Disconnect(fmt.Sprintf("s%03d", i))
		}(i)
	}
	wg.Wait()
	if r.Online() != 0 || r.Active() != 0 || r.Peak() != n {
		t.Fatalf("全部下线后状态错误: online=%d active=%d peak=%d", r.Online(), r.Active(), r.Peak())
	}
}
