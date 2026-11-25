package main

// 详情屏分页编排（R-06）。
//
// 分工：分页算术在 detail_paginate.go（纯函数），行高测量在 terminal.BodyLines /
// terminal.RowsLines（纯函数），这里只负责「按页取数 → 装箱 → 组装本页 Screen」。
//
// 一页的组成：正文的某一段物理行（可为空）+ 本页装得下的若干整条回复。
// `n` 往下读一页，`p` 回第 1 页（与列表页既有语义一致，不做"上一页"栈）。

import (
	"context"
	"strconv"

	"github.com/jeeinn/motd/internal/application/viewmodel"
	"github.com/jeeinn/motd/internal/presentation/terminal"
)

const (
	// detailFixedLines 是详情屏除「正文 / 回复 / 帖子标题」外的固定行数：
	// 屏标题、`──`、`── 回复 ──`、状态栏、`位置:`、footer、页码提示 —— 共 7 行。
	// 帖子标题会折行，单独按 DetailHeaderLines 动态计算（真机实测：按 6 算会多出 1 行）。
	detailFixedLines = 7
	// detailReplyPageSize 是每页回复条数上限（拍板：行数为主、条数为辅）。
	detailReplyPageSize = 20
)

// detailState 是一个会话的详情屏分页状态。
type detailState struct {
	pos     detailPagePos    // 当前页起点（已消费的正文行数 + 回复条数）
	page    int              // 1-based 页码
	rows    []viewmodel.Row  // 已加载的全部回复（跨批次累加，下标即全局序号）
	screen  viewmodel.Screen // 最近一次组装结果（含标题/正文，Rows 不用）
	loaded  bool             // 是否已成功取过一批（区分"没加载过"与"加载了空批"）
	hasMore bool             // 是否还有未加载的回复
	next    int64            // 下一批的 afterID（keyset 游标）
}

// loadDetailBatch 加载从全局序号 from 开始的一批回复，并记录本批的屏与游标。
func (p *pageController) loadDetailBatch(ctx context.Context, sess pageSession, from int) error {
	v := p.viewContext(sess)
	v.RowOffset = from
	afterID := int64(0)
	if from > 0 {
		afterID = p.detail.next
	}
	sc, err := p.service.ScreenThreadDetailPage(ctx, p.threadID, afterID, detailReplyPageSize, v)
	if err != nil {
		return err
	}
	p.detail.screen = sc
	p.detail.rows = append(p.detail.rows, sc.Rows...)
	p.detail.loaded = true
	p.detail.hasMore = sc.Pagination.HasMore
	p.detail.next = 0
	if sc.Pagination.NextCursor != "" {
		if id, err := strconv.ParseInt(sc.Pagination.NextCursor, 10, 64); err == nil {
			p.detail.next = id
		}
	}
	return nil
}

// detailHeights 返回已加载回复的行高；下标即全局回复序号（累加缓冲，无需补位）。
func (p *pageController) detailHeights(width int) []int {
	return terminal.RowsLines(p.detail.rows, viewmodel.ScreenThreadDetail, width)
}

// detailViewport 返回详情屏的可用列数与每页可容纳的物理行数。
// 无 pty-req（如 ssh -T）时与 render() 的 24 行兜底保持一致。
func (p *pageController) detailViewport(sess pageSession, title string) (width, capacity int) {
	w, h, _ := sess.Window()
	if w == 0 {
		w = 80
	}
	if h == 0 {
		h = 24
	}
	capacity = int(h) - detailFixedLines - terminal.DetailHeaderLines(title, int(w))
	if capacity < 1 {
		capacity = 1
	}
	return int(w), capacity
}

// planDetailPage 保证数据足够并算出从 pos 开始的一页。
func (p *pageController) planDetailPage(ctx context.Context, sess pageSession, pos detailPagePos) (detailPage, error) {
	// 首次进入（还没取过数）或本页需要的回复已超出已加载范围时，先补一批。
	// 只向前加载，不回退：p 走的是"回第 1 页"，会重置状态重新取。
	// 剩余不足一批就补：避免每批 20 条、容量 16 时出现"16 条 / 4 条"交替的碎页。
	remaining := len(p.detail.rows) - pos.ReplyIndex
	needLoad := !p.detail.loaded || (p.detail.hasMore && remaining < detailReplyPageSize)
	if needLoad {
		if err := p.loadDetailBatch(ctx, sess, len(p.detail.rows)); err != nil {
			return detailPage{}, err
		}
	}

	// 视口必须在加载之后算：标题与正文都来自刚取到的屏。
	width, capacity := p.detailViewport(sess, p.detail.screen.Detail.Title)
	bodyRows := terminal.BodyLines(p.detail.screen.Detail.Body, width)
	page := paginateDetail(bodyRows, p.detailHeights(width), detailReplyPageSize, capacity, pos)
	return page, nil
}

// renderDetailPage 从 pos 起渲染一页详情。
func (p *pageController) renderDetailPage(ctx context.Context, sess pageSession, pos detailPagePos) error {
	if p.detail.page < 1 {
		p.detail.page = 1
	}
	page, err := p.planDetailPage(ctx, sess, pos)
	if err != nil {
		return p.fail(sess, err)
	}
	// 必须在 planDetailPage 之后算：它可能刚加载了第一批数据，
	// 此前的 p.detail.screen 还是空的（bodyRows 会算出 0，进而把 HasMore 判错）。
	width, _ := p.detailViewport(sess, p.detail.screen.Detail.Title)
	bodyRows := terminal.BodyLines(p.detail.screen.Detail.Body, width)
	p.detail.pos = pos

	sc := p.detail.screen
	sc.Detail.BodyWindowed = true
	sc.Detail.BodyFrom = page.BodyFrom
	sc.Detail.BodyTo = page.BodyTo

	lo, hi := page.ReplyFrom, page.ReplyTo
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	if hi > len(p.detail.rows) {
		hi = len(p.detail.rows)
	}
	sc.Rows = p.detail.rows[lo:hi]

	moreAfter := page.BodyTo < bodyRows || page.ReplyTo < len(p.detail.rows) || p.detail.hasMore
	// 单页详情不显示页码：既无翻页可做，页码只是噪音（也与"不出现做不到的操作"一致）。
	pageNo := p.detail.page
	if !moreAfter && pageNo <= 1 {
		pageNo = 0
	}
	sc.Pagination.Page = pageNo
	sc.Pagination.HasMore = moreAfter
	sc.Pagination.HasPrev = pos != detailPagePos{}
	sc.Pagination.FirstIndex = page.ReplyFrom + 1
	sc.Pagination.Loaded = len(sc.Rows)
	sc.StatusBar.Notice = p.notice

	focus := 0
	if len(sc.Rows) > 0 {
		focus = sc.Rows[0].Index
	}
	sc.Selection = viewmodel.Selection{FocusedIndex: focus, TopRowIndex: focus}

	p.screen = sc
	p.notice = viewmodel.Notice{}
	return p.render(sess)
}

// detailNextPage 往下读一页；已到末尾时不做任何事（footer 也不再提示 n）。
func (p *pageController) detailNextPage(ctx context.Context, sess pageSession) error {
	// 末尾页（含回复后定位的末页视图）没有下一页：直接返回，
	// 不能让 planDetailPage 从 pos 零点重新算，否则会莫名跳回第 1 页。
	if !p.screen.Pagination.HasMore || !p.detail.loaded {
		return nil
	}
	page, err := p.planDetailPage(ctx, sess, p.detail.pos)
	if err != nil {
		return p.fail(sess, err)
	}
	if page.Next == p.detail.pos {
		return nil // 没有下一页
	}
	p.detail.page++
	return p.renderDetailPage(ctx, sess, page.Next)
}

// detailFirstPage 回到第 1 页（与列表页 p 的语义一致）。
func (p *pageController) detailFirstPage(ctx context.Context, sess pageSession) error {
	// 已经停在第 1 页且数据在位时才是空操作；末页视图（page=0、未加载批次）
	// 必须真正重建第 1 页，否则 p 会毫无反应。
	if p.detail.page == 1 && p.detail.loaded {
		return nil
	}
	p.detail = detailState{page: 1}
	return p.renderDetailPage(ctx, sess, detailPagePos{})
}

// showDetailAtReply 渲染「包含刚发出那条回复的末页」，用于回复成功后的焦点锚定
// （ADR-068）。新回复必然在末尾，故取末页批次即可，无需计算它是第几页：
//   - 页码留空（Page=0）——不知道是第几页时不要写一个错的数字；
//   - HasPrev 按"是否还有更早的回复"给出，允许 p 回第 1 页；
//   - HasMore=false——已经在末尾，n 不再有下一页。
//
// 版面有两种：
//  1. 正文与末页回复能同屏（短主题，占绝大多数）→ 正文照常显示；
//  2. 放不下（长正文或回复多）→ 只显示回复，并**从末尾往前装**，
//     保证刚发的这条一定在页内（否则锚定又落空）。
func (p *pageController) showDetailAtReply(ctx context.Context, sess pageSession, threadID, anchor int64) error {
	if p.service == nil {
		return p.writeFixed(sess, "论坛服务尚未就绪")
	}
	p.threadID = threadID
	p.detail = detailState{} // 末页视图不参与前后翻页，清空分页游标

	sc, err := p.service.ScreenThreadDetailTail(ctx, threadID, detailReplyPageSize, p.viewContext(sess))
	if err != nil {
		return p.fail(sess, err)
	}
	total, err := p.service.CountReplies(ctx, threadID)
	if err != nil {
		return p.fail(sess, err)
	}

	width, capacity := p.detailViewport(sess, sc.Detail.Title)
	heights := terminal.RowsLines(sc.Rows, viewmodel.ScreenThreadDetail, width)
	bodyRows := terminal.BodyLines(sc.Detail.Body, width)
	need := bodyRows
	for _, h := range heights {
		if h < 1 {
			h = 1
		}
		need += h
	}
	if need > capacity {
		// 装不下：放弃正文，从末尾往前装回复。
		sc.Detail.Body = ""
		sc.Detail.BodyWindowed = true
		used, start := 0, len(sc.Rows)
		for start > 0 {
			h := heights[start-1]
			if h < 1 {
				h = 1
			}
			if used+h > capacity && used > 0 {
				break
			}
			used += h
			start--
		}
		sc.Rows = sc.Rows[start:]
	}

	sc.Pagination.Page = 0
	sc.Pagination.HasPrev = total > len(sc.Rows)
	sc.Pagination.HasMore = false
	sc.Pagination.Loaded = len(sc.Rows)
	sc.StatusBar.Notice = p.notice
	if len(sc.Rows) > 0 {
		sc.Selection = viewmodel.Selection{
			FocusedIndex: sc.Rows[0].Index,
			TopRowIndex:  sc.Rows[0].Index,
		}
	}

	p.screen = sc
	p.notice = viewmodel.Notice{}
	p.restoreFocus(anchor)
	return p.render(sess)
}
