package main

// P0-19 呈现层回归：固定文本走 RenderSystem、外部字段走 RenderUser、
// LF→CRLF 在呈现层完成、Notice 只有白名单常量才允许展示。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

func TestToCRLFNormalizesLineEndings(t *testing.T) {
	cases := map[string]string{
		"a\nb":       "a\r\nb",
		"a\r\nb":     "a\r\nb",
		"a\r\n\r\nb": "a\r\n\r\nb",
		"":           "",
		"a":          "a",
	}
	for in, want := range cases {
		if got := toCRLF(in); got != want {
			t.Errorf("toCRLF(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestLoginPresenterLineUsesRenderSystemAndCRLF(t *testing.T) {
	var b bytes.Buffer
	p := loginPresenter{}
	if err := p.line(&b, auth.LoginTextStart, safe.StyleBold); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, auth.LoginTextStart) || !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("固定文本应原样输出并以 CRLF 收尾: %q", out)
	}
	if strings.Contains(out, "\n") && !strings.Contains(out, "\r\n") {
		t.Fatal("不得出现裸 LF")
	}
	// 服务端固定文本含控制字符时必须被 RenderSystem 拒绝，而不是静默擦除。
	var bad bytes.Buffer
	if err := p.line(&bad, "x\x1b[2Jy", safe.StylePlain); err == nil {
		t.Fatal("RenderSystem 应拒绝含控制字符的服务端文本")
	}
}

func TestLoginPresenterExternalSanitizesTerminalControl(t *testing.T) {
	var b bytes.Buffer
	p := loginPresenter{}
	malicious := "\x1b[2J\x1b]0;pwned\x07\x1b]52;c;YWJj\x07\u202e"
	if err := p.external(&b, malicious); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\x07") {
		t.Fatalf("外部字段必须经净化，输出不得含原始控制字节: %q", out)
	}
	if !strings.Contains(out, "␛") {
		t.Fatalf("ESC 应被可见化为控制图示: %q", out)
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("外部行也必须以 CRLF 收尾: %q", out)
	}
}

func TestShowStartContainsOnlyWhitelistedFields(t *testing.T) {
	var b bytes.Buffer
	p := loginPresenter{}
	v := auth.LoginView{
		UserCode:         "ABCD-EFGH",
		VerificationURI:  "https://github.com/login/device",
		RemainingSeconds: 900,
		IntervalSeconds:  5,
	}
	if err := p.showStart(&b, v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"ABCD-EFGH", "https://github.com/login/device", "900", auth.LoginTextCancelHint, auth.LoginTextStart} {
		if !strings.Contains(out, want) {
			t.Errorf("发起屏缺少 %q", want)
		}
	}
	if strings.Count(out, "\r\n") < 5 {
		t.Fatalf("发起屏应为多行固定文本输出: %q", out)
	}
}

func TestShowSuccessDisplaysOnlyWhitelistedNotice(t *testing.T) {
	p := loginPresenter{}
	for _, notice := range []string{auth.EmailOccupiedNotice, "任意外部提示 \x1b[2J", ""} {
		var b bytes.Buffer
		if err := p.showSuccess(&b, "alice_", notice, true); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		if !strings.Contains(out, "alice_") || !strings.Contains(out, auth.LoginTextSuccessCreate) {
			t.Fatalf("成功屏缺少账号 / 固定文案: %q", out)
		}
		if notice == auth.EmailOccupiedNotice {
			if !strings.Contains(out, notice) {
				t.Fatal("白名单 Notice 应展示")
			}
		} else if strings.Contains(out, notice) && notice != "" {
			t.Fatalf("非白名单 Notice 不得展示: %q", out)
		}
		if strings.Contains(out, "\x1b[2J") {
			t.Fatalf("成功屏不得出现原始控制序列: %q", out)
		}
	}
}
