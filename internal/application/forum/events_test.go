package forum

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/thread"
)

type recordingSink struct {
	events []event.DomainEvent
	fail   error
}

func (s *recordingSink) Publish(_ context.Context, e event.DomainEvent) error {
	s.events = append(s.events, e)
	return s.fail
}

type observedFailure struct {
	e   event.DomainEvent
	err error
}

type observerLog struct{ failures []observedFailure }

func (l *observerLog) observe(e event.DomainEvent, err error) {
	l.failures = append(l.failures, observedFailure{e: e, err: err})
}

func newEventService(repos *fakeRepos, sink *recordingSink, opts ...ServiceOption) (*Service, *observerLog) {
	obs := &observerLog{}
	all := []ServiceOption{
		WithClock(func() time.Time { return testBase }),
		WithEventSink(sink),
		WithPublishObserver(obs.observe),
	}
	all = append(all, opts...)
	return NewService(repos, all...), obs
}

func threadForEvent(id int64) thread.Thread {
	return thread.Thread{
		ID: id, NodeID: 1, UserID: 1, Title: "事件测试主题", Score: 0,
		LastReplyAt: testBase.Add(-2 * time.Hour),
		CreatedAt:   testBase.Add(-2 * time.Hour),
	}
}

func TestCreateThreadPublishesAfterCommit(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	sink := &recordingSink{}
	svc, _ := newEventService(repos, sink)

	id, err := svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	})
	if err != nil {
		t.Fatalf("发帖失败: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("应发布 1 条 PostCreated，实际 %d", len(sink.events))
	}
	e := sink.events[0]
	if e.Type != event.EventPostCreated || !e.FirstPost || e.ActorID != 1 ||
		e.NodeID != 1 || e.ThreadID != id || e.PostID <= 0 || e.ID == "" {
		t.Fatalf("事件字段错误: %+v", e)
	}
	if !e.OccurredAt.Equal(testBase) {
		t.Fatalf("事件时间应为用例时钟: %v", e.OccurredAt)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("发布的事件应自洽: %v", err)
	}
	if svc.PublishFailures() != 0 {
		t.Fatal("正常发布不应计入失败")
	}
}

func TestCreatePostPublishesAfterCommit(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadForEvent(1))
	sink := &recordingSink{}
	// 用可控时钟跨过 60 秒冷却，避免误判为限流。
	svc, _ := newEventService(repos, sink, WithClock(func() time.Time { return testBase.Add(2 * time.Minute) }))
	postID, err := svc.CreatePost(ctx, activeActor(), CreatePostInput{ThreadID: 1, Body: "回复"})
	if err != nil {
		t.Fatalf("回复失败: %v", err)
	}
	if len(sink.events) != 1 {
		t.Fatalf("应发布 1 条事件，实际 %d", len(sink.events))
	}
	e := sink.events[0]
	if e.FirstPost || e.PostID != postID || e.ThreadID != 1 || e.NodeID != 0 {
		t.Fatalf("回复事件字段错误: %+v", e)
	}
}

func TestPublishFailureDoesNotFailCommittedWrite(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	sink := &recordingSink{fail: errors.New("插件投递失败")}
	svc, obs := newEventService(repos, sink)

	if _, err := svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	}); err != nil {
		t.Fatalf("事件投递失败不应让已提交的发帖看起来失败: %v", err)
	}
	if svc.PublishFailures() != 1 || len(obs.failures) != 1 {
		t.Fatalf("失败必须可观测: failures=%d observers=%d", svc.PublishFailures(), len(obs.failures))
	}
	// 数据确实已落库：再读一次版块下的主题能看到它。
	page, err := svc.ListThreads(ctx, ListThreadsQuery{NodeID: 1, Sort: SortLatest})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("事务应已提交: rows=%d err=%v", len(page.Rows), err)
	}
}

func TestWriteFailureDoesNotPublish(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.failInsertPost = errors.New("模拟插入失败")
	sink := &recordingSink{}
	svc, _ := newEventService(repos, sink)

	if _, err := svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	}); err == nil {
		t.Fatal("注入的写入失败应让用例返回错误")
	}
	if len(sink.events) != 0 {
		t.Fatal("事务未提交成功时绝不能发布领域事件")
	}
}

func TestNoSinkKeepsP010Behavior(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newFakeRepos(), WithClock(func() time.Time { return testBase }))
	if _, err := svc.CreateThread(ctx, activeActor(), CreateThreadInput{
		NodeID: 1, Title: "标题", Body: "正文",
	}); err != nil {
		t.Fatalf("未注入 sink 时行为不应改变: %v", err)
	}
	if svc.PublishFailures() != 0 {
		t.Fatal("未注入 sink 不应产生失败计数")
	}
}

func TestPublicQueryThreadTitle(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadForEvent(3))
	q := NewPublicQuery(repos)

	title, err := q.ThreadTitle(ctx, 3)
	if err != nil || title != "事件测试主题" {
		t.Fatalf("只读查询应返回标题: %q err=%v", title, err)
	}
	if _, err := q.ThreadTitle(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的主题应透传 ErrNotFound: %v", err)
	}
}
