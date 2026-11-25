package main

// R-09 resize 即时对齐：transport 只投递通知，Handler 主循环收到后按新尺寸
// 整屏重绘当前屏；交互态（行输入 / 编辑器 / 登录 / 邮箱）跳过，避免抹掉
// 用户已输入但尚未提交的内容。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
	sshd "github.com/jeeinn/motd/internal/transport/ssh"
)

func TestResizeRedrawable(t *testing.T) {
	cases := []struct {
		name    string
		cl      *commandLine
		pc      *pageController
		inLogin bool
		want    bool
	}{
		{"纯浏览态", &commandLine{}, &pageController{}, false, true},
		{"命令输入态", &commandLine{active: true, runes: []rune{'/', 'n'}}, &pageController{}, false, false},
		{"编辑器态", &commandLine{}, &pageController{editor: &pageEditor{}}, false, false},
		{"邮箱流程", &commandLine{}, &pageController{emailFlow: &emailFlow{}}, false, false},
		{"登录流程", &commandLine{}, &pageController{}, true, false},
		{"多重叠加", &commandLine{active: true}, &pageController{editor: &pageEditor{}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resizeRedrawable(tc.cl, tc.pc, tc.inLogin); got != tc.want {
				t.Fatalf("resizeRedrawable=%v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestResizeRedrawsCurrentScreen 走真实 SSH + 真实 SQLite：窗口变化后不按键，
// 服务端也应立刻按新尺寸重绘当前屏；进入命令输入态后则不重绘，退出后恢复。
func TestResizeRedrawsCurrentScreen(t *testing.T) {
	_, h, rt, pools, _ := setupMVPRuntime(t)
	k := newMVPKey(t)

	srv, err := sshd.New(sshd.Config{
		Addr:     "127.0.0.1:0",
		DataDir:  t.TempDir(),
		MaxConns: 10,
		Resolver: auth.NewResolver(sqlite.NewSSHKeyLookup(pools)),
		Presence: rt.Presence,
		Handler:  h,
	})
	if err != nil {
		t.Fatalf("启动 SSH 服务: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(serveCtx) }()
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("SSH 监听未就绪")
	}
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})

	a := dialMVP(t, addr, k)
	defer a.close()

	// 首帧：版块列表（H+2J 整屏干净重绘，ADR-072）。
	first := a.out.mark()
	a.out.waitSince(t, first, "/dev/ai")
	if !strings.Contains(a.out.text()[first:], "\x1b[H\x1b[2J") {
		t.Fatalf("首帧应是 H+2J 整屏重绘: %q", a.out.text()[first:])
	}

	// 缩放窗口：不按键，服务端应立即按新尺寸重绘当前页，不等下一次 render。
	mark := a.out.mark()
	if err := a.sess.WindowChange(30, 120); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}
	a.out.waitSince(t, mark, "\x1b[H\x1b[2J")
	a.out.waitSince(t, mark, "/dev/ai") // 仍是版块列表，没有重新取数 / 跳页

	// 命令输入态：整屏重绘会抹掉已回显的命令行，必须跳过。
	a.send(t, "/")
	a.out.waitSince(t, mark, "已进入命令输入")
	quiet := a.out.mark()
	if err := a.sess.WindowChange(24, 90); err != nil {
		t.Fatalf("命令输入态 WindowChange: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if strings.Contains(a.out.text()[quiet:], "\x1b[H\x1b[2J") {
		t.Fatal("命令输入态不应整屏重绘（会抹掉用户已输入的内容）")
	}

	// 退出命令输入态后恢复：resize 再次触发重绘。
	a.send(t, "\x1b")
	a.out.waitSince(t, quiet, "（已取消命令输入）")
	restored := a.out.mark()
	if err := a.sess.WindowChange(20, 100); err != nil {
		t.Fatalf("恢复浏览态 WindowChange: %v", err)
	}
	a.out.waitSince(t, restored, "\x1b[H\x1b[2J")
}
