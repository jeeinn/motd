-- 0004_threads_status：主题软删除状态（P1-M1 / R9）
--
-- status 取 active/deleted/hidden，DEFAULT 'active' 是常量，SQLite 会在
-- ALTER TABLE 时用它自动回填所有历史行，无需额外 UPDATE，也不会破坏旧数据。
-- 公开读取（ListThreads / GetThread）只认 active：把主题置为 deleted/hidden
-- 等同于让它从公开视图消失，而不物理删除（与 posts.status 的软删除口径一致）。
ALTER TABLE threads
  ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
  CHECK (status IN ('active','deleted','hidden'));
