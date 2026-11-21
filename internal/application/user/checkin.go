package user

// 每日签到与连续天数（v0.6.0 / P3-M6 扩展）。
// 登录成功后调用 CheckIn：幂等记录当天日期、计算连续天数、达标经 AchievementGate
// 申请 streak_7 成就。本层不碰数据库、不算日期字符串——这些由注入的端口完成。

import (
	"context"
	"fmt"
	"time"

	domainuser "github.com/jeeinn/motd/internal/domain/user"
)

// CheckInStore 是签到写入端口（接口在使用方）。
type CheckInStore interface {
	// Record 幂等写入某用户某本地日期的签到（UNIQUE 由仓储保证）。
	Record(ctx context.Context, userID int64, localDate string, at time.Time) error
	// Streak 返回截至 localDate 的连续签到天数：从当天往前数，缺一天即断。
	// 当天未签到时不计入（调用方先 Record 再读）。
	Streak(ctx context.Context, userID int64, localDate string) (int, error)
}

// CheckInService 编排签到与成就申请。
type CheckInService struct {
	store CheckInStore
	gate  AchievementGate
	now   func() time.Time
	loc   *time.Location
	goal  int
}

func NewCheckInService(store CheckInStore, gate AchievementGate, now func() time.Time, loc *time.Location) *CheckInService {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.UTC
	}
	return &CheckInService{store: store, gate: gate, now: now, loc: loc, goal: 7}
}

// CheckInResult 是签到结果，供展示 ASCII 进度条。
type CheckInResult struct {
	LocalDate string
	Streak    int
	Goal      int
	Unlocked  bool // 本次是否首次达标
}

// CheckIn 记录今天签到并返回连续天数。同一天重复调用只记录一条、
// 连续天数不变；达标只申请一次成就（底层 Gate 幂等）。
func (s *CheckInService) CheckIn(ctx context.Context, userID int64) (CheckInResult, error) {
	now := s.now().In(s.loc)
	date := now.Format("2006-01-02")
	if err := s.store.Record(ctx, userID, date, now); err != nil {
		return CheckInResult{}, fmt.Errorf("记录签到: %w", err)
	}
	streak, err := s.store.Streak(ctx, userID, date)
	if err != nil {
		return CheckInResult{}, fmt.Errorf("计算连续天数: %w", err)
	}
	res := CheckInResult{LocalDate: date, Streak: streak, Goal: s.goal}
	if streak >= s.goal {
		if s.gate != nil {
			if _, err := s.gate.Apply(ctx, AchievementRequest{
				UserID:   userID,
				Code:     domainuser.AchievementStreak7,
				Evidence: fmt.Sprintf("checkin:%s streak=%d", date, streak),
			}); err != nil {
				return CheckInResult{}, fmt.Errorf("申请签到成就: %w", err)
			}
		}
		res.Unlocked = true
	}
	return res, nil
}

// ProgressBar 返回 ASCII 进度条，固定文案由本地数字拼成（不含用户数据）。
func (s *CheckInService) ProgressBar(streak int) string {
	n := streak
	if n > s.goal {
		n = s.goal
	}
	bar := ""
	for i := 0; i < s.goal; i++ {
		if i < n {
			bar += "█" // U+2588 全块，可读且非控制字符
		} else {
			bar += "░" // U+2591 浅块
		}
	}
	return fmt.Sprintf("签到 %s %d/%d 天", bar, n, s.goal)
}
