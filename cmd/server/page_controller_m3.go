package main

// P1-M3 页面控制器的投票与检索动作。单独成文件，避免继续膨胀 page_controller.go。
// 这里只做「命令参数 → 用例调用 → 屏幕刷新」的编排，业务规则不在此处重写。

import (
	"context"

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

// vote 只作用于当前打开的主题。参数来自用户文本，先白名单匹配成领域值，
// 绝不把原文写进提示或日志；post 目标暂不通过命令行暴露（见交付说明）。
func (p *pageController) vote(ctx context.Context, sess pageSession, arg string) error {
	if p.screen.Kind != viewmodel.ScreenThreadDetail || p.threadID <= 0 {
		return p.writeFixed(sess, "请先打开一个主题再投票")
	}
	actor, err := p.actor(ctx, sess)
	if err != nil {
		return p.fail(sess, err)
	}
	target := vote.TargetThread
	switch arg {
	case "up", "down":
		v := vote.Up
		if arg == "down" {
			v = vote.Down
		}
		_, err = p.service.CastVote(ctx, actor, target, p.threadID, v)
	case "rm":
		_, err = p.service.Unvote(ctx, actor, target, p.threadID)
	default:
		return p.writeFixed(sess, "用法：/vote <up|down|rm>")
	}
	if err != nil {
		return p.fail(sess, err)
	}
	p.notice = viewmodel.Notice{Kind: viewmodel.NoticeSuccess, Code: "voted", Message: "投票已记录"}
	return p.showDetail(ctx, sess, p.threadID)
}
