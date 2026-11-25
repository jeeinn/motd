package forum

import (
	"context"
	"fmt"
	"time"

	"github.com/jeeinn/motd/internal/application/admin"
	"github.com/jeeinn/motd/internal/domain/event"
)

// 0002_seed_nodes.sql 的 5 个种子版块。ADR-048 + 用户 2026-11-20 修订：
// 可归档、不可改名；slug 名额仍占用，故归档后也不能新建同名 slug。
var seedNodeSlugs = map[string]struct{}{
	"/dev/ai": {}, "/dev/rust": {}, "/dev/jobs": {}, "/etc/rant": {}, "/bin/help": {},
}

func isSeedNodeSlug(slug string) bool {
	_, ok := seedNodeSlugs[slug]
	return ok
}

// WithAdminWhitelist 注入管理员公钥指纹白名单。
// 复制成不可变快照存进原子容器，避免装配后外部 map 被改动导致权限漂移，
// 也让运行期 SIGHUP 重载能原子换入新快照（ADR-079）。
func WithAdminWhitelist(whitelist map[string]struct{}) ServiceOption {
	return func(s *Service) {
		snap := make(map[string]struct{}, len(whitelist))
		for fp := range whitelist {
			snap[fp] = struct{}{}
		}
		s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: snap})
	}
}

// requireAdmin 是所有节点治理命令唯一的准入判定：
// 只看服务端会话指纹是否在白名单里。治理权与内容账号解耦（ADR-049）——
// 未绑定公钥的访客也能 bootstrap 治理；用户账号冻结与否不影响治理权，
// 白名单外一律 ErrAdminDenied，不给探测者任何差异化回显。
//
// 这里刻意不调 checkWrite：它会把访客（UserID=0）挡在门外，正是要修的鸡生蛋。
func (s *Service) requireAdmin(actor Actor) error {
	// 从原子快照取当前白名单：热重载换入的新快照立即生效，
	// 且读到的 map 永不修改，无数据竞争。CanAdmin 单点不变（archtest 守卫）。
	if !admin.CanAdmin(actor.Fingerprint, s.currentAdminWhitelist()) {
		return ErrAdminDenied
	}
	return nil
}

// adminActionEvent 构造治理审计事件。command 只取固定字面量，target 是版块 slug，
// result 只记成功：失败用例不产生业务写，也就没有可审计的已提交动作。
// ActorID 对访客管理员为 0；「谁」由 AdminFingerprint 承担（DomainEvent.Validate 放行）。
func adminActionEvent(command, target string, nodeID int64, actor Actor, now time.Time) event.DomainEvent {
	return event.DomainEvent{
		ID:               fmt.Sprintf("admin:%s:%d:%d:%s", command, nodeID, now.UnixNano(), actor.Fingerprint),
		Type:             event.EventAdminAction,
		OccurredAt:       now,
		ActorID:          actor.UserID,
		NodeID:           nodeID,
		AdminFingerprint: actor.Fingerprint,
		AdminCommand:     command,
		AdminTarget:      target,
		AdminResult:      "success",
	}
}

// ListAdminNodes 是管理员专用只读变体：列出 active + archived。
// 公开列表仍走 ListNodes（只 active），这里不复用它是刻意的：
// 两类视图的可见性边界不能靠调用方自觉。
func (s *Service) ListAdminNodes(ctx context.Context, actor Actor) ([]NodeAdminView, error) {
	if err := s.requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.repos.ListAllNodes(ctx)
}

// CreateNode 创建版块。slug 先过 ADR-048 校验，再在写事务内靠 UNIQUE 判重。
func (s *Service) CreateNode(ctx context.Context, actor Actor, slug, name string) (int64, error) {
	if err := s.requireAdmin(actor); err != nil {
		return 0, err
	}
	if err := ValidateSlug(slug); err != nil {
		return 0, err
	}
	if err := validateNodeName(name); err != nil {
		return 0, err
	}

	now := s.now().UTC()
	var id int64
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		nodeID, err := tx.CreateNode(ctx, NewNode{Slug: slug, Name: name})
		if err != nil {
			return err
		}
		id = nodeID
		ev = adminActionEvent("node create", slug, id, actor, now)
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.afterCommit(ctx, ev)
	return id, nil
}

// RenameNode 只改 name。slug 是稳定标识，任何路径都不改它；
// 5 个 seed 的 slug 精确匹配后直接拒绝，不给「先归档再改名」留缝。
func (s *Service) RenameNode(ctx context.Context, actor Actor, slug, newName string) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}
	if err := validateNodeName(newName); err != nil {
		return err
	}
	if isSeedNodeSlug(slug) {
		return ErrSeedNodeRename
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		id, err := tx.NodeIDBySlug(ctx, slug)
		if err != nil {
			return ErrNotFound // 固定文案：不回显用户提交的 slug
		}
		if err := tx.RenameNode(ctx, id, newName); err != nil {
			return err
		}
		ev = adminActionEvent("node rename", slug, id, actor, now)
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}

// ArchiveNode 只置 nodes.status='archived'，threads 一行不动。
// 归档幂等：重复归档返回成功；每次成功调用各记一条审计（事件 ID 含纳秒时间戳，
// 不会被领域总线的幂等去重误吞）。
func (s *Service) ArchiveNode(ctx context.Context, actor Actor, slug string) error {
	if err := s.requireAdmin(actor); err != nil {
		return err
	}

	now := s.now().UTC()
	var ev event.DomainEvent
	err := s.repos.Write(ctx, func(tx WriteTx) error {
		id, err := tx.NodeIDBySlug(ctx, slug)
		if err != nil {
			return ErrNotFound
		}
		if err := tx.ArchiveNode(ctx, id); err != nil {
			return err
		}
		ev = adminActionEvent("node archive", slug, id, actor, now)
		if s.outbox != nil {
			return tx.AppendOutbox(ctx, ev)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.afterCommit(ctx, ev)
	return nil
}
