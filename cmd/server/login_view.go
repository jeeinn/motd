package main

// P0-19 最小交互层的**呈现适配器**（docs/DEVIATIONS.md R18 / R20 的 P0-24 替换点）。
//
// 职责只有一件：把「服务端固定文案」与「来自 GitHub 的外部字段」分别送进
// P0-23 的两条渲染路径，并完成 PTY 需要的 LF→CRLF 转换。
//
// 分层约束（§3.5 / ADR-019 / archtest/safe_test.go）：
//   - 固定文案 → safe.RenderSystem（服务端可信路径，不共用 SanitizeText）；
//   - user_code / verification_uri / username → safe.RenderUser（外部数据净化路径）；
//   - 两类文本**绝不拼进同一次渲染调用**，否则外部数据会绕过净化。
//
// P0-24 落地时整体替换本文件为 ViewModel + 双 Renderer 页面控制器；
// application/auth 与 transport/ssh 协议层零改动。

import (
	"fmt"
	"io"
	"strings"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

type loginPresenter struct{}

// line 渲染一行服务端固定文本。text 必须是编译期常量或由本地数值拼成的
// 可信文本；任何来自响应的字符串都不允许传进来。
func (loginPresenter) line(out io.Writer, text string, style safe.Style) error {
	rendered, err := safe.RenderSystem(text, style)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, toCRLF(rendered)+"\r\n")
	return err
}

// external 渲染一行外部数据（GitHub 响应的 user_code / verification_uri、
// 或派生出的 username）。先经 ProjectLoginView 的格式白名单，再走净化路径。
func (loginPresenter) external(out io.Writer, text string) error {
	_, err := io.WriteString(out, toCRLF(safe.RenderUser(text, safe.Options{}))+"\r\n")
	return err
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// showStart 输出 /login 发起屏：固定提示 + 外部字段 + 剩余时间 + 取消键。
// user_code 与 verification_uri 分开两行走 external，不与固定文案拼接。
func (p loginPresenter) showStart(out io.Writer, v auth.LoginView) error {
	if err := p.line(out, auth.LoginTextStart, safe.StyleBold); err != nil {
		return err
	}
	if err := p.line(out, auth.LoginTextOpenHint, safe.StylePlain); err != nil {
		return err
	}
	if err := p.external(out, v.VerificationURI); err != nil {
		return err
	}
	if err := p.external(out, "验证码："+v.UserCode); err != nil {
		return err
	}
	// 秒数来自本地时间计算（int），不是响应原文，可以进 RenderSystem。
	if err := p.line(out, fmt.Sprintf("有效期约 %d 秒", v.RemainingSeconds), safe.StyleDim); err != nil {
		return err
	}
	return p.line(out, auth.LoginTextCancelHint, safe.StyleDim)
}

// showSuccess 输出成功屏。username 走 external；Notice 只允许是白名单常量
// （auth.EmailOccupiedNotice），其它一律不展示，避免外部字符串进 RenderSystem。
func (p loginPresenter) showSuccess(out io.Writer, username, notice string, created bool) error {
	if created {
		if err := p.line(out, auth.LoginTextSuccessCreate, safe.StyleBold); err != nil {
			return err
		}
	} else if err := p.line(out, auth.LoginTextSuccessLogin, safe.StyleBold); err != nil {
		return err
	}
	if err := p.line(out, auth.LoginTextWelcome, safe.StylePlain); err != nil {
		return err
	}
	if err := p.external(out, username); err != nil {
		return err
	}
	if err := p.line(out, auth.LoginTextWriteReady, safe.StylePlain); err != nil {
		return err
	}
	if notice == auth.EmailOccupiedNotice {
		return p.line(out, notice, safe.StyleDim)
	}
	return nil
}
