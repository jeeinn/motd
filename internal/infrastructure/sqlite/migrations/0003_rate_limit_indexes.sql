-- 0003_rate_limit_indexes：P0-10 发帖限流查询所需的覆盖索引
--
-- CreateThread / CreatePost 会在写事务内调用 RecentWriteStats，按 (user_id, created_at)
-- 统计最近一小时的主题 + 内容数量与最早/最晚时间。0001 只有
-- idx_threads_node(node_id, last_reply_at) 与 idx_posts_thread(thread_id, created_at)，
-- 按 user_id 过滤会退化为全表扫描；每次写入都扫一遍在压测阶段必然暴露。
--
-- 用 CREATE INDEX IF NOT EXISTS：重复迁移、从旧备份恢复后版本登记丢失时都安全。
CREATE INDEX IF NOT EXISTS idx_threads_user_created ON threads(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_posts_user_created   ON posts(user_id, created_at);
