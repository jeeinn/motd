package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

// P1-M4 运维回归：过期设备码清理只碰「自身已过期」的行。
// pending 是真实在途登录——未过期时删除会让客户端轮询突然拿到「码不存在」。

type authRowSeed struct {
	code     string
	status   string
	expDelta int64 // 相对 now 的过期偏移，负数=已过期
}

func seedDeviceAuthRows(t *testing.T, w *Writer, now time.Time, rows []authRowSeed) {
	t.Helper()
	ctx := context.Background()
	err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, r := range rows {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO device_auth_states
					(provider, device_code, user_code, polling_interval, expires_at, status, session_ref, created_at)
				VALUES ('github', ?, ?, 5, ?, ?, ?, ?)`,
				r.code, "USER-"+r.code, now.Unix()+r.expDelta, r.status, "sess-"+r.code, now.Unix()-60)
			if err != nil {
				return fmt.Errorf("插入 %s: %w", r.code, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("预置设备码状态: %v", err)
	}
}

func countAuthByCode(t *testing.T, pools *Pools, code string) int {
	t.Helper()
	var n int
	if err := pools.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM device_auth_states WHERE device_code = ?`, code).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", code, err)
	}
	return n
}

func TestCleanupExpiredDeviceAuthOnlyRemovesExpiredRows(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)

	now := time.Unix(1_800_000_000, 0).UTC()
	seed := []authRowSeed{
		{"authorized-expired", "authorized", -10},
		{"denied-expired", "denied", -10},
		{"expired-expired", "expired", -10},
		{"pending-expired", "pending", -10}, // pending 到自身过期后可清
		{"authorized-live", "authorized", 600},
		{"pending-live", "pending", 600}, // 在途登录：必须保留
	}
	seedDeviceAuthRows(t, w, now, seed)

	m := NewMaintenance(w)
	removed, err := m.CleanupExpiredDeviceAuth(ctx, now)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if removed != 4 {
		t.Fatalf("应清掉 4 条过期行，实际 %d", removed)
	}
	for _, r := range seed {
		want := 0
		if r.expDelta > 0 {
			want = 1
		}
		if got := countAuthByCode(t, pools, r.code); got != want {
			t.Errorf("%s(status=%s,exp=%d) 残留 %d 行，期望 %d", r.code, r.status, r.expDelta, got, want)
		}
	}
}

func TestCleanupExpiredDeviceAuthKeepsInFlightLoginUsable(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)

	now := time.Unix(1_800_000_000, 0).UTC()
	seedDeviceAuthRows(t, w, now, []authRowSeed{
		{"live", "pending", 600},
		{"stale", "authorized", -10},
	})
	m := NewMaintenance(w)
	if _, err := m.CleanupExpiredDeviceAuth(ctx, now); err != nil {
		t.Fatal(err)
	}

	// 在途登录仍可被状态机读到，并完成 pending → authorized 迁移。
	repos := NewDeviceAuthRepos(pools, w)
	st, err := repos.GetByDeviceCode(ctx, "live")
	if err != nil {
		t.Fatalf("在途码应仍可读: %v", err)
	}
	if st.Status != auth.StatusPending {
		t.Fatalf("在途码状态被破坏: %s", st.Status)
	}
	if err := repos.Write(ctx, func(tx auth.DeviceAuthWriteTx) error {
		return tx.Transition(ctx, st.ID, auth.StatusPending, auth.StatusAuthorized)
	}); err != nil {
		t.Fatalf("在途码应仍可迁移: %v", err)
	}
	after, err := repos.GetByDeviceCode(ctx, "live")
	if err != nil || after.Status != auth.StatusAuthorized {
		t.Fatalf("迁移未生效: status=%s err=%v", after.Status, err)
	}
}

func TestCleanupExpiredDeviceAuthIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)

	now := time.Unix(1_800_000_000, 0).UTC()
	seedDeviceAuthRows(t, w, now, []authRowSeed{
		{"gone", "denied", -10},
		{"keep", "pending", 600},
	})
	m := NewMaintenance(w)
	first, err := m.CleanupExpiredDeviceAuth(ctx, now)
	if err != nil || first != 1 {
		t.Fatalf("首轮应清 1 条: removed=%d err=%v", first, err)
	}
	second, err := m.CleanupExpiredDeviceAuth(ctx, now)
	if err != nil {
		t.Fatalf("重跑不得报错: %v", err)
	}
	if second != 0 {
		t.Fatalf("重跑应零删除（幂等），实际 %d", second)
	}
	if got := countAuthByCode(t, pools, "keep"); got != 1 {
		t.Fatalf("在途行不得被清理，残留 %d", got)
	}
}
