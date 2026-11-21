-- 0010_eggs：v0.6.0 彩蛋基础设施（签到 + 用户级隐藏版块解锁）
--
-- 1) daily_checkins：每日签到，按 (user_id, local_date) 唯一。
--    local_date 用 MOTD_DISPLAY_TIMEZONE 的本地日期（YYYY-MM-DD），由应用写入。
--    签到是幂等的：同一天多次登录只留一条。
-- 2) user_node_unlocks：用户级隐藏版块解锁（仅 /dev/null 使用）。
--    按用户而非全局，未解锁用户的列表查询过滤该节点。
-- 3) /dev/null 种子节点：status='hidden'，默认所有人不可见，
--    只有 user_node_unlocks 有记录的用户能看到。

CREATE TABLE IF NOT EXISTS daily_checkins (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  local_date TEXT NOT NULL,             -- YYYY-MM-DD（展示时区本地日期）
  created_at INTEGER NOT NULL,
  UNIQUE (user_id, local_date)
);

CREATE INDEX IF NOT EXISTS idx_daily_checkins_user_date
  ON daily_checkins(user_id, local_date DESC);

CREATE TABLE IF NOT EXISTS user_node_unlocks (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  node_id    INTEGER NOT NULL REFERENCES nodes(id),
  unlocked_at INTEGER NOT NULL,
  UNIQUE (user_id, node_id)
);

INSERT OR IGNORE INTO nodes (slug, name, description, status) VALUES
  ('/dev/null', '黑洞', '此处空空如也——也可能是别有洞天', 'hidden');
