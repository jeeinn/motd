package forum

// P2-M1b 治理用例回归：权限单点、hidden 语义、幂等、固定文案、同事务审计。
// 全部用内存 fake，真实 SQL / 审计落库见 infrastructure/sqlite 集成测试。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
)

const moderationAdminFP = "SHA256:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"

func moderationAdmin(userID int64) Actor {
	return Actor{UserID: userID, Fingerprint: moderationAdminFP}
}

func moderationService(repos Repos, opts ...ServiceOption) *Service {
	// 默认启用 outbox：治理写必须走 AppendOutbox，不能只走提交后 publish。
	store := newFakeOutboxStore(0)
	sink := &recordingSink{}
	outbox, err := NewOutbox(store, sink, nil)
	if err != nil {
		panic(err)
	}
	switch r := repos.(type) {
	case *fakeRepos:
		r.outboxStore = store
	case *rollbackAfterAuditRepos:
		r.fakeRepos.outboxStore = store
	}
	all := append([]ServiceOption{
		WithClock(func() time.Time { return testBase }),
		WithAdminWhitelist(map[string]struct{}{moderationAdminFP: {}}),
		WithOutbox(outbox), WithEventSink(sink),
	}, opts...)
	return NewService(repos, all...)
}

// 管理员隐藏他人回复：只置 hidden，他人其它回复不受影响，审计事件完整。
func TestHidePostByAdminHidesOthersContent(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadOf(10, 7))
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	repos.addPost(postOf(21, 10, 8, post.StatusNormal))
	svc := moderationService(repos)

	// 管理员是 9，内容作者是 7：治理权与作者身份无关。
	if err := svc.HidePost(ctx, moderationAdmin(9), 20); err != nil {
		t.Fatalf("管理员隐藏他人回复应成功: %v", err)
	}
	if got := repos.state.posts[20].Status; got != post.StatusHidden {
		t.Fatalf("目标应为 hidden（不是 deleted），实际 %q", got)
	}
	if got := repos.state.posts[21].Status; got != post.StatusNormal {
		t.Fatalf("其它回复不得被级联，实际 %q", got)
	}
	visible, _, err := repos.ListPosts(ctx, PostQuery{ThreadID: 10, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != 21 {
		t.Fatalf("隐藏内容不得出现在公开列表: %+v", visible)
	}
	if len(repos.state.outboxEvents) != 1 {
		t.Fatalf("应记一条审计，实际 %d", len(repos.state.outboxEvents))
	}
	ev := repos.state.outboxEvents[0]
	if ev.Type != event.EventAdminAction || ev.AdminCommand != "post hide" ||
		ev.AdminTarget != "post:20" || ev.PostID != 20 || ev.ActorID != 9 ||
		ev.AdminFingerprint != moderationAdminFP || ev.AdminResult != "success" {
		t.Fatalf("审计事件字段不全: %+v", ev)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("审计事件必须通过领域校验: %v", err)
	}
}

// 管理员隐藏他人主题：主题对公开视图不可见，回复行原样保留。
func TestHideThreadByAdminKeepsReplyRows(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadOf(10, 7))
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	svc := moderationService(repos)

	if err := svc.HideThread(ctx, moderationAdmin(9), 10); err != nil {
		t.Fatalf("管理员隐藏他人主题应成功: %v", err)
	}
	if got := repos.state.threadStatus[10]; got != "hidden" {
		t.Fatalf("主题应为 hidden，实际 %q", got)
	}
	if _, err := repos.GetThread(ctx, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("隐藏主题应对公开详情不可见: %v", err)
	}
	rows, _, err := repos.ListThreads(ctx, ThreadQuery{NodeID: 1, Sort: SortLatest, Limit: 20})
	if err != nil || len(rows) != 0 {
		t.Fatalf("隐藏主题不得出现在公开列表: rows=%+v err=%v", rows, err)
	}
	if got := repos.state.posts[20].Status; got != post.StatusNormal {
		t.Fatalf("隐藏主题不得级联改回复行，实际 %q", got)
	}
	ev := repos.state.outboxEvents[0]
	if ev.AdminCommand != "thread hide" || ev.AdminTarget != "thread:10" || ev.ThreadID != 10 {
		t.Fatalf("隐藏主题的审计命令/目标不对: %+v", ev)
	}
}

// 越权：空白名单、名单外指纹、空指纹一律 ErrAdminDenied，且数据零变更、无审计。
func TestHideRejectsNonAdminWithoutChanges(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		actor Actor
		list  map[string]struct{}
	}{
		{"空白名单", moderationAdmin(9), nil},
		{"名单外指纹", Actor{UserID: 9, Fingerprint: "SHA256:" + strings.Repeat("B", 43)}, map[string]struct{}{moderationAdminFP: {}}},
		{"空指纹访客", Actor{Guest: true}, map[string]struct{}{moderationAdminFP: {}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := newFakeRepos()
			repos.addThread(threadOf(10, 7))
			repos.addPost(postOf(20, 10, 7, post.StatusNormal))
			before := repos.state.clone()
			svc := NewService(repos, WithClock(func() time.Time { return testBase }), WithAdminWhitelist(tc.list))

			if err := svc.HidePost(ctx, tc.actor, 20); !errors.Is(err, ErrAdminDenied) {
				t.Errorf("HidePost 应被拒，实际 %v", err)
			}
			if err := svc.HideThread(ctx, tc.actor, 10); !errors.Is(err, ErrAdminDenied) {
				t.Errorf("HideThread 应被拒，实际 %v", err)
			}
			if !reflect.DeepEqual(before, repos.state) {
				t.Fatal("越权调用不得改变任何数据，也不得写审计")
			}
			if n := NoticeFromError(ErrAdminDenied); n.Code != "admin_denied" || n.Message != "仅管理员可执行" {
				t.Fatalf("越权提示口径不对: %+v", n)
			}
		})
	}
}

// bootstrap：白名单内的访客（UserID=0）也能隐藏他人内容，审计主体用指纹。
func TestWhitelistedGuestCanHideOthersContent(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadOf(10, 7))
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	svc := moderationService(repos)
	guest := Actor{Guest: true, Fingerprint: moderationAdminFP}

	if err := svc.HidePost(ctx, guest, 20); err != nil {
		t.Fatalf("白名单访客必须能隐藏他人回复: %v", err)
	}
	if err := svc.HideThread(ctx, guest, 10); err != nil {
		t.Fatalf("白名单访客必须能隐藏他人主题: %v", err)
	}
	for i, ev := range repos.state.outboxEvents {
		if ev.ActorID != 0 || ev.AdminFingerprint != moderationAdminFP || ev.Type != event.EventAdminAction {
			t.Fatalf("第 %d 条审计必须以指纹标识访客主体: %+v", i, ev)
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("访客审计事件应通过领域校验: %v", err)
		}
	}
}

// 管理员隐藏自己的内容也允许：授权只看白名单，不做作者检查。
func TestAdminCanHideOwnContent(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadOf(10, 7))
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	svc := moderationService(repos)
	actor := moderationAdmin(7)

	if err := svc.HidePost(ctx, actor, 20); err != nil {
		t.Fatalf("管理员隐藏自己的回复应成功: %v", err)
	}
	if err := svc.HideThread(ctx, actor, 10); err != nil {
		t.Fatalf("管理员隐藏自己的主题应成功: %v", err)
	}
	if repos.state.posts[20].Status != post.StatusHidden || repos.state.threadStatus[10] != "hidden" {
		t.Fatalf("自己的内容也应置 hidden: post=%q thread=%q", repos.state.posts[20].Status, repos.state.threadStatus[10])
	}
}

// 幂等：重复隐藏不报错；每次成功调用仍各记一条审计（事件 ID 含纳秒，不被去重）。
func TestHideIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repos := newFakeRepos()
	repos.addThread(threadOf(10, 7))
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	// 时钟每调用前进 1ns：生产用 time.Now()，这里模拟两次独立调用的审计 ID 不同。
	now := testBase
	svc := moderationService(repos, WithClock(func() time.Time {
		now = now.Add(time.Nanosecond)
		return now
	}))

	for i := 0; i < 2; i++ {
		if err := svc.HidePost(ctx, moderationAdmin(9), 20); err != nil {
			t.Fatalf("第 %d 次隐藏回复应幂等成功: %v", i+1, err)
		}
		if err := svc.HideThread(ctx, moderationAdmin(9), 10); err != nil {
			t.Fatalf("第 %d 次隐藏主题应幂等成功: %v", i+1, err)
		}
	}
	if len(repos.state.outboxEvents) != 4 {
		t.Fatalf("两次调用各记两条审计，实际 %d", len(repos.state.outboxEvents))
	}
	if repos.state.outboxEvents[0].ID == repos.state.outboxEvents[2].ID {
		t.Fatal("重复隐藏的审计事件 ID 不得相同（否则被 outbox 去重）")
	}
}

// 不存在 / 作者已删 / 非法 ID 收敛为固定文案，不回显输入，不泄漏状态差异。
func TestHideNotFoundUsesFixedMessage(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		postID   int64
		threadID int64
		seed     func(*fakeRepos)
	}{
		{"不存在的回复", 999, 0, func(*fakeRepos) {}},
		{"不存在的主题", 0, 999, func(*fakeRepos) {}},
		{"作者已删的回复", 20, 0, func(r *fakeRepos) {
			r.addPost(postOf(20, 10, 7, post.StatusDeleted))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := newFakeRepos()
			tc.seed(repos)
			svc := moderationService(repos)
			var err error
			if tc.postID != 0 {
				err = svc.HidePost(ctx, moderationAdmin(9), tc.postID)
			} else {
				err = svc.HideThread(ctx, moderationAdmin(9), tc.threadID)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("应统一返回 ErrNotFound，实际 %v", err)
			}
			n := NoticeFromError(err)
			if n.Code != "not_found" || n.Message != "对象不存在" || strings.Contains(n.Message, "999") {
				t.Fatalf("固定文案不对或回显输入: %+v", n)
			}
		})
	}
}

// rollbackAfterAuditRepos 在 AppendOutbox 之后、事务提交之前注入失败，
// 证明业务写与审计事件同事务提交：失败时两者一起回滚。
type rollbackAfterAuditRepos struct{ *fakeRepos }

func (r *rollbackAfterAuditRepos) Write(ctx context.Context, fn func(WriteTx) error) error {
	return r.fakeRepos.Write(ctx, func(tx WriteTx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return errors.New("注入提交前失败")
	})
}

func TestHideAuditAndWriteShareTransaction(t *testing.T) {
	ctx := context.Background()
	repos := &rollbackAfterAuditRepos{fakeRepos: newFakeRepos()}
	repos.addPost(postOf(20, 10, 7, post.StatusNormal))
	svc := moderationService(repos)

	if err := svc.HidePost(ctx, moderationAdmin(9), 20); err == nil {
		t.Fatal("注入的提交前失败必须让用例返回错误")
	}
	if got := repos.state.posts[20].Status; got != post.StatusNormal {
		t.Fatalf("回滚后内容状态不得变为 hidden: %q", got)
	}
	if len(repos.state.outboxEvents) != 0 {
		t.Fatalf("回滚后审计事件也不得留下: %+v", repos.state.outboxEvents)
	}
}

func threadOf(id, owner int64) thread.Thread {
	return thread.Thread{ID: id, NodeID: 1, UserID: owner, Title: "主题", CreatedAt: testBase}
}

func postOf(id, threadID, owner int64, status post.Status) post.Post {
	return post.Post{ID: id, ThreadID: threadID, UserID: owner, BodyMD: "内容", Status: status, CreatedAt: testBase}
}
