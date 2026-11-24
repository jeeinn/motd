package terminal

// W13 验收：RenderScreen 原签名默认 zh 逐字节不变；RenderScreenWithLocale 切 en
// 后 notice 前缀随之切换，而消息正文仍走 RenderUser（信任域不变）。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal/i18n"
)

func noticeScreen(kind viewmodel.NoticeKind, message string) viewmodel.Screen {
	s := baseScreen(viewmodel.ScreenNodes)
	s.Rows = []viewmodel.Row{
		{Index: 1, ID: 1, Title: "中文标题", Meta: viewmodel.RowMeta{Node: "/dev/ai"}},
	}
	s.StatusBar.Notice = viewmodel.Notice{Kind: kind, Message: message}
	return s
}

func TestRenderScreenWithLocaleDefaultIsZHByteForByte(t *testing.T) {
	s := noticeScreen(viewmodel.NoticeError, "出错了")
	gotDefault, err := RenderScreen(s, 80)
	if err != nil {
		t.Fatal(err)
	}
	gotZH, err := RenderScreenWithLocale(s, 80, i18n.LocaleZH)
	if err != nil {
		t.Fatal(err)
	}
	if gotDefault != gotZH {
		t.Fatalf("默认 RenderScreen 应与显式 zh 逐字节一致\n got: %q\nwant: %q", gotDefault, gotZH)
	}
	if !strings.Contains(gotDefault, "[错误] ") || !strings.Contains(gotDefault, "出错了") {
		t.Fatalf("默认 zh 前缀与正文均应在: %q", gotDefault)
	}
}

func TestRenderScreenWithLocaleEnglishPrefix(t *testing.T) {
	for _, tc := range []struct {
		kind   viewmodel.NoticeKind
		want   string
		banned string
	}{
		{viewmodel.NoticeError, "[Error] ", "[错误]"},
		{viewmodel.NoticeWarning, "[Info] ", "[提示]"},
	} {
		s := noticeScreen(tc.kind, "user <boom> text")
		got, err := RenderScreenWithLocale(s, 80, i18n.LocaleEN)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.banned) {
			t.Fatalf("en notice 前缀应为 %q 且不含 %q: %q", tc.want, tc.banned, got)
		}
		if !strings.Contains(got, "user <boom> text") {
			t.Fatalf("消息正文仍须保留（走 RenderUser）: %q", got)
		}
		assertOnlySGR(t, got) // ADR-027 白名单守卫不回退
	}
}

func TestRenderScreenWithLocaleUnknownFallsBackZH(t *testing.T) {
	s := noticeScreen(viewmodel.NoticeError, "出错了")
	got, err := RenderScreenWithLocale(s, 80, "xx")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := RenderScreenWithLocale(s, 80, i18n.LocaleZH)
	if got != want {
		t.Fatalf("未知 locale 应回退 zh，got %q want %q", got, want)
	}
}
