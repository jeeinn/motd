-- 0013_game_scores：v0.11.0 /game 游戏指令与排行榜（R-14 / ADR-078）。
-- 注意：ADR-078 原写 0012，但 0012_threads_mark 已被 R-15 占用，故顺延为 0013。
--
-- 两张表：
--   game_scores        每局成绩流水（有效局 + 超限局都写，is_counted 留痕）
--   game_leaderboard_week 周榜增量聚合（仅 is_counted=true 的局 UPSERT best_score）
--
-- 限流陷阱（ADR-078 范围裁定 3）：CountToday 数的是 game_scores 当日行数，
-- 因此超限局（is_counted=false）也必须写本表，否则第 12 局会被误判"当日仅 10 局"
-- 而绕过上限。is_valid=false 的局完全不写本表。
CREATE TABLE IF NOT EXISTS game_scores (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  game_key    TEXT    NOT NULL,           -- 服务端白名单标识（2048/snake），不接收用户输入
  score       INTEGER NOT NULL CHECK (score >= 0),
  is_counted  INTEGER NOT NULL CHECK (is_counted IN (0, 1)), -- 1=计入榜单，0=每日超限局
  duration_sec INTEGER NOT NULL DEFAULT 0 CHECK (duration_sec >= 0),
  local_date  TEXT    NOT NULL,           -- YYYY-MM-DD（MOTD_DISPLAY_TIMEZONE，ADR-046）
  created_at  INTEGER NOT NULL            -- Unix 秒
);

-- Top N：按游戏取高分，仅 counted 局参与；同分先到者靠前。
CREATE INDEX IF NOT EXISTS idx_game_scores_top
  ON game_scores(game_key, is_counted, score DESC, created_at ASC, id ASC);

-- 每日次数限流：按 (user_id, game_key, local_date) 计数当日行数（含超限局）。
CREATE INDEX IF NOT EXISTS idx_game_scores_user_day
  ON game_scores(user_id, game_key, local_date);

-- 周榜：当周每用户每游戏最佳单局分。结算增量 UPSERT，rank 查询期派生不落库。
-- week_key 形如 2026-W48（ISO 周），跨周自然隔离。
CREATE TABLE IF NOT EXISTS game_leaderboard_week (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  week_key   TEXT    NOT NULL,
  game_key   TEXT    NOT NULL,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  best_score INTEGER NOT NULL CHECK (best_score >= 0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (week_key, game_key, user_id)
);

-- Top N：按周按游戏取最高分。
CREATE INDEX IF NOT EXISTS idx_game_week_top
  ON game_leaderboard_week(week_key, game_key, best_score DESC, id ASC);
