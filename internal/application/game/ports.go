package game

// 端口定义在使用方（ADR 架构约定）：application/game 声明它需要的存储能力，
// infrastructure/sqlite 实现。本层不 import sqlite，也不 import forum——
// 两条独立业务线零耦合（ADR-078：游戏分绝不写 users.karma）。
//
// 原子性：SubmitRecord 内部的实现必须用同一个写事务完成
// 「CountToday 计数 → INSERT game_scores → 条件 UPSERT 周榜」，
// 靠 ADR-010 单 worker 串行保证限流原子（免分布式锁）。
import "context"

// ScoreRecord 是一次结算的写入数据。score 由服务端领域状态机算出——
// 本结构**没有分数入参给客户端**，cmd 层只喂按键（ADR-078 防伪造）。
type ScoreRecord struct {
	UserID    int64
	GameKey   string // 服务端白名单常量（2048/snake）
	Score     int    // 领域状态机产出，非用户提交
	IsValid   bool   // 有效局门槛（domain 判定）：false 时 cmd 层不调用 SubmitRecord
	MaxPerDay int    // 每日计分局数上限（application 规则值，Store 在同事务内据此判定 counted）
	WeekKey   string // ISO 周，如 2026-W48
	LocalDate string // YYYY-MM-DD（展示时区 ADR-046）
	Duration  int    // 秒
	CreatedAt int64  // Unix 秒
}

// Store 是 game 用例需要的全部存储能力（读端口与写端口合一，由同一仓储实现，
// 内部读走 pools 读池、写走 writer 单写队列）。
type Store interface {
	// SubmitRecord 在同一写事务内完成计数+写分+条件 UPSERT 周榜。
	// 返回本局实际是否计入（用例已判定 IsCounted，仓储再以 CountToday 复核一次防绕过）。
	SubmitRecord(ctx context.Context, rec ScoreRecord) (counted bool, err error)

	// Top 返回周榜前 N：按 best_score DESC，只含 counted 局聚合结果。
	Top(ctx context.Context, weekKey, gameKey string, n int) ([]Entry, error)

	// AllTime 返回历史荣誉墙：从周表按 user_id 取 MAX(best_score)，不建新表。
	AllTime(ctx context.Context, gameKey string, n int) ([]Entry, error)
}

// Entry 是榜单一行。Username 由 cmd 层用 RenderUser 逐段渲染（绝不拼进 RenderSystem）。
type Entry struct {
	UserID   int64
	Username string
	Score    int
	WeekKey  string
	GameKey  string
}
