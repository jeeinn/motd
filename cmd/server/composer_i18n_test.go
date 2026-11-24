package main

// W14 双语参数化（DESIGN-v090 §4.4 / TC-03 / TC-A13）：
//   - 核心光标/退格/无 CUU 断言中英双语各跑一遍（复用组 A/B/C 的窄屏夹具）；
//   - 正文段回显与 hint 文案长度、终端宽度无关：40 vs 200 列 + 与 resumeDraft 对照。
//
// 架构红线：TestHintSingleCRLF 保持不参数化（程序层仅 1 CRLF），本文件不触碰它。

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/presentation/terminal/i18n"
)

func bilingualLocales() []string { return []string{i18n.LocaleZH, i18n.LocaleEN} }

// 核心光标/退格断言：同一组逻辑在中英两种文案长度下都必须成立。
func TestDraftRateLimitBilingualCarets(t *testing.T) {
	for _, locale := range bilingualLocales() {
		t.Run(locale, func(t *testing.T) {
			clock := fixedDraftClock()
			ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
			sess := newFakePageSessionWithWidth(40, 1)
			pc := h.newPageControllerWithLocale(sess, locale)
			threadID := seedFirstPost(t, ctx, pc, sess, rt)

			if err := pc.showDetail(ctx, sess, threadID); err != nil {
				t.Fatal(err)
			}
			sess.reset()
			if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
				t.Fatal(err)
			}
			typeInto(t, ctx, sess, pc, "回复正文你好")
			sess.reset()
			submitBody(t, ctx, sess, pc) // 限流 → A 路径 B 追加重建

			if pc.editor == nil || pc.draft == nil {
				t.Fatal("应进入 A 路径（编辑器与草稿都在）")
			}
			if len(pc.echo.cols) != 6 {
				t.Fatalf("cols 应按末行 6 字重建，实际 %v", pc.echo.cols)
			}
			if cuu := cuuParams(sess.output()); len(cuu) != 0 {
				t.Fatalf("%s 路径应无 CUU，实际 %v", locale, cuu)
			}

			sess.reset()
			if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyBackspace}); err != nil {
				t.Fatal(err)
			}
			if got := pc.editor.body.Text(); got != "回复正文你" {
				t.Fatalf("退格应删末字，缓冲现为 %q", got)
			}
			if n := strings.Count(sess.output(), "\b \b"); n != 2 {
				t.Fatalf("末字宽 2 列应有 2 组 \\b \\b，实际 %d: %q", n, sess.output())
			}
			cmdOnlyWhitelistedANSI(t, sess.output())
		})
	}
}

// TC-A13（评审 B3）：正文段输出与 hint 文案长度无关。
// 无缝写法一：40 vs 200 列；写法二：与 resumeDraft 的正文段逐字节对照。
func TestBodyEchoIndependentOfHintLength(t *testing.T) {
	const body = "回复正文你好"
	for _, locale := range bilingualLocales() {
		t.Run(locale, func(t *testing.T) {
			var bpathEcho string
			for i, width := range []int{40, 200} {
				clock := fixedDraftClock()
				ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
				sess := newFakePageSessionWithWidth(uint32(width), 1)
				pc := h.newPageControllerWithLocale(sess, locale)
				threadID := seedFirstPost(t, ctx, pc, sess, rt)

				if err := pc.showDetail(ctx, sess, threadID); err != nil {
					t.Fatal(err)
				}
				sess.reset()
				if err := pc.command(ctx, sess, command.Invocation{Name: "reply"}); err != nil {
					t.Fatal(err)
				}
				typeInto(t, ctx, sess, pc, body)
				sess.reset()
				submitBody(t, ctx, sess, pc)

				out := sess.output()
				idx := strings.LastIndex(out, body)
				if idx < 0 {
					t.Fatalf("输出中找不到正文段: %q", out)
				}
				echo := out[idx:] // B 路径以正文无缝收尾
				if strings.Contains(echo, "\r\n") {
					t.Fatalf("正文段不得被 hint 文案长度带出额外 CRLF: %q", echo)
				}
				if i == 0 {
					bpathEcho = echo
				} else if echo != bpathEcho {
					t.Fatalf("40 vs 200 列正文段应逐字节一致: %q vs %q", bpathEcho, echo)
				}
			}

			// 与 resumeDraft 对照：同一 locale、同一正文，恢复路径的正文段一致。
			clock := fixedDraftClock()
			ctx, h, rt, _, _ := setupDraftRuntime(t, &clock)
			sess := newFakePageSessionWithWidth(40, 1)
			pc := h.newPageControllerWithLocale(sess, locale)
			threadID := seedFirstPost(t, ctx, pc, sess, rt)
			sess.reset()
			if err := pc.resumeDraft(sess, &composerDraft{body: body, threadID: threadID}); err != nil {
				t.Fatal(err)
			}
			out := sess.output()
			idx := strings.LastIndex(out, body)
			if idx < 0 {
				t.Fatalf("resumeDraft 输出中找不到正文段: %q", out)
			}
			if got := out[idx:]; got != bpathEcho {
				t.Fatalf("B 重建与 resumeDraft 的正文段应逐字节一致: %q vs %q", bpathEcho, got)
			}
		})
	}
}

// 双语 hint 前缀与灰阶：zh/zh+en 各自单点查表拼接，仍只走白名单 SGR。
// 不检查 CRLF 计数——那是 TestHintSingleCRLF 的专属红线，保持不参数化。
func TestWriteHintBilingualPrefix(t *testing.T) {
	for _, tc := range []struct {
		locale string
		prefix string
	}{
		{i18n.LocaleZH, "[提示] "},
		{i18n.LocaleEN, "[Info] "},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			sess := newFakePageSession(1)
			pc := newInputHandler().newPageControllerWithLocale(sess, tc.locale)
			sess.reset()
			if err := pc.writeHint(sess, i18n.KeyComposerBody); err != nil {
				t.Fatal(err)
			}
			out := sess.output()
			if !strings.Contains(out, tc.prefix) {
				t.Fatalf("%s hint 应带 %q 前缀: %q", tc.locale, tc.prefix, out)
			}
			if !strings.Contains(out, "\x1b[2m") {
				t.Fatalf("hint 仍应走灰阶 SGR: %q", out)
			}
			cmdOnlyWhitelistedANSI(t, out)
		})
	}
}
