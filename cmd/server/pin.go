package main

// R-15 / ADR-079 主题置顶的命令接线：只做「参数 → 用例 → 刷新」，
// 权限判定唯一在 application 的 requireAdmin（cmd 不写任何权限表达式），
// 文案全是服务端固定串，不回显用户数据。

import (
	"context"
	"errors"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/viewmodel"
)

const (
	pinNeedDetailMessage = "请先打开一个主题再置顶或取消置顶"
	pinBadServiceMessage = "论坛服务尚未就绪"
	pinOKMessage         = "已置顶当前主题"
	unpinOKMessage       = "已取消置顶"
)

// pin 处理 /pin 与 /unpin：作用于当前打开的主题。
// 置顶后回版块列表（让置顶效果立即可见），取消置顶后刷新当前详情。
func (p *pageController) pin(ctx context.Context, sess pageSession, set bool) error {
	if p.service == nil {
		return p.writeFixed(sess, pinBadServiceMessage)
	}
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, pinNeedDetailMessage)
	}
	// 复用 M1 的 nodeActor：只从服务端 Principal 取指纹，访客管理员合法。
	actor, err := p.nodeActor(ctx, sess)
	if err != nil {
		return p.writeFixed(sess, adminDeniedMessage)
	}

	var nodeID int64
	if set {
		nodeID, err = p.service.PinThread(ctx, actor, p.threadID)
	} else {
		nodeID, err = p.service.UnpinThread(ctx, actor, p.threadID)
	}
	if err != nil {
		// 越权走固定文案（与 hide 一致），不回显指纹、不把内部错误写进状态栏。
		if errors.Is(err, forum.ErrAdminDenied) {
			return p.writeFixed(sess, adminDeniedMessage)
		}
		return p.fail(sess, err)
	}
	if set {
		p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "pinned", Message: pinOKMessage}
		return p.showThreads(ctx, sess, nodeID, p.nodeSlug, p.cursor, p.offset)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "unpinned", Message: unpinOKMessage}
	return p.showDetail(ctx, sess, p.threadID)
}
