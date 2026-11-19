-- 0007_posts_fts：正文全文索引（P1-M3 / FTS5 trigram）
--
-- 外部内容表（content='posts'）：索引只存倒排数据，正文仍以 posts 为准，
-- 避免出现「索引里有一份、表里另一份」的双写真相。rowid 直接对应 posts.id。
--
-- tokenize='trigram' 的原因：默认 unicode61 分词对中文整句不切词，trigram
-- 按 3 字符滑窗切分，中日韩短语与英文子串都能命中；代价是 <3 字符的查询
-- 无结果，这类查询在应用层回退 LIKE（见 application/forum/search.go）。
--
-- posts_fts_indexed 记录哪些 rowid 真正进过索引。FTS5 外部内容表对「索引里
-- 没有的行」执行 'delete' 命令会损坏索引（实测报 disk image is malformed），
-- 而直接 SELECT rowid FROM posts_fts 又会从外部表合成行、无法判断是否已索引。
-- 因此用这张小表作为唯一事实来源：只有确认在索引里的行才发 'delete'。
--
-- status 转 deleted/hidden 时从索引移除，与 PostReader 只暴露 status='normal'
-- 的可见性口径保持一致。

CREATE VIRTUAL TABLE posts_fts USING fts5(
  body_md,
  content='posts',
  content_rowid='id',
  tokenize='trigram'
);

CREATE TABLE posts_fts_indexed (
  post_id INTEGER PRIMARY KEY
);

-- 存量数据回填：升级路径上 posts 已有行，索引必须立刻可查。
INSERT INTO posts_fts(rowid, body_md)
SELECT id, body_md FROM posts WHERE status = 'normal';
INSERT INTO posts_fts_indexed(post_id)
SELECT id FROM posts WHERE status = 'normal';

CREATE TRIGGER posts_fts_insert AFTER INSERT ON posts
WHEN NEW.status = 'normal'
BEGIN
  INSERT INTO posts_fts(rowid, body_md) VALUES (NEW.id, NEW.body_md);
  INSERT OR IGNORE INTO posts_fts_indexed(post_id) VALUES (NEW.id);
END;

CREATE TRIGGER posts_fts_delete AFTER DELETE ON posts
BEGIN
  -- 顺序不能反：先按「曾索引」标记删倒排项，再删标记。
  INSERT INTO posts_fts(posts_fts, rowid, body_md)
  SELECT 'delete', OLD.id, OLD.body_md
  WHERE EXISTS (SELECT 1 FROM posts_fts_indexed WHERE post_id = OLD.id);
  DELETE FROM posts_fts_indexed WHERE post_id = OLD.id;
END;

CREATE TRIGGER posts_fts_update AFTER UPDATE ON posts
BEGIN
  INSERT INTO posts_fts(posts_fts, rowid, body_md)
  SELECT 'delete', OLD.id, OLD.body_md
  WHERE EXISTS (SELECT 1 FROM posts_fts_indexed WHERE post_id = OLD.id);
  DELETE FROM posts_fts_indexed WHERE post_id = OLD.id;
  INSERT INTO posts_fts(rowid, body_md)
  SELECT NEW.id, NEW.body_md WHERE NEW.status = 'normal';
  INSERT OR IGNORE INTO posts_fts_indexed(post_id)
  SELECT NEW.id WHERE NEW.status = 'normal';
END;
