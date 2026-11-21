package terminal

import (
	"strconv"
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

// v0.2.1：标题与正文之间必须有固定虚线，且不改动用户正文的任何一个字节。
// lineContaining 返回 stripSGR 后包含 sub 的第一行；找不到返回空串。
// 测试用它锁定「某一行」而不是整屏，避免状态栏/页脚里的数字干扰断言。
func lineContaining(out, sub string) string {
	for _, line := range strings.Split(stripSGR(out), "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// rowCommandID 从渲染行里抠出 " id=<n>" 的 n，模拟用户复制该数字当命令参数。
func rowCommandID(line string) (int64, bool) {
	i := strings.Index(line, " id=")
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(" id="):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil
}

// v0.3.3 格式契约（golden）：CreatedAt=0 去掉时间后输出完全确定。
// 三段按「id= → 作者 → 时间/回复数」拼接；顺序或分隔符任一改动都会让本断言失败，
// 防止后续改动悄悄破坏「帖子 ID、作者、赞数三者形态不同」的约定。
func TestMetaPartsFormatContract(t *testing.T) {
	row := viewmodel.Row{Index: 1, ID: 123, Meta: viewmodel.RowMeta{AuthorID: 1}}
	cases := []struct {
		kind viewmodel.ScreenKind
		want string
	}{
		{viewmodel.ScreenThreads, " id=123 #1"},
		{viewmodel.ScreenThreadDetail, " id=123 #1"},
		{viewmodel.ScreenSearchResults, " id=123 #1"},
		{viewmodel.ScreenNodes, " #1"}, // node ID 不是 /delete 目标，刻意不展示
	}
	for _, c := range cases {
		got := metaID(row, c.kind) + authorSegment(row) + metaTail(row, c.kind)
		if got != c.want {
			t.Errorf("kind=%d: 元信息=%q, want %q", c.kind, got, c.want)
		}
	}
}

// 无 ID / 负 ID 的系统行不显示 id=。
func TestMetaFixedOmitsNonPositiveID(t *testing.T) {
	for _, id := range []int64{0, -1} {
		row := viewmodel.Row{ID: id, Meta: viewmodel.RowMeta{AuthorID: 2}}
		for _, kind := range []viewmodel.ScreenKind{
			viewmodel.ScreenThreads, viewmodel.ScreenThreadDetail, viewmodel.ScreenSearchResults,
		} {
			got := metaID(row, kind)
			if strings.Contains(got, "id=") {
				t.Errorf("kind=%d ID=%d 不应展示 id=，实际 %q", kind, id, got)
			}
		}
	}
}

// 主题列表每行都能看到主题 ID，且能直接当 /open 的参数；与作者 #ID、赞数 [N] 不混淆。
func TestRenderScreenThreadsShowsThreadID(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{
		{Index: 1, ID: 101, Score: 0, Title: "唯一标题甲", Meta: viewmodel.RowMeta{AuthorID: 7}},
		{Index: 2, ID: 202, Score: 3, Title: "唯一标题乙", Meta: viewmodel.RowMeta{AuthorID: 7}},
	}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	for _, want := range []string{"id=101", "id=202"} {
		if !strings.Contains(out, want) {
			t.Errorf("主题列表缺少 %q: %q", want, stripSGR(out))
		}
	}
	line := lineContaining(out, "唯一标题甲")
	if !strings.Contains(line, "#7") {
		t.Errorf("作者 ID 丢失: %q", line)
	}
	if strings.Contains(line, "#101") {
		t.Errorf("主题 ID 被误写成作者 ID: %q", line)
	}
	if id, ok := rowCommandID(line); !ok || id != 101 {
		t.Errorf("行内 ID 不是可直接使用的十进制参数: %q", line)
	}
	// 赞数仍用 [N]，没被新格式顶掉。
	if !strings.Contains(lineContaining(out, "唯一标题乙"), "[3]") {
		t.Error("赞数 [N] 格式被 ID 显示破坏")
	}
}

// 详情页每条回复显示回复 ID，可直接当 /delete <id> 的参数；作者与回复同为 42 时仍可区分。
func TestRenderScreenDetailShowsReplyID(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreadDetail)
	s.Detail = viewmodel.Detail{ID: 3, Title: "主题锚点", Body: "正文锚点"}
	s.Rows = []viewmodel.Row{
		{Index: 1, ID: 42, Body: "回复锚点甲", Meta: viewmodel.RowMeta{AuthorID: 42}},
		{Index: 2, ID: 98765, Body: "回复锚点乙", Meta: viewmodel.RowMeta{AuthorID: 8}},
	}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	line := lineContaining(out, "回复锚点甲")
	if strings.Count(line, "id=42") != 1 || strings.Count(line, "#42") != 1 {
		t.Fatalf("作者 #42 与回复 id=42 必须各出现一次且形态不同: %q", line)
	}
	if id, ok := rowCommandID(line); !ok || id != 42 {
		t.Errorf("回复 ID 不是可直接使用的 /delete 参数: %q", line)
	}
	if id, ok := rowCommandID(lineContaining(out, "回复锚点乙")); !ok || id != 98765 {
		t.Errorf("大数值回复 ID 解析失败: %q", stripSGR(out))
	}
}

// 检索结果列表同样显示主题 ID。
func TestRenderScreenSearchResultsShowsThreadID(t *testing.T) {
	s := baseScreen(viewmodel.ScreenSearchResults)
	s.Rows = []viewmodel.Row{{Index: 1, ID: 555, Body: "检索锚点"}}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	if id, ok := rowCommandID(lineContaining(out, "检索锚点")); !ok || id != 555 {
		t.Errorf("检索结果未显示可用的主题 ID: %q", stripSGR(out))
	}
}

// 负向：ID 是本地数值，只经 RenderSystem；带敌意标题时不得因新增 ID 而产生
// 任何新转义/控制字符（ADR-014 三层守卫 / ADR-019 终端白名单）。
func TestRowIDIntroducesNoControlCharacters(t *testing.T) {
	hostile := "标题\x1b[2J\x1b]52;c;YWJj\x07 \r id=666"
	row := viewmodel.Row{Index: 1, ID: 777, Score: 1, Title: hostile, Meta: viewmodel.RowMeta{AuthorID: 9}}
	s := baseScreen(viewmodel.ScreenThreads)
	s.Rows = []viewmodel.Row{row}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	assertOnlySGR(t, out)
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\r", "\x07"} {
		if strings.Contains(out, bad) {
			t.Errorf("泄露控制字符 %q: %q", bad, out)
		}
	}
	// 系统 ID 只由本地数值拼成、形状固定且经 RenderSystem；用户文本里的
	// "id=666" 是惰性可见文本，不会被当成系统 ID（真正的 ID 是元信息里的 777）。
	if got, want := metaID(row, viewmodel.ScreenThreads)+authorSegment(row)+metaTail(row, viewmodel.ScreenThreads), " id=777 #9"; got != want {
		t.Errorf("元信息=%q, want %q", got, want)
	}
	if !strings.Contains(stripSGR(out), "id=777 #9") {
		t.Errorf("系统 ID 应原样渲染进元信息: %q", stripSGR(out))
	}
}

func TestRenderScreenDetailHasSeparatorBetweenTitleAndBody(t *testing.T) {
	s := baseScreen(viewmodel.ScreenThreadDetail)
	s.Detail = viewmodel.Detail{ID: 3, Title: "标题锚点", Body: "正文锚点"}
	s.Rows = []viewmodel.Row{{Index: 1, ID: 9, Body: "回复锚点"}}
	s.Selection.FocusedIndex = 1

	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	// 固定文案只走 SGR 样式，不引入任何光标定位 / 清屏序列。
	assertOnlySGR(t, out)

	lines := strings.Split(stripSGR(out), "\n")
	titleLine, sepLine, bodyLine := -1, -1, -1
	for i, line := range lines {
		switch line {
		case "标题锚点":
			titleLine = i
		case "──":
			if sepLine < 0 {
				sepLine = i
			}
		case "正文锚点":
			bodyLine = i
		}
	}
	if titleLine < 0 || bodyLine < 0 {
		t.Fatalf("标题或正文未作为独立行出现: %q", stripSGR(out))
	}
	if sepLine < 0 || !(titleLine < sepLine && sepLine < bodyLine) {
		t.Fatalf("虚线必须位于标题与正文之间: %q", stripSGR(out))
	}
	if !strings.Contains(out, "回复锚点") {
		t.Fatal("虚线插入不得吞掉下方回复行")
	}
}

// P4-T1：状态栏等级着色只覆盖 L3/L4 两字符，且 "L%d" 文字标签始终保留。
func TestStatusBarLevelColorIsScoped(t *testing.T) {
	for _, tc := range []struct {
		level uint8
		label string
	}{
		{1, "L1"}, {2, "L2"}, {3, "L3"}, {4, "L4"},
	} {
		s := baseScreen(viewmodel.ScreenNodes)
		s.StatusBar.Level = tc.level
		out, err := RenderScreen(s, 80)
		if err != nil {
			t.Fatalf("level=%d 渲染失败: %v", tc.level, err)
		}
		assertOnlySGR(t, out)

		if !strings.Contains(out, tc.label) {
			t.Errorf("level=%d 必须保留文字标签 %q: %q", tc.level, tc.label, out)
		}
		if tc.level >= 3 {
			if !strings.Contains(out, "\x1b[34m"+tc.label+"\x1b[0m") {
				t.Errorf("level=%d 的 %s 应走 Accent(34): %q", tc.level, tc.label, out)
			}
		} else {
			// L1/L2 整行保持一条 Dim 序列（与既有输出逐字节一致）。
			if !strings.Contains(out, "\x1b[2m") || !strings.Contains(out, tc.label+"\x1b[0m") {
				t.Errorf("level=%d 的 %s 应保持在 Dim(2) 段落内: %q", tc.level, tc.label, out)
			}
			if strings.Contains(out, "\x1b[34m") {
				t.Errorf("level=%d 不应出现 Accent 色: %q", tc.level, out)
			}
		}
		// 固定数值区不得被染色：颜色面积只限等级两字符。
		if strings.Contains(out, "\x1b[34monline=") || strings.Contains(out, "\x1b[34mkarma=") {
			t.Errorf("level=%d 状态栏数值区不应染色: %q", tc.level, out)
		}
	}
}

// P4-T1：提示行按 NoticeKind 给固定前缀分色，消息正文仍走 RenderUser。
func TestNoticePrefixColorAndUserBodyIsolation(t *testing.T) {
	for _, tc := range []struct {
		kind   viewmodel.NoticeKind
		prefix string
	}{
		{viewmodel.NoticeError, "\x1b[31m[错误] \x1b[0m"},
		{viewmodel.NoticeWarning, "\x1b[33m[提示] \x1b[0m"},
		{viewmodel.NoticeInfo, "\x1b[2m[提示] \x1b[0m"},
		{viewmodel.NoticeSuccess, "\x1b[2m[提示] \x1b[0m"},
	} {
		s := baseScreen(viewmodel.ScreenNodes)
		// 正文含用户提交的颜色 SGR，必须被净化成可见字符而非真正上色。
		s.StatusBar.Notice = viewmodel.Notice{Kind: tc.kind, Message: "用户正文\x1b[31m红\x1b[0m"}
		out, err := RenderScreen(s, 80)
		if err != nil {
			t.Fatalf("kind=%d 渲染失败: %v", tc.kind, err)
		}
		assertOnlySGR(t, out)

		if !strings.Contains(out, tc.prefix) {
			t.Errorf("kind=%d 缺少着色前缀 %q: %q", tc.kind, tc.prefix, out)
		}
		if !strings.Contains(out, "␛[31m红␛[0m") {
			t.Errorf("kind=%d 用户 SGR 应转成可见图示（走 RenderUser）: %q", tc.kind, out)
		}
		for _, forbidden := range []string{
			"\x1b[31m用户正文", "\x1b[33m用户正文", "\x1b[34m用户正文",
		} {
			if strings.Contains(out, forbidden) {
				t.Errorf("kind=%d 用户正文被染色 %q: %q", tc.kind, forbidden, out)
			}
		}
		// 错误色只允许落在 [错误] 前缀，不得泄漏到正文行其他位置。
		if tc.kind != viewmodel.NoticeError && strings.Count(out, "\x1b[31m") != 0 {
			t.Errorf("kind=%d 不应出现 Error 色: %q", tc.kind, out)
		}
	}
}

// P4-T1：没有 Notice 时不得凭空多出前缀着色。
func TestNoticeAbsentHasNoPrefixedLine(t *testing.T) {
	s := baseScreen(viewmodel.ScreenNodes)
	out, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"[错误]", "[提示]"} {
		if strings.Contains(out, prefix) {
			t.Errorf("无提示时不应输出 %q: %q", prefix, out)
		}
	}
	if strings.Contains(out, "\x1b[31m") || strings.Contains(out, "\x1b[33m") {
		t.Errorf("无提示时不应出现 Error/Notice 色: %q", out)
	}
}
