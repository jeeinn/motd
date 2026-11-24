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
// MVP 收尾状态：论坛四页（版块/主题/详情/编辑）已由 page_controller.go 接通
// ViewModel + 终端 Renderer；本文件仅剩 /login 的呈现尚未替换，仍保留为
// 独立替换点（DEVIATIONS.md R18/R20）。application/auth 与 transport/ssh 协议层零改动。

import (
	"io"
	"strings"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal"
	"github.com/jeeinn/motd/internal/presentation/terminal/safe"
)

type loginPresenter struct {
	// status 是论坛同一份 StatusBar 语义投影（在线/活跃/身份/等级），
	// 由装配层从 presence 快照注入，保证 /login 与四页论坛状态栏一致。
	status viewmodel.StatusBar
	// width 来自当前连接的 pty-req；0 时 Renderer 按 80 列降级。
	width int
	// height 来自当前连接的 pty-req；>0 时 RenderCleanFrame 用它决定末行是否
	// 补 CRLF（n>=height 不补，防底行换行滚屏，ADR-072）；0 表示未知、保守不补。
	height int
}

// line 渲染一行服务端固定文本。text 必须是编译期常量或由本地数值拼成的
// 可信文本；任何来自响应的字符串都不允许传进来。
//
// 逐行反馈（device code 轮询、成功屏）保持**追加**语义：日志式的多步输出
// 若每次清屏会当场丢内容，用户需要能回看（ADR-072 §4.3）。
func (p *loginPresenter) line(out io.Writer, text string, style safe.Style) error {
	rendered, err := safe.RenderSystem(text, style)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, toCRLF(rendered)+"\r\n")
	return err
}

// external 渲染一行外部数据（GitHub 响应的 user_code / verification_uri、
// 或派生出的 username）。先经 ProjectLoginView 的格式白名单，再走净化路径。
func (p *loginPresenter) external(out io.Writer, text string) error {
	_, err := io.WriteString(out, toCRLF(safe.RenderUser(text, safe.Options{}))+"\r\n")
	return err
}

func toCRLF(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// showStart 输出 /login 发起屏：复用论坛四页的 ViewModel + 共享渲染器，
// 状态栏与论坛同源。固定文案与外部字段在 loginScreen 里已经分流到
// RenderSystem / RenderUser 两条路径，这里只负责把渲染结果写进 PTY。
//
// 发起屏是**整屏状态更新**，与普通导航页一致走 RenderCleanFrame（H + 2J）：
// 从左上角重画，消除残留与空行；其后的轮询状态行仍按追加输出（line/external）。
//
// 渲染失败时失败关闭：只输出一行服务端固定文案，绝不把可疑 ViewModel
// 或外部原文原样写回终端。
func (p *loginPresenter) showStart(out io.Writer, v auth.LoginView) error {
	text, err := terminal.RenderScreen(loginScreen(v, p.status), p.width)
	if err != nil {
		return p.line(out, auth.LoginTextDeviceCodeInvalidated, safe.StylePlain)
	}
	frame, _ := terminal.RenderCleanFrame(toCRLF(text), p.height)
	_, err = io.WriteString(out, frame)
	return err
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
