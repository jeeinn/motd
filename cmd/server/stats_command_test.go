package main

// /stats 输出层的文案回归（v0.7.1 / P4.5-M1 / ADR-064）。
// 断言整个屏幕逐行确定：行数、行宽、数字就位、"--" 的出现条件。

import (
	"strings"
	"testing"

	appuser "github.com/jeeinn/motd/internal/application/user"
)

// 全部观测到的健康状态：6 行（含起点行），无图例。
func TestStatsTextAllObserved(t *testing.T) {
	got := statsText(appuser.StatsSnapshot{
		TodayLogins: 12, YesterdayLogins: 9,
		TodayPeak: 7, TodayPeakObserved: true,
		YesterdayPeak: 5, YesterdayPeakSeen: true,
		AllTimePeak: 15, AllTimePeakSeen: true,
		CurrentOnline: 3, CurrentActive: 2,
		UserCount: 87, TotalLogins: 233,
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 6 {
		t.Fatalf("应为 6 行（标题 + 4 行数据 + 起点），实际 %d 行:\n%s", len(lines), got)
	}
	want := []string{
		"论坛统计",
		"  今日登录 12 · 昨日登录 9",
		"  今日在线峰值 7 · 当前在线 3（活跃 2）",
		"  昨日在线峰值 5 · 历史在线峰值 15",
		"  注册用户 87 · 累计登录 233 人次",
		"  登录自 v0.6.0；峰值与累计登录自 v0.7.1",
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("第 %d 行应为 %q，实际 %q", i+1, w, lines[i])
		}
	}
	if strings.Contains(got, statsDash) {
		t.Fatalf("全部观测到时不应出现 %q:\n%s", statsDash, got)
	}
}

// 无观测：三个峰值都显示 "--"，并补一行图例（共 7 行）。
func TestStatsTextNoObservationAddsLegend(t *testing.T) {
	got := statsText(appuser.StatsSnapshot{CurrentOnline: 0})
	lines := strings.Split(got, "\n")
	if len(lines) != 7 {
		t.Fatalf("无观测时应为 7 行（多一行图例），实际 %d 行:\n%s", len(lines), got)
	}
	if lines[2] != "  今日在线峰值 -- · 当前在线 0（活跃 0）" {
		t.Errorf("今日峰值行应为 --，实际 %q", lines[2])
	}
	if lines[3] != "  昨日在线峰值 -- · 历史在线峰值 --" {
		t.Errorf("昨日/历史峰值行应为 --，实际 %q", lines[3])
	}
	if lines[5] != "  -- 表示暂无在线观测数据" {
		t.Errorf("图例行不对: %q", lines[5])
	}
	// 计数类指标即使为 0 也显示数字：0 是「观测到没人」，不是「没有数据」。
	if lines[1] != "  今日登录 0 · 昨日登录 0" {
		t.Errorf("登录人数应为 0 而非 --，实际 %q", lines[1])
	}
	if lines[4] != "  注册用户 0 · 累计登录 0 人次" {
		t.Errorf("注册/累计应为 0 而非 --，实际 %q", lines[4])
	}
}

// 行宽：终端窄屏可读（40 显示列以内，中文按 2 列）。
func TestStatsTextLinesFitNarrowTerminal(t *testing.T) {
	// 40 列是排版**目标**，只在文案 v3 的 fixture 数值下成立（ADR-064）：
	// 数字涨到三位数时正解是重排行，不是压宽度，也不该让断言假红。
	got := statsText(appuser.StatsSnapshot{
		TodayLogins: 12, YesterdayLogins: 9,
		TodayPeak: 7, TodayPeakObserved: true,
		YesterdayPeak: 5, YesterdayPeakSeen: true,
		AllTimePeak: 15, AllTimePeakSeen: true,
		CurrentOnline: 3, CurrentActive: 2,
		UserCount: 87, TotalLogins: 233,
	})
	for _, line := range strings.Split(got, "\n") {
		if w := displayWidthOf(line); w > 40 {
			t.Errorf("行宽 %d 超过 40 显示列: %q", w, line)
		}
	}
}

// displayWidthOf 按「CJK/全角算 2 列」估算显示宽度（测试用，不引渲染层依赖）。
func displayWidthOf(s string) int {
	w := 0
	for _, r := range s {
		if r >= 0x1100 && (r <= 0x115F || r == 0x2329 || r == 0x232A ||
			(r >= 0x2E80 && r <= 0xA4CF) || (r >= 0xAC00 && r <= 0xD7A3) ||
			(r >= 0xF900 && r <= 0xFAFF) || (r >= 0xFE30 && r <= 0xFE6F) ||
			(r >= 0xFF00 && r <= 0xFF60) || (r >= 0xFFE0 && r <= 0xFFE6)) {
			w += 2
			continue
		}
		w++
	}
	return w
}
