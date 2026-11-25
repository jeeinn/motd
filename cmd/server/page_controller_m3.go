package main

// P1-M3 页面控制器的投票与检索动作。单独成文件，避免继续膨胀 page_controller.go。
// 这里只做「命令参数 → 用例调用 → 屏幕刷新」的编排，业务规则不在此处重写。

import (
	"context"
	"strconv"

	"github.com/jeeinn/motd/internal/application/forum"
	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/domain/vote"
)

// showSearch 输出检索结果屏。关键词只作为查询参数进入用例层，
// 状态栏 Context 用固定文案，绝不把用户原文拼进系统文本。
func (p *pageController) showSearch(ctx context.Context, sess pageSession, query string, page int) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	if page < 1 {
		page = 1
	}
	p.searchQuery, p.searchPage = query, page
	p.offset = (page - 1) * forum.SearchMaxPageSize
	v := p.viewContext(sess)
	v.Context = "搜索"
	sc, err := p.service.ScreenSearchResults(ctx, forum.SearchInput{
		Query: query, Page: page, Limit: forum.SearchMaxPageSize,
	}, v)
	if err != nil {
		return p.fail(sess, err)
	}
	p.setScreen(sc)
	p.nodeID, p.nodeSlug, p.threadID, p.cursor = 0, "", 0, ""
	return p.render(sess)
}

// vote 给当前主题或指定回复投票（R-15）：无 id 参数作用于当前主题（既有口径），
// 带 id 参数作用于该回复（id 取详情页显示的 id=<n>）。参数来自用户文本，
// 方向先白名单匹配成领域值、id 按十进制正整数解析，绝不把原文写进提示或日志。
// 投票的业务规则（权限、目标存在性、越权、self-vote、karma、幂等）全在 Service 层，
// 这里只做参数解析与用例调用，不重写任何规则。
func (p *pageController) vote(ctx context.Context, sess pageSession, args []string) error {
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, "请先打开一个主题再投票")
	}
	if len(args) == 0 || len(args) > 2 {
		return p.writeFixed(sess, "用法：/vote <up|down|rm> [回复id]")
	}
	actor, err := p.actor(ctx, sess)
	if err != nil {
		return p.fail(sess, err)
	}

	// 目标：无 id = 当前主题；带 id = 指定回复。id 必须是正整数，不回显原文。
	target, targetID := vote.TargetThread, p.threadID
	if len(args) == 2 {
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return p.writeFixed(sess, "回复 ID 必须是正整数")
		}
		target, targetID = vote.TargetPost, id
	}

	switch args[0] {
	case "up", "down":
		v := vote.Up
		if args[0] == "down" {
			v = vote.Down
		}
		_, err = p.service.CastVote(ctx, actor, target, targetID, v)
	case "rm":
		_, err = p.service.Unvote(ctx, actor, target, targetID)
	default:
		return p.writeFixed(sess, "用法：/vote <up|down|rm> [回复id]")
	}
	if err != nil {
		return p.fail(sess, err)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "voted", Message: "投票已记录"}
	return p.showDetail(ctx, sess, p.threadID)
}
