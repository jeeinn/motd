package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

func newAuditTestWriter(t *testing.T) (*Pools, *Writer) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return pools, w
}

func TestAuthEventStoreBatchInsert(t *testing.T) {
	ctx := context.Background()
	pools, w := newAuditTestWriter(t)
	store := NewAuthEventStore(w)
	at := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	events := []auth.AuditEvent{
		{Event: auth.AuditLoginFailure, Provider: "github", IPHash: "hash-a", DetailJSON: `{}`, CreatedAt: at},
		{Event: auth.AuditLogout, UserID: 7, Provider: "github", IPHash: "hash-b", DetailJSON: `{"sessions_closed":2}`, CreatedAt: at.Add(time.Second)},
	}
	if err := store.Flush(ctx, events); err != nil {
		t.Fatalf("批量写入: %v", err)
	}
	if n, err := pools.CountAuthEvents(ctx, ""); err != nil || n != 2 {
		t.Fatalf("应落库 2 条，实际 n=%d err=%v", n, err)
	}
	if n, err := pools.CountAuthEvents(ctx, auth.AuditLogout); err != nil || n != 1 {
		t.Fatalf("按事件名统计应得 1，实际 n=%d err=%v", n, err)
	}

	row := pools.QueryRowContext(ctx, `SELECT user_id, event, provider, ip_hash, detail_json, created_at FROM auth_events ORDER BY id LIMIT 1`)
	var nUID *int64
	var created int64
	var event, provider, ipHash, detail string
	if err := row.Scan(&nUID, &event, &provider, &ipHash, &detail, &created); err != nil {
		t.Fatal(err)
	}
	if nUID != nil || event != auth.AuditLoginFailure || provider != "github" || ipHash != "hash-a" {
		t.Fatalf("首行内容不符: uid=%v event=%q provider=%q ip=%q", nUID, event, provider, ipHash)
	}
	if created != at.Unix() {
		t.Fatalf("created_at 应为 Unix 秒 %d，实际 %d", at.Unix(), created)
	}

	// 空批量是幂等 no-op，不占用写队列。
	before := w.Commits()
	if err := store.Flush(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if w.Commits() != before {
		t.Fatal("空批量不应提交事务")
	}
}

func TestAuthEventStoreDefaultsForNullableColumns(t *testing.T) {
	ctx := context.Background()
	pools, w := newAuditTestWriter(t)
	store := NewAuthEventStore(w)
	if err := store.Flush(ctx, []auth.AuditEvent{{
		Event: auth.AuditSessionRevoked, CreatedAt: time.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
	row := pools.QueryRowContext(ctx, `SELECT user_id, provider, ip_hash, detail_json FROM auth_events`)
	// user_id / provider / ip_hash 允许 NULL，detail_json 走兜底 '{}'。
	var nUID *int64
	var nProvider, nIPHash *string
	var detail string
	if err := row.Scan(&nUID, &nProvider, &nIPHash, &detail); err != nil {
		t.Fatal(err)
	}
	if nUID != nil || nProvider != nil || nIPHash != nil {
		t.Fatalf("空字段应写 NULL: uid=%v provider=%v ip=%v", nUID, nProvider, nIPHash)
	}
	if detail != "{}" {
		t.Fatalf("空 detail 应回落 {}，实际 %q", detail)
	}
}

func TestAuditLoggerEndToEndToSQLite(t *testing.T) {
	ctx := context.Background()
	pools, w := newAuditTestWriter(t)
	logger := auth.NewAuditLogger(NewAuthEventStore(w), auth.WithAuditCapacity(8))
	for i := 0; i < 3; i++ {
		if err := logger.Record(auth.AuditLoginFailure, 0, "github", "hash", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	logger.Tick(ctx)
	if depth, dropped, flushed, _ := logger.Stats(); depth != 0 || dropped != 0 || flushed != 3 {
		t.Fatalf("审计器状态不符: depth=%d dropped=%d flushed=%d", depth, dropped, flushed)
	}
	if n, err := pools.CountAuthEvents(ctx, auth.AuditLoginFailure); err != nil || n != 3 {
		t.Fatalf("端到端落库应为 3 条，实际 n=%d err=%v", n, err)
	}
}
