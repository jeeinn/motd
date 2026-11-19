-- 0005_post_parent_thread：限定回复的父帖必须与新帖在同一主题（P1-M1 / R8）
--
-- 只加触发器、不重建 posts 表：触发器对历史数据零改写，也不触碰既有外键。
-- SQLite 的 CREATE TRIGGER 一次只能声明一个事件（不支持 INSERT, UPDATE 连写），
-- 因此拆成 BEFORE INSERT 与 BEFORE UPDATE 两条，覆盖「插入时指向跨主题父帖」
-- 和「之后把子帖改到别的主题」两条路径；NOT EXISTS 同时挡住悬空父帖。
-- 应用层 InsertPost 也会在同一事务里先读父帖 thread_id 再插（双保险，错误更可读）。
CREATE TRIGGER posts_parent_same_thread_insert
BEFORE INSERT ON posts
WHEN NEW.parent_post_id IS NOT NULL
BEGIN
  SELECT CASE
    WHEN NOT EXISTS (
      SELECT 1 FROM posts
      WHERE id = NEW.parent_post_id AND thread_id = NEW.thread_id
    )
    THEN RAISE(ABORT, 'parent post must be in same thread')
  END;
END;

CREATE TRIGGER posts_parent_same_thread_update
BEFORE UPDATE ON posts
WHEN NEW.parent_post_id IS NOT NULL
BEGIN
  SELECT CASE
    WHEN NOT EXISTS (
      SELECT 1 FROM posts
      WHERE id = NEW.parent_post_id AND thread_id = NEW.thread_id
    )
    THEN RAISE(ABORT, 'parent post must be in same thread')
  END;
END;
