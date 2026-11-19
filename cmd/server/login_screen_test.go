package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal"
)

func renderScreenToString(sc viewmodel.Screen) (string, error) {
	return terminal.RenderScreen(sc, 80)
}

func testLoginView() auth.LoginView {
	return auth.LoginView{
		UserCode:         "ABCD-EFGH",
		VerificationURI:  "https://github.com/login/device",
		ExpiresAt:        time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		IntervalSeconds:  5,
		RemainingSeconds: 900,
	}
}

func TestLoginScreenUsesFourPageViewModelAndStatusBar(t *testing.T) {
	sb := viewmodel.StatusBar{
		Online: 7, Active: 3,
		Identity: viewmodel.Identity{Guest: true},
		Level:    1,
	}
	sc := loginScreen(testLoginView(), sb)
	if sc.Kind != viewmodel.ScreenLogin {
		t.Fatalf("页面种类应为 ScreenLogin，实际 %v", sc.Kind)
	}
	if sc.Overlay.Kind != viewmodel.OverlayDeviceCode {
		t.Fatal("登录发起屏必须携带 Device Flow 浮层")
	}
	if sc.StatusBar != sb || sc.StatusBar.Online != 7 || sc.StatusBar.Active != 3 {
		t.Fatalf("状态栏必须与论坛同源: %+v", sc.StatusBar)
	}
	if err := sc.Validate(); err != nil {
		t.Fatalf("ViewModel 必须自洽: %v", err)
	}
	if len(sc.Rows) != 0 {
		t.Fatalf("登录屏没有列表行，实际 %d", len(sc.Rows))
	}
}

func TestLoginScreenHasNoSecretFields(t *testing.T) {
	// LoginView 结构不得含 device_code / token（ADR-021）；
	// 用反射而非源码文本，保证新增字段也会被测到。
	v := testLoginView()
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(name, "token") || strings.Contains(name, "devicecode") {
			t.Fatalf("LoginView 不得含敏感字段: %s", rt.Field(i).Name)
		}
	}
	sc := loginScreen(v, viewmodel.StatusBar{})
	b, err := renderScreenToString(sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"device_code", "access_token", "gho_", "ghu_"} {
		if strings.Contains(strings.ToLower(b), banned) {
			t.Fatalf("渲染结果泄漏 %s: %q", banned, b)
		}
	}
}

func TestLoginScreenSeparatesFixedAndExternalText(t *testing.T) {
	sc := loginScreen(testLoginView(), viewmodel.StatusBar{})
	o := sc.Overlay
	if o.Title != auth.LoginTextStart || o.Body != "有效期约 900 秒" {
		t.Fatalf("固定文案位置错误: title=%q body=%q", o.Title, o.Body)
	}
	if len(o.Fields) != 1 || o.Fields[0].Value != "https://github.com/login/device" {
		t.Fatalf("verification_uri 应只出现在 Field.Value: %+v", o.Fields)
	}
	if o.Fields[0].Label != auth.LoginTextOpenHint {
		t.Fatal("固定提示应走 Field.Label（RenderSystem）")
	}
	if o.Progress.UserCode != "ABCD-EFGH" || o.Progress.State != viewmodel.DeviceFlowWaiting {
		t.Fatalf("用户码/阶段错误: %+v", o.Progress)
	}
	if o.Actions[0].Key != "Esc" || o.Actions[0].Label != auth.LoginTextCancelHint {
		t.Fatalf("取消键文案错误: %+v", o.Actions)
	}
}

func TestLoginPresenterShowStartRendersScreenWithCRLF(t *testing.T) {
	var b bytes.Buffer
	p := &loginPresenter{status: viewmodel.StatusBar{
		Online: 2, Active: 1, Identity: viewmodel.Identity{Guest: true}, Level: 1,
	}}
	if err := p.showStart(&b, testLoginView()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"motd · 登录", auth.LoginTextStart, "ABCD-EFGH",
		"https://github.com/login/device", "900", "online=2 active=1",
		auth.LoginTextCancelHint, "Esc 取消登录",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("发起屏缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n") && !strings.Contains(out, "\r\n") {
		t.Fatal("不得出现裸 LF")
	}
	// SGR 上色允许；清屏 / 清行 / 光标上移不允许出现在首次追加渲染里。
	for _, banned := range []string{"\x1b[2J", "\x1b[K", "\x1b[1A"} {
		if strings.Contains(out, banned) {
			t.Fatalf("首次渲染不应含 %q: %q", banned, out)
		}
	}
	if p.screenLines <= 0 {
		t.Fatal("应记住本屏行数，供后续原地重绘判定")
	}
}

func TestLoginPresenterLineBreaksScreenContinuity(t *testing.T) {
	p := &loginPresenter{}
	p.screenLines = 9
	if err := p.line(&bytes.Buffer{}, "等待授权中", 0); err != nil {
		t.Fatal(err)
	}
	if p.screenLines != 0 {
		t.Fatal("非屏幕文本必须打断连续性，避免 CursorUp 覆盖错位置")
	}
}
