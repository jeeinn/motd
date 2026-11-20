package terminal

// P2-M2 / ADR-051 的详情页「已编辑」标记渲染回归：
//  1. 仅详情页出现，格式与列表一致（MM-DD HH:MM），走展示时区；
//  2. EditedAt<=0（从未编辑）不出现该行；
//  3. 列表行不加任何标记（80 列已紧，用户拍板）；
//  4. 标记是 sys()→RenderSystem 固定文案，不含非白名单控制序列。

import (
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

func detailWithEditedAt(edited int64) viewmodel.Screen {
	s := baseScreen(viewmodel.ScreenThreadDetail)
	s.Detail = viewmodel.Detail{
		ID: 1, Title: "主题", Body: "正文", AuthorID: 2, EditedAt: edited,
	}
	s.Selection.FocusedIndex = 0
	return s
}

func TestRenderEditedMarkerInDetailOnlyWithDisplayTimezone(t *testing.T) {
	withDisplayLocation(t, mustLocation(t, "Asia/Shanghai"))
	out, err := RenderScreen(detailWithEditedAt(sampleUnix), 80)
	if err != nil {
		t.Fatal(err)
	}
	// sampleUnix 在上海时区是 11-15 06:13，与列表 formatTime 同一口径。
	if !strings.Contains(out, "已编辑 11-15 06:13") {
		t.Fatalf("详情页正文下方应有「已编辑 MM-DD HH:MM」: %q", out)
	}
	// 必须位于正文与「── 回复 ──」之间。
	iBody, iEdited, iReplies := strings.Index(out, "正文"), strings.Index(out, "已编辑"), strings.Index(out, "── 回复 ──")
	if !(iBody < iEdited && iEdited < iReplies) {
		t.Fatalf("已编辑行应夹在正文与回复之间: body=%d edited=%d replies=%d\n%s", iBody, iEdited, iReplies, out)
	}
	assertOnlySGR(t, out)
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\r"} {
		if strings.Contains(out, bad) {
			t.Fatalf("固定文案泄漏控制序列 %q", bad)
		}
	}
}

func TestRenderEditedMarkerHiddenWhenNeverEdited(t *testing.T) {
	for _, edited := range []int64{0, -1} {
		out, err := RenderScreen(detailWithEditedAt(edited), 80)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "已编辑") {
			t.Fatalf("EditedAt=%d 不应显示已编辑标记: %q", edited, out)
		}
	}
}

func TestRenderListsCarryNoEditedMarker(t *testing.T) {
	withDisplayLocation(t, time.UTC)
	for _, kind := range []viewmodel.ScreenKind{
		viewmodel.ScreenNodes, viewmodel.ScreenThreads, viewmodel.ScreenSearchResults,
	} {
		s := baseScreen(kind)
		s.Rows = []viewmodel.Row{{
			Index: 1, ID: 1, Title: "标题", Body: "正文",
			Meta: viewmodel.RowMeta{CreatedAt: sampleUnix, LastReplyAt: sampleUnix},
		}}
		s.Selection.FocusedIndex = 1
		sc, err := RenderScreen(s, 80)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sc, "已编辑") {
			t.Fatalf("kind=%d 列表行不应有已编辑标记: %q", kind, sc)
		}
	}
}
