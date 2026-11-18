package presence

// P0-19：会话升级在内存连接注册表里的对应动作。
// 复用 registry_test.go 的 newFakeClock，避免再建第二套时钟替身。

import (
	"errors"
	"testing"
	"time"
)

func TestAttachUserOnlyOnceFromGuest(t *testing.T) {
	r := New(WithClock(newFakeClock().Now))
	if err := r.Connect("s-1", KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.AttachUser("s-1", 42); err != nil {
		t.Fatalf("访客升级应成功: %v", err)
	}
	got, ok := r.Get("s-1")
	if !ok || got.UserID != 42 {
		t.Fatalf("注册表未同步 UserID: %+v", got)
	}

	// 重复升级、横向改号都必须拒绝，且原 UserID 不变。
	for _, uid := range []int64{42, 99} {
		if err := r.AttachUser("s-1", uid); !errors.Is(err, ErrAlreadyAttached) {
			t.Errorf("重复升级 %d 应被拒绝，实际 %v", uid, err)
		}
	}
	got, _ = r.Get("s-1")
	if got.UserID != 42 {
		t.Fatalf("被拒绝的升级不得改动 UserID，实际 %d", got.UserID)
	}
}

func TestAttachUserRejectsInvalidInput(t *testing.T) {
	r := New()
	if err := r.AttachUser("", 1); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("空会话 ID 应被拒绝，实际 %v", err)
	}
	if err := r.AttachUser("s-1", 0); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("非正 UserID 应被拒绝，实际 %v", err)
	}
	if err := r.AttachUser("s-1", -7); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("负 UserID 应被拒绝，实际 %v", err)
	}
	if err := r.AttachUser("s-missing", 1); !errors.Is(err, ErrUnknownSession) {
		t.Errorf("未登记会话应被拒绝，实际 %v", err)
	}
}

func TestAttachUserRefreshesHeartbeatButNotWrite(t *testing.T) {
	clk := newFakeClock()
	r := New(WithClock(clk.Now))
	if err := r.Connect("s-1", KindSSH, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Get("s-1")
	clk.Advance(30 * time.Second) // 超过默认 20s 心跳窗口
	if err := r.AttachUser("s-1", 7); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Get("s-1")
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Fatal("升级应顺带刷新心跳时间")
	}
	// 纯内存动作：Online 不变，不产生任何持久化写入（本包不 import infrastructure）。
	if r.Online() != 1 {
		t.Fatalf("升级不得改变连接总数，实际 %d", r.Online())
	}
}
