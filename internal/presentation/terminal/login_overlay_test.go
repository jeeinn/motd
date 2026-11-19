package terminal

import (
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

func deviceCodeScreen() viewmodel.Screen {
	return viewmodel.Screen{
		Kind: viewmodel.ScreenLogin,
		StatusBar: viewmodel.StatusBar{
			Online: 4, Active: 2,
			Identity: viewmodel.Identity{Guest: true},
			Level:    1,
		},
		Overlay: viewmodel.Overlay{
			Kind:   viewmodel.OverlayDeviceCode,
			Title:  "motd · 登录",
			Body:   "有效期约 900 秒",
			Fields: []viewmodel.Field{{Label: "请在浏览器打开：", Value: "https://github.com/login/device"}},
			Actions: []viewmodel.Action{{
				Key: "Esc", Label: "按 Esc 取消本次登录。", Command: "cancel", Primary: true,
			}},
			Progress: viewmodel.Progress{
				UserCode: "ABCD-EFGH", PollIntervalSec: 5, State: viewmodel.DeviceFlowWaiting,
			},
		},
	}
}

func TestRenderScreenDeviceCodeOverlay(t *testing.T) {
	out, err := RenderScreen(deviceCodeScreen(), 80)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"motd · 登录", "有效期约 900 秒", "请在浏览器打开：",
		"https://github.com/login/device", "验证码：", "ABCD-EFGH",
		"[Esc] 按 Esc 取消本次登录。", "online=4 active=2 | 访客 | karma=0 L1",
		"Esc 取消登录 · 浏览器完成授权后自动继续",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("渲染缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "（暂无内容）") {
		t.Fatal("浮层屏不应出现空内容占位行")
	}
	if strings.Contains(out, "j/k 移动") {
		t.Fatal("登录屏不应显示论坛导航提示")
	}
	// SGR 配色是允许的；清屏 / 清行 / 光标定位不允许出现在纯渲染结果里。
	for _, banned := range []string{"\x1b[2J", "\x1b[K", "\x1b[1A", "\x1b[H"} {
		if strings.Contains(out, banned) {
			t.Fatalf("RenderScreen 不得产生光标控制 %q: %q", banned, out)
		}
	}
}

func TestRenderScreenOverlaySanitizesExternalFields(t *testing.T) {
	sc := deviceCodeScreen()
	sc.Overlay.Fields[0].Value = "https://x.example/\x1b[2J\x07"
	sc.Overlay.Progress.UserCode = "AB\x1b]0;pwned\x07CD"
	out, err := RenderScreen(sc, 80)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatalf("外部字段必须经 RenderUser 净化: %q", out)
	}
	if !strings.Contains(out, "␛") {
		t.Fatal("控制字符应被可见化为控制图示")
	}
}

func TestRenderScreenRejectsControlInServerOverlayText(t *testing.T) {
	sc := deviceCodeScreen()
	sc.Overlay.Title = "motd\x1b[2J"
	if _, err := RenderScreen(sc, 80); err == nil {
		t.Fatal("服务端固定文案含控制字符必须失败关闭，而不是静默输出")
	}
}

func TestRenderScreenOtherScreensKeepForumFooter(t *testing.T) {
	sc := deviceCodeScreen()
	sc.Kind = viewmodel.ScreenNodes
	sc.Overlay = viewmodel.Overlay{}
	out, err := RenderScreen(sc, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "j/k 移动 · Enter 打开") {
		t.Fatal("论坛页面的页脚不能被登录屏改动")
	}
	if !strings.Contains(out, "（暂无版块）") {
		t.Fatal("无内容页面仍应显示占位提示")
	}
}
