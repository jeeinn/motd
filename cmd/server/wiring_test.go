package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/domain/event"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// 端到端：真实 SQLite + 真实写入队列 + 真实 Service + 真实事件总线
// + 真实 demo 插件 + 真实 application/user 校验。不用任何 fake。
func setupRuntime(t *testing.T) (context.Context, *forumRuntime) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "motd.db")
	pools, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(func() {
		writer.Stop()
		_ = pools.Close()
	})

	// 论坛写用例与成就校验都会读 users.status，先插一个 active 账号。
	now := time.Now().Unix()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
			"tester", now, now)
		return err
	}); err != nil {
		t.Fatalf("准备测试用户: %v", err)
	}

	// 固定白天时刻：避免墙钟落在 02:00–05:00（UTC）时 social 插件额外解锁
	// night_owl，让"首帖只解锁 first_post"的断言变成随机的（v0.6.0 实测 bug）。
	fixedNoon := func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	rt, err := newForumRuntime(pools, writer, nil, nil, nil, fixedNoon)
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	return ctx, rt
}

func TestEndToEndPostCreatedTriggersPluginAchievement(t *testing.T) {
	ctx, rt := setupRuntime(t)
	actor := forum.Actor{UserID: 1, Status: domainuser.StatusActive}

	if len(rt.Plugins) == 0 || rt.Bus.SubscriberCount(event.EventPostCreated) == 0 {
		t.Fatal("装配后应有插件订阅 PostCreated")
	}
	if rt.Presence == nil || rt.Presence.Online() != 0 || rt.Presence.Timeout() != presence.DefaultHeartbeatTimeout {
		t.Fatal("在线注册表应装配为纯内存、空表、20s 心跳超时")
	}

	threadID, err := rt.Service.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: 1, Title: "集成测试主题", Body: "首帖正文",
	})
	if err != nil {
		t.Fatalf("真实链路发帖失败: %v", err)
	}
	if threadID <= 0 {
		t.Fatal("主题 ID 应有效")
	}

	if rt.Bus.Published() != 1 || rt.Bus.Failures() != 0 {
		t.Fatalf("事件总线计数异常: published=%d failures=%d", rt.Bus.Published(), rt.Bus.Failures())
	}
	if rt.Service.PublishFailures() != 0 {
		t.Fatal("事件投递不应失败")
	}
	if rt.Store.GrantedCount() != 1 {
		t.Fatalf("demo 插件应解锁 1 个成就，实际 %d", rt.Store.GrantedCount())
	}
	grants := rt.Store.Entries()
	if len(grants) != 1 || grants[0].UserID != 1 || grants[0].Code != domainuser.AchievementFirstPost {
		t.Fatalf("成就记录错误: %+v", grants)
	}

	// 帖子本身确实落库，且事件内容指向它。
	page, err := rt.Service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Title != "集成测试主题" {
		t.Fatalf("发帖未落库: rows=%d err=%v", len(page.Rows), err)
	}
}

func TestEndToEndFailedWritePublishesNothing(t *testing.T) {
	ctx, rt := setupRuntime(t)
	actor := forum.Actor{UserID: 1, Status: domainuser.StatusActive}

	if _, err := rt.Service.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: 999999, Title: "不存在的版块", Body: "正文",
	}); err == nil {
		t.Fatal("不存在的版块应让用例失败")
	}
	if rt.Bus.Published() != 0 || rt.Service.PublishFailures() != 0 {
		t.Fatalf("事务失败不应发布事件: published=%d failures=%d", rt.Bus.Published(), rt.Service.PublishFailures())
	}
	if rt.Store.GrantedCount() != 0 {
		t.Fatal("事务失败不应触发成就申请")
	}
}
