package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

type recordingToucher struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (t *recordingToucher) TouchLastUsed(_ context.Context, fp string, _ time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, fp)
	return t.err
}

func (t *recordingToucher) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

func TestDefaultKeyUsageIntervalIsAtLeastFiveMinutes(t *testing.T) {
	if DefaultKeyUsageInterval < 5*time.Minute {
		t.Fatalf("节流窗口不得小于 5 分钟，实际 %s", DefaultKeyUsageInterval)
	}
}

func TestKeyUsageRecorderThrottlesSameFingerprint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	touch := &recordingToucher{}
	r := NewKeyUsageRecorder(touch, DefaultKeyUsageInterval, WithKeyUsageClock(func() time.Time { return now }))

	fp := "SHA256:" + repeatByte('A', 43)
	for i := 0; i < 5; i++ {
		r.NoteUse(fp) // 同一时刻连续 5 次连接
	}
	r.Wait()
	if got := len(touch.snapshot()); got != 1 {
		t.Fatalf("同一钥匙 5 分钟内只应写 1 次，实际 %d", got)
	}

	now = now.Add(DefaultKeyUsageInterval) // 跨过节流窗口
	r.NoteUse(fp)
	r.Wait()
	if got := len(touch.snapshot()); got != 2 {
		t.Fatalf("跨过 5 分钟后应再写 1 次，实际 %d", got)
	}
}

func TestKeyUsageRecorderDifferentFingerprintsAreIndependent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	touch := &recordingToucher{}
	r := NewKeyUsageRecorder(touch, DefaultKeyUsageInterval, WithKeyUsageClock(func() time.Time { return now }))

	a := "SHA256:" + repeatByte('A', 43)
	b := "SHA256:" + repeatByte('B', 43)
	r.NoteUse(a)
	r.NoteUse(b)
	r.NoteUse(a) // a 被节流，b 不受影响
	r.Wait()
	got := touch.snapshot()
	seen := map[string]bool{}
	for _, fp := range got {
		seen[fp] = true
	}
	if len(got) != 2 || !seen[a] || !seen[b] {
		t.Fatalf("不同钥匙应各自独立记录一次（异步写顺序不保证）: %v", got)
	}
}

func TestKeyUsageRecorderWriteFailureNeverBlocks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	touch := &recordingToucher{err: errors.New("write failed")}
	r := NewKeyUsageRecorder(touch, DefaultKeyUsageInterval, WithKeyUsageClock(func() time.Time { return now }))

	done := make(chan struct{})
	go func() {
		r.NoteUse("SHA256:" + repeatByte('C', 43))
		r.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("写失败不得阻塞 NoteUse / Wait")
	}
}

func TestTouchLastUsedUpdatesOnlyNonRevokedKey(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)

	now := time.Unix(1_700_000_000, 0).UTC()
	seed := []struct {
		fp      string
		revoked bool
	}{
		{"SHA256:" + repeatByte('E', 43), false},
		{"SHA256:" + repeatByte('F', 43), true},
	}
	if err := w.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES ('usageuser', 'active', ?, ?)`,
			now.Unix(), now.Unix()); err != nil {
			return err
		}
		for _, s := range seed {
			var revoked any
			if s.revoked {
				revoked = now.Unix()
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at, revoked_at)
				VALUES (1, ?, 'ssh-ed25519 AAAAExample', ?, ?)`, s.fp, now.Unix(), revoked); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	repos := NewKeyBindingRepos(pools, w)
	touched := now.Add(10 * time.Minute)
	for _, s := range seed {
		if err := repos.TouchLastUsed(ctx, s.fp, touched); err != nil {
			t.Fatalf("TouchLastUsed(%s): %v", s.fp, err)
		}
	}

	var live, revoked sql.NullInt64
	if err := pools.QueryRowContext(ctx, `SELECT last_used_at FROM ssh_keys WHERE fingerprint = ?`, seed[0].fp).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := pools.QueryRowContext(ctx, `SELECT last_used_at FROM ssh_keys WHERE fingerprint = ?`, seed[1].fp).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !live.Valid || live.Int64 != touched.Unix() {
		t.Fatalf("有效钥匙应更新 last_used_at: %+v", live)
	}
	if revoked.Valid {
		t.Fatalf("已吊销钥匙不得更新 last_used_at: %+v", revoked)
	}

	list, err := repos.ListActive(ctx, 1)
	if err != nil || len(list) != 1 || !list[0].HasLastUsed || list[0].LastUsedUnix != touched.Unix() {
		t.Fatalf("/keys 详情应看到新的 last_used_at: list=%+v err=%v", list, err)
	}
}

var _ auth.KeyUsageRecorder = (*KeyUsageRecorder)(nil)
