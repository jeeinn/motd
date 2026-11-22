package main

// 登录成功后的副作用钩子装配（v0.6.0 签到 + v0.7.1 login_success 审计 / ADR-064）。
//
// 抽成独立函数而不是塞在 main 的闭包里，是为了能单测「审计失败不阻断签到」
// 与「签到失败不丢审计」两条顺序保证——它们正是这个功能最容易写错的地方。

import (
	"context"
	"log/slog"

	appuser "github.com/jeeinn/motd/internal/application/user"
)

// newOnLoginHook 组装两个调用点（GitHub Device Flow / 邮箱密码）共用的登录后钩子。
//
// 顺序与容错是**契约**，不是实现细节：
//  1. 审计先行：NoteLoginSuccess 只做环形缓冲入队（纯内存），不依赖 DB 写队列；
//     失败（队列满）只记 Warn——登录此刻已经成功，绝不能回滚或阻断。
//  2. 签到在后：CheckIn 需要 DB 写事务，可能因队列满/超时失败；失败只记 Warn。
//  3. 两者互不短路：v0.6.0 的实现里签到失败会提前 return，如果审计放在后面，
//     login_success 就会跟着一起丢，累计登录人次被系统性低估（QA P0-2）。
func newOnLoginHook(
	checkIn func(ctx context.Context, userID int64) (appuser.CheckInResult, error),
	noteLoginSuccess func(userID int64, provider string) error,
	logger *slog.Logger,
) loginHook {
	return func(ctx context.Context, userID int64, provider string) {
		if noteLoginSuccess != nil {
			if err := noteLoginSuccess(userID, provider); err != nil {
				// 不打印 user_id 之外的任何身份信息；provider 是白名单常量。
				logger.Warn("login_success 审计入队失败（登录已成功，不回滚）",
					"user_id", userID, "provider", provider, "error", err)
			}
		}
		if checkIn == nil {
			return
		}
		res, err := checkIn(ctx, userID)
		if err != nil {
			logger.Warn("签到写入失败", "user_id", userID, "error", err)
			return
		}
		if res.Unlocked {
			logger.Info("用户达成连续签到成就", "user_id", userID, "streak", res.Streak)
		}
	}
}
