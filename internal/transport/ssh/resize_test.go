package ssh

// R-09 resize 即时对齐：transport 只投递「窗口变了」的信号，绝不直接重绘
// 终端。这里守两条契约：容量 1 合并连续事件、通道永不关闭。

import (
	"context"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestNotifyResizeCoalescesAndNeverCloses(t *testing.T) {
	s := &Session{resizeC: make(chan struct{}, 1)}
	for i := 0; i < 10; i++ {
		s.notifyResize() // 容量 1：连续事件必须合并，且绝不阻塞
	}
	select {
	case <-s.ResizeNotify():
	default:
		t.Fatal("首次 notifyResize 应投递成功")
	}
	select {
	case <-s.ResizeNotify():
		t.Fatal("连续事件必须被容量 1 合并为 1 次通知")
	default:
	}
	// 消费完再投一次仍可用：通道永不关闭，会话收口后迟到的 window-change
	// 走非阻塞投递不会 panic。
	s.notifyResize()
	select {
	case <-s.ResizeNotify():
	default:
		t.Fatal("消费后应能再次收到通知")
	}
}

func TestSetSizeReportsChange(t *testing.T) {
	s := &Session{resizeC: make(chan struct{}, 1)}
	if !s.setSize(100, 30) {
		t.Fatal("首次设置尺寸应返回 changed=true")
	}
	if s.setSize(100, 30) {
		t.Fatal("尺寸未变应返回 false，避免冗余整屏重绘")
	}
	if !s.setSize(80, 24) {
		t.Fatal("尺寸变化应返回 true")
	}
	w, h, _ := s.Window()
	if w != 80 || h != 24 {
		t.Fatalf("Window() 尺寸不符: %dx%d", w, h)
	}
}

// resizeHandler 只消费 ResizeNotify、不写终端，用于验证 window-change 的
// 生产端投递与「transport 不直接重绘」的分层约定。
type resizeHandler struct {
	resized chan [2]uint32
}

func (h *resizeHandler) Serve(ctx context.Context, sess *Session) error {
	for {
		select {
		case <-sess.ResizeNotify():
			w, ht, _ := sess.Window()
			select {
			case h.resized <- [2]uint32{w, ht}:
			default:
			}
		case <-ctx.Done():
			return nil
		case <-sess.Done():
			return nil
		}
	}
}

func TestWindowChangeNotifiesHandler(t *testing.T) {
	h := &resizeHandler{resized: make(chan [2]uint32, 4)}
	_, addr := startServer(t, Config{Handler: h})

	cl, err := gossh.Dial("tcp", addr, clientConfig(t))
	if err != nil {
		t.Fatalf("SSH 拨号: %v", err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("Shell: %v", err)
	}

	// pty-req 只 setWindow，不投递通知（首帧由 Handler 自己输出）。
	select {
	case got := <-h.resized:
		t.Fatalf("pty-req 不应触发 resize 通知: %v", got)
	case <-time.After(50 * time.Millisecond):
	}

	// 尺寸真的变化才通知，且 Handler 读到的是更新后的 Window()。
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}
	waitWindow(t, h.resized, [2]uint32{100, 30})

	// 重复相同尺寸不触发新通知（setSize 返回 false）。
	if err := sess.WindowChange(30, 100); err != nil {
		t.Fatalf("重复 WindowChange: %v", err)
	}
	select {
	case got := <-h.resized:
		t.Fatalf("尺寸未变不应触发新通知: %v", got)
	case <-time.After(50 * time.Millisecond):
	}
}
