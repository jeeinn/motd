package user

import (
	"context"
	"errors"
	"testing"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

type fakeStatusReader struct {
	status domainuser.Status
	err    error
}

func (f fakeStatusReader) Status(context.Context, int64) (domainuser.Status, error) {
	return f.status, f.err
}

func newTestGate(t *testing.T) (*AchievementService, *MemoryAchievementStore, fakeStatusReader) {
	t.Helper()
	store := NewMemoryAchievementStore()
	reader := fakeStatusReader{status: domainuser.StatusActive}
	clock := func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	return NewAchievementService(store, reader, clock), store, reader
}

func TestAchievementGrantedOnce(t *testing.T) {
	ctx := context.Background()
	gate, store, _ := newTestGate(t)
	req := AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost, Evidence: "post:1"}

	first, err := gate.Apply(ctx, req)
	if err != nil || !first.Granted || first.Already {
		t.Fatalf("首次申请应授予: %+v err=%v", first, err)
	}
	if store.GrantedCount() != 1 {
		t.Fatalf("应落 1 条记录，实际 %d", store.GrantedCount())
	}
	entries := store.Entries()
	if len(entries) != 1 || entries[0].UserID != 1 || entries[0].Code != domainuser.AchievementFirstPost {
		t.Fatalf("记录内容错误: %+v", entries)
	}

	second, err := gate.Apply(ctx, req)
	if err != nil || second.Granted || !second.Already {
		t.Fatalf("重复申请应返回 Already 而非报错: %+v err=%v", second, err)
	}
	if store.GrantedCount() != 1 {
		t.Fatalf("重复申请不应新增记录，实际 %d", store.GrantedCount())
	}
}

func TestAchievementValidation(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		req     AchievementRequest
		status  domainuser.Status
		wantErr error
	}{
		{"非法代码", AchievementRequest{UserID: 1, Code: "made_up"}, domainuser.StatusActive, ErrInvalidAchievement},
		{"非法用户", AchievementRequest{UserID: 0, Code: domainuser.AchievementFirstPost}, domainuser.StatusActive, ErrAchievementDenied},
		{"待激活账号", AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost}, domainuser.StatusPending, nil},
		{"注销中账号", AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost}, domainuser.StatusDeleting, nil},
		{"已注销账号", AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost}, domainuser.StatusDeleted, nil},
	}
	for _, c := range cases {
		store := NewMemoryAchievementStore()
		gate := NewAchievementService(store, fakeStatusReader{status: c.status}, time.Now)
		decision, err := gate.Apply(ctx, c.req)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("%s: 期望 %v，实际 %v", c.name, c.wantErr, err)
			}
		} else if err != nil {
			t.Errorf("%s: 业务拒绝不应返回系统错误: %v", c.name, err)
		}
		if decision.Granted || store.GrantedCount() != 0 {
			t.Errorf("%s: 不应写入成就: %+v count=%d", c.name, decision, store.GrantedCount())
		}
	}
}

func TestAchievementReadAndWriteFailures(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAchievementStore()
	sentinel := errors.New("db down")

	gate := NewAchievementService(store, fakeStatusReader{err: sentinel}, time.Now)
	if _, err := gate.Apply(ctx, AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost}); !errors.Is(err, sentinel) {
		t.Fatalf("状态读取失败应冒泡: %v", err)
	}
	if store.GrantedCount() != 0 {
		t.Fatal("读取失败不应写入")
	}

	store.SetGrantFailure(sentinel)
	gate = NewAchievementService(store, fakeStatusReader{status: domainuser.StatusActive}, time.Now)
	if _, err := gate.Apply(ctx, AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost}); !errors.Is(err, sentinel) {
		t.Fatalf("写入失败应冒泡: %v", err)
	}
	if store.GrantedCount() != 0 {
		t.Fatal("写入失败不应留下半状态")
	}

	// 失败是可恢复的，不会污染后续申请。
	store.SetGrantFailure(nil)
	decision, err := gate.Apply(ctx, AchievementRequest{UserID: 1, Code: domainuser.AchievementFirstPost})
	if err != nil || !decision.Granted || store.GrantedCount() != 1 {
		t.Fatalf("失败恢复后应能授予: %+v err=%v count=%d", decision, err, store.GrantedCount())
	}
}

func TestAchievementCodeWhitelist(t *testing.T) {
	for _, c := range []domainuser.AchievementCode{
		domainuser.AchievementFirstPost, domainuser.AchievementNightOwl, domainuser.AchievementFirstVote,
	} {
		if !c.Valid() {
			t.Errorf("白名单成就 %q 应合法", c)
		}
	}
	for _, c := range []domainuser.AchievementCode{"", "karma_boost", "FIRST_POST"} {
		if c.Valid() {
			t.Errorf("非白名单成就 %q 不应合法", c)
		}
	}
}

func TestMemoryStoreGrantIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryAchievementStore()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := store.Grant(ctx, 1, domainuser.AchievementFirstPost, at); err != nil {
			t.Fatal(err)
		}
	}
	if store.GrantedCount() != 1 {
		t.Fatalf("重复 Grant 应幂等，实际 %d", store.GrantedCount())
	}
	entries := store.Entries()
	entries[0].Code = "tampered"
	if store.Entries()[0].Code != domainuser.AchievementFirstPost {
		t.Fatal("Entries 返回了内部引用，外部可篡改")
	}
}
