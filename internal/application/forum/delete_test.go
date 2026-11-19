package forum

// 作者删除用例的回归集（v0.2.1）。对齐 PHASE2-PLAN §1.2 三条验收：
// ① 作者删主题 → 软删、主题与回复不可见、回复行仍在库；
// ② 非作者 / 访客 → 拒绝且数据零变更；
// ③ 不存在 / 已删目标 → 统一提示，不泄漏状态。
// 真实 SQL 的「不物理删除 / 不级联 / FTS 同步」由 sqlite 集成测试覆盖。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

func TestCanModify(t *testing.T) {
	cases := []struct {
		name            string
		userID, ownerID int64
		want            bool
	}{
		{"作者本人", 7, 7, true},
		{"他人", 8, 7, false},
		{"访客", 0, 7, false},
		{"零作者", 7, 0, false},
	}
	for _, c := range cases {
		if got := canModify(c.userID, c.ownerID); got != c.want {
			t.Errorf("%s: canModify(%d,%d)=%v，期望 %v", c.name, c.userID, c.ownerID, got, c.want)
		}
	}
}

// 验收①：作者删主题后主题不可见，但下属回复行原样留在库里。
func TestDeleteOwnThreadKeepsReplyRows(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "待删主题", CreatedAt: testBase})
	e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 8, BodyMD: "他人回复", Status: post.StatusNormal, CreatedAt: testBase})

	res, err := e.svc.Delete(ctx, Actor{UserID: 7, Status: user.StatusActive}, DeleteThread, 10)
	if err != nil {
		t.Fatalf("作者删主题应成功: %v", err)
	}
	if res.TargetID != 10 || res.ThreadID != 10 || res.TargetType != DeleteThread {
		t.Fatalf("删除结果不正确: %+v", res)
	}
	if _, err := e.repos.GetThread(ctx, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已删主题应对公开读取不可见: %v", err)
	}
	if _, err := e.svc.GetThread(ctx, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("详情用例应拒绝已删主题: %v", err)
	}
	p, ok := e.repos.state.posts[20]
	if !ok || p.Status != post.StatusNormal {
		t.Fatalf("回复行必须原样留在库里（软删不级联），实际 %+v ok=%v", p, ok)
	}
}

// 验收②：非作者与访客一律被拒，且数据零变更；提示文案统一。
func TestDeleteByOtherOrGuestIsRejectedWithoutChanges(t *testing.T) {
	actors := []struct {
		name  string
		actor Actor
	}{
		{"非作者", Actor{UserID: 8, Status: user.StatusActive}},
		{"访客", Actor{Guest: true}},
	}
	for _, tc := range actors {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			ctx := context.Background()
			e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "他人主题", CreatedAt: testBase})
			e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "他人回复", Status: post.StatusNormal, CreatedAt: testBase})
			before := e.repos.state.clone()

			for _, target := range []struct {
				typ DeleteTargetType
				id  int64
			}{{DeleteThread, 10}, {DeletePost, 20}} {
				_, err := e.svc.Delete(ctx, tc.actor, target.typ, target.id)
				if !errors.Is(err, ErrNotOwner) {
					t.Errorf("%s 删 %v:%d 应返回 ErrNotOwner，实际 %v", tc.name, target.typ, target.id, err)
				}
				if n := NoticeFromError(err); n.Code != "not_owner" || n.Message != "仅作者可删除" {
					t.Errorf("提示口径不对: %+v", n)
				}
			}
			if !reflect.DeepEqual(before, e.repos.state) {
				t.Fatal("被拒后数据必须零变更")
			}
		})
	}
}

// 作者删自己的回复：只置 deleted，主题与其它回复不受影响。
func TestDeleteOwnPostMarksDeletedKeepsThread(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "主题", CreatedAt: testBase})
	e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "待删回复", Status: post.StatusNormal, CreatedAt: testBase})
	e.repos.addPost(post.Post{ID: 21, ThreadID: 10, UserID: 9, BodyMD: "另一条回复", Status: post.StatusNormal, CreatedAt: testBase})

	res, err := e.svc.Delete(ctx, Actor{UserID: 7, Status: user.StatusActive}, DeletePost, 20)
	if err != nil {
		t.Fatalf("作者删回复应成功: %v", err)
	}
	if res.TargetType != DeletePost || res.TargetID != 20 {
		t.Fatalf("删除结果不正确: %+v", res)
	}
	if p := e.repos.state.posts[20]; p.Status != post.StatusDeleted {
		t.Fatalf("回复应软删为 deleted，实际 %q", p.Status)
	}
	if p := e.repos.state.posts[21]; p.Status != post.StatusNormal {
		t.Fatalf("其它回复不得被级联: %q", p.Status)
	}
	if _, err := e.repos.GetThread(ctx, 10); err != nil {
		t.Fatalf("主题不得因删回复而消失: %v", err)
	}
	visible, _, err := e.repos.ListPosts(ctx, PostQuery{ThreadID: 10, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != 21 {
		t.Fatalf("已删回复不应出现在可见列表: %+v", visible)
	}
}

// 验收③：目标不存在 / 已软删 / ID 非法，即便调用者是作者也只拿到统一提示。
func TestDeleteNeverLeaksExistenceOrStatus(t *testing.T) {
	cases := []struct {
		name string
		typ  DeleteTargetType
		id   int64
		seed func(*testEnv)
	}{
		{"不存在的主题", DeleteThread, 999, func(*testEnv) {}},
		{"不存在的回复", DeletePost, 999, func(*testEnv) {}},
		{"非法ID", DeleteThread, 0, func(*testEnv) {}},
		{"已软删的回复", DeletePost, 20, func(e *testEnv) {
			e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, Status: post.StatusDeleted, CreatedAt: testBase})
		}},
	}
	actor := Actor{UserID: 7, Status: user.StatusActive}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			tc.seed(e)
			_, err := e.svc.Delete(context.Background(), actor, tc.typ, tc.id)
			if !errors.Is(err, ErrNotOwner) {
				t.Fatalf("应统一返回 ErrNotOwner，实际 %v", err)
			}
			if n := NoticeFromError(err); n.Message != "仅作者可删除" {
				t.Fatalf("对外提示不得泄漏状态或事务内部细节: %q", n.Message)
			}
		})
	}
}

func TestDeleteRejectsInvalidTargetType(t *testing.T) {
	e := newTestEnv()
	_, err := e.svc.Delete(context.Background(), Actor{UserID: 7}, DeleteTargetType(99), 10)
	if !errors.Is(err, ErrInvalidDeleteTarget) {
		t.Fatalf("非法目标类型应拒绝，实际 %v", err)
	}
}
