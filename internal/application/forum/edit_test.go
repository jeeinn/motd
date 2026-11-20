package forum

// 作者编辑用例的回归集（P2-M2 / ADR-051）：
//  ① 作者编辑首帖 / 回复成功：正文覆盖、edited_at 落库，status/created_at/last_reply_at 不变；
//  ② 非作者 / 访客 / 不存在 / 非法 ID 一律 ErrNotEditor「仅作者可编辑」，数据零变更；
//  ③ 正文校验复用发帖约束；CanEditPost 前置判权与 EditPost 同口径且只读。
// 真实 SQL（不碰 last_reply_at、FTS 重建）由 sqlite 集成测试覆盖。

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
	"github.com/jeeinn/motd/internal/domain/user"
)

func TestCanEditPostPrecheck(t *testing.T) {
	ctx := context.Background()
	author := Actor{UserID: 7, Status: user.StatusActive}

	cases := []struct {
		name   string
		actor  Actor
		postID int64
		seed   bool
		want   error
	}{
		{"作者本人", author, 20, true, nil},
		{"非作者", Actor{UserID: 8, Status: user.StatusActive}, 20, true, ErrNotEditor},
		{"访客", Actor{Guest: true}, 20, true, ErrNotEditor},
		{"目标不存在", author, 999, false, ErrNotEditor},
		{"非法ID", author, 0, true, ErrNotEditor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv()
			if c.seed {
				e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "原文", Status: post.StatusNormal, CreatedAt: testBase})
			}
			err := e.svc.CanEditPost(ctx, c.actor, c.postID)
			if !errors.Is(err, c.want) {
				t.Fatalf("CanEditPost 期望 %v，实际 %v", c.want, err)
			}
			if c.want != nil {
				if n := NoticeFromError(err); n.Code != "not_editor" || n.Message != "仅作者可编辑" {
					t.Fatalf("提示口径不对: %+v", n)
				}
			}
		})
	}
}

// 作者编辑回复：正文覆盖、edited_at = 用例时钟，其它字段（含所属主题 last_reply_at）不变。
func TestEditPostByOwnerTouchesOnlyBodyAndEditedAt(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "主题", LastReplyAt: testBase, CreatedAt: testBase})
	e.repos.addPost(post.Post{
		ID: 20, ThreadID: 10, UserID: 7, BodyMD: "原文", Status: post.StatusNormal,
		ParentID: func() *int64 { v := int64(0); return &v }(), CreatedAt: testBase,
	})
	beforeThread := e.repos.state.threads[10]

	editedAt := testBase.Add(120 * time.Second)
	e.now = editedAt
	if err := e.svc.EditPost(ctx, Actor{UserID: 7, Status: user.StatusActive}, EditPostInput{
		PostID: 20, Body: "全新正文",
	}); err != nil {
		t.Fatalf("作者编辑应成功: %v", err)
	}

	got := e.repos.state.posts[20]
	if got.BodyMD != "全新正文" {
		t.Fatalf("正文应被覆盖，实际 %q", got.BodyMD)
	}
	if got.EditedAt != editedAt.Unix() {
		t.Fatalf("edited_at 应为用例时钟 Unix 秒 %d，实际 %d", editedAt.Unix(), got.EditedAt)
	}
	if got.Status != post.StatusNormal || !got.CreatedAt.Equal(testBase) {
		t.Fatalf("编辑不得改 status / created_at: %+v", got)
	}
	if got.ParentID == nil || *got.ParentID != 0 {
		t.Fatalf("编辑不得改 parent_post_id: %+v", got)
	}
	if after := e.repos.state.threads[10]; !reflect.DeepEqual(beforeThread, after) {
		t.Fatalf("编辑绝不碰主题（尤其 last_reply_at）: before=%+v after=%+v", beforeThread, after)
	}
}

// 编辑主题首帖（正文）与编辑回复走同一条 posts 路径。
func TestEditPostFirstPostWorksToo(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.repos.addThread(thread.Thread{ID: 10, NodeID: 1, UserID: 7, Title: "主题", CreatedAt: testBase})
	e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "首帖原文", Status: post.StatusNormal, CreatedAt: testBase})
	e.repos.addPost(post.Post{ID: 21, ThreadID: 10, UserID: 8, BodyMD: "他人回复", Status: post.StatusNormal, CreatedAt: testBase})

	if err := e.svc.EditPost(ctx, Actor{UserID: 7, Status: user.StatusActive}, EditPostInput{
		PostID: 20, Body: "首帖新正文",
	}); err != nil {
		t.Fatalf("编辑首帖应成功: %v", err)
	}
	if e.repos.state.posts[20].BodyMD != "首帖新正文" {
		t.Fatal("首帖正文未更新")
	}
	if e.repos.state.posts[21].BodyMD != "他人回复" {
		t.Fatal("不得牵连其它 post")
	}

	// 详情页正文与 edited_at 来自首帖。
	sc, err := e.svc.ScreenThreadDetail(ctx, 10, ViewContext{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Detail.Body != "首帖新正文" || sc.Detail.EditedAt != e.repos.state.posts[20].EditedAt {
		t.Fatalf("详情正文 / 编辑标记应取自首帖: %+v", sc.Detail)
	}
}

// 非作者 / 访客 / 不存在 / 已删 / 非法 ID：统一 ErrNotEditor，数据零变更。
func TestEditPostRejectedUniformlyWithoutChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actor  Actor
		postID int64
	}{
		{"非作者", Actor{UserID: 8, Status: user.StatusActive}, 20},
		{"访客", Actor{Guest: true}, 20},
		{"不存在", Actor{UserID: 7, Status: user.StatusActive}, 999},
		{"非法ID", Actor{UserID: 7, Status: user.StatusActive}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv()
			e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "原文", Status: post.StatusNormal, CreatedAt: testBase})
			before := e.repos.state.clone()

			err := e.svc.EditPost(context.Background(), tc.actor, EditPostInput{PostID: tc.postID, Body: "篡改"})
			if !errors.Is(err, ErrNotEditor) {
				t.Fatalf("应返回 ErrNotEditor，实际 %v", err)
			}
			if n := NoticeFromError(err); n.Message != "仅作者可编辑" {
				t.Fatalf("固定文案不对: %q", n.Message)
			}
			if !reflect.DeepEqual(before, e.repos.state) {
				t.Fatal("被拒后数据必须零变更")
			}
		})
	}
}

// 已软删 / 隐藏的内容不可编辑（PostOwner 只认 normal）。
func TestEditPostRejectsInvisibleTarget(t *testing.T) {
	for _, status := range []post.Status{post.StatusDeleted, post.StatusHidden} {
		t.Run(string(status), func(t *testing.T) {
			e := newTestEnv()
			e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "原文", Status: status, CreatedAt: testBase})
			err := e.svc.EditPost(context.Background(), Actor{UserID: 7, Status: user.StatusActive}, EditPostInput{
				PostID: 20, Body: "篡改",
			})
			if !errors.Is(err, ErrNotEditor) {
				t.Fatalf("不可见内容应统一拒绝，实际 %v", err)
			}
		})
	}
}

// 正文校验复用发帖约束：空白 / 超 8 KiB 拒绝且不落库；边界值放行。
func TestEditPostBodyValidation(t *testing.T) {
	e := newTestEnv()
	ctx := context.Background()
	e.repos.addPost(post.Post{ID: 20, ThreadID: 10, UserID: 7, BodyMD: "原文", Status: post.StatusNormal, CreatedAt: testBase})
	actor := Actor{UserID: 7, Status: user.StatusActive}

	for _, body := range []string{"", " \t\n", strings.Repeat("x", MaxBodyBytes+1)} {
		err := e.svc.EditPost(ctx, actor, EditPostInput{PostID: 20, Body: body})
		if !errors.Is(err, ErrInvalidBody) {
			t.Errorf("非法正文应返回 ErrInvalidBody，实际 %v", err)
		}
		if e.repos.state.posts[20].BodyMD != "原文" {
			t.Fatal("校验失败不得改正文")
		}
	}
	if err := e.svc.EditPost(ctx, actor, EditPostInput{PostID: 20, Body: strings.Repeat("x", MaxBodyBytes)}); err != nil {
		t.Fatalf("8 KiB 边界应放行: %v", err)
	}
}
