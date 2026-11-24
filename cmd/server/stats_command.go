package main

// /stats 社区统计看板的输出层（v0.7.1 / P4.5-M1 / ADR-064）。
//
// 这里的每一行都是**固定文案 + 本地数字**，没有任何用户文本：
// 因此整段走 safe.RenderSystem（writeSystem），不进 RenderUser 净化链，
// 也不存在信任域风险（ADR-019）。"--" 是唯一表示「无观测」的记号，
// 与数字 0 严格区分——0 是「观测到了，就是没人」。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appuser "github.com/jeeinn/motd/internal/application/user"
)

// statsDash 是「暂无观测数据」的固定记号（ASCII，避免全角字符在窄终端换行）。
const statsDash = "--"

// statsText 把统计快照格式化为多行固定文案（行间用 \n，由 writeSystem 转 CRLF）。
// 抽成纯函数是为了让文案与取值逻辑都可单测：给定快照，输出逐字节确定。
func statsText(s appuser.StatsSnapshot) string {
	showDash := !s.TodayPeakObserved || !s.YesterdayPeakSeen || !s.AllTimePeakSeen

	var b strings.Builder
	b.WriteString("论坛统计\n")
	fmt.Fprintf(&b, "  今日登录 %d · 昨日登录 %d\n", s.TodayLogins, s.YesterdayLogins)
	fmt.Fprintf(&b, "  今日在线峰值 %s · 当前在线 %d（活跃 %d）\n",
		peakValue(s.TodayPeak, s.TodayPeakObserved), s.CurrentOnline, s.CurrentActive)
	fmt.Fprintf(&b, "  昨日在线峰值 %s · 历史在线峰值 %s\n",
		peakValue(s.YesterdayPeak, s.YesterdayPeakSeen), peakValue(s.AllTimePeak, s.AllTimePeakSeen))
	fmt.Fprintf(&b, "  注册用户 %d · 累计登录 %d 人次\n", s.UserCount, s.TotalLogins)
	if showDash {
		// 只在真的出现 "--" 时补一行图例，避免健康状态下的额外噪音。
		b.WriteString("  -- 表示暂无在线观测数据\n")
	}
	// 起点声明（文案 v3 冻结，产品四轮零变更）：先概览「登录自 v0.6.0」，
	// 再用「峰值与累计登录自 v0.7.1」说明两项更晚起步、口径更保守的指标。
	// 该行显示宽度恰好 40 列（CJK 按 2 列），是窄屏可读的上限，不再加字。
	b.WriteString("  登录自 v0.6.0；峰值与累计登录自 v0.7.1")
	return b.String()
}

// peakValue 把（值, 是否观测到）渲染成数字或 "--"。
func peakValue(v int, observed bool) string {
	if !observed {
		return statsDash
	}
	return strconv.Itoa(v)
}

// runStats 读取并渲染统计看板。失败时只给固定文案，不回显底层错误
// （DB 错误链可能含表名/路径，ADR-026 零回显口径）。
func (h *inputHandler) runStats(ctx context.Context) string {
	if h.stats == nil {
		return "统计服务尚未就绪"
	}
	live := appuser.LiveCounters{}
	if h.presence != nil {
		live.Online, live.Active = h.presence.Online(), h.presence.Active()
	}
	snap, err := h.stats.Snapshot(ctx, live)
	if err != nil {
		return "统计查询失败，请稍后重试"
	}
	// 版本随统计一并给出：首帧整屏重绘会擦掉连接时的版本行（ADR-072），
	// /stats 是用户能主动查到当前服务端版本的固定入口。
	return statsText(snap) + fmt.Sprintf("\n  服务端版本 %s", version)
}
