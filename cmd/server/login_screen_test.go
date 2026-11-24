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
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
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
	p := &loginPresenter{
		status: viewmodel.StatusBar{
			Online: 2, Active: 1, Identity: viewmodel.Identity{Guest: true}, Level: 1,
		},
		width: 80, height: 24,
	}
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
	// ADR-072：发起屏是整屏状态更新 → 以 H + 2J 开头，整帧各出现一次。
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("发起屏应以 ESC[H + ESC[2J 开头，实际 %q", out[:min(24, len(out))])
	}
	if n := strings.Count(out, "\x1b[H"); n != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[H，实际 %d", n)
	}
	if n := strings.Count(out, "\x1b[2J"); n != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[2J，实际 %d", n)
	}
	// 整屏重绘不再逐行补清、也不上移光标。
	for _, banned := range []string{"\x1b[K", "\x1b[1A", "\x1b[2K", "\x1b[3J", "\x1b[1J"} {
		if strings.Contains(out, banned) {
			t.Fatalf("整屏重绘不应含 %q: %q", banned, out)
		}
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("n<height 时末行应补 CRLF（光标到输入位），实际 %q", out[len(out)-8:])
	}
}

// TestLoginPresenterLineAppendsWithoutClear：逐行反馈（轮询/成功屏）保持追加，
// 不整屏清屏——否则用户看不到上一步状态，也无法回看（ADR-072 §4.3）。
func TestLoginPresenterLineAppendsWithoutClear(t *testing.T) {
	var b bytes.Buffer
	p := &loginPresenter{}
	if err := p.line(&b, "等待授权中", safe.StylePlain); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "等待授权中") {
		t.Fatalf("追加输出应直接以文案开头，实际 %q", out)
	}
	for _, banned := range []string{"\x1b[H", "\x1b[2J", "\x1b[K", "\x1b["} {
		if strings.Contains(out, banned) {
			t.Fatalf("逐行追加不得含控制序列 %q: %q", banned, out)
		}
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("追加行应以 CRLF 收尾: %q", out)
	}
}
