package game

// /game 用例层（R-14 / ADR-078）。负责：
//   - is_counted 判定（当日本游戏前 10 局计入，第 11 局起仍可玩但不计入）
//   - 展示时区的 local_date 与 ISO week_key 计算（ADR-046）
//   - 周榜 / 总榜查询裁剪
//
// 不碰终端、不碰 ESC、不 import presentation。分数由领域状态机算出，
// 本层只编排存储，绝不接受客户端传入分数（ADR-078 防伪造）。
import (
	"context"
	"fmt"
	"time"
)

const dailyLimit = 10 // 每日每游戏计分局数上限（观察期可校准参数）

// Service 是游戏积分与榜单用例。
// 榜单用户名由 Store 查询时 join users 投影，cmd 层拿到后必须逐段 RenderUser。
type Service struct {
	store Store
	now   func() time.Time
	loc   *time.Location
}

func NewService(store Store, now func() time.Time, loc *time.Location) *Service {
	if now == nil {
		now = time.Now
	}
	if loc == nil {
		loc = time.UTC
	}
	return &Service{store: store, now: now, loc: loc}
}

// SubmitResult 是结算结果，供 cmd 层给固定提示（不回显用户输入）。
type SubmitResult struct {
	Counted   bool   // 是否计入榜单
	Reason    string // 固定提示码（"ok" / "invalid" / "daily_limit"）
	LocalDate string
	WeekKey   string
}

// Submit 结算一局。is_valid=false 直接拒绝（不写任何成绩表）；
// 否则判定 is_counted 并交给 Store 原子落库。
//
// 写库矩阵（ADR-078 范围裁定 3）：
//   - is_valid=false            → 什么都不写（仅返回 invalid）
//   - is_valid=true, counted=false → 写 game_scores（is_counted=0 留痕），不 UPSERT 周表
//   - 两者 true                  → 写 game_scores + UPSERT 周表
func (s *Service) Submit(ctx context.Context, key string, score int, isValid bool, userID int64, duration int) (SubmitResult, error) {
	now := s.now().In(s.loc)
	localDate := now.Format("2006-01-02")
	weekKey := isoWeekKey(now)
	if !isValid {
		return SubmitResult{Reason: "invalid", LocalDate: localDate, WeekKey: weekKey}, nil
	}
	rec := ScoreRecord{
		UserID: userID, GameKey: key, Score: score,
		IsValid: true, MaxPerDay: dailyLimit, // 上限由 application 定；counted 由 Store 在同事务内 CountToday 判定
		WeekKey: weekKey, LocalDate: localDate,
		Duration: duration, CreatedAt: now.Unix(),
	}
	counted, err := s.store.SubmitRecord(ctx, rec)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("记录成绩: %w", err)
	}
	reason := "daily_limit"
	if counted {
		reason = "ok"
	}
	return SubmitResult{Counted: counted, Reason: reason, LocalDate: localDate, WeekKey: weekKey}, nil
}

// isoWeekKey 返回 YYYY-Www（ISO 8601 周）。
func isoWeekKey(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// Top 返回本周周榜前 N，并投影用户名。
func (s *Service) Top(ctx context.Context, key string, n int) ([]Entry, error) {
	if n <= 0 {
		n = 10
	}
	weekKey := isoWeekKey(s.now().In(s.loc))
	entries, err := s.store.Top(ctx, weekKey, key, n)
	if err != nil {
		return nil, fmt.Errorf("查询周榜: %w", err)
	}
	return entries, nil
}

// AllTime 返回历史荣誉墙前 N，并投影用户名。
func (s *Service) AllTime(ctx context.Context, key string, n int) ([]Entry, error) {
	if n <= 0 {
		n = 10
	}
	entries, err := s.store.AllTime(ctx, key, n)
	if err != nil {
		return nil, fmt.Errorf("查询总榜: %w", err)
	}
	return entries, nil
}

// CurrentWeekKey 供 cmd 层展示当前周标识（固定文案的一部分）。
func (s *Service) CurrentWeekKey() string {
	return isoWeekKey(s.now().In(s.loc))
}
