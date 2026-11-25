package main

import "testing"

// R-06 装箱分页的表驱动测试。覆盖：正文优先、跨页续读、回复不被切断、
// 条数上限、单条超高时的前进保证、容量退化、从中间续页、整篇遍历不重不漏。

func TestPaginateDetailBodyFirst(t *testing.T) {
	// 正文 3 行 + 3 条各 1 行的回复，容量 5：正文先占满，回复只装得下 2 条。
	page := paginateDetail(3, []int{1, 1, 1}, 20, 5, detailPagePos{})
	if page.BodyFrom != 0 || page.BodyTo != 3 {
		t.Fatalf("第 1 页应装完整正文 [0,3)，实际 [%d,%d)", page.BodyFrom, page.BodyTo)
	}
	if page.ReplyFrom != 0 || page.ReplyTo != 2 {
		t.Fatalf("剩余 2 行应装 2 条回复 [0,2)，实际 [%d,%d)", page.ReplyFrom, page.ReplyTo)
	}
	if page.Used != 5 {
		t.Fatalf("本页应恰好用满 5 行，实际 %d", page.Used)
	}
	if page.Next != (detailPagePos{BodyLine: 3, ReplyIndex: 2}) {
		t.Fatalf("下一页游标错误: %+v", page.Next)
	}

	// 第 2 页只剩余 1 条回复。
	page2 := paginateDetail(3, []int{1, 1, 1}, 20, 5, page.Next)
	if page2.BodyFrom != 3 || page2.BodyTo != 3 {
		t.Fatalf("正文已读完，第 2 页不应再有正文，实际 [%d,%d)", page2.BodyFrom, page2.BodyTo)
	}
	if page2.ReplyTo != 3 {
		t.Fatalf("第 2 页应收尾到最后一条，实际 ReplyTo=%d", page2.ReplyTo)
	}
}

func TestPaginateDetailReplyCountCap(t *testing.T) {
	// 容量足够（100），但条数上限 20：一页最多 20 条。
	replyRows := make([]int, 30)
	for i := range replyRows {
		replyRows[i] = 1
	}
	page := paginateDetail(0, replyRows, 20, 100, detailPagePos{})
	if got := page.ReplyTo - page.ReplyFrom; got != 20 {
		t.Fatalf("条数上限应为 20，实际 %d", got)
	}
	if page.Used != 20 {
		t.Fatalf("本页应占 20 行，实际 %d", page.Used)
	}
}

func TestPaginateDetailNoReplySplit(t *testing.T) {
	// 容量 6，回复高度 2/1/3/1/1/4：第 4 条（3 行）放不下时留给下一页，绝不切半条。
	replyRows := []int{2, 1, 3, 1, 1, 4}
	page := paginateDetail(0, replyRows, 0, 6, detailPagePos{})
	if page.ReplyTo != 3 {
		t.Fatalf("Capacity 6 应装下 2+1+3=6 的三条，实际 ReplyTo=%d", page.ReplyTo)
	}
	if page.Used != 6 {
		t.Fatalf("本页应占 6 行，实际 %d", page.Used)
	}
}

func TestPaginateDetailOversizedReplyStillProgresses(t *testing.T) {
	// 单条 10 行 > 容量 5：空页也必须装它，否则调用方会停在空页死循环。
	page := paginateDetail(0, []int{10, 1}, 0, 5, detailPagePos{})
	if page.ReplyTo != 1 {
		t.Fatalf("超高单条应独占一页并前进，实际 ReplyTo=%d", page.ReplyTo)
	}
	if page.Used != 10 {
		t.Fatalf("本页应记录实际占用的 10 行（允许溢出），实际 %d", page.Used)
	}
	if page.empty() {
		t.Fatal("超高单条不应产生空页")
	}
}

func TestPaginateDetailDegenerateCapacity(t *testing.T) {
	// 容量 0/负数按 1 行处理：至少能翻动一行，不会卡死。
	for _, capacity := range []int{0, -3} {
		page := paginateDetail(2, nil, 0, capacity, detailPagePos{})
		if page.BodyTo != 1 {
			t.Fatalf("容量 %d 应至少前进 1 行，实际 BodyTo=%d", capacity, page.BodyTo)
		}
	}
}

func TestPaginateDetailResumeFromMiddle(t *testing.T) {
	// 从 {正文 2 行, 回复 3 条} 续页：只装剩下的内容。
	replyRows := []int{1, 1, 1, 1, 1}
	page := paginateDetail(4, replyRows, 0, 4, detailPagePos{BodyLine: 2, ReplyIndex: 3})
	if page.BodyFrom != 2 || page.BodyTo != 4 {
		t.Fatalf("剩余正文应为 [2,4)，实际 [%d,%d)", page.BodyFrom, page.BodyTo)
	}
	if page.ReplyTo != 5 {
		t.Fatalf("剩余 2 行应装完剩下 2 条回复，实际 ReplyTo=%d", page.ReplyTo)
	}
}

func TestPaginateDetailFullTraversalCoversEverything(t *testing.T) {
	// 整篇遍历：正文与回复各自被完整、不重复地覆盖，且回复区间总是整条边界。
	bodyRows := 7
	replyRows := []int{2, 1, 3, 1, 1, 4, 2}
	capacity, maxReplies := 6, 3

	pos := detailPagePos{}
	bodySeen := 0
	replySeen := 0
	pages := 0
	for {
		page := paginateDetail(bodyRows, replyRows, maxReplies, capacity, pos)
		if page.empty() {
			break
		}
		pages++
		if pages > 50 {
			t.Fatal("分页未收敛，疑似死循环")
		}
		if page.BodyFrom != bodySeen {
			t.Fatalf("第 %d 页正文起点应为 %d，实际 %d（有重叠或跳行）", pages, bodySeen, page.BodyFrom)
		}
		if page.ReplyFrom != replySeen {
			t.Fatalf("第 %d 页回复起点应为 %d，实际 %d（有重叠或跳条）", pages, replySeen, page.ReplyFrom)
		}
		if page.BodyTo-page.BodyFrom > capacity {
			// 正文一行一行装，不可能超过容量（只有单条回复才会因保底而溢出）。
			t.Fatalf("第 %d 页正文超出容量: %d > %d", pages, page.BodyTo-page.BodyFrom, capacity)
		}
		if page.ReplyTo-page.ReplyFrom > maxReplies {
			t.Fatalf("第 %d 页回复条数超出上限: %d > %d", pages, page.ReplyTo-page.ReplyFrom, maxReplies)
		}
		bodySeen = page.BodyTo
		replySeen = page.ReplyTo
		pos = page.Next
	}

	if bodySeen != bodyRows {
		t.Fatalf("正文应被完整覆盖 %d 行，实际 %d", bodyRows, bodySeen)
	}
	if replySeen != len(replyRows) {
		t.Fatalf("回复应被完整覆盖 %d 条，实际 %d", len(replyRows), replySeen)
	}
}
