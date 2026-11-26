package sqlite

// R-14 / ADR-078 限流陷阱专项：第 11、12 局不得绕过每日上限。
// 关键不变量：CountToday 数的是 game_scores 当日行数，**超限局也必须写表**（is_counted=0），
// 否则第 12 局会被误判"当日仅 10 局"而重新计入。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/game"
)

func setupGameStore(t *testing.T) (*GameStore, context.Context, int64) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, t.TempDir()+"/motd.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	writer := pools.NewWriter()
	writer.Start()
	t.Cleanup(writer.Stop)
	now := time.Now().Unix()
	var uid int64
	if err := writer.Submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`, "gamer", now, now)
		if err != nil {
			return err
		}
		uid, _ = res.LastInsertId()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return NewGameStore(pools, writer), ctx, uid
}

func TestGameDailyLimitCannotBeBypassed(t *testing.T) {
	store, ctx, uid := setupGameStore(t)
	const limit = 10
	const total = 12
	rec := game.ScoreRecord{
		UserID: uid, GameKey: "2048", MaxPerDay: limit,
		WeekKey: "2026-W48", LocalDate: "2026-11-25", Duration: 30,
	}
	for i := 1; i <= total; i++ {
		rec.Score = i * 2 // 递增，便于验证最佳分
		rec.CreatedAt = int64(1764000000 + i)
		counted, err := store.SubmitRecord(ctx, rec)
		if err != nil {
			t.Fatalf("第 %d 局提交失败: %v", i, err)
		}
		want := i <= limit
		if counted != want {
			t.Fatalf("第 %d 局 counted=%v，期望 %v", i, counted, want)
		}
	}

	// ① 12 局全部落 game_scores（含超限局留痕）。
	var rows int
	if err := store.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM game_scores WHERE user_id=? AND game_key=?`, uid, "2048",
	).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != total {
		t.Fatalf("game_scores 应有 %d 行（含超限局），实际 %d", total, rows)
	}

	// ② 超限局 is_counted=0，且恰好 2 行。
	var over int
	if err := store.pools.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM game_scores WHERE user_id=? AND is_counted=0`, uid,
	).Scan(&over); err != nil {
		t.Fatal(err)
	}
	if over != 2 {
		t.Fatalf("超限局应为 2 行（is_counted=0），实际 %d", over)
	}

	// ③ 周榜只聚合 counted 局的最佳分：第 10 局 score=20（第 11、12 局 22/24 不计）。
	entries, err := store.Top(ctx, "2026-W48", "2048", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Score != 20 {
		t.Fatalf("周榜应为单用户最佳 20（前 10 局最高），实际 %+v", entries)
	}
	if entries[0].Username != "gamer" {
		t.Fatalf("榜单用户名投影失败: %q", entries[0].Username)
	}

	// ④ 总榜与周榜口径一致：同样只取 20。
	all, err := store.AllTime(ctx, "2048", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Score != 20 {
		t.Fatalf("总榜应与周榜一致取 20，实际 %+v", all)
	}
}

func TestGameBestScoreIsMaxNotSum(t *testing.T) {
	store, ctx, uid := setupGameStore(t)
	base := game.ScoreRecord{
		UserID: uid, GameKey: "snake", MaxPerDay: 10,
		WeekKey: "2026-W48", LocalDate: "2026-11-25", Duration: 20,
	}
	for i, score := range []int{10, 50, 30} {
		base.Score = score
		base.CreatedAt = int64(1764100000 + i)
		if _, err := store.SubmitRecord(ctx, base); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.Top(ctx, "2026-W48", "snake", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Score != 50 {
		t.Fatalf("最佳成绩制应取最高 50 而非累加 90，实际 %+v", entries)
	}
}

// 2026-11-26 实测 bug 复现：结算写 game_key='2048'，但 /game top 无参时
// 把空串当具体 game_key 查询，导致"已计入周榜"却"暂无记录"。
func TestGameTopWithoutGameKeyReturnsAllGames(t *testing.T) {
	store, ctx, uid := setupGameStore(t)
	rec := game.ScoreRecord{
		UserID: uid, GameKey: "2048", Score: 100, MaxPerDay: 10,
		WeekKey: "2026-W48", LocalDate: "2026-11-26", CreatedAt: 1764100000,
	}
	counted, err := store.SubmitRecord(ctx, rec)
	if err != nil || !counted {
		t.Fatalf("结算应计入: counted=%v err=%v", counted, err)
	}
	// 无 game_key 过滤（对应 /game top 无参）应返回全部游戏成绩。
	entries, err := store.Top(ctx, "2026-W48", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Score != 100 || entries[0].GameKey != "2048" {
		t.Fatalf("无游戏名查询应返回 2048 的成绩，实际 %+v", entries)
	}
	all, err := store.AllTime(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].GameKey != "2048" {
		t.Fatalf("总榜无游戏名查询应返回 2048 的成绩，实际 %+v", all)
	}
}
