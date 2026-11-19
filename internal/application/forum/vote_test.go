package forum

// P1-M3 投票用例测试：全部走内存 fake，锁定幂等、增量、撤销、权限与事务回滚。
// 真实 SQLite 下的 last_reply_at 不变由 infrastructure 集成测试覆盖。

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/domain/vote"
)

func voteTestEnv(t *testing.T) (*testEnv, int64, int64) {
	t.Helper()
	e := newTestEnv()
	// 启用 outbox，投票事件随业务写落库，提交后再投递。
	o, err := NewOutbox(newFakeOutboxStore(0), &recordingSink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = NewService(e.repos, WithClock(func() time.Time { return e.now }), WithOutbox(o))
	tid := e.repos.state.nextThreadID
	e.repos.addThread(thread.Thread{
		NodeID: 1, UserID: 2, Title: "投票目标", Score: 0,
		LastReplyAt: testBase, CreatedAt: testBase,
	})
	pid := e.repos.state.nextPostID
	e.repos.addPost(post.Post{
		ThreadID: tid, UserID: 2, BodyMD: "正文", Status: post.StatusNormal, CreatedAt: testBase,
	})
	return e, tid, pid
}

func TestCastVoteIsIdempotentAndDeltaIsExact(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	ctx := context.Background()
	actor := activeActor()

	first, err := e.svc.CastVote(ctx, actor, vote.TargetThread, tid, vote.Up)
	if err != nil || first.Delta != 1 || first.Value != vote.Up {
		t.Fatalf("首次 up 应 +1: res=%+v err=%v", first, err)
	}
	if got := e.repos.state.threads[tid].Score; got != 1 {
		t.Fatalf("首次 up 后分数应为 1，实际 %d", got)
	}

	again, err := e.svc.CastVote(ctx, actor, vote.TargetThread, tid, vote.Up)
	if err != nil || again.Delta != 0 {
		t.Fatalf("重复同值投票应零增量: res=%+v err=%v", again, err)
	}
	if got := e.repos.state.threads[tid].Score; got != 1 {
		t.Fatalf("重复投票不应加分，实际 %d", got)
	}
	if n := len(e.repos.state.outboxEvents); n != 1 {
		t.Fatalf("幂等空操作不应产生第二个事件，实际 %d", n)
	}

	// 改票：-1 - (+1) = -2，分数从 1 变 -1。
	flipped, err := e.svc.CastVote(ctx, actor, vote.TargetThread, tid, vote.Down)
	if err != nil || flipped.Delta != -2 {
		t.Fatalf("up→down 应 -2: res=%+v err=%v", flipped, err)
	}
	if got := e.repos.state.threads[tid].Score; got != -1 {
		t.Fatalf("改票后分数应为 -1，实际 %d", got)
	}
	if v, ok := e.repos.state.votes[voteKey{1, vote.TargetThread, tid}]; !ok || v != vote.Down {
		t.Fatalf("投票记录应更新为 down: v=%v ok=%v", v, ok)
	}
}

func TestUnvoteRemovesVoteAndIsIdempotent(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	ctx := context.Background()
	actor := activeActor()

	if _, err := e.svc.CastVote(ctx, actor, vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	removed, err := e.svc.Unvote(ctx, actor, vote.TargetThread, tid)
	if err != nil || removed.Delta != -1 || removed.Value != 0 {
		t.Fatalf("撤销应 -1 且 Value=0: res=%+v err=%v", removed, err)
	}
	if got := e.repos.state.threads[tid].Score; got != 0 {
		t.Fatalf("撤销后分数应回到 0，实际 %d", got)
	}
	if _, ok := e.repos.state.votes[voteKey{1, vote.TargetThread, tid}]; ok {
		t.Fatal("撤销后不应残留投票记录")
	}

	before := len(e.repos.state.outboxEvents)
	noOp, err := e.svc.Unvote(ctx, actor, vote.TargetThread, tid)
	if err != nil || noOp.Delta != 0 {
		t.Fatalf("重复撤销应零增量: res=%+v err=%v", noOp, err)
	}
	if len(e.repos.state.outboxEvents) != before {
		t.Fatal("重复撤销不应产生事件")
	}
}

func TestVoteOnPostAttributesScoreToParentThreadAndKeepsActivity(t *testing.T) {
	e, tid, pid := voteTestEnv(t)
	ctx := context.Background()
	before := e.repos.state.threads[tid].LastReplyAt

	res, err := e.svc.CastVote(ctx, activeActor(), vote.TargetPost, pid, vote.Up)
	if err != nil {
		t.Fatalf("给内容投票失败: %v", err)
	}
	if res.ThreadID != tid || res.TargetID != pid {
		t.Fatalf("内容投票应记到所属主题: res=%+v", res)
	}
	if got := e.repos.state.threads[tid].Score; got != 1 {
		t.Fatalf("主题分数应为 1，实际 %d", got)
	}
	if !e.repos.state.threads[tid].LastReplyAt.Equal(before) {
		t.Fatalf("投票不得推进 last_reply_at: before=%v after=%v", before, e.repos.state.threads[tid].LastReplyAt)
	}
}

func TestVoteAppendsVoteCastEvent(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	if _, err := e.svc.CastVote(context.Background(), activeActor(), vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	events := e.repos.state.outboxEvents
	if len(events) != 1 {
		t.Fatalf("应追加 1 条事件，实际 %d", len(events))
	}
	ev := events[0]
	if ev.Type != event.EventVoteCast || ev.ID != "vote:1:thread:"+strconv.FormatInt(tid, 10) || ev.ThreadID != tid || ev.ActorID != 1 {
		t.Fatalf("投票事件字段不正确: %+v", ev)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("投票事件应能通过校验: %v", err)
	}
}

func TestVoteRejectsGuestAndFrozenUser(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.CastVote(ctx, Actor{Guest: true}, vote.TargetThread, tid, vote.Up); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("访客投票应拒绝: %v", err)
	}
	frozen := Actor{UserID: 1, Status: user.StatusPending}
	if _, err := e.svc.CastVote(ctx, frozen, vote.TargetThread, tid, vote.Up); !errors.Is(err, ErrForbidden) {
		t.Fatalf("非 active 用户投票应拒绝: %v", err)
	}
	// 事务内二次复核：Actor 快照是 active，但库里已冻结。
	e.repos.state.users[1] = user.StatusDeleted
	if _, err := e.svc.CastVote(ctx, activeActor(), vote.TargetThread, tid, vote.Up); !errors.Is(err, ErrForbidden) {
		t.Fatalf("事务内状态复核应拒绝: %v", err)
	}
}

func TestVoteRejectsMissingTargetAndInvalidInput(t *testing.T) {
	e, _, pid := voteTestEnv(t)
	ctx := context.Background()
	actor := activeActor()

	for _, tc := range []struct {
		name string
		typ  vote.TargetType
		id   int64
	}{
		{"主题不存在", vote.TargetThread, 9999},
		{"内容不存在", vote.TargetPost, 9999},
		{"软删除内容", vote.TargetPost, func() int64 {
			e.repos.state.posts[pid] = post.Post{ID: pid, ThreadID: 1, Status: post.StatusDeleted}
			return pid
		}()},
	} {
		if _, err := e.svc.CastVote(ctx, actor, tc.typ, tc.id, vote.Up); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s 应返回 ErrNotFound，实际 %v", tc.name, err)
		}
	}
	if _, err := e.svc.CastVote(ctx, actor, vote.TargetType("bogus"), 1, vote.Up); !errors.Is(err, ErrInvalidVoteTarget) {
		t.Errorf("非法目标类型应拒绝: %v", err)
	}
	if _, err := e.svc.CastVote(ctx, actor, vote.TargetThread, 1, vote.Value(0)); !errors.Is(err, ErrInvalidVote) {
		t.Errorf("非法投票值应拒绝: %v", err)
	}
}

func TestVoteRollsBackWholeWriteOnScoreFailure(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	e.repos.failAdjustScore = errors.New("注入分数失败")
	if _, err := e.svc.CastVote(context.Background(), activeActor(), vote.TargetThread, tid, vote.Up); err == nil {
		t.Fatal("改分失败应透传错误")
	}
	if _, ok := e.repos.state.votes[voteKey{1, vote.TargetThread, tid}]; ok {
		t.Fatal("改分失败时投票记录必须随事务回滚")
	}
	if got := e.repos.state.threads[tid].Score; got != 0 {
		t.Fatalf("回滚后分数应为 0，实际 %d", got)
	}
	if len(e.repos.state.outboxEvents) != 0 {
		t.Fatal("回滚后不应留下事件")
	}
}

func TestVoteDownFirstThenUpDeltaIsTwo(t *testing.T) {
	e, tid, _ := voteTestEnv(t)
	ctx := context.Background()
	if _, err := e.svc.CastVote(ctx, activeActor(), vote.TargetThread, tid, vote.Down); err != nil {
		t.Fatal(err)
	}
	up, err := e.svc.CastVote(ctx, activeActor(), vote.TargetThread, tid, vote.Up)
	if err != nil || up.Delta != 2 {
		t.Fatalf("down→up 应 +2: res=%+v err=%v", up, err)
	}
	if got := e.repos.state.threads[tid].Score; got != 1 {
		t.Fatalf("最终分数应为 1，实际 %d", got)
	}
}
