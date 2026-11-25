package sqlite

import (
	"testing"

	"github.com/jeeinn/motd/internal/domain/post"
)

// R-06：ListPostsTail 是「跳到末页」的专用查询（回复成功后定位到自己刚发的那条）。
// 三条口径必须守住：排除正文、过滤软删、返回升序的末尾 N 条。

func TestListPostsTailReturnsLastAscending(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	threadID := e.insertThread(t, "分页主题", 0, e.clock)
	opID := e.insertPost(t, threadID, "正文", post.StatusNormal, e.clock)
	r1 := e.insertPost(t, threadID, "回复1", post.StatusNormal, e.clock)
	r2 := e.insertPost(t, threadID, "回复2", post.StatusNormal, e.clock)
	r3 := e.insertPost(t, threadID, "回复3", post.StatusNormal, e.clock)

	got, err := e.repos.ListPostsTail(ctx, threadID, opID, 2)
	if err != nil {
		t.Fatalf("查询末尾内容失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(got))
	}
	// 升序返回，且是最后两条（不是前两条）。
	if got[0].ID != r2 || got[1].ID != r3 {
		t.Fatalf("应返回最后两条升序 [%d %d]，实际 [%d %d]", r2, r3, got[0].ID, got[1].ID)
	}
	// 正文必须被排除：afterID 就是首帖 id。
	for _, p := range got {
		if p.ID == opID {
			t.Fatal("返回集合不得包含正文（首帖）")
		}
	}
	_ = r1

	// limit 大于剩余条数时返回全部，不报错、不补齐。
	all, err := e.repos.ListPostsTail(ctx, threadID, opID, 100)
	if err != nil {
		t.Fatalf("limit 超大时应正常返回: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("应返回全部 3 条回复，实际 %d", len(all))
	}
	if all[0].ID != r1 || all[2].ID != r3 {
		t.Fatalf("全部回复应升序 [%d..%d]，实际 [%d..%d]", r1, r3, all[0].ID, all[2].ID)
	}
}

func TestListPostsTailSkipsDeleted(t *testing.T) {
	e := newForumEnv(t)
	ctx := e.ctx
	threadID := e.insertThread(t, "含软删", 0, e.clock)
	opID := e.insertPost(t, threadID, "正文", post.StatusNormal, e.clock)
	e.insertPost(t, threadID, "正常回复", post.StatusNormal, e.clock)
	e.insertPost(t, threadID, "已删回复", post.StatusDeleted, e.clock)

	got, err := e.repos.ListPostsTail(ctx, threadID, opID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("软删条目应被过滤，实际返回 %d 条", len(got))
	}
	if got[0].BodyMD != "正常回复" {
		t.Fatalf("返回的应是正常回复，实际 %q", got[0].BodyMD)
	}
}

func TestListPostsTailOnlyBody(t *testing.T) {
	e := newForumEnv(t)
	threadID := e.insertThread(t, "只有正文", 0, e.clock)
	opID := e.insertPost(t, threadID, "正文", post.StatusNormal, e.clock)

	got, err := e.repos.ListPostsTail(e.ctx, threadID, opID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("无回复时应返回空集合，实际 %d 条", len(got))
	}
}
