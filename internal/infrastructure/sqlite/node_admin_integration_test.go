package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/admin"
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/user"
)

// adminTestSink 是测试内的最小事件出口：只把治理事件交给真实 AuditBridge。
// 不在 infrastructure 测试里 import plugin（依赖方向守卫同样扫测试文件）。
type adminTestSink struct {
	bridge func(context.Context, event.DomainEvent) error
}

func (s *adminTestSink) Publish(ctx context.Context, e event.DomainEvent) error {
	if e.Type != event.EventAdminAction {
		return nil
	}
	return s.bridge(ctx, e)
}

const integrationAdminFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// 真实 SQLite + 真实 WriteTx + 真实 outbox + 真实 DomainBus + 真实审计落库。
// 重点证明三件事：slug 唯一冲突可见、归档不碰 threads、审计经 auth_events 落地。
func TestNodeAdminIntegration(t *testing.T) {
	ctx := context.Background()
	e := newForumEnv(t)
	auditLogger := auth.NewAuditLogger(NewAuthEventStore(e.writer))
	sink := &adminTestSink{bridge: admin.AuditBridge(auditLogger)}
	outbox, err := forum.NewOutbox(NewOutboxStore(e.writer), sink, nil)
	if err != nil {
		t.Fatalf("装配 outbox: %v", err)
	}
	svc := forum.NewService(e.repos,
		forum.WithClock(func() time.Time { return testBaseTime }),
		forum.WithEventSink(sink), forum.WithOutbox(outbox),
		forum.WithAdminWhitelist(map[string]struct{}{integrationAdminFP: {}}),
	)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive, Fingerprint: integrationAdminFP}

	// 1) 越权：白名单为空的用例不得建节点。
	denied := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	if _, err := denied.CreateNode(ctx, actor, "/denied/node", "越权"); !errors.Is(err, forum.ErrAdminDenied) {
		t.Fatalf("未注入白名单必须 fail-closed: %v", err)
	}

	// 2) 创建 + slug 唯一冲突。
	id, err := svc.CreateNode(ctx, actor, "/new/node", "新节点")
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if _, err := svc.CreateNode(ctx, actor, "/new/node", "重复"); !errors.Is(err, forum.ErrNodeSlugExists) {
		t.Fatalf("slug 冲突应返回哨兵错误: %v", err)
	}

	// 3) 改名只改 name，slug/id 稳定。
	if err := svc.RenameNode(ctx, actor, "/new/node", "改名后"); err != nil {
		t.Fatalf("改名: %v", err)
	}
	var slug, name string
	if err := e.pools.QueryRowContext(ctx, `SELECT slug, name FROM nodes WHERE id = ?`, id).Scan(&slug, &name); err != nil {
		t.Fatalf("读取改名结果: %v", err)
	}
	if slug != "/new/node" || name != "改名后" {
		t.Fatalf("改名不得动 slug: slug=%q name=%q", slug, name)
	}

	// 4) 归档：threads 一行不动，公开列表过滤、管理列表保留。
	res, err := e.pools.writer.ExecContext(ctx, `
		INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at)
		VALUES (?, ?, '归档前的帖子', 0, ?, ?)`, id, e.userID, e.clock, e.clock)
	if err != nil {
		t.Fatalf("插入测试主题: %v", err)
	}
	threadID, _ := res.LastInsertId()
	if err := svc.ArchiveNode(ctx, actor, "/new/node"); err != nil {
		t.Fatalf("归档: %v", err)
	}
	if err := svc.ArchiveNode(ctx, actor, "/new/node"); err != nil {
		t.Fatalf("重复归档应幂等: %v", err)
	}
	var status string
	if err := e.pools.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("读取节点状态: %v", err)
	}
	if status != "archived" {
		t.Fatalf("归档状态错误: %q", status)
	}
	var threadStatus string
	var nodeIDOfThread int64
	if err := e.pools.QueryRowContext(ctx, `SELECT node_id, status FROM threads WHERE id = ?`, threadID).Scan(&nodeIDOfThread, &threadStatus); err != nil {
		t.Fatalf("读取主题: %v", err)
	}
	if nodeIDOfThread != id || threadStatus != "active" {
		t.Fatalf("归档节点不得改动 threads: node=%d status=%q", nodeIDOfThread, threadStatus)
	}
	public, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("公开列表: %v", err)
	}
	for _, n := range public {
		if n.ID == id {
			t.Fatal("归档节点不得出现在公开列表")
		}
	}
	adminRows, err := svc.ListAdminNodes(ctx, actor)
	if err != nil {
		t.Fatalf("管理列表: %v", err)
	}
	var found forum.NodeAdminView
	for _, n := range adminRows {
		if n.ID == id {
			found = n
		}
	}
	if found.Status != "archived" || found.Slug != "/new/node" {
		t.Fatalf("管理列表应含归档节点: %+v", found)
	}

	// 5) seed：归档允许，改名拒绝（真实 DB 上复核）。
	if err := svc.RenameNode(ctx, actor, "/dev/ai", "改名"); !errors.Is(err, forum.ErrSeedNodeRename) {
		t.Fatalf("seed 改名应被拒: %v", err)
	}
	if err := svc.ArchiveNode(ctx, actor, "/dev/ai"); err != nil {
		t.Fatalf("seed 应允许归档: %v", err)
	}

	// 6) 审计：outbox 提交后投递 → AuditBridge → Record → Flush → auth_events。
	if err := auditLogger.Flush(ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	count, err := e.pools.CountAuthEvents(ctx, auth.AuditAdminAction)
	if err != nil {
		t.Fatalf("统计审计事件: %v", err)
	}
	if count < 4 { // create / rename / archive / seed archive
		t.Fatalf("治理审计事件数 = %d，期望至少 4", count)
	}
	var detail string
	if err := e.pools.QueryRowContext(ctx,
		`SELECT detail_json FROM auth_events WHERE event = ? ORDER BY id DESC LIMIT 1`,
		auth.AuditAdminAction).Scan(&detail); err != nil {
		t.Fatalf("读取审计详情: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatalf("审计详情不是 JSON: %v", err)
	}
	if fields["command"] != "node archive" || fields["target"] != "/dev/ai" ||
		fields["fingerprint"] != integrationAdminFP || fields["result"] != "success" ||
		fields["occurred_at"] == "" {
		t.Fatalf("审计字段不全: %+v", fields)
	}
	if strings.Contains(detail, "\x1b") {
		t.Fatalf("审计详情不得含控制字符: %q", detail)
	}

	// bootstrap：白名单访客（未绑定账号）也能治理，审计以指纹为主体、user_id 为空。
	guest := forum.Actor{Guest: true, Fingerprint: integrationAdminFP}
	guestID, err := svc.CreateNode(ctx, guest, "/guest/bootstrap", "访客建的节点")
	if err != nil {
		t.Fatalf("白名单访客建版块失败: %v", err)
	}
	if err := auditLogger.Flush(ctx); err != nil {
		t.Fatalf("访客审计落库: %v", err)
	}
	var guestDetail string
	var guestUserID sql.NullInt64
	if err := e.pools.QueryRowContext(ctx,
		`SELECT detail_json, user_id FROM auth_events WHERE event = ? ORDER BY id DESC LIMIT 1`,
		auth.AuditAdminAction).Scan(&guestDetail, &guestUserID); err != nil {
		t.Fatalf("读取访客审计: %v", err)
	}
	if err := json.Unmarshal([]byte(guestDetail), &fields); err != nil {
		t.Fatalf("访客审计详情不是 JSON: %v", err)
	}
	if guestUserID.Valid || fields["fingerprint"] != integrationAdminFP ||
		fields["command"] != "node create" || fields["target"] != "/guest/bootstrap" ||
		fields["node_id"] != strconv.FormatInt(guestID, 10) {
		t.Fatalf("访客审计必须以指纹标识主体: userID=%v detail=%s", guestUserID, guestDetail)
	}
}
