package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strconv"
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
	"github.com/jeeinn/motd/internal/presentation/terminal"
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

	rt, err := newForumRuntime(pools, writer, nil, nil, nil)
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
	width     uint32 // 终端列数：虚拟网格复现窄屏折行用，0 表示未显式设置
	height    uint32 // 终端行数：高度维钳制回归用（ADR-071），0 表示沿用默认 24
}

// newFakePageSession 保持历史默认 80 列——既有 80 列调用点行为逐字节不变。
func newFakePageSession(userID int64) *fakePageSession {
	return newFakePageSessionWithWidth(80, userID)
}

// newFakePageSessionWithWidth 造一个指定列数的会话替身，供
// 24/40/80/125/200 列的可配宽度回归使用（W1）。高度固定 24、term 固定 xterm，
// 与旧实现的非宽度字段完全一致。
func newFakePageSessionWithWidth(width uint32, userID int64) *fakePageSession {
	p := auth.Guest("SHA256:fake")
	if userID > 0 {
		p = auth.Principal{Kind: auth.KindUser, UserID: userID, KeyFingerprint: "SHA256:fake"}
	}
	return &fakePageSession{principal: p, width: width}
}

// newFakePageSessionWithSize 造一个指定宽高的会话替身，供高度维钳制回归
// （ADR-071）：height 传 0 沿用默认 24，width 传 0 沿用默认 80。
func newFakePageSessionWithSize(width, height uint32, userID int64) *fakePageSession {
	p := auth.Guest("SHA256:fake")
	if userID > 0 {
		p = auth.Principal{Kind: auth.KindUser, UserID: userID, KeyFingerprint: "SHA256:fake"}
	}
	return &fakePageSession{principal: p, width: width, height: height}
}

func (s *fakePageSession) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *fakePageSession) ID() string                  { return "fake-session" }
func (s *fakePageSession) Principal() auth.Principal   { return s.principal }
func (s *fakePageSession) Window() (uint32, uint32, string) {
	w, h := s.width, s.height
	if w == 0 {
		w = 80 // 直接结构体字面量构造的兜底，行为同旧实现
	}
	if h == 0 {
		h = 24
	}
	return w, h, "xterm"
}
func (s *fakePageSession) SetState(st presence.State) error {
	s.states = append(s.states, st)
	return nil
}
func (s *fakePageSession) output() string { return s.buf.String() }
func (s *fakePageSession) reset()         { s.buf.Reset() }

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
		t.Fatalf("按键失败: %v", err)
	}
}

// submitBody 用 Ctrl-D 提交正文（P1-M4 起正文 Enter 只换行，标题仍 Enter 确认）。
func submitBody(t *testing.T, ctx context.Context, sess *fakePageSession, pc *pageController) {
	t.Helper()
	if err := pc.editorKey(ctx, sess, input.KeyMsg{Kind: input.KeyCtrlD}); err != nil {
		t.Fatalf("提交正文失败: %v", err)
	}
}

// cmdOnlyWhitelistedANSI 是 D5 在页面链路上的判据（ADR-027，ADR-072 扩充）：
// 输出里的 ESC 只能是服务端白名单序列——SGR（m）、EL 清行（K）、CUU 上移（A）、
// **无参** CUP home（\x1b[H）、**无参** ED 2 清当前屏（\x1b[2J）。
// 带参 CUP（\x1b[2;3H）、ED 1J/3J/无参 [J、OSC 52、备用屏 ?1049h/l 一律不接受。
func cmdOnlyWhitelistedANSI(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		if i+2 >= len(s) || s[i+1] != '[' {
			t.Fatalf("输出含非 CSI 的 ESC 序列: %q", s)
		}
		j := i + 2
		for j < len(s) {
			c := s[j]
			if c == 'm' || c == 'K' || c == 'A' || c == 'H' || c == 'J' {
				break
			}
			if (c < '0' || c > '9') && c != ';' {
				t.Fatalf("ESC 序列不属于白名单: %q", s[i:min(i+24, len(s))])
			}
			j++
		}
		if j >= len(s) {
			t.Fatalf("未闭合的 ESC 序列: %q", s[i:])
		}
		// ADR-072：H 只允许无参（\x1b[H），J 只允许 ED 2（\x1b[2J）。
		// 带参 CUP、1J/3J/无参 [J 仍判违规。
		switch s[j] {
		case 'H':
			if j-i != 2 {
				t.Fatalf("只允许无参 CUP home \\x1b[H，实际 %q", s[i:j+1])
			}
		case 'J':
			if s[i:j+1] != "\x1b[2J" {
				t.Fatalf("只允许 ED 2 清屏 \\x1b[2J，实际 %q", s[i:j+1])
			}
		}
		i = j
	}
}

// cuuRE 只认 CUU（\x1b[<n>A）。SGR（\x1b[2m/\x1b[0m）与 EL（\x1b[K）
// 的终结符不是 A，天然不匹配；这是替代 Contains("\x1b[") 假通过判据的基础。
var cuuRE = regexp.MustCompile(`\x1b\[(\d+)A`)

// cuuParams 按出现顺序提取输出字节流里所有 CUU 的上移行数 n。
// 只认带数字的 \x1b[<n>A（服务端 safe.CursorUp 的产物）；畸形参数跳过，不 panic。
func cuuParams(out string) []int {
	matches := cuuRE.FindAllStringSubmatch(out, -1)
	params := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		params = append(params, n)
	}
	return params
}

// TestCuuParamsExtractsOnlyCUU 自证：全部 CUU 数值按序提取，SGR/EL 不误报。
// 旧判据 strings.Contains(out, "\x1b[") 在灰阶 hint（永远含 \x1b[2m）下恒真。
func TestCuuParamsExtractsOnlyCUU(t *testing.T) {
	got := cuuParams("前缀\x1b[2m灰阶\x1b[0m\x1b[K\x1b[2A甲\x1b[5A\x1b[K尾\x1b[10A")
	want := []int{2, 5, 10}
	if len(got) != len(want) {
		t.Fatalf("CUU 参数个数不符：got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("CUU 参数[%d] 不符：got %d want %d（全量 %v）", i, got[i], want[i], got)
		}
	}

	// 纯 SGR + EL：旧判据会假通过，cuuParams 必须为空。
	dimOnly := "\x1b[2m[提示] 仅灰阶与清行\x1b[0m\x1b[K"
	if n := len(cuuParams(dimOnly)); n != 0 {
		t.Fatalf("SGR/EL 不应被判为 CUU，实际提取到 %v", cuuParams(dimOnly))
	}
	if n := len(cuuParams("")); n != 0 {
		t.Fatalf("空串不应有 CUU，实际 %v", cuuParams(""))
	}
}

// TestFakePageSessionWidth 自证：默认 80 列不变，指定宽度可在
// 24/40/80/125/200 列下取到（W1 验收）。
func TestFakePageSessionWidth(t *testing.T) {
	w, h, term := newFakePageSession(1).Window()
	if w != 80 || h != 24 || term != "xterm" {
		t.Fatalf("默认宽度应保持 80/24/xterm，实际 %d/%d/%q", w, h, term)
	}
	for _, width := range []uint32{24, 40, 80, 125, 200} {
		gotW, gotH, gotTerm := newFakePageSessionWithWidth(width, 1).Window()
		if gotW != width || gotH != 24 || gotTerm != "xterm" {
			t.Errorf("宽度 %d 未生效：实际 %d/%d/%q", width, gotW, gotH, gotTerm)
		}
	}
	// 直接结构体字面量（无构造函数）也应回落旧默认，避免新字段引入零宽回归。
	gotW, gotH, _ := (&fakePageSession{}).Window()
	if gotW != 80 || gotH != 24 {
		t.Fatalf("零值会话应回落 80/24，实际 %d/%d", gotW, gotH)
	}
}

// D1：陌生连接首屏即可看到版块列表。
func TestPageControllerD1ShowsNodeList(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)

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
	cmdOnlyWhitelistedANSI(t, out)
}

// P0-25：j/k 移动焦点，Enter/h/q 逐层回退且不丢页面状态。
func TestPageControllerNavigation(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)
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

// ADR-072：每次渲染都以 H + 2J 整屏重绘（不再记忆上一屏行数）；
// 任何非屏幕输出之后，下一次渲染仍是整屏清屏回页面，而不是追加。
func TestNavigationRedrawsInPlace(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.output(), "\x1b[H\x1b[2J") {
		t.Fatalf("首屏应为整屏重绘（H + 2J 开头），实际 %q", sess.output()[:min(24, len(sess.output()))])
	}
	if strings.Contains(sess.output(), "\x1b[K") {
		t.Fatal("整屏重绘不再使用 EL 逐行补清")
	}
	cmdOnlyWhitelistedANSI(t, sess.output())
	sess.reset()

	for i := 0; i < 2; i++ {
		if err := pc.key(ctx, sess, actionMoveDown); err != nil {
			t.Fatal(err)
		}
		out := sess.output()
		if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
			t.Fatalf("导航第 %d 次应整屏重绘（H + 2J 开头），实际 %q", i+1, out[:min(24, len(out))])
		}
		if strings.Contains(out, "\x1b[K") {
			t.Fatalf("导航重绘不应再逐行 EL 补清: %q", out)
		}
		if regexp.MustCompile(`\x1b\[\d+A`).MatchString(out) {
			t.Fatalf("整屏重绘不需要 CUU 上移: %q", out)
		}
		cmdOnlyWhitelistedANSI(t, out)
		sess.reset()
	}

	// 插入一条固定提示后，下一次渲染仍整屏清屏回页面（提示留在 scrollback）。
	if err := pc.writeFixed(sess, "（一条提示）"); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.key(ctx, sess, actionMoveUp); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("提示之后仍应整屏重绘回页面，实际 %q", out[:min(24, len(out))])
	}
	cmdOnlyWhitelistedANSI(t, out)
}

// D3 + D4：已登录会话可直接写；另一个账号能看到主题、正文与回复。
func TestPageControllerD4TwoUsersSeeEachOthersPosts(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	a, b := newFakePageSession(1), newFakePageSession(2)
	pa, pb := h.newPageController(a), h.newPageController(b)

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
	submitBody(t, ctx, a, pa)
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
	submitBody(t, ctx, b, pb)

	detail, err := rt.Service.GetThread(ctx, threadID)
	if err != nil || len(detail.Posts) != 2 || detail.Posts[1].BodyMD != "他人可见回复" {
		t.Fatalf("回复未落库: posts=%d err=%v", len(detail.Posts), err)
	}
	if !strings.Contains(b.output(), "他人可见回复") || !strings.Contains(b.output(), "第一条正文") {
		t.Fatalf("详情屏未同时展示首帖与回复: %q", b.output())
	}
	cmdOnlyWhitelistedANSI(t, a.output())
	cmdOnlyWhitelistedANSI(t, b.output())
}

// 访客只读：不进入编辑态、不产生任何写入。
func TestPageControllerGuestIsReadOnly(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)
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
	pc := h.newPageController(sess)
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
	cmdOnlyWhitelistedANSI(t, out)
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
	pb := h.newPageController(b)
	if err := pb.showDetail(ctx, b, threadID); err != nil {
		t.Fatal(err)
	}
	out := b.output()
	cmdOnlyWhitelistedANSI(t, out)
	// ADR-072：帧首那一次 2J 是服务端整屏重绘的合法序列；用户正文里的 2J
	// 必须已被净化成可见图示，因此整帧 2J 只能出现一次且位于帧首。
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("详情屏应为整屏重绘，实际 %q", out[:min(24, len(out))])
	}
	if n := strings.Count(out, "\x1b[2J"); n != 1 {
		t.Fatalf("整帧应只含帧首一次清屏，实际 %d 次（用户注入的 2J 不得变成真序列）: %q", n, out)
	}
	for _, bad := range []string{"\x1b]52", "\x1b]8", "\x1b]0;", "\r覆盖"} {
		if strings.Contains(out, bad) {
			t.Errorf("他人终端收到控制序列 %q", bad)
		}
	}
	if !strings.Contains(out, "␛") {
		t.Error("ESC 应被转成可见控制图示而非透传")
	}
}

// TestBackRedrawsCleanFrame 回归 ADR-072：从详情返回列表是整屏重绘（H + 2J），
// 不需要 CUU、不需要逐行 EL，因此详情屏比列表长时不会残留空白带或光标落底。
// 帧末规则同时被验证：详情 n>=height 末行不写 CRLF；列表 n<height 末行补一个 CRLF。
func TestBackRedrawsCleanFrame(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	// 极小可视高度：详情屏必然超过它。
	const viewport = 5
	sess := newFakePageSessionWithSize(80, viewport, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	detail := sess.output()
	if !strings.HasPrefix(detail, "\x1b[H\x1b[2J") {
		t.Fatalf("详情屏应为整屏重绘，实际 %q", detail[:min(24, len(detail))])
	}
	// 前提：详情屏行数 >= 可视高度 → 末行不写 CRLF（防底行换行触发滚屏）。
	if strings.HasSuffix(detail, "\r\n") {
		t.Fatalf("前提：详情屏应超过可视高度 %d（末行无 CRLF），实际 %q", viewport, detail)
	}
	sess.reset()

	if err := pc.back(ctx, sess); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	cmdOnlyWhitelistedANSI(t, out)
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("back 应整屏重绘（H + 2J 开头），实际 %q", out[:min(24, len(out))])
	}
	if params := cuuParams(out); len(params) != 0 {
		t.Fatalf("整屏重绘不需要 CUU，实际 %v: %q", params, out)
	}
	if strings.Contains(out, "\x1b[K") {
		t.Fatalf("整屏重绘不再逐行 EL 补清: %q", out)
	}
	// 取最后一帧（back 双渲染，第二次覆盖第一次）单独检查“无空白带”与帧末 CRLF 规则。
	last := out[strings.LastIndex(out, "\x1b[H\x1b[2J"):]
	if strings.HasSuffix(last, "\r\n\r\n") {
		t.Fatalf("列表帧末不应多出空行（空白带）: %q", last)
	}
	want, err := terminal.RenderScreen(pc.screen, 80)
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Count(toCRLF(want), "\r\n") // 内容行数
	if n < viewport {
		// n<height：每行都带 CRLF，光标落到下一行输入位。
		if got := strings.Count(last, "\r\n"); got != n {
			t.Fatalf("列表帧应含 %d 个 CRLF（每行一个），实际 %d: %q", n, got, last)
		}
		if !strings.HasSuffix(last, "\r\n") {
			t.Fatalf("n<可视高度时应以 CRLF 收尾把光标带到输入位: %q", last)
		}
	} else {
		// n>=height：末行不写 CRLF，避免底行换行触发滚屏。
		if got := strings.Count(last, "\r\n"); got != n-1 {
			t.Fatalf("列表帧应含 %d 个 CRLF（末行不写），实际 %d: %q", n-1, got, last)
		}
		if strings.HasSuffix(last, "\r\n") {
			t.Fatalf("n>=可视高度时末行不得写 CRLF（会滚屏把首行顶出）: %q", last)
		}
	}
}

// TestPageRenderUsesCleanFrame 证明页面渲染走整屏重绘：帧以 H + 2J 开头，
// 且整帧 H / 2J 各恰好一次（不重复清屏、不夹带其他光标控制）。
func TestPageRenderUsesCleanFrame(t *testing.T) {
	ctx, h, _, _, _ := setupMVPRuntime(t)
	sess := newFakePageSession(0)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("页面渲染应以 ESC[H + ESC[2J 打头，实际 %q", out[:min(24, len(out))])
	}
	if n := strings.Count(out, "\x1b[H"); n != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[H，实际 %d", n)
	}
	if n := strings.Count(out, "\x1b[2J"); n != 1 {
		t.Fatalf("整帧应只出现 1 次 ESC[2J，实际 %d", n)
	}
	if strings.Contains(out, "\x1b[K") || len(cuuParams(out)) != 0 {
		t.Fatalf("整屏重绘不应出现 EL / CUU: %q", out)
	}
	cmdOnlyWhitelistedANSI(t, out)
}

// TestBackNoBlankBand 回归用户截图的问题：详情（更长）返回列表（更短）后，
// 输出行数恰为列表内容行数，不再有「补清到上一屏行数」留下的空白带。
func TestBackNoBlankBand(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	sess := newFakePageSessionWithSize(80, 24, 1)
	pc := h.newPageController(sess)
	threadID := seedFirstPost(t, ctx, pc, sess, rt)

	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	sess.reset()
	if err := pc.back(ctx, sess); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	cmdOnlyWhitelistedANSI(t, out)
	// 双渲染：第一帧是 showThreads，第二帧是 restoreFocus 后的重绘。
	last := out[strings.LastIndex(out, "\x1b[H\x1b[2J"):]
	if strings.Contains(last, "\x1b[K") {
		t.Fatalf("列表帧不应再逐行补清（旧实现的空白带来源）: %q", last)
	}
	// 列表 n<height → 每行以 CRLF 结尾、末行额外一个 CRLF 到输入位；
	// 若多补空行，会出现连续两个 CRLF 结尾。
	if strings.HasSuffix(last, "\r\n\r\n") {
		t.Fatalf("列表帧末多出空行（空白带）: %q", last)
	}
	// 内容行数 = CRLF 数，必须与当前列表屏的渲染行数一致（无补行）。
	want, err := terminal.RenderScreen(pc.screen, 80)
	if err != nil {
		t.Fatal(err)
	}
	wantCRLF := strings.Count(toCRLF(want), "\r\n")
	if got := strings.Count(last, "\r\n"); got != wantCRLF {
		t.Fatalf("列表帧行数应等于内容行数 %d，实际 %d（多出即空白带）: %q", wantCRLF, got, last)
	}
}

// TestLongContentRedrawNoScroll 验证 n >= height 时末行不写 CRLF：
// 在屏幕底行换行会触发滚屏，把首行顶出可视区（ADR-071 修过的问题）。
func TestLongContentRedrawNoScroll(t *testing.T) {
	ctx, h, rt, _, _ := setupMVPRuntime(t)
	const viewport = 5
	sess := newFakePageSessionWithSize(80, viewport, 1)
	pc := h.newPageController(sess)
	if err := pc.showNodes(ctx, sess); err != nil {
		t.Fatal(err)
	}
	nodeID := pc.screen.Rows[0].ID
	// 多行正文 → 详情屏行数远超 5 行可视高度。
	body := strings.Repeat("长正文的一行\r\n", 10)
	actor := forum.Actor{UserID: 1, Status: domainuser.StatusActive}
	threadID, err := rt.Service.CreateThread(ctx, actor, forum.CreateThreadInput{
		NodeID: nodeID, Title: "长主题", Body: body,
	})
	if err != nil {
		t.Fatalf("准备长主题失败: %v", err)
	}
	sess.reset()
	if err := pc.showDetail(ctx, sess, threadID); err != nil {
		t.Fatal(err)
	}
	out := sess.output()
	cmdOnlyWhitelistedANSI(t, out)
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Fatalf("详情屏应为整屏重绘，实际 %q", out[:min(24, len(out))])
	}
	if strings.HasSuffix(out, "\r\n") {
		t.Fatalf("n>=height 时末行不得写 CRLF（否则底行换行会滚屏）: %q", out[len(out)-16:])
	}
}
