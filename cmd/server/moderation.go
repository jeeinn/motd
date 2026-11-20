package main

// P2-M1b 内容治理命令面 + ADR-053 的 /unhide（隐藏可逆）。
// 只做「参数解析 → 复用 M1 的身份映射 → 调用用例 → 固定文案」，
// 不出现任何权限谓词、不查 users 状态、不回显用户原文（archtest 静态守卫）。
//
// 采用与 /delete 对称的口径（方案 a）：
//   /hide        → 隐藏当前打开的主题；/unhide 恢复它；
//   /hide <id>   → 隐藏 ID 为 <id> 的回复；/unhide <id> 恢复它（按当前打开主题导航）。
//
// 管理员应能处理整条垃圾主题，而不只是单条回复；且 /delete 已是作者自删的
// 单一职责点，不能用它承载治理语义（hidden 与 deleted 审计命令名也不同）。

import (
	"context"
	"strconv"

	"github.com/jeeinn/motd/internal/application/viewmodel"
)

const (
	hideNeedDetailMessage = "请先打开一个主题再隐藏"
	hideBadIDMessage      = "ID 必须是正整数"
	hideThreadMessage     = "已隐藏主题"
	hidePostMessage       = "已隐藏回复"

	unhideNeedDetailMessage = "请先打开一个主题再恢复"
	unhideBadIDMessage      = "ID 必须是正整数"
	unhideThreadMessage     = "已恢复主题"
	unhidePostMessage       = "已恢复回复"

	unthreadBadIDMessage  = "主题 ID 必须是正整数"
	unthreadThreadMessage = "已恢复主题"
	unthreadNeedIDMessage = "请提供要恢复的主题 ID"
)

func (p *pageController) hide(ctx context.Context, sess pageSession, args []string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	// 与 /delete 一样要求详情页：目标明确、隐藏后导航确定。
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, hideNeedDetailMessage)
	}

	// 复用 M1 的 nodeActor：只从服务端 Principal 取指纹，访客管理员合法。
	// 这里不做账号状态查询，也不做权限判定——唯一谓词在 application 的 requireAdmin。
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}

	hideThread := len(args) == 0
	if !hideThread {
		id, parseErr := strconv.ParseInt(args[0], 10, 64)
		if parseErr != nil || id <= 0 {
			return p.writeFixed(sess, hideBadIDMessage)
		}
		err = p.service.HidePost(ctx, actor, id)
	} else {
		err = p.service.HideThread(ctx, actor, p.threadID)
	}
	if err != nil {
		// ErrAdminDenied / ErrNotFound 都由 NoticeFromError 收敛为固定文案。
		return p.fail(sess, err)
	}

	if hideThread {
		// E29 误报说明：上面的 Code 是状态栏提示分类字段，不是登录/注册/支付端点；
		// /hide 无独立网络入口，鉴权走单点 requireAdmin，写入经进程内单一串行写队列。
		p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "hidden", Message: hideThreadMessage}
		return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "hidden", Message: hidePostMessage}
	return p.showDetail(ctx, sess, p.threadID)
}

// /unhide 与 /hide 完全对称（ADR-053）：无参恢复当前主题，带 id 恢复该主题下的回复。
// 权限仍只由 application 的 requireAdmin 判定；这里只做参数解析与固定文案。
func (p *pageController) unhide(ctx context.Context, sess pageSession, args []string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, unhideNeedDetailMessage)
	}

	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}

	unhideThread := len(args) == 0
	if !unhideThread {
		id, parseErr := strconv.ParseInt(args[0], 10, 64)
		if parseErr != nil || id <= 0 {
			return p.writeFixed(sess, unhideBadIDMessage)
		}
		err = p.service.UnhidePost(ctx, actor, id)
	} else {
		err = p.service.UnhideThread(ctx, actor, p.threadID)
	}
	if err != nil {
		// ErrAdminDenied / ErrNotFound 都由 NoticeFromError 收敛为固定文案。
		return p.fail(sess, err)
	}

	if unhideThread {
		p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "unhidden", Message: unhideThreadMessage}
		return p.showThreads(ctx, sess, p.nodeID, p.nodeSlug, p.cursor, p.offset)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "unhidden", Message: unhidePostMessage}
	return p.showDetail(ctx, sess, p.threadID)
}

// /unthread <thread-id> 是 ADR-053 的补丁：从**任意屏幕**按 ID 恢复被隐藏的主题。
// 隐藏主题已被 GetThread 过滤、打不开详情，因此不复用要求详情页的 /unhide。
// 权限仍只由 application 的 requireAdmin 判定；命令层只解析 ID 与固定文案。
func (p *pageController) unthread(ctx context.Context, sess pageSession, args []string) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	if len(args) != 1 {
		return p.writeFixed(sess, unthreadNeedIDMessage)
	}
	threadID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || threadID <= 0 {
		return p.writeFixed(sess, unthreadBadIDMessage)
	}
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}
	if err := p.service.Unthread(ctx, actor, threadID); err != nil {
		// ErrAdminDenied / ErrNotFound 都由 NoticeFromError 收敛为固定文案。
		return p.fail(sess, err)
	}

	// 恢复已成功落库，主题现在可被 GetThread 打开：直接落到该主题详情，
	// 既证明恢复了，也不会让用户停在旧的列表 / 版块屏。
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "unhidden", Message: unthreadThreadMessage}
	return p.showDetail(ctx, sess, threadID)
}
