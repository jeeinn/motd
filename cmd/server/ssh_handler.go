package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

// inputHandler 把 SSH 字节流归一化后交给命令注册表的唯一入口（§3.2）。
// P0-24 的页面控制器替换 handle 的业务分发，但解码器与命令表不变。
//
// 安全约束：绝不把 KeyMsg.Raw / KeyMsg.Text 写回终端。用户提交的文本
// 只能经 P0-23 的净化 + 白名单渲染后才能出现；在此之前一律零回显。
type inputHandler struct {
	reg *command.Registry

	// deviceFlow 由 P0-15 装配，P0-16 状态机 / P0-19 的 /login 消费。
	// 端口类型是 application/auth.DeviceFlowClient，cmd 不碰 HTTP 细节。
	deviceFlow auth.DeviceFlowClient
}

func newInputHandler() *inputHandler {
	return &inputHandler{reg: command.Default()}
}

// withDeviceFlow 注入 GitHub Device Flow 客户端，保持 newInputHandler() 的无参
// 签名不变（现有会话层测试零改动）。P0-16 之后由页面控制器直接读取该字段。
func (h *inputHandler) withDeviceFlow(df auth.DeviceFlowClient) *inputHandler {
	h.deviceFlow = df
	return h
}

// commandLine 是斜杠命令的行编辑缓冲。P0-22 只做收集与提交，不回显原文。
type commandLine struct {
	active bool
	runes  []rune
}

type cmdAction uint8

const (
	actionNone cmdAction = iota
	actionSubmit
	actionCancel
	actionQuit
	actionHelp
	actionCommandStart
	actionPasteNotice
	actionEscNotice
)

func (h *inputHandler) Serve(ctx context.Context, sess *sshd.Session) error {
	_, _ = fmt.Fprintf(sess, "motd %s\r\n", version)
	_, _ = io.WriteString(sess, "访客只读。按 ? 查看命令；页面将在 P0-24 接入。\r\n")
	_ = sess.SetState(presence.StateBrowsing)

	dec := sshd.NewKeyDecoder()
	msgs := make(chan input.KeyMsg, 64)
	buf := make([]byte, 4096)
	go func() {
		defer close(msgs)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				for _, m := range dec.Feed(buf[:n]) {
					select {
					case msgs <- m:
					case <-ctx.Done():
						return
					case <-sess.Done():
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	var cl commandLine
	ticker := time.NewTicker(sshd.EscPollInterval)
	defer ticker.Stop()
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				return nil
			}
			if h.handle(sess, m, &cl) {
				return nil
			}
		case <-ticker.C:
			for _, m := range dec.Poll() {
				if h.handle(sess, m, &cl) {
					return nil
				}
			}
		case <-ctx.Done():
			return nil
		case <-sess.Done():
			return nil
		}
	}
}

func (h *inputHandler) handle(sess *sshd.Session, m input.KeyMsg, cl *commandLine) bool {
	action, line, pasteLen := applyKey(cl, m)
	switch action {
	case actionQuit:
		return true
	case actionCancel:
		_, _ = io.WriteString(sess, "（已取消命令输入）\r\n")
	case actionHelp:
		_, _ = io.WriteString(sess, h.reg.Help())
	case actionCommandStart:
		_, _ = io.WriteString(sess, "命令输入：以 / 开头，Enter 提交，Esc 取消。\r\n")
	case actionPasteNotice:
		_, _ = fmt.Fprintf(sess, "（已收到粘贴块 %d 字节，P0-24 接入编辑器）\r\n", pasteLen)
	case actionEscNotice:
		_, _ = io.WriteString(sess, "（取消）\r\n")
	case actionSubmit:
		inv, err := h.reg.Parse(line)
		_, _ = io.WriteString(sess, formatResult(h.reg, inv, err))
		return inv.Name == "quit" && err == nil
	}
	return false
}

// applyKey 是纯状态机，不碰 IO，便于在没有 SSH 会话的情况下回归。
// 返回动作、提交时的命令行文本、粘贴块字节数。
func applyKey(cl *commandLine, m input.KeyMsg) (cmdAction, string, int) {
	if cl.active {
		switch m.Kind {
		case input.KeyEnter:
			line := string(cl.runes)
			cl.active, cl.runes = false, nil
			return actionSubmit, line, 0
		case input.KeyEsc, input.KeyCtrlC:
			cl.active, cl.runes = false, nil
			return actionCancel, "", 0
		case input.KeyCtrlD:
			return actionQuit, "", 0
		case input.KeyBackspace:
			if len(cl.runes) > 1 {
				cl.runes = cl.runes[:len(cl.runes)-1]
			}
			return actionNone, "", 0
		case input.KeyRune:
			cl.runes = append(cl.runes, m.Rune)
			return actionNone, "", 0
		case input.KeyPaste:
			for _, r := range m.Text {
				cl.runes = append(cl.runes, r)
			}
			return actionNone, "", 0
		default:
			return actionNone, "", 0
		}
	}

	switch m.Kind {
	case input.KeyCtrlC, input.KeyCtrlD:
		return actionQuit, "", 0
	case input.KeyRune:
		switch m.Rune {
		case '?':
			return actionHelp, "", 0
		case '/':
			cl.active = true
			cl.runes = []rune{'/'}
			return actionCommandStart, "", 0
		}
	case input.KeyEsc:
		return actionEscNotice, "", 0
	case input.KeyPaste:
		return actionPasteNotice, "", len(m.Text)
	}
	return actionNone, "", 0
}

// formatResult 只输出服务端生成的文本或注册表白名单里的命令名，
// 绝不把用户提交的原始字节（含 ESC / CSI / OSC）写回终端。
// 参数 inv.Args 也刻意不输出：它来自用户输入，不是注册表常量。
func formatResult(reg *command.Registry, inv command.Invocation, err error) string {
	switch {
	case errors.Is(err, command.ErrUnknownCommand):
		return "未知命令（按 ? 查看全部命令）\r\n"
	case errors.Is(err, command.ErrArgCount):
		return "参数个数不符（按 ? 查看用法）\r\n"
	case errors.Is(err, command.ErrEmptyCommand), errors.Is(err, command.ErrNotCommand):
		return "（未识别的命令输入）\r\n"
	case err != nil:
		return "命令无法解析\r\n"
	}
	switch inv.Name {
	case "help":
		return reg.Help()
	case "quit":
		return "再见\r\n"
	default:
		return fmt.Sprintf("已收到 /%s（P0-24 接入业务动作）\r\n", inv.Name)
	}
}
