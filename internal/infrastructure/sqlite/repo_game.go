package sqlite

// /game 仓储（R-14 / ADR-078）。实现 application/game.Store。
// 读走 Pools 只读门面；写走 Writer.Submit 单事务。
// 关键：SubmitRecord 在同一 *sql.Tx 内完成 CountToday → INSERT → 条件 UPSERT，
// 靠 ADR-010 单 worker 串行化保证限流原子（第 11、12 局不会被误判绕过）。
import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jeeinn/motd/internal/application/game"
)

var _ game.Store = (*GameStore)(nil)

type GameStore struct {
	pools  *Pools
	writer *Writer
}

func NewGameStore(pools *Pools, writer *Writer) *GameStore {
	return &GameStore{pools: pools, writer: writer}
}

// SubmitRecord 在同一写事务内：统计当日行数 → 写 game_scores（含超限局留痕）→
// 仅 counted 时 UPSERT 周榜。返回实际是否计入。
func (s *GameStore) SubmitRecord(ctx context.Context, rec game.ScoreRecord) (bool, error) {
	var counted bool
	err := s.writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var today int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM game_scores WHERE user_id=? AND game_key=? AND local_date=?`,
			rec.UserID, rec.GameKey, rec.LocalDate,
		).Scan(&today); err != nil {
			return fmt.Errorf("统计当日局数: %w", err)
		}
		counted = today < rec.MaxPerDay
		isCounted := 0
		if counted {
			isCounted = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO game_scores (user_id, game_key, score, is_counted, duration_sec, local_date, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rec.UserID, rec.GameKey, rec.Score, isCounted, rec.Duration, rec.LocalDate, rec.CreatedAt,
		); err != nil {
			return fmt.Errorf("写入成绩: %w", err)
		}
		if counted {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO game_leaderboard_week (week_key, game_key, user_id, best_score, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT(week_key, game_key, user_id)
				 DO UPDATE SET best_score = MAX(best_score, excluded.best_score), updated_at = excluded.updated_at`,
				rec.WeekKey, rec.GameKey, rec.UserID, rec.Score, rec.CreatedAt, rec.CreatedAt,
			); err != nil {
				return fmt.Errorf("更新周榜: %w", err)
			}
		}
		return nil
	})
	return counted, err
}

// Top 查周榜前 N，join users 取用户名（注销用户保留原名，cmd 层仍走 RenderUser）。
// gameKey 为空串表示**不过滤游戏**（/game top 无参）：跨游戏按分数统一排序，
// Entry.GameKey 带来源游戏供 cmd 层显示。绝不用字符串拼接 SQL——空串语义靠
// 参数化条件 (?=” OR game_key=?) 表达，保持零注入面。
func (s *GameStore) Top(ctx context.Context, weekKey, gameKey string, n int) ([]game.Entry, error) {
	if n <= 0 {
		n = 10
	}
	rows, err := s.pools.QueryContext(ctx,
		`SELECT w.user_id, COALESCE(u.username, ''), w.best_score, w.week_key, w.game_key
		   FROM game_leaderboard_week w LEFT JOIN users u ON u.id = w.user_id
		  WHERE w.week_key=? AND (?='' OR w.game_key=?)
		  ORDER BY w.best_score DESC, w.id ASC LIMIT ?`,
		weekKey, gameKey, gameKey, n,
	)
	if err != nil {
		return nil, fmt.Errorf("查询周榜: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

// AllTime 从周表按 user_id 取 MAX(best_score) 聚合历史荣誉墙，不建新表。
func (s *GameStore) AllTime(ctx context.Context, gameKey string, n int) ([]game.Entry, error) {
	if n <= 0 {
		n = 10
	}
	// gameKey 为空串同样表示不过滤游戏（/game top all 无参）。
	rows, err := s.pools.QueryContext(ctx,
		`SELECT w.user_id, COALESCE(u.username, ''), MAX(w.best_score) AS best, '', w.game_key
		   FROM game_leaderboard_week w LEFT JOIN users u ON u.id = w.user_id
		  WHERE ?='' OR w.game_key=?
		  GROUP BY w.user_id
		  ORDER BY best DESC, w.user_id ASC LIMIT ?`,
		gameKey, gameKey, n,
	)
	if err != nil {
		return nil, fmt.Errorf("查询总榜: %w", err)
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) ([]game.Entry, error) {
	var out []game.Entry
	for rows.Next() {
		var e game.Entry
		if err := rows.Scan(&e.UserID, &e.Username, &e.Score, &e.WeekKey, &e.GameKey); err != nil {
			return nil, fmt.Errorf("扫描榜单: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历榜单: %w", err)
	}
	return out, nil
}
