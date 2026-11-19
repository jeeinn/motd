-- 0006_domain_event_outbox：领域事件事务性发件箱（P1-M1 / R12）
--
-- 事件与业务写在同一事务内落库（WriteTx.AppendOutbox），提交后由 outbox
-- 调度器领取并投递；投递状态留在表里，进程崩溃后重启可重扫 pending。
-- event_id UNIQUE 让同一事件（幂等键 DomainEvent.ID）无法重复入库；
-- 订阅者侧仍按 DomainEvent.ID 去重，覆盖「投递后崩溃、重启重投」的至少一次语义。
CREATE TABLE domain_event_outbox (
  id           INTEGER PRIMARY KEY,
  event_id     TEXT NOT NULL UNIQUE,
  type         TEXT NOT NULL,
  payload      TEXT NOT NULL,
  status       TEXT NOT NULL CHECK(status IN ('pending','dispatching','delivered','failed')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  delivered_at INTEGER
);
CREATE INDEX idx_outbox_status_created ON domain_event_outbox(status, created_at);
