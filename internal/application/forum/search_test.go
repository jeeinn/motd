package forum

// P1-M3 检索用例测试：查询词清洗、FTS/LIKE 转义、摘要边界与分页裁剪。
// 真实索引与触发器的行为由 infrastructure 集成测试覆盖。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/domain/post"
	"github.com/jeeinn/motd/internal/domain/thread"
)

func searchFakeEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv()
	e.repos.addThread(thread.Thread{NodeID: 1, UserID: 2, Title: "检索目标", LastReplyAt: testBase, CreatedAt: testBase})
	for i, body := range []string{"共同词 甲", "共同词 乙", "共同词 丙"} {
		e.repos.addPost(post.Post{
			ThreadID: 1, UserID: 2, BodyMD: body,
			Status: post.StatusNormal, CreatedAt: testBase.Add(time.Duration(i) * time.Second),
		})
	}
	return e
}

func TestNormalizeSearchQueryStripsControlCharacters(t *testing.T) {
	got := NormalizeSearchQuery(" 你\x1b[31m好\x00  ")
	if strings.ContainsAny(got, "\x1b\x00") {
		t.Fatalf("控制字符必须被清洗: %q", got)
	}
	if got != "你 [31m好" {
		t.Fatalf("清洗结果不符: %q", got)
	}
	if NormalizeSearchQuery("  \t ") != "" {
		t.Fatal("纯空白查询应归一化为空")
	}
}

func TestFTSMatchTermQuotesAndDoublesInnerQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"你好世", `"你好世"`},
		{`a"b`, `"a""b"`},
		{`*"`, `"*"""`},
	}
	for _, c := range cases {
		if got := FTSMatchTerm(c.in); got != c.want {
			t.Errorf("FTSMatchTerm(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestEscapeLikePatternCoversAllWildcards(t *testing.T) {
	if got := EscapeLikePattern(`a\%_b`); got != `a\\\%\_b` {
		t.Fatalf("LIKE 转义不完整: %q", got)
	}
}

func TestTruncateExcerptKeepsRuneBoundary(t *testing.T) {
	s := strings.Repeat("中", 100)
	got := TruncateExcerpt(s, 20)
	if len(got) > 20 {
		t.Fatalf("摘要不能超过字节上限: %d", len(got))
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("截断不得切出半个 UTF-8 码点")
		}
	}
	if TruncateExcerpt("短", 1) != "" {
		t.Fatal("预算不足时应返回空串")
	}
}

func TestSearchValidatesAndPaginates(t *testing.T) {
	e := searchFakeEnv(t)
	ctx := context.Background()

	if _, err := e.svc.Search(ctx, SearchInput{Query: ""}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("空查询应拒绝: %v", err)
	}
	res, err := e.svc.Search(ctx, SearchInput{Query: "共同词", Limit: 2, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 || !res.HasMore || res.Page != 1 {
		t.Fatalf("第一页应 2 条且有下一页: %+v", res)
	}
	second, err := e.svc.Search(ctx, SearchInput{Query: "共同词", Limit: 2, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Hits) != 1 || second.HasMore {
		t.Fatalf("第二页应 1 条且无下一页: %+v", second)
	}
	if second.Hits[0].PostID == res.Hits[0].PostID {
		t.Fatal("两页不应重复")
	}
}

func TestSearchCapsPageSizeAtTwenty(t *testing.T) {
	e := searchFakeEnv(t)
	res, err := e.svc.Search(context.Background(), SearchInput{Query: "共同词", Limit: 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) > SearchMaxPageSize {
		t.Fatalf("页长上限未生效: %d", len(res.Hits))
	}
}

func TestSearchRejectsHugePage(t *testing.T) {
	e := searchFakeEnv(t)
	if _, err := e.svc.Search(context.Background(), SearchInput{Query: "共同词", Page: 1 << 30}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("超大页码应拒绝而不是溢出: %v", err)
	}
}
