-- 0008_nodes_status：版块归档状态（P2-M0 / ADR-048 节点三不变量）
--
-- 与 0004_threads_status 同构：status 取 active/archived，DEFAULT 'active' 是常量，
-- SQLite 在 ALTER TABLE 时用它自动回填全部历史行（含 0002 的 5 个 seed 节点），
-- 无需 UPDATE，也不改动 slug/name/description。
--
-- 为什么不物理删：threads.node_id INTEGER NOT NULL REFERENCES nodes(id)，
-- 有帖节点物理删会被外键拒绝；归档只改 status，公开列表按 active 过滤，
-- 与 threads/posts 的软删口径一致（ADR-038）。
--
-- 本迁移不加 merged_into_id：节点合并已推 P3，M0 不做。
ALTER TABLE nodes
  ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
  CHECK (status IN ('active','archived'));
