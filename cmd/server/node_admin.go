package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jeeinn/motd/internal/application/command"
	"github.com/jeeinn/motd/internal/application/forum"
)

// P2-M1 节点治理命令面。四条子命令都只做「取会话身份 → 调用用例 → 固定文案回显」，
// 业务判定、slug 校验、审计全部在 application/forum 内；这里不出现任何权限谓词，
// 也不把命令参数当指纹使用（archtest 静态守卫）。

const (
	adminDeniedMessage   = "仅管理员可执行"
	nodeNotFoundMessage  = "版块不存在"
	nodeCreatedMessage   = "版块已创建"
	nodeRenamedMessage   = "版块已改名"
	nodeArchivedMessage  = "版块已归档"
	nodeGenericFail      = "操作失败，请稍后重试"
	invalidSlugMessage   = "slug 不合法：仅允许小写字母、数字与 /，1–2 段"
	invalidNodeNameMsg   = "版块名称不合法：不能空白、超过 64 字符或含控制字符"
	seedRenameDeniedMsg  = "种子版块不可改名"
	slugAlreadyExistsMsg = "slug 已存在"
)

func (p *pageController) nodeCommand(ctx context.Context, sess pageSession, inv command.Invocation) error {
	switch inv.Sub {
	case "list":
		return p.nodeList(ctx, sess)
	case "create":
		return p.nodeCreate(ctx, sess, inv.Args[0], inv.Args[1])
	case "rename":
		return p.nodeRename(ctx, sess, inv.Args[0], inv.Args[1])
	case "archive":
		return p.nodeArchive(ctx, sess, inv.Args[0])
	default:
		return p.writeFixed(sess, "该命令暂未接入（按 ? 查看全部命令）")
	}
}

// nodeActor 只从服务端 Principal 构造身份快照，不经过 p.actor：
// p.actor 对访客返回 ErrReadOnly，而白名单内的访客正是 bootstrap 管理员。
// 账号状态对治理权无影响，这里不查 users，也不做权限判定。
func (p *pageController) nodeActor(ctx context.Context, sess pageSession) (forum.Actor, error) {
	principal := sess.Principal()
	return forum.Actor{
		UserID:      principal.UserID,
		Guest:       !principal.IsUser(),
		Fingerprint: principal.KeyFingerprint,
	}, nil
}

// fixedAdminError 只映射到服务端固定文案。用户提交的 slug / name 一律不回显，
// 内部错误链也不经这里泄漏给终端。
func fixedAdminError(err error) string {
	switch {
	case errors.Is(err, forum.ErrAdminDenied):
		return adminDeniedMessage
	case errors.Is(err, forum.ErrNotFound):
		return nodeNotFoundMessage
	case errors.Is(err, forum.ErrInvalidSlug):
		return invalidSlugMessage
	case errors.Is(err, forum.ErrInvalidNodeName):
		return invalidNodeNameMsg
	case errors.Is(err, forum.ErrNodeSlugExists):
		return slugAlreadyExistsMsg
	case errors.Is(err, forum.ErrSeedNodeRename):
		return seedRenameDeniedMsg
	default:
		return nodeGenericFail
	}
}

// nodeList 用管理员专用只读变体：列出 active + archived。
// 公开列表仍只给 active（ADR-048），管理员需要看到归档版块才能判断重建与否。
func (p *pageController) nodeList(ctx context.Context, sess pageSession) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}
	rows, err := p.service.ListAdminNodes(ctx, actor)
	if err != nil {
		return p.writeFixed(sess, fixedAdminError(err))
	}
	var b strings.Builder
	b.WriteString("版块列表（含归档）：\n")
	for _, n := range rows {
		_, _ = fmt.Fprintf(&b, "#%d  %s  %s  [%s]\n", n.ID, n.Slug, n.Name, n.Status)
	}
	return p.writeFixed(sess, strings.TrimRight(b.String(), "\n"))
}

func (p *pageController) nodeCreate(ctx context.Context, sess pageSession, slug, name string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}
	if _, err := p.service.CreateNode(ctx, actor, slug, name); err != nil {
		return p.writeFixed(sess, fixedAdminError(err))
	}
	return p.writeFixed(sess, nodeCreatedMessage)
}

func (p *pageController) nodeRename(ctx context.Context, sess pageSession, slug, name string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}
	if err := p.service.RenameNode(ctx, actor, slug, name); err != nil {
		return p.writeFixed(sess, fixedAdminError(err))
	}
	return p.writeFixed(sess, nodeRenamedMessage)
}

func (p *pageController) nodeArchive(ctx context.Context, sess pageSession, slug string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}
	if err := p.service.ArchiveNode(ctx, actor, slug); err != nil {
		return p.writeFixed(sess, fixedAdminError(err))
	}
	return p.writeFixed(sess, nodeArchivedMessage)
}
