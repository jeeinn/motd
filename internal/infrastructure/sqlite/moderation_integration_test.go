package sqlite

// P2-M1b 真实 SQLite 集成：隐藏 → 公开视图消失 / DB 写 hidden（不是 deleted）/
// 审计经 outbox → AuditBridge → auth_events 落库。全部使用 t.TempDir() 的隔离库，
// 生产 data/ 全程不触碰。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/admin"
	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/user"
)

const moderationFP = "SHA256:DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"

func setupModerationService(t *testing.T, e *forumEnv) (*forum.Service, *auth.AuditLogger) {
	t.Helper()
	logger := auth.NewAuditLogger(NewAuthEventStore(e.writer))
	sink := &adminTestSink{bridge: admin.AuditBridge(logger)}
	outbox, err := forum.NewOutbox(NewOutboxStore(e.writer), sink, nil)
	if err != nil {
		t.Fatalf("装配 outbox: %v", err)
	}
	// 每次调用时钟前进 1ns：生产用 time.Now()，这里保证两次隐藏的审计事件 ID 不同，
	// 否则固定时钟会让 outbox 把第二次事件按幂等键去重。
	now := testBaseTime
	svc := forum.NewService(e.repos,
		forum.WithClock(func() time.Time {
			now = now.Add(time.Nanosecond)
			return now
		}),
		forum.WithEventSink(sink), forum.WithOutbox(outbox),
		forum.WithAdminWhitelist(map[string]struct{}{moderationFP: {}}),
	)
	return svc, logger
}

// 造一个「他人垃圾主题」：作者 spammerID，含首帖与一条回复。
func seedSpamThread(t *testing.T, e *forumEnv, spammerID int64) (int64, int64, int64) {
	t.Helper()
	var threadID, firstID, replyID int64
	err := e.writer.Submit(e.ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO threads (node_id, user_id, title, score, last_reply_at, created_at)
			VALUES (?, ?, '垃圾主题', 0, ?, ?)`, e.nodeID, spammerID, e.clock, e.clock)
		if err != nil {
			return err
		}
		threadID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		for i, body := range []string{"垃圾首帖", "垃圾回复"} {
			res, err = tx.ExecContext(ctx, `
				INSERT INTO posts (thread_id, user_id, body_md, status, created_at)
				VALUES (?, ?, ?, 'normal', ?)`, threadID, spammerID, body, e.clock+int64(i))
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if i == 0 {
				firstID = id
			} else {
				replyID = id
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("准备垃圾数据: %v", err)
	}
	return threadID, firstID, replyID
}

func insertUser(t *testing.T, e *forumEnv, name string) int64 {
	t.Helper()
	var id int64
	err := e.writer.Submit(e.ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
			name, e.clock, e.clock)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("准备用户 %s: %v", name, err)
	}
	return id
}

func statusInDB(t *testing.T, e *forumEnv, table string, id int64) string {
	t.Helper()
	var status string
	if err := e.pools.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT status FROM %s WHERE id = ?`, table), id).Scan(&status); err != nil {
		t.Fatalf("查询 %s:%d 状态: %v", table, id, err)
	}
	return status
}

func readLastAdminAudit(t *testing.T, e *forumEnv) (string, sql.NullInt64) {
	t.Helper()
	var detail string
	var userID sql.NullInt64
	if err := e.pools.QueryRowContext(e.ctx, `
		SELECT detail_json, user_id FROM auth_events WHERE event = ? ORDER BY id DESC LIMIT 1`,
		auth.AuditAdminAction).Scan(&detail, &userID); err != nil {
		t.Fatalf("读取治理审计: %v", err)
	}
	return detail, userID
}

// 管理员隐藏他人的回复：DB 写 hidden（不是 deleted）、公开列表与检索都不再返回、审计落库。
func TestHidePostIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	adminID := e.userID // 管理员与内容作者不同
	_, firstID, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	actor := forum.Actor{UserID: adminID, Status: user.StatusActive, Fingerprint: moderationFP}

	// 越权：没有白名单的同一会话不得改数据。
	denied := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	if err := denied.HidePost(e.ctx, actor, replyID); !errors.Is(err, forum.ErrAdminDenied) {
		t.Fatalf("越权隐藏应被拒: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "normal" {
		t.Fatalf("越权后数据不得变更: %q", got)
	}

	if err := svc.HidePost(e.ctx, actor, replyID); err != nil {
		t.Fatalf("管理员隐藏他人回复失败: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "hidden" {
		t.Fatalf("DB 必须写 hidden（治理语义），实际 %q", got)
	}
	if got := statusInDB(t, e, "posts", firstID); got != "normal" {
		t.Fatalf("其它回复不得被级联: %q", got)
	}
	visible, _, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: threadOfPost(t, e, replyID), Limit: 20})
	if err != nil || len(visible) != 1 || visible[0].ID != firstID {
		t.Fatalf("隐藏回复应从公开列表消失: rows=%+v err=%v", visible, err)
	}
	hits, _, err := e.repos.SearchPosts(e.ctx, forum.SearchQuery{Query: "垃圾回复", Limit: 20})
	if err != nil || len(hits) != 0 {
		t.Fatalf("隐藏回复不得被检索命中: hits=%+v err=%v", hits, err)
	}

	// 幂等：再次隐藏不报错，状态仍是 hidden。
	if err := svc.HidePost(e.ctx, actor, replyID); err != nil {
		t.Fatalf("重复隐藏应幂等: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "hidden" {
		t.Fatalf("重复隐藏后状态仍应为 hidden: %q", got)
	}

	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	count, err := e.pools.CountAuthEvents(e.ctx, auth.AuditAdminAction)
	if err != nil || count != 2 {
		t.Fatalf("两次隐藏各应落一条审计，实际 count=%d err=%v", count, err)
	}
	detail, userID := readLastAdminAudit(t, e)
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatalf("审计详情不是 JSON: %v", err)
	}
	if fields["command"] != "post hide" || fields["target"] != fmt.Sprintf("post:%d", replyID) ||
		fields["fingerprint"] != moderationFP || fields["result"] != "success" ||
		!userID.Valid || userID.Int64 != adminID {
		t.Fatalf("审计字段不全: detail=%s userID=%v", detail, userID)
	}
	if strings.Contains(detail, "\x1b") || strings.Contains(detail, "垃圾回复") {
		t.Fatalf("审计详情不得含控制字符或用户正文: %s", detail)
	}
}

// 管理员隐藏他人主题：threads.status=hidden、回复行保留但随主题不可见、审计命令名 thread hide。
func TestHideThreadIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	threadID, firstID, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive, Fingerprint: moderationFP}

	if err := svc.HideThread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("管理员隐藏他人主题失败: %v", err)
	}
	if got := statusInDB(t, e, "threads", threadID); got != "hidden" {
		t.Fatalf("主题 DB 状态必须是 hidden，实际 %q", got)
	}
	if got := statusInDB(t, e, "posts", firstID); got != "normal" || statusInDB(t, e, "posts", replyID) != "normal" {
		t.Fatal("隐藏主题不得级联改回复行")
	}
	if _, err := e.repos.GetThread(e.ctx, threadID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("隐藏主题应对公开详情不可见: %v", err)
	}
	page, err := svc.ListThreads(e.ctx, forum.ListThreadsQuery{NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 20})
	if err != nil || len(page.Rows) != 0 {
		t.Fatalf("隐藏主题不得出现在公开列表: page=%+v err=%v", page, err)
	}
	if err := svc.HideThread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("重复隐藏主题应幂等: %v", err)
	}
	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	detail, _ := readLastAdminAudit(t, e)
	if !strings.Contains(detail, `"command":"thread hide"`) ||
		!strings.Contains(detail, fmt.Sprintf(`"target":"thread:%d"`, threadID)) {
		t.Fatalf("隐藏主题的审计命令/目标不对: %s", detail)
	}
}

// bootstrap：白名单访客隐藏他人帖成功，auth_events.user_id 为空、指纹进 detail。
func TestWhitelistedGuestHideIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	_, _, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	guest := forum.Actor{Guest: true, Fingerprint: moderationFP}

	if err := svc.HidePost(e.ctx, guest, replyID); err != nil {
		t.Fatalf("白名单访客必须能隐藏他人回复: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "hidden" {
		t.Fatalf("访客治理未落库: %q", got)
	}
	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	detail, userID := readLastAdminAudit(t, e)
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if userID.Valid || fields["fingerprint"] != moderationFP || fields["command"] != "post hide" {
		t.Fatalf("访客审计必须以指纹标识主体: userID=%v detail=%s", userID, detail)
	}
}

func threadOfPost(t *testing.T, e *forumEnv, postID int64) int64 {
	t.Helper()
	var threadID int64
	if err := e.pools.QueryRowContext(e.ctx, `SELECT thread_id FROM posts WHERE id = ?`, postID).Scan(&threadID); err != nil {
		t.Fatalf("查询内容所属主题: %v", err)
	}
	return threadID
}

// ADR-053 端到端：隐藏 → 恢复 → DB status 回 normal、公开列表与检索重新可见，
// 审计经 outbox → AuditBridge → auth_events 落一条 command="post unhide"。
func TestUnhidePostIntegrationRoundTrip(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	adminID := e.userID
	_, firstID, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	actor := forum.Actor{UserID: adminID, Status: user.StatusActive, Fingerprint: moderationFP}

	if err := svc.HidePost(e.ctx, actor, replyID); err != nil {
		t.Fatalf("准备隐藏失败: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "hidden" {
		t.Fatalf("隐藏后应为 hidden，实际 %q", got)
	}
	if hits, _, err := e.repos.SearchPosts(e.ctx, forum.SearchQuery{Query: "垃圾回复", Limit: 20}); err != nil || len(hits) != 0 {
		t.Fatalf("隐藏后检索不应命中: hits=%+v err=%v", hits, err)
	}

	if err := svc.UnhidePost(e.ctx, actor, replyID); err != nil {
		t.Fatalf("管理员恢复他人回复失败: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "normal" {
		t.Fatalf("DB 必须恢复 normal，实际 %q", got)
	}
	if got := statusInDB(t, e, "posts", firstID); got != "normal" {
		t.Fatalf("其它回复不得被牵连: %q", got)
	}
	visible, _, err := e.repos.ListPosts(e.ctx, forum.PostQuery{ThreadID: threadOfPost(t, e, replyID), Limit: 20})
	if err != nil || len(visible) != 2 {
		t.Fatalf("恢复后公开列表应重新包含该回复: rows=%+v err=%v", visible, err)
	}
	// 0007 的 posts_fts_update 触发器必须在同一事务里重建索引，恢复即刻可检索。
	hits, _, err := e.repos.SearchPosts(e.ctx, forum.SearchQuery{Query: "垃圾回复", Limit: 20})
	if err != nil || len(hits) != 1 || hits[0].PostID != replyID {
		t.Fatalf("恢复后检索应立即命中: hits=%+v err=%v", hits, err)
	}

	// 幂等：未隐藏的目标再恢复一次仍成功，状态不变。
	if err := svc.UnhidePost(e.ctx, actor, replyID); err != nil {
		t.Fatalf("未隐藏目标恢复应幂等成功: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "normal" {
		t.Fatalf("幂等恢复后状态仍应为 normal: %q", got)
	}

	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	count, err := e.pools.CountAuthEvents(e.ctx, auth.AuditAdminAction)
	if err != nil || count != 3 {
		t.Fatalf("隐藏 + 两次恢复应落三条审计，实际 count=%d err=%v", count, err)
	}
	detail, userID := readLastAdminAudit(t, e)
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatalf("审计详情不是 JSON: %v", err)
	}
	if fields["command"] != "post unhide" || fields["target"] != fmt.Sprintf("post:%d", replyID) ||
		fields["fingerprint"] != moderationFP || fields["result"] != "success" ||
		!userID.Valid || userID.Int64 != adminID {
		t.Fatalf("恢复审计字段不全: detail=%s userID=%v", detail, userID)
	}
	if strings.Contains(detail, "垃圾回复") || strings.Contains(detail, "\x1b") {
		t.Fatalf("审计详情不得含用户正文或控制字符: %s", detail)
	}
}

// ADR-053 端到端：主题隐藏 → 恢复，threads.status 回 active、回复行不变、审计命令名 thread unhide。
func TestUnhideThreadIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	threadID, firstID, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive, Fingerprint: moderationFP}

	if err := svc.HideThread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("准备隐藏主题失败: %v", err)
	}
	if err := svc.UnhideThread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("管理员恢复他人主题失败: %v", err)
	}
	if got := statusInDB(t, e, "threads", threadID); got != "active" {
		t.Fatalf("主题 DB 状态必须恢复 active，实际 %q", got)
	}
	if got := statusInDB(t, e, "posts", firstID); got != "normal" || statusInDB(t, e, "posts", replyID) != "normal" {
		t.Fatal("恢复主题不得级联改回复行")
	}
	if _, err := e.repos.GetThread(e.ctx, threadID); err != nil {
		t.Fatalf("恢复后主题应对公开详情重新可见: %v", err)
	}
	page, err := svc.ListThreads(e.ctx, forum.ListThreadsQuery{NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 20})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].ID != threadID {
		t.Fatalf("恢复后主题应重回公开列表: page=%+v err=%v", page, err)
	}
	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	detail, _ := readLastAdminAudit(t, e)
	if !strings.Contains(detail, `"command":"thread unhide"`) ||
		!strings.Contains(detail, fmt.Sprintf(`"target":"thread:%d"`, threadID)) {
		t.Fatalf("恢复主题的审计命令/目标不对: %s", detail)
	}
}

// ADR-053 bootstrap：白名单访客恢复他人帖，auth_events.user_id 为空、指纹进 detail。
func TestWhitelistedGuestUnhideIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	_, _, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	admin := forum.Actor{UserID: e.userID, Fingerprint: moderationFP}
	guest := forum.Actor{Guest: true, Fingerprint: moderationFP}
	if err := svc.HidePost(e.ctx, admin, replyID); err != nil {
		t.Fatalf("准备隐藏失败: %v", err)
	}

	// 名单外访客恢复应被拒，数据保持 hidden。
	outsider := forum.Actor{Guest: true, Fingerprint: "SHA256:" + strings.Repeat("E", 43)}
	if err := svc.UnhidePost(e.ctx, outsider, replyID); !errors.Is(err, forum.ErrAdminDenied) {
		t.Fatalf("名单外访客恢复应被拒，实际 %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "hidden" {
		t.Fatalf("越权恢复不得改数据，实际 %q", got)
	}

	if err := svc.UnhidePost(e.ctx, guest, replyID); err != nil {
		t.Fatalf("白名单访客必须能恢复他人回复: %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "normal" {
		t.Fatalf("访客恢复未落库: %q", got)
	}
	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	detail, userID := readLastAdminAudit(t, e)
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if userID.Valid || fields["fingerprint"] != moderationFP || fields["command"] != "post unhide" {
		t.Fatalf("访客恢复审计必须以指纹标识主体: userID=%v detail=%s", userID, detail)
	}
}

// ADR-053：真实 SQLite 下作者已删的目标不可恢复，收敛为 forum.ErrNotFound。
func TestUnhideDeletedTargetIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	_, _, replyID := seedSpamThread(t, e, spammer)
	svc, _ := setupModerationService(t, e)
	actor := forum.Actor{UserID: e.userID, Fingerprint: moderationFP}
	if err := e.writer.Submit(e.ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE posts SET status = 'deleted' WHERE id = ?`, replyID)
		return err
	}); err != nil {
		t.Fatalf("准备已删目标: %v", err)
	}
	if err := svc.UnhidePost(e.ctx, actor, replyID); !errors.Is(err, forum.ErrNotFound) {
		t.Fatalf("已删除目标不得恢复，应返回 ErrNotFound，实际 %v", err)
	}
	if got := statusInDB(t, e, "posts", replyID); got != "deleted" {
		t.Fatalf("失败调用不得改状态，实际 %q", got)
	}
}

// ADR-053 补丁端到端：主题隐藏后由 /unthread 的用例恢复，DB 回 active、
// 公开视图重新可见、审计落 command="thread unthread"（与 thread unhide 分开）。
func TestUnthreadIntegration(t *testing.T) {
	e := newForumEnv(t)
	spammer := insertUser(t, e, "spammer")
	threadID, firstID, replyID := seedSpamThread(t, e, spammer)
	svc, logger := setupModerationService(t, e)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive, Fingerprint: moderationFP}

	denied := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	if err := denied.Unthread(e.ctx, actor, threadID); !errors.Is(err, forum.ErrAdminDenied) {
		t.Fatalf("非管理员恢复应被拒: %v", err)
	}
	if got := statusInDB(t, e, "threads", threadID); got != "active" {
		t.Fatalf("越权后数据不得变更: %q", got)
	}

	if err := svc.HideThread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("准备隐藏失败: %v", err)
	}
	if got := statusInDB(t, e, "threads", threadID); got != "hidden" {
		t.Fatalf("隐藏后应为 hidden: %q", got)
	}
	if err := svc.Unthread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("恢复隐藏主题失败: %v", err)
	}
	if got := statusInDB(t, e, "threads", threadID); got != "active" {
		t.Fatalf("恢复后应回 active: %q", got)
	}
	if got := statusInDB(t, e, "posts", firstID); got != "normal" || statusInDB(t, e, "posts", replyID) != "normal" {
		t.Fatal("恢复主题不得级联改回复行")
	}
	if _, err := e.repos.GetThread(e.ctx, threadID); err != nil {
		t.Fatalf("恢复后应可打开详情: %v", err)
	}
	page, err := svc.ListThreads(e.ctx, forum.ListThreadsQuery{NodeID: e.nodeID, Sort: forum.SortLatest, Limit: 20})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].ID != threadID {
		t.Fatalf("恢复后应重回公开列表: page=%+v err=%v", page, err)
	}
	// 幂等：未隐藏目标再恢复一次仍成功。
	if err := svc.Unthread(e.ctx, actor, threadID); err != nil {
		t.Fatalf("幂等恢复应成功: %v", err)
	}

	if err := logger.Flush(e.ctx); err != nil {
		t.Fatalf("审计落库: %v", err)
	}
	count, err := e.pools.CountAuthEvents(e.ctx, auth.AuditAdminAction)
	if err != nil || count != 3 {
		t.Fatalf("隐藏 + 两次恢复应落三条审计，实际 count=%d err=%v", count, err)
	}
	detail, userID := readLastAdminAudit(t, e)
	var fields map[string]string
	if err := json.Unmarshal([]byte(detail), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["command"] != "thread unthread" || fields["target"] != fmt.Sprintf("thread:%d", threadID) ||
		fields["fingerprint"] != moderationFP || fields["result"] != "success" ||
		!userID.Valid || userID.Int64 != e.userID {
		t.Fatalf("恢复审计字段不全: detail=%s userID=%v", detail, userID)
	}
}
