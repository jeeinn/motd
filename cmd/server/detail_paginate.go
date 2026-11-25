package main

// R-06 详情屏内容装箱分页（纯逻辑：零 IO、零渲染、零状态）。
//
// 设计依据 docs/DESIGN-detail-pagination.md（方案 D：整块装箱）：
//   - 正文按**物理行**切（每行 1 行，跨页续读）；
//   - 回复按**整条**装箱，绝不跨页切断；
//   - 一页容量由终端高度减去固定开销得出（由调用方传入），回复另有条数上限。
//
// 这里只做「给多少行、装哪些内容」的算术，不关心正文与回复怎么渲染——
// 行高由 terminal.BodyLines / terminal.RowsLines 提供，保持单一口径。

// detailPagePos 是分页游标：已翻过的正文行数与已翻过的回复条数。
// 两个分量都是「已消费量」而不是「页码」，因此内容增删时不会整页错位。
type detailPagePos struct {
	BodyLine   int
	ReplyIndex int
}

// detailPage 是一页的内容区间与下一页游标。
// 区间均为左闭右开：正文 [BodyFrom, BodyTo)、回复 [ReplyFrom, ReplyTo)。
type detailPage struct {
	BodyFrom   int
	BodyTo     int
	ReplyFrom  int
	ReplyTo    int
	Used       int // 本页占用的物理行数（可能超过容量：单块内容过高时的保底）
	Next       detailPagePos
}

// paginateDetail 从 from 起按 capacity 装箱，返回本页内容与下一页游标。
//
// 参数：
//   - bodyRows  正文物理行数（terminal.BodyLines 的结果）
//   - replyRows 已加载回复各自的物理行数（terminal.RowsLines 的结果）
//   - maxReplies 每页回复条数上限，<=0 表示不限
//   - capacity  本页可容纳的物理行数（调用方按终端高度减去固定开销算出）
//
// 保证：
//  1. **前进**：只要还有内容，本页至少装一个单位（正文一行或一条回复）。
//     单条回复高过 capacity 时宁可溢出，也不返回空页——否则调用方会死循环。
//  2. **不切断回复**：ReplyTo 只会落在整条边界上。
//  3. **顺序**：正文全部翻完之前不会开始装回复。
func paginateDetail(bodyRows int, replyRows []int, maxReplies, capacity int, from detailPagePos) detailPage {
	if capacity < 1 {
		capacity = 1
	}
	if bodyRows < 0 {
		bodyRows = 0
	}
	if from.BodyLine < 0 {
		from.BodyLine = 0
	}
	if from.BodyLine > bodyRows {
		from.BodyLine = bodyRows
	}
	if from.ReplyIndex < 0 {
		from.ReplyIndex = 0
	}
	if from.ReplyIndex > len(replyRows) {
		from.ReplyIndex = len(replyRows)
	}

	page := detailPage{
		BodyFrom:  from.BodyLine,
		BodyTo:    from.BodyLine,
		ReplyFrom: from.ReplyIndex,
		ReplyTo:   from.ReplyIndex,
	}
	used := 0

	// ① 正文：一行占一行，先装满或装完为止。
	for page.BodyTo < bodyRows && used < capacity {
		page.BodyTo++
		used++
	}

	// ② 回复：整条装；放不下就留给下一页（保证本条完整）。
	count := 0
	for page.ReplyTo < len(replyRows) {
		if maxReplies > 0 && count >= maxReplies {
			break
		}
		need := replyRows[page.ReplyTo]
		if need < 1 {
			need = 1 // 空行/测量失败按 1 行算，避免零高度回复挤爆一页
		}
		// used > 0：本页已有内容，放不下就给下一页；
		// used == 0：本页还是空的，即使单条超高也必须装，保证前进。
		if used+need > capacity && used > 0 {
			break
		}
		page.ReplyTo++
		used += need
		count++
	}

	page.Used = used
	page.Next = detailPagePos{BodyLine: page.BodyTo, ReplyIndex: page.ReplyTo}
	return page
}

// empty 判断本页是否没有任何内容（调用方据此终止翻页，避免停在空页）。
func (p detailPage) empty() bool {
	return p.BodyTo == p.BodyFrom && p.ReplyTo == p.ReplyFrom
}
