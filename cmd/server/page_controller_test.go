package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/input"
	"github.com/jeeinn/motd/internal/application/presence"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
	"github.com/jeeinn/motd/internal/infrastructure/sqlite"
)

// setupMVPRuntime 用真实 SQLite + 真实写入队列 + 真实 Service 装配页面控制器，
// 并预置两个 active 账号。不用任何业务 fake，保证「页面 → 用例 → DB」不漂移。
func setupMVPRuntime(t *testing.T) (context.Context, *inputHandler, *forumRuntime, *sqlite.Pools, *sqlite.Writer) {
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
		for _, name := range []string{"alice", "bob"} {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
				name, now, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("准备测试账号: %v", err)
	}

	rt, err := newForumRuntime(pools, writer, nil)
	if err != nil {
		t.Fatalf("装配运行时: %v", err)
	}
	h := newInputHandler().withForum(rt.Service, sqlite.NewUserStatusReader(pools), rt.Presence)
	return ctx, h, rt, pools, writer
}

// fakePageSession 实现 pageSession：*sshd.Session 的受限替身，
// 不必起真实 SSH 就能回归整条业务链路（与 loginConn 同一做法）。
type fakePageSession struct {
	buf       bytes.Buffer
	principal auth.Principal
	states    []presence.State
}

func newFakePageSession(userID int64) *fakePageSession {
	p := auth.Guest("SHA256:fake")
	if userID > 0 {
		p = auth.Principal{Kind: auth.KindUser, UserID: userID, KeyFingerprint: "SHA256:fake"}
	}
	return &fakePageSession{principal: p}
}

func (s *fakePageSession) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *fakePageSession) ID() string                  { return "fake-session" }
func (s *fakePageSession) Principal() auth.Principal    { return s.principal }
func (s *fakePageSession) Window() (uint32, uint32, string) {
	return 80, 24, "xterm"
}
func (s *fakePageSession) SetState(st presence.State) error {
	s.states = append(s.states, st)
	return nil
}
func (s *fakePageSession) output() string { return s.buf.String() }
func (s *fakePageSession) reset()        { s.buf.Reset() }

func typeInto(t *testing.T, ctx context.Context, sess *fakePageSession, pc *pageController, text string) {
	t.Helper()
	for _, r := range text {
		if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyRune, Rune: r}); err != nil {
			t.Fatalf("输入 %q 失败: %v", r, err)
		}
	}
}

func pressEnter(t *testing.T, ctx context.Context, sess *fakePageSession, pc *pageController) {
	t.Helper()
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyEnter}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
}

// cmdOnlySGR 是 D5 在页面链路上的判据：输出里出现 ESC 就只能是纯 SGR。
func cmdOnlySGR(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		if i+2 >= len(s) || s[i+1] != '[' {
			t.Fatalf("输出含非 CSI 的 ESC 序列: %q", s)
		}
		j := i + 2
		for j < len(s) && s[j] != 'm' {
			if c := s[j]; (c < '0' || c > '9') && c != ';' {
				t.Fatalf("ESC 序列不是纯 SGR: %q", s[i:min(i+24, len(s))])
			}
			j++
		}
		if j >= len(s) {
			t.Fatalf("未闭合的 SGR: %q", s[i:])
		}
		i = j
	}
}

// D1：陌生连接首屏即可看到版块列表。
func TestPageControllerD1ShowsNodeList(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController()

	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatalf("首屏渲染失败: %v", err)
	}
	out := sess.output()
	for _, slug := range []string{"/dev/ai", "/dev/rust", "/dev/jobs", "/etc/rant", "/bin/help"} {
		if !strings.Contains(out, slug) {
			t.Errorf("首屏缺少版块 %q: %q", slug, out)
		}
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatalf("首屏应为版块列表，实际 %v", pc.screen.Kind)
	}
	cmdOnlySGR(t, out)
}

// P0-25：j/k 移动焦点，Enter/h/q 逐层回退且不丢页面状态。
func TestPageControllerNavigation(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController()
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}

	first := pc.screen.Selection.FocusedIndex
	if err := pc.key(ctx, sess, actionMoveDown); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Selection.FocusedIndex != first+1 {
		t.Fatalf("j 应下移一行，焦点 %d → %d", first, pc.screen.Selection.FocusedIndex)
	}
	if err := pc.key(ctx, sess, actionMoveUp); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Selection.FocusedIndex != first {
		t.Fatal("k 应回到原焦点")
	}

	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenThreads || pc.nodeID <= 0 {
		t.Fatalf("Enter 应进入主题列表并记住版块，kind=%v nodeID=%d", pc.screen.Kind, pc.nodeID)
	}
	if err := pc.key(ctx, sess, actionBack); err != nil {
		t.Fatal(err)
	}
	if pc.screen.Kind != viewmodel.ScreenNodes {
		t.Fatal("q/h 应回退到版块列表")
	}
}

// D3 + D4：已登录会话可直接写；另一个账号能看到主题、正文与回复。
func TestPageControllerD4TwoUsersSeeEachOthersPosts(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	a, b := newFakePageSession(1), newFakePageSession(2)
	pa, pb := h.newPageController(), h.newPageController()

	if err := pa.showNodes(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := pa.key(ctx, a, actionOpen); err != nil {
		t.Fatal(err)
	}
	if err := pa.command(ctx, a, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if pa.editor == nil || pa.editor.stage != stageTitle {
		t.Fatal("已登录用户在版块内 /new 应进入标题编辑态")
	}
	typeInto(t, ctx, a, pa, "MVP 首帖")
	pressEnter(t, ctx, a, pa)
	if pa.editor == nil || pa.editor.stage != stageBody {
		t.Fatal("提交标题后应进入正文编辑态")
	}
	typeInto(t, ctx, a, pa, "第一条正文")
	pressEnter(t, ctx, a, pa)
	if pa.editor != nil {
		t.Fatal("提交正文后应退出编辑态")
	}

	page, err := rt.Service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Title != "MVP 首帖" {
		t.Fatalf("发帖未落库: rows=%d err=%v", len(page.Rows), err)
	}
	threadID := page.Rows[0].ID

	// 另一个账号：打开同一主题，看到首帖，再回复。
	if err := pb.showDetail(ctx, b, threadID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.output(), "第一条正文") {
		t.Fatalf("第二个账号未看到首帖正文: %q", b.output())
	}
	b.reset()
	if err := pb.command(ctx, b, command.Invocation{Name: "reply"}); err != nil {
		t.Fatal(err)
	}
	if pb.editor == nil || pb.editor.stage != stageBody {
		t.Fatal("回复应直接进入正文编辑态")
	}
	typeInto(t, ctx, b, pb, "他人可见回复")
	pressEnter(t, ctx, b, pb)

	detail, err := rt.Service.GetThread(ctx, threadID)
	if err != nil || len(detail.Posts) != 2 || detail.Posts[1].BodyMD != "他人可见回复" {
		t.Fatalf("回复未落库: posts=%d err=%v", len(detail.Posts), err)
	}
	if !strings.Contains(b.output(), "他人可见回复") || !strings.Contains(b.output(), "第一条正文") {
		t.Fatalf("详情屏未同时展示首帖与回复: %q", b.output())
	}
	cmdOnlySGR(t, a.output())
	cmdOnlySGR(t, b.output())
}

// 访客只读：不进入编辑态、不产生任何写入。
func TestPageControllerGuestIsReadOnly(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController()
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := pc.key(ctx, sess, actionOpen); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.command(ctx, sess, command.Invocation{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if pc.editor != nil {
		t.Fatal("访客不能进入编辑态")
	}
	if !strings.Contains(sess.output(), "访客只读") {
		t.Fatalf("应给出只读提示: %q", sess.output())
	}
	page, err := rt.Service.ListThreads(ctx, forum.ListThreadsQuery{NodeID: 1, Sort: forum.SortLatest})
	if err != nil || len(page.Rows) != 0 {
		t.Fatalf("访客不得产生写入: rows=%d err=%v", len(page.Rows), err)
	}
}

// /open 的参数来自用户文本：只做严格数字解析，绝不回显原文。
func TestPageControllerOpenNeverEchoesArgument(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController()
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	bad := "3\x1b[2J\x1b]52;c;YWJj\x07"
	if err := pc.command(ctx, sess, command.Invocation{Name: "open", Args: []string{bad}}); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "pwned") || strings.Contains(out, bad) {
		t.Fatalf("回显泄露了用户提交的原文: %q", out)
	}
	if !strings.Contains(out, "必须是正整数") {
		t.Fatalf("应给出固定错误提示: %q", out)
	}
	cmdOnlySGR(t, out)
}

// D5 链路：恶意内容落库后，由另一个会话读出并渲染，输出不得影响其终端。
func TestPageControllerHostilePostIsSafeForOtherSession(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	hostile := "标题\x1b[2J\x1b]52;c;YWJj\x07\r覆盖\u202eRTL [x](file:///etc/passwd)"
	actor := forum.Actor{UserID: 1, Status: domainuser.StatusActive}
	threadID, err := rt.Service.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: 1, Title: "正常标题", Body: hostile,
	})
	if err != nil {
		t.Fatalf("恶意样本落库失败: %v", err)
	}

	b := newFakePageSession(2)
	pb := h.newPageController()
	if err := pb.showDetail(ctx, b, threadID); err != nil {
		t.Fatal(err)
	}
	out := b.output()
	cmdOnlySGR(t, out)
	for _, bad := range []string{"\x1b[2J", "\x1b]52", "\x1b]8", "\x1b]0;", "\r覆盖"} {
		if strings.Contains(out, bad) {
			t.Errorf("他人终端收到控制序列 %q", bad)
		}
	}
	if !strings.Contains(out, "␛") {
		t.Error("ESC 应被转成可见控制图示而非透传")
	}
}
