package sqlite

// P2-M2 / ADR-051 的真实 SQLite 集成：application/forum.Service 直接驱动仓储。
// 验证 ① body_md 覆盖 + edited_at 落库 + GetThread 可见新正文；
// ② status/created_at/last_reply_at 均不被推进（列表排序不变）；
// ③ FTS 索引随编辑重建（检索命中新正文、旧正文不再命中）；
// ④ ScreenThreadDetail 的 Detail.EditedAt 取自首帖。
// 用例层的权限/校验语义由 forum/edit_test.go 覆盖，这里只看真实 DB 契约。

import (
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/user"
)

func TestEditPostRealSQLite(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	svc := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}

	threadID, err := svc.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: e.nodeID, Title: "编辑验收主题", Body: "原始正文FTSOLD",
	})
	if err != nil {
		t.Fatalf("建主题: %v", err)
	}
	detail, err := svc.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	firstID := detail.Posts[0].ID
	originalCreated := detail.Posts[0].CreatedAt
	originalLastReply := detail.Thread.LastReplyAt

	// 跨 2 分钟编辑，edited_at 必须独立于 created_at。
	editedAt := testBaseTime.Add(2 * time.Minute)
	svc2 := forum.NewService(e.repos, forum.WithClock(func() time.Time { return editedAt }))
	if err := svc2.EditPost(ctx, actor, forum.EditPostInput{PostID: firstID, Body: "改后正文FTSNEW"}); err != nil {
		t.Fatalf("编辑首帖: %v", err)
	}

	// ① 库里正文与 edited_at。
	var body string
	var edited int64
	var status string
	var created int64
	if err := e.pools.reader.QueryRowContext(ctx,
		`SELECT body_md, edited_at, status, created_at FROM posts WHERE id = ?`, firstID).
		Scan(&body, &edited, &status, &created); err != nil {
		t.Fatalf("读取编辑后行: %v", err)
	}
	if body != "改后正文FTSNEW" {
		t.Fatalf("正文未覆盖: %q", body)
	}
	if edited != editedAt.Unix() {
		t.Fatalf("edited_at 应为 %d，实际 %d", editedAt.Unix(), edited)
	}
	if status != "normal" || created != originalCreated.Unix() {
		t.Fatalf("编辑不得改 status / created_at: status=%q created=%d", status, created)
	}

	// ② last_reply_at 不变 → 列表排序不因编辑被顶到最新。
	after, err := svc.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Thread.LastReplyAt.Equal(originalLastReply) {
		t.Fatalf("编辑推进了 last_reply_at: before=%v after=%v", originalLastReply, after.Thread.LastReplyAt)
	}
	if after.Posts[0].BodyMD != "改后正文FTSNEW" || after.Posts[0].EditedAt != editedAt.Unix() {
		t.Fatalf("GetThread 应可见新正文与 edited_at: %+v", after.Posts[0])
	}
	page, err := svc.ListThreads(ctx, forum.ListThreadsQuery{NodeID: e.nodeID, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 1 || !page.Rows[0].LastReplyAt.Equal(originalLastReply) {
		t.Fatalf("列表排序应保持: rows=%+v err=%v", page, err)
	}

	// ③ FTS 重建：新词命中、旧词不再命中。
	hits, err := svc.Search(ctx, forum.SearchInput{Query: "FTSNEW", Limit: 10})
	if err != nil || len(hits.Hits) == 0 {
		t.Fatalf("检索应命中新正文: hits=%+v err=%v", hits, err)
	}
	oldHits, err := svc.Search(ctx, forum.SearchInput{Query: "FTSOLD", Limit: 10})
	if err != nil || len(oldHits.Hits) != 0 {
		t.Fatalf("检索不应再命中旧正文: hits=%+v err=%v", oldHits, err)
	}

	// ④ 详情页 Detail.EditedAt 来自首帖。
	sc, err := svc.ScreenThreadDetail(ctx, threadID, forum.ViewContext{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Detail.EditedAt != editedAt.Unix() || !strings.Contains(sc.Detail.Body, "改后正文") {
		t.Fatalf("详情正文 / 编辑标记取自首帖: %+v", sc.Detail)
	}
}

func TestEditPostReplyRealSQLite(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	svc := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime }))
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}

	tid, err := svc.CreateThread(ctx, actor, forum.CreateThreadInput{NodeID: e.nodeID, Title: "回复编辑", Body: "首帖"})
	if err != nil {
		t.Fatal(err)
	}
	// 跨过 60 秒冷却再回复（主题与回复共用发帖配额）。
	svcReply := forum.NewService(e.repos, forum.WithClock(func() time.Time { return testBaseTime.Add(90 * time.Second) }))
	pid, err := svcReply.CreatePost(ctx, actor, forum.CreatePostInput{ThreadID: tid, Body: "回复原文"})
	if err != nil {
		t.Fatal(err)
	}
	editedAt := testBaseTime.Add(3 * time.Minute)
	svc2 := forum.NewService(e.repos, forum.WithClock(func() time.Time { return editedAt }))
	if err := svc2.EditPost(ctx, actor, forum.EditPostInput{PostID: pid, Body: "回复新文"}); err != nil {
		t.Fatalf("编辑回复: %v", err)
	}

	detail, err := svc.GetThread(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Posts) != 2 || detail.Posts[0].BodyMD != "首帖" || detail.Posts[0].EditedAt != 0 {
		t.Fatalf("首帖不得被回复编辑牵连: %+v", detail.Posts)
	}
	if detail.Posts[1].BodyMD != "回复新文" || detail.Posts[1].EditedAt != editedAt.Unix() {
		t.Fatalf("回复正文 / edited_at 应更新: %+v", detail.Posts[1])
	}
}
