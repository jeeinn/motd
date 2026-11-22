-- 0011_ops_visibility：v0.7.1 运营可见性（/latest 跨版块列表 + /stats 统计看板）
--
-- 三件事一次落库，编号只前进一个（P4.5-PLAN §4.5 DoD1）：
--
-- 1) idx_threads_last_reply：跨版块最新列表的排序索引。
--    现有 idx_threads_node(node_id, last_reply_at) 的首列是 node_id，
--    跨版块查询(t.node_id IN (...), ORDER BY last_reply_at DESC, id DESC)用不上它，
--    会退化成全表扫 + temp b-tree 排序。该索引与查找键完全同序，EXPLAIN 验证走索引扫描。
--
-- 2) stats_daily：每日峰值在线（全局每日一行，无 user_id）。
--    local_date 语义与 daily_checkins 一致：MOTD_DISPLAY_TIMEZONE 的本地日期 YYYY-MM-DD
--    （ADR-046 口径）。只升不降由 UPSERT 的 MAX 保证；不做任何历史回填——
--    进程未运行的时段不可观测，宁缺勿造（P4.5-PLAN §4.3 / ADR-064）。
--    updated_at 是「该行最后一次真的变高」的 Unix 秒（只在 MAX 生效时推进），
--    供运维判断最后观测时刻；只升不降由单条 INSERT ... ON CONFLICT 的 MAX 保证
--    （不在 Go 侧读旧值再比较，跨天也不重置）。
--
-- 3) idx_auth_events_event：按事件类型计数（累计登录人次 = COUNT(event='login_success')）。
--    现有索引只有 created_at，按 event 过滤会全表扫。
--
-- 全部 IF NOT EXISTS：与 0003/0010 同口径，手工重放、从旧备份恢复后版本登记丢失都安全。

CREATE INDEX IF NOT EXISTS idx_threads_last_reply
  ON threads(last_reply_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS stats_daily (
  local_date  TEXT NOT NULL PRIMARY KEY,     -- YYYY-MM-DD（展示时区本地日期）
  peak_online INTEGER NOT NULL DEFAULT 0,    -- 当日观测到的峰值在线连接数
  updated_at  INTEGER NOT NULL DEFAULT 0 CHECK (updated_at >= 0), -- 最后一次真的变高（Unix 秒）
  -- SQLite 的 TEXT PRIMARY KEY **允许 NULL 且允许多行 NULL**（QA 实测）：
  -- 一次空日期写入就会造出多行「无日期」记录，把 MAX(peak_online) 全历史污染成
  -- 一条无日期的最大值。NOT NULL 是唯一防线；CHECK 再挡空串/短串，
  -- 与 repo_checkin.validDate 的 YYYY-MM-DD 口径对齐。
  -- 只校验长度挡不住 "2026/11/22"——必须校验**形状**。
  CHECK (local_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  -- 负值没有任何语义，直接拒绝：守住「MAX 只增不减」的前提（否则一个负数
  -- 也能被 MAX 静默吃掉，问题拖到排查时才暴露）。
  CHECK (peak_online >= 0)
);

CREATE INDEX IF NOT EXISTS idx_auth_events_event
  ON auth_events(event);
