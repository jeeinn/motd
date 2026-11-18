package terminal

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

// assertOnlySGR 是 D5 在 Renderer 层的最小判据：输出里出现 ESC 就必须是纯 SGR。
// 清屏 2J、光标 H/K、OSC 52 / OSC 8、备用屏 ?1049h 都不满足这个形态。
func assertOnlySGR(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		if i+2 >= len(s) || s[i+1] != '[' {
			t.Fatalf("输出含非 CSI 的 ESC 序列（位置 %d）: %q", i, s)
		}
		j := i + 2
		for j < len(s) && s[j] != 'm' {
			c := s[j]
			if (c < '0' || c > '9') && c != ';' {
				t.Fatalf("ESC 序列不是纯 SGR: %q", s[i:min(i+24, len(s))])
			}
			j++
		}
		if j >= len(s) {
			t.Fatalf("未闭合的 SGR 序列: %q", s[i:])
		}
		i = j
	}
}

func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+2 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func baseScreen(kind viewmodel.ScreenKind) viewmodel.Screen {
	return viewmodel.Screen{
		Kind:      kind,
		StatusBar: viewmodel.StatusBar{Online: 2, Active: 1, Identity: viewmodel.Identity{Guest: true}, Level: 1},
	}
}

func TestRenderScreenNodesContainsSemanticContent(t *testing.T) {
	s := baseScreen(viewmodel.ScreenNodes)
	s.Rows = []viewmodel.Row{
		{Index: 1, ID: 1, Title: "人工智能", Meta: viewmodel.RowMeta{Node: "/dev/ai"}},
		{Index: 2, ID: 2, Title: "Rust", Meta: viewmodel.RowMeta{Node: "/dev/rust"}},
	}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	for _, want := range []string{"版块列表", "/dev/ai", "/dev/rust", "人工智能", "Rust", "online=2", "L1"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q: %q", want, out)
		}
	}
	assertOnlySGR(t, out)
}

func TestRenderScreenRejectsInvalidViewModel(t *testing.T) {
	if _, err := RenderScreen(viewmodel.Screen{}, 80); err == nil {
		t.Fatal("非法 ScreenKind 必须失败关闭")
	}
	s := baseScreen(viewmodel.ScreenNodes)
	s.Rows = []viewmodel.Row{{Index: 2}, {Index: 1}}
	if _, err := RenderScreen(s, 80); err == nil {
		t.Fatal("行序号非递增必须失败关闭")
	}
}

func TestRenderScreenNeutralizesHostileContent(t *testing.T) {
	hostile := "标题\x1b[2J\x1b]52;c;YWJj\x07\r覆盖\u202eRTL [点我](javascript:alert(1)) `\x1b[31m`"
	for _, kind := range []viewmodel.ScreenKind{
		viewmodel.ScreenNodes, viewmodel.ScreenThreads, viewmodel.ScreenThreadDetail,
	} {
		s := baseScreen(kind)
		s.Rows = []viewmodel.Row{{Index: 1, ID: 1, Title: hostile, Body: hostile}}
		s.Selection.FocusedIndex = 1
		if kind == viewmodel.ScreenThreadDetail {
			s.Detail = viewmodel.Detail{ID: 1, Title: hostile, Body: hostile}
		}
		out, err := RenderScreen(s, 80)
		if err != nil {
			t.Fatalf("kind=%d 渲染失败: %v", kind, err)
		}
		assertOnlySGR(t, out)
		for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x1b]8", "\x1b]0;", "\x1b[?1049h"} {
			if strings.Contains(out, bad) {
				t.Errorf("kind=%d 泄露了控制序列 %q", kind, bad)
			}
		}
		if !strings.Contains(out, "␛") {
			t.Errorf("kind=%d 用户提交的 ESC 应转成可见控制图示", kind)
		}
		if strings.Contains(out, "\r") {
			t.Errorf("kind=%d 不应保留 CR（\\r 覆盖攻击面）", kind)
		}
	}
}

func TestRenderScreenNarrowWidthWrapsWithoutTruncating(t *testing.T) {
	title := strings.Repeat("中文", 30)
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{{Index: 1, ID: 1, Score: 7, Title: title}}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 40)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	plain := stripSGR(out)
	if got := strings.Count(plain, "中"); got != 30 {
		t.Fatalf("40 列折行不得截断内容：期望 30 个「中」，实际 %d", got)
	}
	if !strings.Contains(out, "[7]") {
		t.Fatal("主题列表应显示赞数")
	}
}

func TestRenderScreenDetailShowsBodyAndReplies(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreadDetail)
	s.Detail = viewmodel.Detail{ID: 3, Title: "主题标题", Body: "首帖正文"}
	s.Rows = []viewmodel.Row{{Index: 1, ID: 9, Body: "第一条回复"}}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	for _, want := range []string{"主题标题", "首帖正文", "第一条回复", "回复"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q", want)
		}
	}
	assertOnlySGR(t, out)
}
