package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

const cmdAdminFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func setupNodeAdminController(t *testing.T, whitelist map[string]struct{}, audit *auth.AuditLogger) (*pageController, context.Context, *sqlite.Pools) {
	t.Helper()
	ctx := context.Background()
	pools, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "motd.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(func() {
		writer.Stop()
		_ = pools.Close()
	})
	now := time.Now().Unix()
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES ('admin', 'active', ?, ?)`,
			now, now)
		return err
	}); err != nil {
		t.Fatalf("准备账号: %v", err)
	}
	rt, err := newForumRuntime(pools, writer, nil, whitelist, audit)
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	pc := rt.newPageControllerForTest()
	pc.users = sqlite.NewUserStatusReader(pools)
	return pc, ctx, pools
}

func nodeAdminSession(userID int64, fp string, guest bool) *fakePageSession {
	p := auth.Principal{Kind: auth.KindUser, UserID: userID, KeyFingerprint: fp}
	if guest {
		p = auth.Guest(fp)
	}
	return &fakePageSession{principal: p}
}

func runNodeCommand(t *testing.T, pc *pageController, ctx context.Context, sess *fakePageSession, line string) {
	t.Helper()
	inv, err := command.Default().Parse(line)
	if err != nil {
		t.Fatalf("解析 %q: %v", line, err)
	}
	if err := pc.command(ctx, sess, inv); err != nil {
		t.Fatalf("执行 %q: %v", line, err)
	}
}

// 补一个测试用的控制器构造器：真实装配路径与生产一致，只是显式传 out/echo。
func (rt *forumRuntime) newPageControllerForTest() *pageController {
	var out bytes.Buffer
	return &pageController{
		service: rt.Service, presence: rt.Presence, reg: command.Default(),
		screen: emptyScreen(), echo: newLineEcho(&out),
	}
}

func TestNodeCommandArgBoundaries(t *testing.T) {
	cases := map[string]error{
		"/node":                      command.ErrArgCount,
		"/node list extra":           command.ErrArgCount,
		"/node create /dev/x":        command.ErrArgCount,
		"/node create /dev/x n extra": command.ErrArgCount,
		"/node rename /dev/x":        command.ErrArgCount,
		"/node archive":              command.ErrArgCount,
		"/node archive /dev/x extra": command.ErrArgCount,
		"/node unknown":              command.ErrUnknownCommand,
	}
	for line, want := range cases {
		_, err := command.Default().Parse(line)
		if !errors.Is(err, want) {
			t.Errorf("Parse(%q) = %v，期望 %v", line, err, want)
		}
	}
}

func TestNodeCommandsDenyNonAdminWithFixedText(t *testing.T) {
	sessions := []struct {
		name string
		sess *fakePageSession
	}{
		{"访客且指纹不在名单", nodeAdminSession(0, "SHA256:"+strings.Repeat("B", 43), true)},
		{"白名单外指纹", nodeAdminSession(1, "SHA256:"+strings.Repeat("B", 43), false)},
	}
	commands := []string{
		"/node list",
		"/node create /dev/x 名字",
		"/node rename /dev/x 名字",
		"/node archive /dev/x",
	}
	for _, sc := range sessions {
		t.Run(sc.name, func(t *testing.T) {
			pc, ctx, _ := setupNodeAdminController(t, map[string]struct{}{cmdAdminFP: {}}, nil)
			for _, line := range commands {
				sc.sess.reset()
				runNodeCommand(t, pc, ctx, sc.sess, line)
				out := sc.sess.output()
				if !strings.Contains(out, "仅管理员可执行") {
					t.Errorf("%q 未给固定越权文案: %q", line, out)
				}
			}
		})
	}
}

// bootstrap：未绑定账号的访客只要指纹在白名单里，四条 /node 命令全通。
func TestNodeCommandsAllowWhitelistedGuest(t *testing.T) {
	pc, ctx, pools := setupNodeAdminController(t, map[string]struct{}{cmdAdminFP: {}}, nil)
	sess := nodeAdminSession(0, cmdAdminFP, true)

	runNodeCommand(t, pc, ctx, sess, "/node create /bootstrap/node 引导节点")
	if !strings.Contains(sess.output(), "版块已创建") {
		t.Fatalf("白名单访客创建被拒: %q", sess.output())
	}
	runNodeCommand(t, pc, ctx, sess, "/node rename /bootstrap/node 改名后")
	if !strings.Contains(sess.output(), "版块已改名") {
		t.Fatalf("白名单访客改名被拒: %q", sess.output())
	}
	runNodeCommand(t, pc, ctx, sess, "/node archive /bootstrap/node")
	if !strings.Contains(sess.output(), "版块已归档") {
		t.Fatalf("白名单访客归档被拒: %q", sess.output())
	}
	sess.reset()
	runNodeCommand(t, pc, ctx, sess, "/node list")
	if !strings.Contains(sess.output(), "/bootstrap/node") || !strings.Contains(sess.output(), "[archived]") {
		t.Fatalf("白名单访客列表缺归档节点: %q", sess.output())
	}
	var status string
	if err := pools.QueryRowContext(ctx, `SELECT status FROM nodes WHERE slug = '/bootstrap/node'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "archived" {
		t.Fatalf("访客治理未落库: %q", status)
	}
}

func TestNodeCommandsAdminHappyPathAndNoEcho(t *testing.T) {
	pc, ctx, pools := setupNodeAdminController(t, map[string]struct{}{cmdAdminFP: {}}, nil)
	sess := nodeAdminSession(1, cmdAdminFP, false)

	// 恶意 slug 含 CSI：失败文案固定，输出不含原始字节。
	sess.reset()
	malicious := "/node create \x1b[2J/x 名字"
	inv, err := command.Default().Parse(malicious)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.command(ctx, sess, inv); err != nil {
		t.Fatal(err)
	}
	if out := sess.output(); strings.Contains(out, "\x1b") || !strings.Contains(out, "slug 不合法") {
		t.Fatalf("非法 slug 不得回显原文: %q", out)
	}

	runNodeCommand(t, pc, ctx, sess, "/node create /new/x 新节点")
	if !strings.Contains(sess.output(), "版块已创建") {
		t.Fatalf("创建提示不符: %q", sess.output())
	}
	var slug, name, status string
	if err := pools.QueryRowContext(ctx, `SELECT slug, name, status FROM nodes WHERE slug = '/new/x'`).Scan(&slug, &name, &status); err != nil {
		t.Fatalf("读取创建结果: %v", err)
	}
	if slug != "/new/x" || name != "新节点" || status != "active" {
		t.Fatalf("创建结果错误: %q %q %q", slug, name, status)
	}

	runNodeCommand(t, pc, ctx, sess, "/node rename /new/x 改名后")
	if !strings.Contains(sess.output(), "版块已改名") {
		t.Fatalf("改名提示不符: %q", sess.output())
	}
	runNodeCommand(t, pc, ctx, sess, "/node archive /new/x")
	if !strings.Contains(sess.output(), "版块已归档") {
		t.Fatalf("归档提示不符: %q", sess.output())
	}
	sess.reset()
	runNodeCommand(t, pc, ctx, sess, "/node list")
	out := sess.output()
	if !strings.Contains(out, "/new/x") || !strings.Contains(out, "[archived]") || !strings.Contains(out, "/dev/ai") {
		t.Fatalf("管理列表应含归档节点与 seed: %q", out)
	}
}

func TestNodeAuditSubscriberIsWired(t *testing.T) {
	ctx := context.Background()
	pools, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "motd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlite.Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(func() {
		writer.Stop()
		_ = pools.Close()
	})
	audit := auth.NewAuditLogger(sqlite.NewAuthEventStore(writer))
	rt, err := newForumRuntime(pools, writer, nil, map[string]struct{}{cmdAdminFP: {}}, audit)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Bus.SubscriberCount(event.EventAdminAction); got != 1 {
		t.Fatalf("治理审计订阅者应恰好 1 个，实际 %d", got)
	}
}
