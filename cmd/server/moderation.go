package main

// P2-M1b 内容治理命令面。只做「参数解析 → 复用 M1 的身份映射 → 调用用例 → 固定文案」，
// 不出现任何权限谓词、不查 users 状态、不回显用户原文（archtest 静态守卫）。
//
// 采用与 /delete 对称的口径（方案 a）：
//   /hide        → 隐藏当前打开的主题；
//   /hide <id>   → 隐藏 ID 为 <id> 的回复（按当前打开主题导航）。
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
