package main

// ADR-065：/whoami 与 /who 的组装与边界回归。
// 纯函数（计数 / 排序 / 文案）逐字节断言；处理层覆盖访客、端口缺失、DB 失败、
// 参数非法、页界与负向注入（用户名含 ESC / 双向控制符不得裸写）。

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/presence"
	appuser "github.com/jeeinn/motd/internal/application/user"
	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// fakeProfileStore 同时满足 StatusReader 与 ProfileReader（h.users 的类型断言链）。
type fakeProfileStore struct {
	profiles map[int64]appuser.ProfileView
	err      error
}

func (f fakeProfileStore) Status(context.Context, int64) (domainuser.Status, error) {
	return domainuser.StatusActive, nil
}

func (f fakeProfileStore) ProfilesByIDs(_ context.Context, ids []int64) (map[int64]appuser.ProfileView, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[int64]appuser.ProfileView, len(ids))
	for _, id := range ids {
		if v, ok := f.profiles[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func TestWhoCountsFromSingleSnapshot(t *testing.T) {
	now := time.Now()
	snap := []presence.Session{
		{UserID: 7, State: presence.StateBrowsing, ConnectedAt: now, LastSeenAt: now},
		{UserID: 8, State: presence.StateIdle, ConnectedAt: now, LastSeenAt: now},
		{UserID: 0, State: presence.StateAuthenticating, ConnectedAt: now, LastSeenAt: now},
		{UserID: 0, State: presence.StateBrowsing, ConnectedAt: now, LastSeenAt: now},
	}
	online, active, guests := whoCounts(snap, now, presence.DefaultHeartbeatTimeout)
	// 第 4 条虽是访客连接，但 State=Browsing 仍计入 Active（口径同 Active()）。
	if online != 4 || active != 2 || guests != 2 {
		t.Fatalf("计数应为 online=4 active=2 guests=2，实际 %d/%d/%d", online, active, guests)
	}
}

func TestWhoCountsHeartbeatWindowAlignsWithActive(t *testing.T) {
	// 活跃态但心跳超时（LastSeenAt 过期）不计入活跃，与 presence.Active() 同口径。
	now := time.Now()
	stale := now.Add(-presence.DefaultHeartbeatTimeout - time.Second)
	snap := []presence.Session{
		{UserID: 7, State: presence.StateBrowsing, ConnectedAt: stale, LastSeenAt: stale}, // 超时
		{UserID: 8, State: presence.StateBrowsing, ConnectedAt: now, LastSeenAt: now},     // 窗口内
		{UserID: 9, State: presence.StateIdle, ConnectedAt: now, LastSeenAt: now},         // 空闲
	}
	_, active, _ := whoCounts(snap, now, presence.DefaultHeartbeatTimeout)
	if active != 1 {
		t.Fatalf("心跳窗口内活跃应为 1，实际 %d", active)
	}
}

func TestWhoEntriesActiveFirstAndLastSeenDesc(t *testing.T) {
	now := time.Now()
	older := now.Add(-10 * time.Minute)
	snap := []presence.Session{
		{UserID: 2, State: presence.StateIdle, ConnectedAt: older, LastSeenAt: older},
		{UserID: 1, State: presence.StateBrowsing, ConnectedAt: now, LastSeenAt: now},
		{UserID: 0, State: presence.StateAuthenticating, ConnectedAt: now, LastSeenAt: now}, // 访客不入名单
		{UserID: 3, State: presence.StateComposing, ConnectedAt: older, LastSeenAt: now.Add(-time.Minute)},
	}
	profiles := map[int64]appuser.ProfileView{
		1: {UserID: 1, Username: "alice"},
		2: {UserID: 2, Username: "bob"},
		3: {UserID: 3, Username: "carol"},
	}
	entries := whoEntries(snap, profiles, now)
	if len(entries) != 3 {
		t.Fatalf("应组装 3 行（访客排除），实际 %d", len(entries))
	}
	// 活跃态（1 浏览、3 编辑）在前，按 LastSeenAt 倒序；空闲的 2 垫底。
	if entries[0].userID != 1 || entries[1].userID != 3 || entries[2].userID != 2 {
		t.Fatalf("排序应为 1 → 3 → 2，实际 %d/%d/%d", entries[0].userID, entries[1].userID, entries[2].userID)
	}
}

func TestWhoEntriesFallbackToHashWhenProfileMissing(t *testing.T) {
	now := time.Now()
	snap := []presence.Session{
		{UserID: 9, State: presence.StateBrowsing, ConnectedAt: now, LastSeenAt: now},
	}
	entries := whoEntries(snap, map[int64]appuser.ProfileView{}, now)
	if len(entries) != 1 || entries[0].username != "" {
		t.Fatalf("无投影时 username 应为空（渲染 #ID），实际 %+v", entries[0])
	}
}

func TestWhoOnlineText(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "在线 <1 分钟"},
		{59, "在线 <1 分钟"},
		{60, "在线 1 分钟"},
		{125, "在线 2 分钟"},
		{3600, "在线 60 分钟"},
	}
	for _, c := range cases {
		if got := whoOnlineText(c.sec); got != c.want {
			t.Errorf("sec=%d 得到 %q，期望 %q", c.sec, got, c.want)
		}
	}
}

func TestWhoStateLabel(t *testing.T) {
	cases := map[presence.State]string{
		presence.StateBrowsing:       "浏览中",
		presence.StateComposing:      "编辑中",
		presence.StateIdle:           "空闲",
		presence.StateAuthenticating: "认证中",
	}
	for state, want := range cases {
		if got := whoStateLabel(state); got != want {
			t.Errorf("状态 %d 得到 %q，期望 %q", state, got, want)
		}
	}
}

func TestRunWhoamiGuestFixedHint(t *testing.T) {
	h := newInputHandler()
	var buf bytes.Buffer
	h.runWhoami(context.Background(), &buf, auth.Guest("fp"))
	if !strings.Contains(buf.String(), "请先 /login github 登录后查看我的信息") {
		t.Fatalf("访客应给固定提示，实际 %q", buf.String())
	}
}

func TestRunWhoamiReaderMissing(t *testing.T) {
	h := newInputHandler() // h.users 为 nil，无 ProfileReader
	var buf bytes.Buffer
	h.runWhoami(context.Background(), &buf, auth.Principal{Kind: auth.KindUser, UserID: 7})
	if !strings.Contains(buf.String(), "个人信息查询尚未就绪") {
		t.Fatalf("端口缺失应给固定提示，实际 %q", buf.String())
	}
}

func TestRunWhoamiDBFailure(t *testing.T) {
	h := newInputHandler()
	h.users = fakeProfileStore{err: context.DeadlineExceeded}
	var buf bytes.Buffer
	h.runWhoami(context.Background(), &buf, auth.Principal{Kind: auth.KindUser, UserID: 7})
	if !strings.Contains(buf.String(), "个人信息查询失败，请稍后重试") {
		t.Fatalf("DB 失败应给固定文案，实际 %q", buf.String())
	}
}

func TestRunWhoamiSuccessSegmented(t *testing.T) {
	h := newInputHandler()
	h.users = fakeProfileStore{profiles: map[int64]appuser.ProfileView{
		7: {UserID: 7, Username: "alice", Karma: 12, CreatedAt: 1763188400, Status: domainuser.StatusActive},
	}}
	var buf bytes.Buffer
	h.runWhoami(context.Background(), &buf, auth.Principal{Kind: auth.KindUser, UserID: 7})
	out := buf.String()
	for _, want := range []string{"我的信息", "alice", "ID 7", "注册：", "声望 12", "L2"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：%q", want, out)
		}
	}
}

func TestRunWhoamiUsernameInjectionSanitized(t *testing.T) {
	h := newInputHandler()
	// 用户名带 ESC 清屏序列、回车、双向控制符（RLO U+202E / LRE U+202A），
	// 必须全部被净化，不得裸写进终端或篡改显示顺序。
	h.users = fakeProfileStore{profiles: map[int64]appuser.ProfileView{
		7: {UserID: 7, Username: "ali\x1b[2Jce\revil\u202e\u202a", Karma: 0, CreatedAt: 1763188400},
	}}
	var buf bytes.Buffer
	h.runWhoami(context.Background(), &buf, auth.Principal{Kind: auth.KindUser, UserID: 7})
	out := buf.String()
	// ESC 被净化为可见图示 ␛，原始 0x1b 字节绝不能出现在输出；
	// 残留的 "[2J" 字面量是退化文本，不构成控制序列，允许存在。
	if strings.ContainsRune(out, 0x1b) {
		t.Error("ESC 字节不得出现在输出")
	}
	// 双向控制符被 RenderUser 的 SanitizeText 剥离；固定文案本身含 \r\n，
	// 故不断言 \r（其规范化由 safe 包单测覆盖），这里只验用户数据段。
	if strings.ContainsAny(out, "\u202e\u202a") {
		t.Error("双向控制符不得出现在输出")
	}
}

func TestRunWhoUsernameInjectionSanitized(t *testing.T) {
	// /who 名单里的 @username 同属 DB 用户数据，必须与 whoami 一样走净化链。
	reg := presence.New()
	if err := reg.Connect("s-evil", presence.KindSSH, 7); err != nil {
		t.Fatalf("连接: %v", err)
	}
	_ = reg.SetState("s-evil", presence.StateBrowsing)
	h := newInputHandler()
	h.presence = reg
	h.users = fakeProfileStore{profiles: map[int64]appuser.ProfileView{
		7: {UserID: 7, Username: "e\x1b[2Jvil\u202e\r"},
	}}
	var buf bytes.Buffer
	h.runWho(context.Background(), &buf, command.Invocation{Name: "who"})
	out := buf.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Error("/who 名单中 ESC 字节不得出现在输出")
	}
	if strings.ContainsAny(out, "\u202e\u202a") {
		t.Error("/who 名单中双向控制符不得出现在输出")
	}
}

func TestRunWhoInvalidPageArg(t *testing.T) {
	h := newInputHandler()
	for _, arg := range []string{"abc", "0", "-3", "@alice"} {
		var buf bytes.Buffer
		h.runWho(context.Background(), &buf, command.Invocation{Name: "who", Args: []string{arg}})
		if !strings.Contains(buf.String(), "用法：/who [页]") {
			t.Errorf("参数 %q 应给用法提示，实际 %q", arg, buf.String())
		}
	}
}

func TestRunWhoEmptySnapshot(t *testing.T) {
	h := newInputHandler()
	h.presence = presence.New()
	var buf bytes.Buffer
	h.runWho(context.Background(), &buf, command.Invocation{Name: "who"})
	out := buf.String()
	if !strings.Contains(out, "在线 0 · 活跃 0 · 访客 0") {
		t.Fatalf("汇总行不符：%q", out)
	}
	if !strings.Contains(out, "当前没有在线人员") {
		t.Fatalf("空名单应有固定空态：%q", out)
	}
}

func TestRunWhoHappyPath(t *testing.T) {
	reg := presence.New()
	mustConnect := func(id string, uid int64) {
		if err := reg.Connect(id, presence.KindSSH, uid); err != nil {
			t.Fatalf("连接 %s: %v", id, err)
		}
	}
	mustConnect("s-guest", 0)
	mustConnect("s-alice", 0)
	_ = reg.AttachUser("s-alice", 7)
	_ = reg.SetState("s-alice", presence.StateBrowsing)
	mustConnect("s-bob", 0)
	_ = reg.AttachUser("s-bob", 8)
	_ = reg.SetState("s-bob", presence.StateIdle)

	h := newInputHandler()
	h.presence = reg
	h.users = fakeProfileStore{profiles: map[int64]appuser.ProfileView{
		7: {UserID: 7, Username: "alice"},
		8: {UserID: 8, Username: "bob"},
	}}
	var buf bytes.Buffer
	h.runWho(context.Background(), &buf, command.Invocation{Name: "who"})
	out := buf.String()
	if !strings.Contains(out, "在线 3 · 活跃 1 · 访客 1") {
		t.Fatalf("汇总行不符：%q", out)
	}
	// 活跃的 alice 在前；bob 在后；访客不出现；#ID 回退不触发。
	if strings.Index(out, "alice") > strings.Index(out, "bob") {
		t.Fatalf("活跃用户应排在空闲用户之前：%q", out)
	}
	if strings.Contains(out, "访客连接") || strings.Contains(out, "s-guest") {
		t.Error("访客连接不得列名")
	}
}

func TestRunWhoPageOutOfRange(t *testing.T) {
	reg := presence.New()
	_ = reg.Connect("s-1", presence.KindSSH, 7)
	_ = reg.AttachUser("s-1", 7)
	_ = reg.SetState("s-1", presence.StateBrowsing)
	h := newInputHandler()
	h.presence = reg
	var buf bytes.Buffer
	h.runWho(context.Background(), &buf, command.Invocation{Name: "who", Args: []string{"9"}})
	if !strings.Contains(buf.String(), "页码超出范围（共 1 页）") {
		t.Fatalf("超界应给固定提示，实际 %q", buf.String())
	}
}

func TestRunWhoPaginatesLargeRoster(t *testing.T) {
	reg := presence.New()
	for i := 1; i <= whoPageSize+1; i++ {
		id := "s-" + strconv.Itoa(i)
		if err := reg.Connect(id, presence.KindSSH, int64(i)); err != nil {
			t.Fatalf("连接 %s: %v", id, err)
		}
		_ = reg.SetState(id, presence.StateIdle)
	}
	profiles := map[int64]appuser.ProfileView{}
	for i := 1; i <= whoPageSize+1; i++ {
		profiles[int64(i)] = appuser.ProfileView{UserID: int64(i), Username: "u" + strconv.Itoa(i)}
	}
	h := newInputHandler()
	h.presence = reg
	h.users = fakeProfileStore{profiles: profiles}

	var page1 bytes.Buffer
	h.runWho(context.Background(), &page1, command.Invocation{Name: "who", Args: []string{"1"}})
	var page2 bytes.Buffer
	h.runWho(context.Background(), &page2, command.Invocation{Name: "who", Args: []string{"2"}})

	if !strings.Contains(page1.String(), "第 1/2 页") {
		t.Errorf("第 1 页页脚不符：%q", page1.String())
	}
	if !strings.Contains(page2.String(), "第 2/2 页") {
		t.Errorf("第 2 页页脚不符：%q", page2.String())
	}
	// 不重不漏：两页用户集合的并集恰好是全部 16 人，且交集为空。
	// 不断言「某具体用户在某页」——排序依赖 LastSeenAt 的时钟精度，
	// Windows 粗粒度（时间戳相同→按 ID 序）与 Linux 纳秒（严格递增→倒序）
	// 会给出不同顺序，这样的断言在 CI 上是 flaky 的。
	inPage := func(out, name string) bool {
		return strings.Contains(out, "@"+name+" ")
	}
	seen := map[string]int{}
	for i := 1; i <= whoPageSize+1; i++ {
		name := "u" + strconv.Itoa(i)
		p1, p2 := inPage(page1.String(), name), inPage(page2.String(), name)
		if p1 && p2 {
			t.Errorf("用户 %s 同时出现在两页", name)
		}
		if !p1 && !p2 {
			t.Errorf("用户 %s 在两页中都缺失", name)
		}
		if p1 || p2 {
			seen[name]++
		}
	}
	if len(seen) != whoPageSize+1 {
		t.Errorf("两页应覆盖 %d 名用户，实际 %d", whoPageSize+1, len(seen))
	}
}
