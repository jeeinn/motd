-- 0009_edited_at：内容编辑标记（P2-M2 / ADR-051）
--
-- NULL = 从未编辑；非 NULL = 编辑时刻的 Unix 秒。
-- 不加 NOT NULL / DEFAULT：NULL 本身就是「未编辑」语义，历史行天然保持 NULL，
-- 无需回填，也不改动 status/created_at/last_reply_at 的既有取值。
--
-- 正文存在 posts（主题首帖也是 posts 的一行），threads 不存正文，故只加 posts：
-- 详情页的「已编辑」标记读首帖 edited_at，回复的编辑时间同样落在本行。
-- 编辑只 UPDATE body_md/edited_at，不推进 last_reply_at（同投票 AdjustScore 的教训 ADR-029）。
ALTER TABLE posts
  ADD COLUMN edited_at INTEGER;   -- NULL = 从未编辑；Unix 秒
