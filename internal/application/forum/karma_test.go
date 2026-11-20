package forum

// v0.5.0 / P3-M2 / ADR-057：karma 计分规则回归（P3-PLAN §3.2，用户 2026-11-20 确认）。
// 主题+2 / 回复+1 / 被赞+1 / 被踩不扣 / 改票按差 / 自投票不给 / 删内容不回退。

import (
	"context"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/domain/vote"
)

func karmaTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnv() // 默认无 outbox，避免事件投递干扰计分断言
}

func actor(userID int64) Actor {
	return Actor{UserID: userID, Status: user.StatusActive}
}

func TestKarmaOnCreateThreadAndPost(t *testing.T) {
	ctx := context.Background()
	e := karmaTestEnv(t)

	tid, err := e.svc.CreateThread(ctx, actor(1), CreateThreadInput{NodeID: 1, Title: "标题", Body: "正文"})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[1]; got != 2 {
		t.Fatalf("发主题应 +2，实际 %d", got)
	}

	e.advance(61 * time.Second) // 越过 60s 发帖限流窗口
	if _, err := e.svc.CreatePost(ctx, actor(1), CreatePostInput{ThreadID: tid, Body: "回复"}); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[1]; got != 3 {
		t.Fatalf("再发回复应 +1，累计 3，实际 %d", got)
	}
}

func TestKarmaOnVoteUpDownAndChange(t *testing.T) {
	ctx := context.Background()
	e := karmaTestEnv(t)
	// 主题作者是 2，投票者是 1。
	tid := e.repos.state.nextThreadID
	e.repos.addThread(thread.Thread{
		NodeID: 1, UserID: 2, Title: "目标", Score: 0,
		LastReplyAt: testBase, CreatedAt: testBase,
	})

	// upvote：作者 +1
	if _, err := e.svc.CastVote(ctx, actor(1), vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 1 {
		t.Fatalf("被赞应 +1，实际 %d", got)
	}

	// 改票 up→down：撤销之前的赞，作者回到 0（被踩本身不扣）。
	if _, err := e.svc.CastVote(ctx, actor(1), vote.TargetThread, tid, vote.Down); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 0 {
		t.Fatalf("up→down 应撤销 +1，作者回到 0，实际 %d", got)
	}

	// 再 down→up：作者 +1。
	if _, err := e.svc.CastVote(ctx, actor(1), vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 1 {
		t.Fatalf("down→up 应 +1，实际 %d", got)
	}

	// 撤票：作者回到 0。
	if _, err := e.svc.Unvote(ctx, actor(1), vote.TargetThread, tid); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 0 {
		t.Fatalf("撤赞应 -1，作者回到 0，实际 %d", got)
	}
}

func TestKarmaDownvoteAloneDoesNotPunishAuthor(t *testing.T) {
	ctx := context.Background()
	e := karmaTestEnv(t)
	tid := e.repos.state.nextThreadID
	e.repos.addThread(thread.Thread{
		NodeID: 1, UserID: 2, Title: "目标", Score: 0,
		LastReplyAt: testBase, CreatedAt: testBase,
	})
	if _, err := e.svc.CastVote(ctx, actor(1), vote.TargetThread, tid, vote.Down); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 0 {
		t.Fatalf("被踩不扣作者，应为 0，实际 %d", got)
	}
}

func TestKarmaSelfVoteGivesNothing(t *testing.T) {
	ctx := context.Background()
	e := karmaTestEnv(t)
	tid := e.repos.state.nextThreadID
	e.repos.addThread(thread.Thread{
		NodeID: 1, UserID: 2, Title: "自己的帖", Score: 0,
		LastReplyAt: testBase, CreatedAt: testBase,
	})
	if _, err := e.svc.CastVote(ctx, actor(2), vote.TargetThread, tid, vote.Up); err != nil {
		t.Fatal(err)
	}
	if got := e.repos.state.karma[2]; got != 0 {
		t.Fatalf("给自己投票不应加 karma，实际 %d", got)
	}
}

func TestKarmaRuleConstants(t *testing.T) {
	// 锁定规则常量，防止误改（ADR-057）。
	if karmaForThread != 2 || karmaForPost != 1 || karmaPerUpvote != 1 {
		t.Fatalf("karma 规则常量被误改: thread=%d post=%d upvote=%d",
			karmaForThread, karmaForPost, karmaPerUpvote)
	}
	_ = time.Now // 保持 import 稳定（本文件不直接依赖时钟）
}
