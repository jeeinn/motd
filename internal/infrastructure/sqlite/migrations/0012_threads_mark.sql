-- 0013_threads_mark：v0.11.0 / R-15 主题置顶（ADR-079）
--
-- threads.mark 是列表排序的独立前缀层：置顶主题在版块内/跨版块列表前置，
-- 并带 [置顶] 前缀。不改变 SortLatest/SortScore 语义，不参与 karma、不加权。
-- mark ∈ {0,1}：0=普通，1=置顶。用 CHECK 收敛取值域（同 0004 的 status 口径），
-- 历史行 DEFAULT 0 天然回填，无需 UPDATE。
--
-- 部分索引只覆盖置顶行：方案 C（分层 keyset）按 mark 分两路查询，
-- mark=1 那一路用这个部分索引 seek，实测 SEARCH 无 TEMP B-TREE；
-- 它不影响 idx_threads_last_reply（0011）与现有 EXPLAIN 硬断言。
ALTER TABLE threads ADD COLUMN mark INTEGER NOT NULL DEFAULT 0 CHECK (mark IN (0, 1));

CREATE INDEX IF NOT EXISTS idx_threads_pinned
  ON threads(last_reply_at DESC, id DESC) WHERE mark = 1;
