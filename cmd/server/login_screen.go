package main

import (
	"fmt"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal"
)

// /login 发起屏的 ViewModel 投影（P1-M2：与论坛四页共用 Screen + Renderer）。
//
// 纯函数：不碰 IO、不碰时间、不持有会话。输入只有已过白名单的 auth.LoginView
// （结构里刻意没有 device_code / access_token，ADR-021）与服务端的身份快照。
//
// 信任域：固定文案放 Title / Body / Label，外部字段（user_code /
// verification_uri）只放 Field.Value / Progress.UserCode，由 RenderScreen
// 分别送 RenderSystem / RenderUser。
func loginScreen(v auth.LoginView, sb viewmodel.StatusBar) viewmodel.Screen {
	// 零值 StatusBar 也要能渲染：等级档位必须落在 [1,4]，否则 Validate 失败。
	if sb.Level < 1 {
		sb.Level = 1
	}
	return viewmodel.Screen{
		Kind:      viewmodel.ScreenLogin,
		StatusBar: sb,
		Overlay: viewmodel.Overlay{
			Kind:  viewmodel.OverlayDeviceCode,
			Title: auth.LoginTextStart,
			Body:  fmt.Sprintf("有效期约 %d 秒", v.RemainingSeconds),
			Fields: []viewmodel.Field{{
				Key:   "verification_uri",
				Label: auth.LoginTextOpenHint,
				Value: v.VerificationURI,
			}},
			Actions: []viewmodel.Action{{
				Key:     "Esc",
				Label:   auth.LoginTextCancelHint,
				Command: "cancel", // 归一化命令串，只作语义标记，不被 Renderer 输出
				Primary: true,
			}},
			Progress: viewmodel.Progress{
				UserCode:        v.UserCode,
				ExpiresAt:       v.ExpiresAt.Unix(),
				PollIntervalSec: v.IntervalSeconds,
				State:           viewmodel.DeviceFlowWaiting,
			},
		},
	}
}

// renderLoginScreen 把 ViewModel 交给共享渲染器，并在呈现层完成 LF→CRLF。
// prevLines 是上一屏行数：0 表示此前插入过非屏幕文本，退回追加模式（ADR-027）。
func renderLoginScreen(out *loginPresenter, v auth.LoginView, prevLines int) (string, int, error) {
	text, err := terminal.RenderScreen(loginScreen(v, out.status), out.width)
	if err != nil {
		return "", 0, err
	}
	frame, lines := terminal.RenderFrame(toCRLF(text), prevLines, out.width)
	return frame, lines, nil
}
