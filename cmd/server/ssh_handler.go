package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/presence"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

// guestResolver 是 P0-21 阶段的占位身份解析器。
//
// 账号体系与 ssh_keys 表在 M2（P0-14）才落地；此刻服务端已通过公钥挑战
// 证明对方持有私钥，一律按访客放行。M2 换成查库实现即可，transport/ssh
// 不需要改动（端口定义在 application/auth，ADR-017）。
type guestResolver struct{}

func (guestResolver) Resolve(_ context.Context, fingerprint string) (auth.Principal, error) {
	if fingerprint == "" {
		return auth.Principal{}, fmt.Errorf("公钥指纹为空")
	}
	return auth.Guest(fingerprint), nil
}

// greetingHandler 只输出服务端生成的固定文本，不回显任何用户字节。
// P0-22 接入输入归一化、P0-24 接入页面后替换本实现即可；
// 在此之前回显原始字节会绕过 P0-23 的安全渲染，所以一律不回显。
type greetingHandler struct{}

func (greetingHandler) Serve(_ context.Context, sess *sshd.Session) error {
	w, h, term := sess.Window()
	_, _ = fmt.Fprintf(sess, "motd %s\r\n", version)
	_, _ = fmt.Fprintf(sess, "终端 %s %dx%d；访客只读。\r\n", term, w, h)
	_, _ = io.WriteString(sess, "页面将在 P0-24 接入；当前会话仅用于验证 SSH 链路与在线状态。\r\n")
	_ = sess.SetState(presence.StateBrowsing)

	buf := make([]byte, 256)
	for {
		_, err := sess.Read(buf)
		if err != nil {
			return nil
		}
		// P0-22 前只维持心跳，不解析按键。
	}
}
