package terminal

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// 作者名投影的渲染契约（v0.3.3）：
//   - username 是用户数据，必须与本地数值分开走 RenderUser / RenderSystem；
//   - CJK 按显示列截断，先截断后净化；
//   - 回复数由 " N回复" 改为 " rN"，其余符号不变。

// 用户目标行的 golden（无时间时输出完全确定）：
//
//	>  2. [0] 另一个主题 id=109 @alice r3
func TestRenderScreenThreadsAuthorUsernameAndCompactReplies(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{{
		Index: 2, ID: 109, Score: 0, Title: "另一个主题",
		Meta: viewmodel.RowMeta{AuthorID: 7, Author: "alice", Replies: 3},
	}}
	s.Selection.FocusedIndex = 2

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	line := lineContaining(out, "另一个主题")
	want := ">  2. [0] 另一个主题 id=109 @alice r3"
	if line != want {
		t.Fatalf("目标行不符:\n got %q\nwant %q", line, want)
	}
	if strings.Contains(line, "#7") || strings.Contains(line, "3回复") {
		t.Errorf("旧作者/回复数格式未清除: %q", line)
	}
}

// v0.7.3 顺序契约：回复数 rN 必须排在时间之前（用户 2026-11-23 反馈）。
// CreatedAt 与 Replies 同时存在时，metaTail 输出 " rN MM-DD HH:MM"，不得回退为时间在前。
func TestMetaTailRepliesBeforeTime(t *testing.T) {
	withDisplayLocation(t, time.UTC)
	row := viewmodel.Row{
		Index: 1, ID: 109, Score: 0, Title: "顺序锚点",
		Meta: viewmodel.RowMeta{AuthorID: 7, Author: "alice", Replies: 3, CreatedAt: sampleUnix},
	}
	got := metaID(row, viewmodel.ScreenThreads) + authorSegment(row) + metaTail(row, viewmodel.ScreenThreads)
	want := " id=109 @alice r3 11-14 22:13"
	if got != want {
		t.Fatalf("元信息顺序不符:\n got %q\nwant %q", got, want)
	}
	// 反向守卫：时间不得出现在 rN 之前。
	if strings.Index(got, "r3") > strings.Index(got, "11-14") {
		t.Fatalf("回复数必须在时间之前: %q", got)
	}
}

// 查不到 username 时回退到 #<AuthorID>，且仍走用户内容路径。
func TestRenderScreenAuthorFallsBackToID(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{{
		Index: 1, ID: 1, Score: 0, Title: "历史主题",
		Meta: viewmodel.RowMeta{AuthorID: 7},
	}}
	s.Selection.FocusedIndex = 1
	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	if got, want := lineContaining(out, "历史主题"), ">  1. [0] 历史主题 id=1 #7"; got != want {
		t.Fatalf("回退行不符:\n got %q\nwant %q", got, want)
	}
}

// 详情页回复行同样显示 @username。
func TestRenderScreenDetailRowsUseAuthorUsername(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreadDetail)
	s.Detail = viewmodel.Detail{ID: 3, Title: "标题", Body: "正文", AuthorID: 1, Author: "alice"}
	s.Rows = []viewmodel.Row{{
		Index: 1, ID: 9, Body: "回复正文",
		Meta: viewmodel.RowMeta{AuthorID: 2, Author: "bob"},
	}}
	s.Selection.FocusedIndex = 1
	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	if got := lineContaining(out, "回复正文"); !strings.Contains(got, "@bob") || strings.Contains(got, "#2") {
		t.Fatalf("详情回复行应显示 @bob: %q", got)
	}
}

func TestTruncateAuthorByDisplayWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"短名不截断", "alice", 10, "alice"},
		{"正好十列", "abcdefghij", 10, "abcdefghij"},
		{"超长 ASCII", "abcdefghijk", 10, "abcdefghi~"},
		{"中文超宽", "中文用户名测试", 10, "中文用户~"},
		{"中英混合", "abc中文defgh", 10, "abc中文de~"},
		{"极小预算", "中文", 1, "~"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateDisplayWidth(c.in, c.max)
			if got != c.want {
				t.Errorf("截断结果 %q，期望 %q", got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("截断破坏了 UTF-8: %q", got)
			}
			if w := displayWidth(got); w > c.max {
				t.Errorf("显示宽度 %d 超出上限 %d: %q", w, c.max, got)
			}
		})
	}
}

// 负向注入守卫：作者名含 ESC / 控制字符 / 双向控制符时必须被 sanitize。
// 同时证明作者段没有走 sys()——否则 RenderSystem 会直接拒绝含 ESC 的文本并报错。
func TestAuthorSegmentWithANSIIsSanitized(t *testing.T) {
	hostile := "\x1b[2J\r\x07\u202e"
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{{
		Index: 1, ID: 1, Score: 0, Title: "注入测试",
		Meta: viewmodel.RowMeta{AuthorID: 7, Author: hostile},
	}}
	s.Selection.FocusedIndex = 1
	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("作者名必须经过 RenderUser 净化，不得让 RenderSystem 拒绝: %v", err)
	}
	assertOnlySGR(t, out)
	for _, bad := range []string{"\x1b[2J", "\x1b]", "\r", "\x07", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("泄露控制序列 %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "␛") {
		t.Errorf("ESC 应转成可见控制图示: %q", stripSGR(out))
	}
}

// 长用户名里的 ANSI 也不能借截断绕过净化：先按显示列切 rune，再由 ext() 净化。
func TestTruncatedAuthorWithANSIIsSanitized(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{{
		Index: 1, ID: 1, Score: 0, Title: "长注入",
		Meta: viewmodel.RowMeta{AuthorID: 7, Author: "abcdef\x1b[2Jghijklmnop"},
	}}
	s.Selection.FocusedIndex = 1
	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	if strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "␛") {
		t.Fatalf("截断后的 ESC 仍须被净化为可见图示: %q", stripSGR(out))
	}
}
