package main

// D6 / D7 演练：带上**真实业务数据**（不是空库）做重启持久化与备份恢复。
// 依据：评审稿 §1.3 原则二「服务重启后帖子不能消失」、§7「备份的成功标准是能恢复」，
// 以及 docs/OPS.md §5 自认的待办「不能只靠空库演练结案」。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

func mvpCount(t *testing.T, p *sqlite.Pools, table string) int64 {
	t.Helper()
	var n int64
	// 表名来自固定白名单，不是外部输入。
	if err := p.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("读取 %s 行数: %v", table, err)
	}
	return n
}

func mvpLatestBody(t *testing.T, p *sqlite.Pools) string {
	t.Helper()
	var body string
	if err := p.QueryRowContext(context.Background(),
		`SELECT body_md FROM posts ORDER BY id DESC LIMIT 1`).Scan(&body); err != nil {
		t.Fatalf("读取最新正文: %v", err)
	}
	return body
}

func TestMVPRestartAndBackupWithRealData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "motd.db")

	pools, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()

	now := time.Now().Unix()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, name := range []string{"alice", "bob"} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
				name, now, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("准备账号: %v", err)
	}

	rt, err := newForumRuntime(pools, writer, nil)
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	// 两个账号分别发帖 / 回复，避开 Q6 的 60 秒单用户冷却。
	threadID, err := rt.Service.CreateThread(ctx,
		forum.Actor{UserID: 1, Status: domainuser.StatusActive},
		forum.CreateThreadInput{NodeID: 1, Title: "持久化验收帖", Body: "重启前正文"})
	if err != nil {
		t.Fatalf("造数据发帖: %v", err)
	}
	if _, err := rt.Service.CreatePost(ctx,
		forum.Actor{UserID: 2, Status: domainuser.StatusActive},
		forum.CreatePostInput{ThreadID: threadID, Body: "重启前回复"}); err != nil {
		t.Fatalf("造数据回复: %v", err)
	}

	wantThreads := mvpCount(t, pools, "threads")
	wantPosts := mvpCount(t, pools, "posts")
	wantBody := mvpLatestBody(t, pools)
	if wantThreads != 1 || wantPosts != 2 || wantBody != "重启前回复" {
		t.Fatalf("造数据失败: threads=%d posts=%d body=%q", wantThreads, wantPosts, wantBody)
	}

	// ---- D6：关库 → 重新打开同一文件 → 迁移应 0 个版本、内容与行数不变 ----
	writer.Stop()
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}
	reopened, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("重启后打开数据库: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	applied, err := sqlite.Migrate(ctx, reopened)
	if err != nil {
		t.Fatalf("重启后迁移: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("重启不应重复应用迁移，实际 %v", applied)
	}
	if got := mvpCount(t, reopened, "threads"); got != wantThreads {
		t.Fatalf("重启后 threads 行数 %d，期望 %d", got, wantThreads)
	}
	if got := mvpCount(t, reopened, "posts"); got != wantPosts {
		t.Fatalf("重启后 posts 行数 %d，期望 %d", got, wantPosts)
	}
	if got := mvpLatestBody(t, reopened); got != wantBody {
		t.Fatalf("重启后最新正文 %q，期望 %q", got, wantBody)
	}

	// ---- D7：带数据备份 → 校验 → 恢复到空目录 → 再核对 ----
	info, err := sqlite.Backup(ctx, reopened, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatalf("备份: %v", err)
	}
	vr, err := sqlite.VerifyBackup(ctx, info.Path)
	if err != nil {
		t.Fatalf("备份验收: %v", err)
	}
	if vr.Integrity != "ok" || vr.TableCounts["threads"] != wantThreads || vr.TableCounts["posts"] != wantPosts {
		t.Fatalf("备份验收结果不符: %+v", vr)
	}
	target := filepath.Join(dir, "restored", "motd.db")
	if err := sqlite.Restore(ctx, info.Path, target); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	restored, err := sqlite.Open(ctx, target)
	if err != nil {
		t.Fatalf("打开恢复库: %v", err)
	}
	defer func() { _ = restored.Close() }()
	if got := mvpCount(t, restored, "threads"); got != wantThreads {
		t.Fatalf("恢复后 threads 行数 %d，期望 %d", got, wantThreads)
	}
	if got := mvpCount(t, restored, "posts"); got != wantPosts {
		t.Fatalf("恢复后 posts 行数 %d，期望 %d", got, wantPosts)
	}
	if got := mvpLatestBody(t, restored); got != wantBody {
		t.Fatalf("恢复后最新正文 %q，期望 %q", got, wantBody)
	}
	// 版本数随迁移增长，不硬编码：恢复库必须与源库登记的版本完全一致。
	wantVersions := mvpCount(t, reopened, "schema_migrations")
	if got := mvpCount(t, restored, "schema_migrations"); got != wantVersions {
		t.Fatalf("迁移版本应随备份一起恢复，实际 %d 期望 %d", got, wantVersions)
	}
}
