package forum

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
	"github.com/jeeinn/motd/internal/domain/event"
	"github.com/jeeinn/motd/internal/domain/user"
)

const nodeAdminTestFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func adminActor(userID int64, fp string, status user.Status) Actor {
	return Actor{UserID: userID, Status: status, Fingerprint: fp}
}

func newAdminService(whitelist map[string]struct{}, opts ...ServiceOption) (*Service, *fakeRepos) {
	repos := newFakeRepos()
	all := append([]ServiceOption{
		WithClock(func() time.Time { return testBase }),
		WithAdminWhitelist(whitelist),
	}, opts...)
	return NewService(repos, all...), repos
}

func TestActorCarriesAuthenticatedFingerprint(t *testing.T) {
	p := auth.Principal{Kind: auth.KindUser, UserID: 7, KeyFingerprint: nodeAdminTestFP}
	a := ActorFromPrincipal(p, user.StatusActive)
	if a.Fingerprint != nodeAdminTestFP {
		t.Fatalf("Actor 必须携带服务端会话指纹，实际 %+v", a)
	}
}

func TestNodeAdminDeniesWithoutWhitelistedFingerprint(t *testing.T) {
	ctx := context.Background()
	full := map[string]struct{}{nodeAdminTestFP: {}}
	other := map[string]struct{}{"SHA256:" + strings.Repeat("B", 43): {}}
	cases := []struct {
		name  string
		actor Actor
		list  map[string]struct{}
	}{
		{"空白名单", adminActor(1, nodeAdminTestFP, user.StatusActive), nil},
		{"名单外指纹", adminActor(1, nodeAdminTestFP, user.StatusActive), other},
		{"空指纹", adminActor(1, "", user.StatusActive), full},
		{"访客且指纹不在名单", Actor{Guest: true, Fingerprint: "SHA256:" + strings.Repeat("B", 43)}, full},
		{"冻结账号且指纹不在名单", adminActor(1, nodeAdminTestFP, user.StatusPending), other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repos := newAdminService(tc.list)
			if _, err := svc.CreateNode(ctx, tc.actor, "/new/node", "新节点"); !errors.Is(err, ErrAdminDenied) {
				t.Fatalf("create 应被拒，实际 %v", err)
			}
			if err := svc.RenameNode(ctx, tc.actor, "/new/node", "改名"); !errors.Is(err, ErrAdminDenied) {
				t.Fatalf("rename 应被拒，实际 %v", err)
			}
			if err := svc.ArchiveNode(ctx, tc.actor, "/dev/ai"); !errors.Is(err, ErrAdminDenied) {
				t.Fatalf("archive 应被拒，实际 %v", err)
			}
			if _, err := svc.ListAdminNodes(ctx, tc.actor); !errors.Is(err, ErrAdminDenied) {
				t.Fatalf("list 应被拒，实际 %v", err)
			}
			if len(repos.state.nodes) != 2 || repos.state.nodeStatus[1] != "active" {
				t.Fatalf("越权调用不得改变任何节点: %v / %v", repos.state.nodes, repos.state.nodeStatus)
			}
		})
	}
}

// bootstrap 语义（ADR-049）：白名单指纹本身就是治理凭证，
// 未绑定账号的访客（UserID=0）也必须能建/改/归档/列表，审计主体改用指纹。
func TestWhitelistedGuestCanGovernAndAuditByFingerprint(t *testing.T) {
	ctx := context.Background()
	sink := &recordingSink{}
	store := newFakeOutboxStore(0)
	outbox, err := NewOutbox(store, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, repos := newAdminService(map[string]struct{}{nodeAdminTestFP: {}},
		WithOutbox(outbox), WithEventSink(sink))
	repos.outboxStore = store
	guest := Actor{Guest: true, UserID: 0, Fingerprint: nodeAdminTestFP}

	id, err := svc.CreateNode(ctx, guest, "/bootstrap/node", "引导节点")
	if err != nil {
		t.Fatalf("白名单访客必须能建版块: %v", err)
	}
	if err := svc.RenameNode(ctx, guest, "/bootstrap/node", "改名"); err != nil {
		t.Fatalf("白名单访客必须能改名: %v", err)
	}
	if err := svc.ArchiveNode(ctx, guest, "/bootstrap/node"); err != nil {
		t.Fatalf("白名单访客必须能归档: %v", err)
	}
	rows, err := svc.ListAdminNodes(ctx, guest)
	if err != nil {
		t.Fatalf("白名单访客必须能列表: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == id && row.Status == "archived" {
			found = true
		}
	}
	if !found {
		t.Fatalf("访客治理结果未出现在管理列表: %+v", rows)
	}
	if len(repos.state.outboxEvents) != 3 {
		t.Fatalf("三个写动作各应有一条审计，实际 %d", len(repos.state.outboxEvents))
	}
	for i, ev := range repos.state.outboxEvents {
		if ev.Type != event.EventAdminAction || ev.ActorID != 0 ||
			ev.AdminFingerprint != nodeAdminTestFP || ev.AdminCommand == "" || ev.AdminResult == "" {
			t.Fatalf("第 %d 条审计必须以指纹标识访客主体: %+v", i, ev)
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("访客审计事件应通过领域事件校验: %v", err)
		}
	}
}

func TestWhitelistedFrozenUserStillCanGovern(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAdminService(map[string]struct{}{nodeAdminTestFP: {}})
	frozen := adminActor(7, nodeAdminTestFP, user.StatusPending)
	if _, err := svc.CreateNode(ctx, frozen, "/frozen/admin", "冻结也能管"); err != nil {
		t.Fatalf("治理权与账号状态解耦，冻结账号在白名单内仍可治理: %v", err)
	}
}

func TestCreateNodeValidatesSlugAndDetectsConflict(t *testing.T) {
	ctx := context.Background()
	svc, repos := newAdminService(map[string]struct{}{nodeAdminTestFP: {}})
	actor := adminActor(1, nodeAdminTestFP, user.StatusActive)

	if _, err := svc.CreateNode(ctx, actor, "/Bad/Slug", "名称"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("非法 slug 应被拒: %v", err)
	}
	if _, err := svc.CreateNode(ctx, actor, "/dev/ai", "重复"); !errors.Is(err, ErrNodeSlugExists) {
		t.Fatalf("slug 冲突应返回明确错误: %v", err)
	}
	if _, err := svc.CreateNode(ctx, actor, "/new/node", "\x1b[31m坏名字"); !errors.Is(err, ErrInvalidNodeName) {
		t.Fatalf("控制字符名称应被拒: %v", err)
	}
	id, err := svc.CreateNode(ctx, actor, "/new/node", "新节点")
	if err != nil {
		t.Fatalf("合法创建失败: %v", err)
	}
	if id <= 0 || repos.state.nodes[id].Slug != "/new/node" || repos.state.nodeStatus[id] != "active" {
		t.Fatalf("创建结果不符: id=%d node=%+v status=%q", id, repos.state.nodes[id], repos.state.nodeStatus[id])
	}
}

func TestRenameNodeKeepsSlugAndRejectsAllSeeds(t *testing.T) {
	ctx := context.Background()
	svc, repos := newAdminService(map[string]struct{}{nodeAdminTestFP: {}})
	actor := adminActor(1, nodeAdminTestFP, user.StatusActive)

	for _, slug := range []string{"/dev/ai", "/dev/rust", "/dev/jobs", "/etc/rant", "/bin/help"} {
		if err := svc.RenameNode(ctx, actor, slug, "新名字"); !errors.Is(err, ErrSeedNodeRename) {
			t.Errorf("seed %s 不可改名，实际 %v", slug, err)
		}
	}
	if err := svc.RenameNode(ctx, actor, "/no/such", "新名字"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的 slug 应返回固定错误，实际 %v", err)
	}
	id, err := svc.CreateNode(ctx, actor, "/new/node", "新节点")
	if err != nil {
		t.Fatalf("准备非 seed 节点失败: %v", err)
	}
	if err := svc.RenameNode(ctx, actor, "/new/node", "改名后"); err != nil {
		t.Fatalf("普通节点改名失败: %v", err)
	}
	n := repos.state.nodes[id]
	if n.Slug != "/new/node" || n.Name != "改名后" {
		t.Fatalf("改名只能改 name，slug/id 必须稳定: %+v", n)
	}
}

func TestArchiveNodeAllowsSeedAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, repos := newAdminService(map[string]struct{}{nodeAdminTestFP: {}})
	actor := adminActor(1, nodeAdminTestFP, user.StatusActive)

	if err := svc.ArchiveNode(ctx, actor, "/dev/ai"); err != nil {
		t.Fatalf("seed 允许归档: %v", err)
	}
	if err := svc.ArchiveNode(ctx, actor, "/dev/ai"); err != nil {
		t.Fatalf("重复归档必须幂等: %v", err)
	}
	if repos.state.nodeStatus[1] != "archived" || repos.state.nodes[1].Slug != "/dev/ai" {
		t.Fatalf("归档只改状态，slug 不变: %+v %v", repos.state.nodes[1], repos.state.nodeStatus[1])
	}
	rows, err := svc.ListAdminNodes(ctx, actor)
	if err != nil {
		t.Fatalf("管理列表: %v", err)
	}
	if len(rows) != 2 || rows[0].Status != "archived" || rows[1].Status != "active" {
		t.Fatalf("管理列表必须含归档态: %+v", rows)
	}
}

func TestAdminActionUsesOutboxInSameTransaction(t *testing.T) {
	ctx := context.Background()
	sink := &recordingSink{}
	store := newFakeOutboxStore(0)
	outbox, err := NewOutbox(store, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc, repos := newAdminService(map[string]struct{}{nodeAdminTestFP: {}},
		WithOutbox(outbox), WithEventSink(sink))
	repos.outboxStore = store // 让 fake 的 AppendOutbox 同步落到 fake 调度器
	actor := adminActor(1, nodeAdminTestFP, user.StatusActive)

	id, err := svc.CreateNode(ctx, actor, "/new/node", "新节点")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if len(repos.state.outboxEvents) != 1 {
		t.Fatalf("AppendOutbox 必须在写事务内追加一条，实际 %d 条", len(repos.state.outboxEvents))
	}
	ev := repos.state.outboxEvents[0]
	if ev.Type != event.EventAdminAction || ev.AdminCommand != "node create" ||
		ev.AdminTarget != "/new/node" || ev.AdminFingerprint != nodeAdminTestFP ||
		ev.AdminResult != "success" || ev.ActorID != 1 || ev.NodeID != id {
		t.Fatalf("审计事件字段不全: %+v", ev)
	}
	if len(sink.events) != 1 || sink.events[0].ID != ev.ID {
		t.Fatalf("提交后应由 outbox 投递同一事件: %+v", sink.events)
	}
}

func TestAdminErrorsUseFixedMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrAdminDenied, "仅管理员可执行"},
		{ErrNodeSlugExists, "slug 已存在"},
		{ErrSeedNodeRename, "种子版块不可改名"},
	}
	for _, tc := range cases {
		n := NoticeFromError(tc.err)
		if n.Message != tc.want {
			t.Errorf("NoticeFromError(%v).Message = %q，期望 %q", tc.err, n.Message, tc.want)
		}
	}
	// 写事务包装后也必须收敛为固定文案，不暴露内部错误链。
	wrapped := errors.New("写入事务失败（不可重试）: 用户提交的 <bad>")
	n := NoticeFromError(errors.Join(wrapped, ErrAdminDenied))
	if n.Message != "仅管理员可执行" || strings.Contains(n.Message, "<bad>") {
		t.Fatalf("包装错误不得回显用户原文: %+v", n)
	}
}
