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
	// detailFixedLines 是详情屏除正文与回复外的固定行数：标题、`──`、`── 回复 ──`、
	// 状态栏、footer，外加 1 行页码/提示。按"有提示"保守预留，宁可少装一行，
	// 也不让本页多出 1 行把标题顶出可视区（与 ADR-072 的整屏重绘配合）。
	detailFixedLines = 6
	// detailReplyPageSize 是每页回复条数上限（拍板：行数为主、条数为辅）。
	detailReplyPageSize = 20
)

// detailState 是一个会话的详情屏分页状态。
type detailState struct {
	pos       detailPagePos    // 当前页起点（已消费的正文行数 + 回复条数）
	page      int              // 1-based 页码
	batch     []viewmodel.Row  // 已加载的一批回复（未按页切片）
	batchFrom int              // batch[0] 的全局回复序号（0-based）
	screen    viewmodel.Screen // 批次组装结果（含正文整段与整批 Rows）
	loaded    bool             // 是否已成功取过一批（区分"没加载过"与"加载了空批"）
	hasMore   bool             // 是否还有未加载的回复
	next      int64            // 下一批的 afterID（keyset 游标）
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
	p.detail.batch = sc.Rows
	p.detail.batchFrom = from
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

// detailHeights 返回按**全局回复序号**对齐的行高切片。
// paginateDetail 用绝对下标索引回复，因此已加载批次之前的槽位补 0 占位——
// 那些回复已经翻过去了，不会再被本页选中。
func (p *pageController) detailHeights(width int) []int {
	heights := terminal.RowsLines(p.detail.batch, viewmodel.ScreenThreadDetail, width)
	if p.detail.batchFrom == 0 {
		return heights
	}
	padded := make([]int, p.detail.batchFrom+len(heights))
	copy(padded[p.detail.batchFrom:], heights)
	return padded
}

// planDetailPage 保证数据足够并算出从 pos 开始的一页。
func (p *pageController) planDetailPage(ctx context.Context, sess pageSession, pos detailPagePos) (detailPage, error) {
	width, height, _ := sess.Window()
	if width == 0 {
		width = 80
	}
	if height == 0 {
		height = 24 // 无 pty-req（如 ssh -T）时与 render() 的兜底一致
	}
	capacity := int(height) - detailFixedLines
	if capacity < 1 {
		capacity = 1
	}

	// 首次进入（还没取过数）或本页需要的回复已超出已加载范围时，先补一批。
	// 只向前加载，不回退：p 走的是"回第 1 页"，会重置状态重新取。
	needLoad := !p.detail.loaded ||
		(pos.ReplyIndex >= p.detail.batchFrom+len(p.detail.batch) && p.detail.hasMore)
	if needLoad {
		from := p.detail.batchFrom + len(p.detail.batch)
		if !p.detail.loaded {
			from = 0
		}
		if err := p.loadDetailBatch(ctx, sess, from); err != nil {
			return detailPage{}, err
		}
	}

	bodyRows := terminal.BodyLines(p.detail.screen.Detail.Body, int(width))
	page := paginateDetail(bodyRows, p.detailHeights(int(width)), detailReplyPageSize, capacity, pos)
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
	p.detail.pos = pos

	sc := p.detail.screen
	sc.Detail.BodyFrom = page.BodyFrom
	sc.Detail.BodyTo = page.BodyTo

	lo := page.ReplyFrom - p.detail.batchFrom
	hi := page.ReplyTo - p.detail.batchFrom
	if lo < 0 {
		lo = 0
	}
	if hi < lo {
		hi = lo
	}
	if hi > len(p.detail.batch) {
		hi = len(p.detail.batch)
	}
	sc.Rows = p.detail.batch[lo:hi]

	moreAfter := page.ReplyTo < p.detail.batchFrom+len(p.detail.batch) || p.detail.hasMore
	// 单页详情不显示页码：既无翻页可做，页码只是噪音（也与"不出现做不到的操作"一致）。
	pageNo := p.detail.page
	if !moreAfter && pageNo <= 1 {
		pageNo = 0
	}
	sc.Pagination.Page = pageNo
	sc.Pagination.HasMore = moreAfter
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
	if p.detail.page <= 1 && p.detail.pos == (detailPagePos{}) {
		return nil
	}
	p.detail = detailState{page: 1}
	return p.renderDetailPage(ctx, sess, detailPagePos{})
}
