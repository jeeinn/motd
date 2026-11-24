package i18n

// W11 自证：10 key × 2 locale 齐全；译文无控制字节且能被 safe.RenderSystem 接受；
// %s 槽位、未知 locale/key 回退、notice 前缀四态均符合预期。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

func TestCatalogCompleteness(t *testing.T) {
	keys := Keys()
	if len(keys) != 10 {
		t.Fatalf("hint key 应为 10 个，实际 %d: %v", len(keys), keys)
	}
	for _, loc := range []string{LocaleZH, LocaleEN} {
		for _, key := range keys {
			v, ok := hintCatalog[loc][key]
			if !ok || v == "" {
				t.Errorf("%s/%s 文案缺失", loc, key)
			}
		}
	}
}

// TestZHCatalogMatchesV084Snapshot 是默认 zh 逐字节一致性的实证（D3 红线）：
// 快照取自 v0.8.4 page_controller.go 的 9 个原文案 + hintPrefix，i18n 化不得漂移。
func TestZHCatalogMatchesV084Snapshot(t *testing.T) {
	snapshot := map[string]string{
		KeyHintPrefix:             "[提示] ",
		KeyComposerTitle:          "新建主题：输入标题后按 Enter 确认（Esc 取消）",
		KeyComposerBody:           "回复：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyComposerBodyAfterTitle: "标题已接收：输入正文，Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyComposerEdit:           "编辑：重新输入完整新正文（覆盖原文），Enter 换行；单独一行输入 . 或按 Ctrl-D 提交（Esc 取消）",
		KeyDraftRetainedRate:      "内容已保留为临时草稿（仅本次连接）：请约 %s 后按 Ctrl-D（或单独一行 .）重发；Esc 挂起草稿回浏览屏，回到原帖按 r 恢复",
		KeyDraftSuspended:         "草稿已保留，回到原帖按 r 即可恢复",
		KeyDraftRetained:          "（输入内容已保留为临时草稿，仅本次连接有效；处理完成后重新 /new 或 /reply 即可恢复）",
		KeyDraftRestored:          "草稿已恢复：内容仍在，直接按 Ctrl-D（或单独一行 .）提交",
		KeyDraftDiscarded:         "（此前的临时草稿已不适用，重新开始输入）",
	}
	for key, want := range snapshot {
		if got := Lookup(LocaleZH, key); got != want {
			t.Errorf("zh/%s 与 v0.8.4 快照不一致\n got: %q\nwant: %q", key, got, want)
		}
	}
}

func TestTranslationsSafeForRenderSystem(t *testing.T) {
	esc := string(rune(0x1b))
	for loc, m := range hintCatalog {
		for key, text := range m {
			if strings.ContainsAny(text, esc+"\r\n") {
				t.Errorf("%s/%s 含控制字节: %q", loc, key, text)
			}
			// 翻译产物仍须过服务端固定文本校验（ADR-019 信任域分离）。
			if _, err := safe.RenderSystem(text, safe.StyleDim); err != nil {
				t.Errorf("%s/%s 不被 RenderSystem 接受: %v", loc, key, err)
			}
		}
	}
}

func TestLookupArgsAndFallback(t *testing.T) {
	if got := Lookup(LocaleZH, KeyDraftRetainedRate, "1m0s"); !strings.Contains(got, "1m0s") {
		t.Fatalf("限流文案应注入倒计时槽位，实际 %q", got)
	}
	if got := Lookup(LocaleEN, KeyDraftRetainedRate, "1m0s"); !strings.Contains(got, "1m0s") {
		t.Fatalf("英文限流文案应注入倒计时槽位，实际 %q", got)
	}
	if got := Lookup("xx", KeyComposerTitle); got != Lookup(LocaleZH, KeyComposerTitle) {
		t.Fatalf("未知 locale 应回退 zh，实际 %q", got)
	}
	if got := Lookup(LocaleEN, "missing.key"); got != Lookup(LocaleZH, "missing.key") {
		t.Fatalf("未知 key 应回退 zh，实际 %q", got)
	}
	if got := Lookup(LocaleEN, KeyHintPrefix); got != "[Info] " {
		t.Fatalf("英文 hint 前缀应为 [Info]，实际 %q", got)
	}
}

func TestNoticePrefixLocales(t *testing.T) {
	for _, tc := range []struct {
		locale  string
		isError bool
		want    string
	}{
		{LocaleZH, false, "[提示] "}, {LocaleZH, true, "[错误] "},
		{LocaleEN, false, "[Info] "}, {LocaleEN, true, "[Error] "},
	} {
		if got := NoticePrefix(tc.locale, tc.isError); got != tc.want {
			t.Errorf("NoticePrefix(%q,%v) = %q，want %q", tc.locale, tc.isError, got, tc.want)
		}
	}
}
