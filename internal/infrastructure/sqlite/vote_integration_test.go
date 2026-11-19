package sqlite

// P1-M3 投票的真实链路测试：真实 SQLite + 真实 Writer + 真实事务 + 真实 Outbox。
// 核心断言是「分数变了，last_reply_at 一步没动」——这正是 TouchThread/AdjustScore
// 必须分离的原因。

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/domain/vote"
)

func voteService(t *testing.T, e *forumEnv) (*forum.Service, *captureSink) {
	t.Helper()
	sink := &captureSink{}
	o, err := forum.NewOutbox(NewOutboxStore(e.writer), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	return forum.NewService(e.repos,
		forum.WithClock(func() time.Time { return time.Unix(e.clock, 0).UTC() }),
		forum.WithOutbox(o),
	), sink
}

func threadScoreActivity(t *testing.T, e *forumEnv, threadID int64) (score, lastReply int64) {
	t.Helper()
	if err := e.pools.reader.QueryRowContext(e.ctx,
		`SELECT score, last_reply_at FROM threads WHERE id = ?`, threadID).Scan(&score, &lastReply); err != nil {
		t.Fatalf("读取主题分数与活跃度: %v", err)
	}
	return
}

func TestAdjustScoreNeverTouchesLastReplyAt(t *testing.T) {
	e := newForumEnv(t)
	wantLast := e.clock + 12345
	tid := e.insertThread(t, "投票不改活跃度", 0, wantLast)

	// 直接走受限 WriteTx，证明 AdjustScore 自身的 SQL 只改 score。
	if err := e.repos.Write(e.ctx, func(tx forum.WriteTx) error {
		return tx.AdjustScore(e.ctx, tid, 2)
	}); err != nil {
		t.Fatalf("AdjustScore 失败: %v", err)
	}
	score, last := threadScoreActivity(t, e, tid)
	if score != 2 {
		t.Fatalf("分数应为 2，实际 %d", score)
	}
	if last != wantLast {
		t.Fatalf("last_reply_at 不得被 AdjustScore 改动: want=%d got=%d", wantLast, last)
	}
}

func TestVoteRealSQLiteIdempotencyAndDelta(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "真实投票", 0, e.clock)
	svc, _ := voteService(t, e)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}

	if _, err := svc.CastVote(e.ctx, actor, vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CastVote(e.ctx, actor, vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if score, _ := threadScoreActivity(t, e, tid); score != 1 {
		t.Fatalf("重复投票后分数应为 1，实际 %d", score)
	}
	if _, err := svc.CastVote(e.ctx, actor, vote.TargetThread, tid, vote.Down); err != nil {
		t.Fatal(err)
	}
	if score, _ := threadScoreActivity(t, e, tid); score != -1 {
		t.Fatalf("改票后分数应为 -1，实际 %d", score)
	}
	if _, err := svc.Unvote(e.ctx, actor, vote.TargetThread, tid); err != nil {
		t.Fatal(err)
	}
	if score, _ := threadScoreActivity(t, e, tid); score != 0 {
		t.Fatalf("撤销后分数应为 0，实际 %d", score)
	}
	var votes int
	if err := e.pools.reader.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM votes`).Scan(&votes); err != nil {
		t.Fatal(err)
	}
	if votes != 0 {
		t.Fatalf("撤销后 votes 表应无记录，实际 %d", votes)
	}
}

func TestVoteOnPostRealSQLiteAttributesScoreWithoutBumpingThread(t *testing.T) {
	e := newForumEnv(t)
	wantLast := e.clock + 999
	tid := e.insertThread(t, "被投票主题", 0, wantLast)
	pid := e.insertPost(t, tid, "被投票正文", post.StatusNormal, e.clock)
	svc, _ := voteService(t, e)

	res, err := svc.CastVote(e.ctx, forum.Actor{UserID: e.userID, Status: user.StatusActive},
		vote.TargetPost, pid, vote.Up)
	if err != nil {
		t.Fatalf("给内容投票失败: %v", err)
	}
	if res.ThreadID != tid {
		t.Fatalf("分数应记到所属主题 %d，实际 %d", tid, res.ThreadID)
	}
	score, last := threadScoreActivity(t, e, tid)
	if score != 1 || last != wantLast {
		t.Fatalf("内容投票应只改分数: score=%d last=%d", score, last)
	}
}

func TestVoteEventLandsInOutboxAndIsDelivered(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "事件投票", 0, e.clock)
	svc, sink := voteService(t, e)
	actor := forum.Actor{UserID: e.userID, Status: user.StatusActive}

	if _, err := svc.CastVote(e.ctx, actor, vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	eventID := "vote:" + strconv.FormatInt(e.userID, 10) + ":thread:" + strconv.FormatInt(tid, 10)
	status, _ := outboxStatus(t, e, eventID)
	if status != "delivered" {
		t.Fatalf("投票事件提交后应已投递，实际状态 %q", status)
	}
	if len(sink.events) != 1 || sink.events[0].ID != eventID {
		t.Fatalf("出口应收到 1 条投票事件: %+v", sink.events)
	}
	// 同目标重复投票被幂等吞掉，outbox 不应新增事件。
	if _, err := svc.CastVote(e.ctx, actor, vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if n := outboxCount(t, e); n != 1 {
		t.Fatalf("幂等投票不应新增 outbox 行，实际 %d", n)
	}
}

func TestVoteGuestRejectedOnRealSQLite(t *testing.T) {
	e := newForumEnv(t)
	tid := e.insertThread(t, "访客投票", 0, e.clock)
	svc, _ := voteService(t, e)
	if _, err := svc.CastVote(e.ctx, forum.Actor{Guest: true}, vote.TargetThread, tid, vote.Up); !errors.Is(err, forum.ErrReadOnly) {
		t.Fatalf("访客投票应被拒绝: %v", err)
	}
	if score, _ := threadScoreActivity(t, e, tid); score != 0 {
		t.Fatalf("被拒的投票不应改分数，实际 %d", score)
	}
}
