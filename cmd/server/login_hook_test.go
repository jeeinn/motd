package main

// 登录成功钩子的顺序与容错回归（v0.7.1 / ADR-064）。
// 两件事必须锁死：
//  1. 审计先行、签到在后，且**互不短路**——签到失败不能吞掉 login_success（QA P0-2）；
//  2. 任一步为 nil 时安全跳过（未装配 / 测试场景的零值安全）。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jeeinn/motd/internal/application/auth"
	appuser "github.com/jeeinn/motd/internal/application/user"
)

func TestOnLoginHookAuditFirstAndNeverShortCircuits(t *testing.T) {
	var order []string
	hook := newOnLoginHook(
		func(ctx context.Context, userID int64) (appuser.CheckInResult, error) {
			order = append(order, "checkin")
			return appuser.CheckInResult{}, errors.New("写队列已满")
		},
		func(userID int64, provider string) error {
			order = append(order, "audit:"+provider)
			return nil
		},
		testLogger(),
	)
	hook(context.Background(), 7, auth.ProviderEmail)

	want := []string{"audit:email", "checkin"}
	if len(order) != len(want) {
		t.Fatalf("应依次执行 %v，实际 %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("第 %d 步应为 %q，实际 %q（顺序是契约：签到失败不得吞掉审计）", i+1, want[i], order[i])
		}
	}
}

// 审计失败只是日志，签到照常进行，且钩子不返回错误（登录不回滚）。
func TestOnLoginHookAuditFailureDoesNotBlockCheckIn(t *testing.T) {
	checked := false
	hook := newOnLoginHook(
		func(ctx context.Context, userID int64) (appuser.CheckInResult, error) {
			checked = true
			return appuser.CheckInResult{Streak: 1}, nil
		},
		func(userID int64, provider string) error { return auth.ErrAuditQueueFull },
		testLogger(),
	)
	hook(context.Background(), 7, auth.ProviderGitHub)
	if !checked {
		t.Fatal("审计队列满时签到仍应执行")
	}
}

func TestOnLoginHookNilPartsAreSafe(t *testing.T) {
	called := false
	hook := newOnLoginHook(nil, func(userID int64, provider string) error {
		called = true
		return nil
	}, testLogger())
	hook(context.Background(), 1, auth.ProviderGitHub)
	if !called {
		t.Fatal("缺签到实现时审计仍应记录")
	}

	// 两者都 nil：只允许无操作，不得 panic。
	newOnLoginHook(nil, nil, testLogger())(context.Background(), 1, auth.ProviderGitHub)
}

// 未装配统计服务时 /stats 只给固定提示（不 panic、不回显内部细节）。
func TestRunStatsWithoutService(t *testing.T) {
	h := newInputHandler()
	got := h.runStats(context.Background())
	if got != "统计服务尚未就绪" {
		t.Fatalf("应给出固定提示，实际 %q", got)
	}
}

// 统计查询失败时只给固定文案，绝不把 DB 错误链写进终端（ADR-026 零回显）。
func TestRunStatsFailureUsesFixedText(t *testing.T) {
	h := newInputHandler().withStats(appuser.NewStatsService(failingStatsReader{}, nil, nil))
	got := h.runStats(context.Background())
	if got != "统计查询失败，请稍后重试" {
		t.Fatalf("应给出固定文案，实际 %q", got)
	}
	if strings.Contains(got, "db") || strings.Contains(got, "统计今日") {
		t.Fatalf("不得回显内部错误链，实际 %q", got)
	}
}

type failingStatsReader struct{}

func (failingStatsReader) UserCount(ctx context.Context) (int64, error) {
	return 0, errors.New("db down")
}
func (failingStatsReader) DailyLogins(ctx context.Context, d string) (int64, error) {
	return 0, errors.New("db down")
}
func (failingStatsReader) DailyPeakOnline(ctx context.Context, d string) (int, bool, error) {
	return 0, false, errors.New("db down")
}
func (failingStatsReader) PeakOnlineAllTime(ctx context.Context) (int, bool, error) {
	return 0, false, errors.New("db down")
}
func (failingStatsReader) TotalLogins(ctx context.Context) (int64, error) {
	return 0, errors.New("db down")
}

// testLogger 返回丢弃式 logger：测试只关心行为，不关心日志文本。
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
